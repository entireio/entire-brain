package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEvalMetricsLabeled(t *testing.T) {
	surfaced := []factRecord{
		{ID: "a", Text: "relevant fact one", Paths: []string{"p.q.r"}},
		{ID: "b", Text: "irrelevant", Paths: []string{"p.q.r"}},
		{ID: "c", Text: "relevant fact two", Paths: []string{"p.q.r"}},
	}
	relevant := map[string]struct{}{"a": {}, "c": {}, "d": {}} // d not surfaced
	res := evalMetrics(surfaced, relevant, len(relevant))

	if res.RelevantSurfaced != 2 {
		t.Fatalf("relevant surfaced = %d, want 2", res.RelevantSurfaced)
	}
	if res.Precision != 2.0/3.0 {
		t.Errorf("precision = %v, want 0.667", res.Precision)
	}
	if res.Recall != 2.0/3.0 {
		t.Errorf("recall = %v, want 0.667 (2 of 3 relevant)", res.Recall)
	}
	if res.Tokens <= 0 {
		t.Errorf("tokens should be > 0")
	}
	// useful/1k = relevant(2) / (tokens/1000)
	want := 2.0 / (float64(res.Tokens) / 1000.0)
	if res.UsefulPer1k != want {
		t.Errorf("useful/1k = %v, want %v", res.UsefulPer1k, want)
	}
}

func TestEstimateTokensMonotonic(t *testing.T) {
	few := []factRecord{{Text: "short", Paths: []string{"a.b.c"}}}
	more := append([]factRecord(nil), few...)
	more = append(more, factRecord{Text: "a considerably longer fact statement here", Paths: []string{"a.b.c"}})
	if estimateTokens(more) <= estimateTokens(few) {
		t.Fatalf("more facts should estimate more tokens")
	}
	if estimateTokens(nil) != 0 {
		t.Fatalf("empty should be 0 tokens")
	}
}

func TestParseJudgeOutput(t *testing.T) {
	facts := []factRecord{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	out := "1 yes\n2 no\n3 YES\ngarbage\n9 yes" // 9 out of range, garbage skipped
	rel := parseJudgeOutput(out, facts)
	if _, ok := rel["a"]; !ok {
		t.Errorf("fact 1 should be relevant")
	}
	if _, ok := rel["b"]; ok {
		t.Errorf("fact 2 should not be relevant")
	}
	if _, ok := rel["c"]; !ok {
		t.Errorf("fact 3 (YES) should be relevant")
	}
	if len(rel) != 2 {
		t.Errorf("expected 2 relevant, got %d", len(rel))
	}
}

func TestRunFactsEvalLabeledAndJudge(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	mk := func(text, path string) factRecord {
		p := normalizeFactPaths([]string{path})
		return factRecord{ID: factRecordID(text, p), Paths: p, Text: text, Branch: "main", Status: factStatusActive, UpdatedAt: now}
	}
	f1 := mk("Checkpoints v1.1 read from a custom local ref", "architecture.data.flow")
	f2 := mk("The project uses Go modules", "project.tooling.stack")
	if err := writeFacts(brainDir, "main", []factRecord{f1, f2}); err != nil {
		t.Fatal(err)
	}

	// Labeled task: only f1 is relevant.
	tasks := []evalTask{{ID: "t1", Task: "checkpoints v1.1 read ref", Branch: "main", Relevant: []string{f1.ID}}}
	res, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil)
	if err != nil {
		t.Fatalf("runFactsEval: %v", err)
	}
	if len(res) != 1 || !res[0].Labeled {
		t.Fatalf("expected one labeled result, got %+v", res)
	}
	if res[0].RelevantSurfaced != 1 {
		t.Fatalf("expected f1 surfaced and counted, got %+v", res[0])
	}

	// Judge mode: fake agent marks the first surfaced fact relevant.
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "1 yes\n", nil
	}
	tasksJ := []evalTask{{ID: "t2", Task: "checkpoints", Branch: "main"}}
	resJ, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasksJ, 10, true, fakeRun, []string{"fake"}, loadJudgeCache(""), nil, nil)
	if err != nil {
		t.Fatalf("runFactsEval judge: %v", err)
	}
	if resJ[0].Labeled {
		t.Errorf("judge result should not be marked labeled")
	}
	if resJ[0].RelevantSurfaced != 1 {
		t.Errorf("judge should have marked one fact relevant, got %d", resJ[0].RelevantSurfaced)
	}
	if resJ[0].UsefulPer1k <= 0 {
		t.Errorf("useful/1k should be > 0 when a fact was judged relevant")
	}
}

func TestJudgeCacheReuse(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	p := normalizeFactPaths([]string{"architecture.data.flow"})
	f := factRecord{ID: factRecordID("checkpoint v1.1 read ref", p), Paths: p, Text: "checkpoint v1.1 read ref", Branch: "main", Status: factStatusActive, UpdatedAt: now}
	if err := writeFacts(brainDir, "main", []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	tasks := []evalTask{{ID: "t1", Task: "checkpoint read", Branch: "main"}}
	cachePath := filepath.Join(t.TempDir(), "judge-cache.json")

	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "1 yes\n", nil
	}
	// First run: judges and writes the cache.
	if _, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, true, run, []string{"fake"}, loadJudgeCache(cachePath), nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 agent call on first run, got %d", calls)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	// Second run: served from cache, no agent call.
	if _, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, true, run, []string{"fake"}, loadJudgeCache(cachePath), nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("second run should reuse cache, but agent was called again (calls=%d)", calls)
	}
}

func TestSummarizeEvalByStratum(t *testing.T) {
	results := []evalTaskResult{
		{ID: "a", QueryType: queryTypeCode, Tokens: 100, Precision: 0.8, UsefulPer1k: 8},
		{ID: "b", QueryType: queryTypeCode, Tokens: 200, Precision: 0.4, UsefulPer1k: 2},
		{ID: "c", QueryType: queryTypeConcept, Tokens: 100, Precision: 0.2, UsefulPer1k: 2},
	}
	s := summarizeEval(results)
	if len(s.ByStratum) != 2 {
		t.Fatalf("expected 2 strata, got %d", len(s.ByStratum))
	}
	code := s.ByStratum[queryTypeCode]
	if code.Tasks != 2 || code.MeanTokens != 150 || code.MeanPrecision < 0.59 || code.MeanPrecision > 0.61 {
		t.Fatalf("code stratum aggregation wrong: %+v", code)
	}
	if s.ByStratum[queryTypeConcept].Tasks != 1 {
		t.Fatalf("concept stratum should have 1 task")
	}
}

func TestLoadEvalTasks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	os.WriteFile(path, []byte(`[{"id":"t1","task":"do X","relevant":["fact:a"]}]`), 0o600)
	tasks, err := loadEvalTasks(path)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("loadEvalTasks: %v %+v", err, tasks)
	}
	if _, err := loadEvalTasks(filepath.Join(dir, "missing.json")); err == nil {
		t.Errorf("expected error for missing file")
	}
}
