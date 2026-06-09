package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	res, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, evalRetrieverFacts)
	if err != nil {
		t.Fatalf("runFactsEval: %v", err)
	}
	if len(res) != 1 || !res[0].Labeled {
		t.Fatalf("expected one labeled result, got %+v", res)
	}
	if res[0].RelevantSurfaced != 1 {
		t.Fatalf("expected f1 surfaced and counted, got %+v", res[0])
	}
	if res[0].RelevanceSource != evalRelevanceExplicitLabel {
		t.Fatalf("labeled eval relevance source = %q, want %q", res[0].RelevanceSource, evalRelevanceExplicitLabel)
	}

	// Judge mode: fake agent marks the first surfaced fact relevant.
	fakeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "1 yes\n", nil
	}
	tasksJ := []evalTask{{ID: "t2", Task: "checkpoints", Branch: "main"}}
	resJ, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasksJ, 10, true, fakeRun, []string{"fake"}, loadJudgeCache(""), nil, nil, evalRetrieverFacts)
	if err != nil {
		t.Fatalf("runFactsEval judge: %v", err)
	}
	if resJ[0].Labeled {
		t.Errorf("judge result should not be marked labeled")
	}
	if resJ[0].RelevantSurfaced != 1 {
		t.Errorf("judge should have marked one fact relevant, got %d", resJ[0].RelevantSurfaced)
	}
	if resJ[0].RelevanceSource != evalRelevanceJudge {
		t.Errorf("judge relevance source = %q, want %q", resJ[0].RelevanceSource, evalRelevanceJudge)
	}
	if resJ[0].UsefulPer1k <= 0 {
		t.Errorf("useful/1k should be > 0 when a fact was judged relevant")
	}
}

func TestRunFactsEvalRetrieverArms(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	transcriptRel := "sessions/main/s1.jsonl"
	transcriptPath := filepath.Join(brainDir, filepath.FromSlash(transcriptRel))
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte("alpha raw checkpoint guidance"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := exportSession{SessionID: "session-one", Branch: "main", TranscriptPath: transcriptRel, LatestCheckpoint: "cp1", CreatedAt: now}
	history := historyIndex{GeneratedAt: now, Records: []historyRecord{{
		ID: "history:one", Kind: "decision", Path: transcriptRel, Line: 1, Summary: "alpha history checkpoint guidance",
	}}}
	historyData, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), historyData, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{session}},
			History:  &historySourceManifest{GeneratedAt: now, IndexPath: historyIndexPath, Records: 1, Decisions: 1},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	p := normalizeFactPaths([]string{"architecture.data.flow"})
	fact := factRecord{ID: factRecordID("alpha fact checkpoint guidance", p), Paths: p, Text: "alpha fact checkpoint guidance", Branch: "main", Status: factStatusActive, UpdatedAt: now}
	if err := writeFacts(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}

	tasks := []evalTask{{ID: "t1", Task: "alpha checkpoint guidance", Branch: "main", Relevant: []string{fact.ID}, SourceSessionID: session.SessionID, SourceTranscriptPath: transcriptRel}}
	for _, retriever := range []string{evalRetrieverFacts, evalRetrieverHistory, evalRetrieverQuery, evalRetrieverRawSessions} {
		res, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, retriever)
		if err != nil {
			t.Fatalf("%s eval: %v", retriever, err)
		}
		if len(res) != 1 || res[0].Retriever != retriever {
			t.Fatalf("%s result missing retriever: %+v", retriever, res)
		}
		if res[0].RelevantSurfaced != 1 || res[0].Tokens == 0 {
			t.Fatalf("%s should surface its relevant item with tokens, got %+v", retriever, res[0])
		}
		if (retriever == evalRetrieverFacts || retriever == evalRetrieverQuery) && !res[0].Labeled {
			t.Fatalf("%s should retain explicit relevance labels: %+v", retriever, res[0])
		}
		if (retriever == evalRetrieverFacts || retriever == evalRetrieverQuery) && res[0].RelevanceSource != evalRelevanceExplicitLabel {
			t.Fatalf("%s relevance source = %q, want %q", retriever, res[0].RelevanceSource, evalRelevanceExplicitLabel)
		}
		if retriever != evalRetrieverFacts && retriever != evalRetrieverQuery && res[0].Labeled {
			t.Fatalf("%s source-match relevance should not claim recall labels: %+v", retriever, res[0])
		}
		if (retriever == evalRetrieverHistory || retriever == evalRetrieverRawSessions) && res[0].RelevanceSource != evalRelevanceSourceMatch {
			t.Fatalf("%s relevance source = %q, want %q", retriever, res[0].RelevanceSource, evalRelevanceSourceMatch)
		}
	}

	calls := 0
	judgeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "1 no\n", nil
	}
	res, err := runFactsEvalWithOptions(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, true, judgeRun, []string{"fake"}, loadJudgeCache(""), nil, nil, evalRetrieverRawSessions, factsEvalRunOptions{JudgeSourceMatches: true})
	if err != nil {
		t.Fatalf("raw-session judged source matches: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected judge call for source-match row, got %d", calls)
	}
	if len(res) != 1 || res[0].RelevanceSource != evalRelevanceJudge || res[0].RelevantSurfaced != 0 {
		t.Fatalf("judge should replace source-match proxy relevance, got %+v", res)
	}
}

func TestRunFactsEvalSourceMatchMissDoesNotAutoJudge(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	transcriptRel := "sessions/main/s1.jsonl"
	if err := os.MkdirAll(filepath.Join(brainDir, filepath.Dir(transcriptRel)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(transcriptRel)), []byte("alpha raw checkpoint guidance"), 0o600); err != nil {
		t.Fatal(err)
	}
	history := historyIndex{GeneratedAt: now, Records: []historyRecord{{
		ID: "history:other", Kind: "decision", Path: "sessions/main/other.jsonl", Line: 1, Summary: "alpha history checkpoint guidance",
	}}}
	historyData, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), historyData, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{{SessionID: "session-one", Branch: "main", TranscriptPath: transcriptRel, CreatedAt: now}}},
			History:  &historySourceManifest{GeneratedAt: now, IndexPath: historyIndexPath, Records: 1, Decisions: 1},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	tasks := []evalTask{{ID: "t1", Task: "alpha checkpoint guidance", Branch: "main", SourceSessionID: "session-one", SourceTranscriptPath: transcriptRel}}
	calls := 0
	judgeRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "1 yes\n", nil
	}
	res, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, true, judgeRun, []string{"fake"}, loadJudgeCache(""), nil, nil, evalRetrieverHistory)
	if err != nil {
		t.Fatalf("history eval: %v", err)
	}
	if calls != 0 {
		t.Fatalf("source-match proxy miss should not call judge without --judge-source-matches; calls=%d", calls)
	}
	if len(res) != 1 || res[0].RelevanceSource != evalRelevanceSourceMatch || res[0].RelevantSurfaced != 0 {
		t.Fatalf("expected source-match miss with zero relevance, got %+v", res)
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
	if _, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, true, run, []string{"fake"}, loadJudgeCache(cachePath), nil, nil, evalRetrieverFacts); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 agent call on first run, got %d", calls)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	// Second run: served from cache, no agent call.
	if _, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, true, run, []string{"fake"}, loadJudgeCache(cachePath), nil, nil, evalRetrieverFacts); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("second run should reuse cache, but agent was called again (calls=%d)", calls)
	}
}

func TestJudgeCacheInvalidatesWhenTaskTextChanges(t *testing.T) {
	cache := loadJudgeCache("")
	item := evalRetrievedItem{ID: "raw:s1:1-1", Path: "sessions/main/s1.jsonl:1", Text: "checkpoint setup"}
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		if calls == 1 {
			return "1 yes\n", nil
		}
		return "1 no\n", nil
	}
	first, err := judgeRelevanceItems(context.Background(), run, "/repo", []string{"fake"}, evalTask{ID: "t1", Task: "old task"}, evalRetrieverRawSessions, []evalRetrievedItem{item}, cache)
	if err != nil {
		t.Fatalf("first judge: %v", err)
	}
	if _, ok := first[item.ID]; !ok {
		t.Fatalf("first verdict should be relevant")
	}
	second, err := judgeRelevanceItems(context.Background(), run, "/repo", []string{"fake"}, evalTask{ID: "t1", Task: "new task"}, evalRetrieverRawSessions, []evalRetrievedItem{item}, cache)
	if err != nil {
		t.Fatalf("second judge: %v", err)
	}
	if calls != 2 {
		t.Fatalf("changed task text should bypass cached verdict; calls=%d", calls)
	}
	if _, ok := second[item.ID]; ok {
		t.Fatalf("second verdict should reflect fresh no judgment, got %+v", second)
	}
}

func TestJudgeCacheInvalidatesWhenRetrievedTextChanges(t *testing.T) {
	cache := loadJudgeCache("")
	task := evalTask{ID: "t1", Task: "check checkpoint setup"}
	oldItem := evalRetrievedItem{ID: "raw:s1:1-1", Path: "sessions/main/s1.jsonl:1", Text: "old checkpoint setup"}
	newItem := evalRetrievedItem{ID: "raw:s1:1-1", Path: "sessions/main/s1.jsonl:1", Text: "new checkpoint setup"}
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		if calls == 1 {
			return "1 yes\n", nil
		}
		return "1 no\n", nil
	}
	if _, err := judgeRelevanceItems(context.Background(), run, "/repo", []string{"fake"}, task, evalRetrieverRawSessions, []evalRetrievedItem{oldItem}, cache); err != nil {
		t.Fatalf("first judge: %v", err)
	}
	second, err := judgeRelevanceItems(context.Background(), run, "/repo", []string{"fake"}, task, evalRetrieverRawSessions, []evalRetrievedItem{newItem}, cache)
	if err != nil {
		t.Fatalf("second judge: %v", err)
	}
	if calls != 2 {
		t.Fatalf("changed retrieved text should bypass cached verdict; calls=%d", calls)
	}
	if _, ok := second[newItem.ID]; ok {
		t.Fatalf("second verdict should reflect fresh no judgment, got %+v", second)
	}
}

func TestFactsEvalRejectsJudgeSourceMatchesWithoutJudge(t *testing.T) {
	dir := t.TempDir()
	tasksPath := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(tasksPath, []byte(`[{"id":"t1","task":"check retrieval"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newFactsEvalCommand(Options{})
	_, err := execute(t, cmd, "--tasks", tasksPath, "--judge-source-matches")
	if err == nil || !strings.Contains(err.Error(), "--judge-source-matches requires --judge") {
		t.Fatalf("expected --judge-source-matches validation error, got %v", err)
	}
}

func TestFactsEvalRejectsSemanticForNonFactsRetrievers(t *testing.T) {
	dir := t.TempDir()
	tasksPath := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(tasksPath, []byte(`[{"id":"t1","task":"check retrieval"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, retriever := range []string{evalRetrieverHistory, evalRetrieverQuery, evalRetrieverRawSessions} {
		cmd := newFactsEvalCommand(Options{})
		_, err := execute(t, cmd, "--tasks", tasksPath, "--retriever", retriever, "--semantic")
		if err == nil || !strings.Contains(err.Error(), "--semantic requires --retriever facts") {
			t.Fatalf("%s: expected --semantic retriever validation error, got %v", retriever, err)
		}
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
