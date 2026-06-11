package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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
	tasks := []evalTask{{ID: "t1", Task: "checkpoints v1.1 read ref", Branch: "main", Relevant: []string{f1.ID}, LabelSource: evalLabelSourceHuman}}
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
	if res[0].LabelSource != evalLabelSourceHuman {
		t.Fatalf("label source = %q, want %q", res[0].LabelSource, evalLabelSourceHuman)
	}

	silverTasks := []evalTask{{ID: "silver", Task: "checkpoints v1.1 read ref", Branch: "main", Relevant: []string{f1.ID}, LabelSource: evalLabelSourceProvenanceSilver}}
	silver, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", silverTasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, evalRetrieverFacts)
	if err != nil {
		t.Fatalf("runFactsEval silver: %v", err)
	}
	if len(silver) != 1 || silver[0].Labeled || silver[0].Recall != 0 {
		t.Fatalf("silver labels must not count as proof labels: %+v", silver)
	}
	if silver[0].RelevanceSource != evalRelevanceSilverLabel || silver[0].LabelSource != evalLabelSourceProvenanceSilver {
		t.Fatalf("silver label metadata wrong: %+v", silver[0])
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

	tasks := []evalTask{{ID: "t1", Task: "alpha checkpoint guidance", Branch: "main", Relevant: []string{fact.ID}, LabelSource: evalLabelSourceHuman, SourceSessionID: session.SessionID, SourceTranscriptPath: transcriptRel}}
	for _, retriever := range []string{evalRetrieverFacts, evalRetrieverHistory, evalRetrieverQuery, evalRetrieverRawSessions} {
		baseline, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, retriever)
		if err != nil {
			t.Fatalf("%s baseline eval: %v", retriever, err)
		}
		res, err := runFactsEvalWithOptions(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, retriever, factsEvalRunOptions{IncludeIDs: true})
		if err != nil {
			t.Fatalf("%s eval: %v", retriever, err)
		}
		if len(res) != 1 || res[0].Retriever != retriever {
			t.Fatalf("%s result missing retriever: %+v", retriever, res)
		}
		if len(res[0].RetrievedIDs) == 0 {
			t.Fatalf("%s include ids should retain surfaced ids: %+v", retriever, res[0])
		}
		if baseline[0].RetrievedIDs != nil {
			t.Fatalf("%s baseline should omit retrieved ids: %+v", retriever, baseline[0].RetrievedIDs)
		}
		if baseline[0].Tokens != res[0].Tokens || baseline[0].RelevantSurfaced != res[0].RelevantSurfaced || baseline[0].UsefulPer1k != res[0].UsefulPer1k {
			t.Fatalf("%s include ids changed metrics: baseline=%+v with_ids=%+v", retriever, baseline[0], res[0])
		}
		if res[0].RelevantSurfaced < 1 || res[0].Tokens == 0 {
			t.Fatalf("%s should surface relevant evidence with tokens, got %+v", retriever, res[0])
		}
		if retriever == evalRetrieverFacts && !res[0].Labeled {
			t.Fatalf("%s should retain explicit relevance labels: %+v", retriever, res[0])
		}
		if retriever == evalRetrieverFacts && res[0].RelevanceSource != evalRelevanceExplicitLabel {
			t.Fatalf("%s relevance source = %q, want %q", retriever, res[0].RelevanceSource, evalRelevanceExplicitLabel)
		}
		if retriever == evalRetrieverQuery && res[0].RelevanceSource != evalRelevanceMixedLabelSource {
			t.Fatalf("%s relevance source = %q, want %q", retriever, res[0].RelevanceSource, evalRelevanceMixedLabelSource)
		}
		if retriever != evalRetrieverFacts && res[0].Labeled {
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

func TestRunFactsEvalBranchScopesHistoryAndQuery(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	mainRel := "sessions/main/s1.jsonl"
	featureRel := "sessions/branches/feature/s2.jsonl"
	for rel, text := range map[string]string{
		mainRel:    "main-only-token checkpoint guidance",
		featureRel: "feature-only-token checkpoint guidance",
	} {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	history := historyIndex{GeneratedAt: now, Records: []historyRecord{
		{ID: "history:main", Kind: "decision", Path: mainRel, Line: 1, Summary: "main-only-token checkpoint guidance"},
		{ID: "history:feature", Kind: "decision", Path: featureRel, Line: 1, Summary: "feature-only-token checkpoint guidance"},
	}}
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
			Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
				{SessionID: "s1", Branch: "main", TranscriptPath: mainRel, CreatedAt: now},
				{SessionID: "s2", Branch: "feature", TranscriptPath: featureRel, CreatedAt: now.Add(time.Minute)},
			}},
			History: &historySourceManifest{GeneratedAt: now, IndexPath: historyIndexPath, Records: 2, Decisions: 2},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	mainTask := []evalTask{{ID: "main", Task: "feature-only-token", Branch: "main"}}
	for _, retriever := range []string{evalRetrieverHistory, evalRetrieverQuery, evalRetrieverRawSessions} {
		res, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", mainTask, 10, false, nil, nil, loadJudgeCache(""), nil, nil, retriever)
		if err != nil {
			t.Fatalf("%s main branch eval: %v", retriever, err)
		}
		if len(res) != 1 || res[0].Surfaced != 0 {
			t.Fatalf("%s leaked feature-branch evidence into main task: %+v", retriever, res)
		}
	}

	featureTask := []evalTask{{ID: "feature", Task: "feature-only-token", Branch: "feature"}}
	for _, retriever := range []string{evalRetrieverHistory, evalRetrieverQuery, evalRetrieverRawSessions} {
		res, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", featureTask, 10, false, nil, nil, loadJudgeCache(""), nil, nil, retriever)
		if err != nil {
			t.Fatalf("%s feature branch eval: %v", retriever, err)
		}
		if len(res) != 1 || res[0].Surfaced == 0 {
			t.Fatalf("%s should surface feature-branch evidence for feature task: %+v", retriever, res)
		}
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

func TestJudgeCacheRejectsInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "judge-cache.json")
	if err := os.WriteFile(path, []byte(`{not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadJudgeCache(path).validateLoaded(); err == nil {
		t.Fatal("expected malformed judge cache to be reported")
	}
}

func TestRawSessionSourceMatchRequiresCollisionSafeIDOrTranscript(t *testing.T) {
	sessionID := "abcdefghijkl-full-session-one"
	otherOldStyleID := "raw:" + shortSessionID(sessionID) + ":1-4"
	task := evalTask{
		ID:                   "t1",
		Task:                 "find transcript",
		SourceSessionID:      sessionID,
		SourceTranscriptPath: "sessions/main/one.jsonl",
	}
	if evalItemMatchesTaskSource(task, evalRetrievedItem{ID: otherOldStyleID, Path: "sessions/main/two.jsonl:1"}) {
		t.Fatal("short-session raw id should not override a mismatched transcript path")
	}
	if !evalItemMatchesTaskSource(task, evalRetrievedItem{ID: "raw:legacy:1-4", Path: "sessions/main/one.jsonl:3"}) {
		t.Fatal("matching transcript path should still count as source evidence")
	}
	if !evalItemMatchesTaskSource(evalTask{SourceSessionID: sessionID}, evalRetrievedItem{ID: "raw:" + rawSessionIDToken(sessionID) + ":1-4"}) {
		t.Fatal("hash-token raw id should match when no transcript path is available")
	}
}

func TestSourceMatchRequiresAnchorLineOverlapWhenPresent(t *testing.T) {
	sessionID := "session-with-lines"
	task := evalTask{
		ID:                   "t1",
		Task:                 "find line",
		SourceSessionID:      sessionID,
		SourceTranscriptPath: "sessions/main/one.jsonl",
		SourceLines:          []int{5},
	}
	if !evalItemMatchesTaskSource(task, evalRetrievedItem{ID: "history:one", Path: "sessions/main/one.jsonl:5"}) {
		t.Fatal("matching transcript line should count as source evidence")
	}
	if evalItemMatchesTaskSource(task, evalRetrievedItem{ID: "history:one", Path: "sessions/main/one.jsonl:6"}) {
		t.Fatal("same transcript but non-overlapping line should not count")
	}
	if evalItemMatchesTaskSource(task, evalRetrievedItem{ID: "history:one", Path: "sessions/main/two.jsonl:5"}) {
		t.Fatal("matching line in another transcript should not count")
	}
	if !evalItemMatchesTaskSource(task, evalRetrievedItem{ID: "raw:" + rawSessionIDToken(sessionID) + ":1-9", Path: "sessions/main/one.jsonl:1"}) {
		t.Fatal("raw-session chunk range should count when the anchor line is inside the chunk")
	}
	rawTask := evalTask{SourceSessionID: sessionID, SourceLines: []int{5}}
	if !evalItemMatchesTaskSource(rawTask, evalRetrievedItem{ID: "raw:" + rawSessionIDToken(sessionID) + ":4-6"}) {
		t.Fatal("raw-session chunk range should count when it overlaps the anchor line")
	}
	if evalItemMatchesTaskSource(rawTask, evalRetrievedItem{ID: "raw:" + rawSessionIDToken(sessionID) + ":6-8"}) {
		t.Fatal("raw-session chunk range should not count when it misses the anchor line")
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

func TestFactsEvalRawSessionsJSONIncludesProofBoundaryMetadata(t *testing.T) {
	repoDir := t.TempDir()
	dataDir := t.TempDir()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:      repoDir,
			PluginDataDir: dataDir,
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	transcriptRel := "sessions/main/session-one.jsonl"
	transcriptPath := filepath.Join(storage.BrainDir, filepath.FromSlash(transcriptRel))
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, []byte("alpha raw checkpoint guidance\nbeta unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{{
				SessionID:        "session-one",
				Branch:           "main",
				TranscriptPath:   transcriptRel,
				LatestCheckpoint: "cp1",
				CreatedAt:        now,
			}}},
		},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	tasksPath := filepath.Join(t.TempDir(), "tasks.json")
	tasks := []evalTask{{
		ID:                   "raw-one",
		Task:                 "alpha checkpoint guidance",
		Branch:               "main",
		SourceSessionID:      "session-one",
		SourceTranscriptPath: transcriptRel,
		SourceLines:          []int{1},
	}}
	data, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tasksPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "facts", "eval", "--tasks", tasksPath, "--retriever", evalRetrieverRawSessions, "--include-ids", "--json")
	if err != nil {
		t.Fatalf("facts eval raw-sessions --json: %v\n%s", err, out)
	}
	var summary evalSummary
	if err := json.Unmarshal([]byte(out), &summary); err != nil {
		t.Fatalf("parse summary: %v\n%s", err, out)
	}
	if summary.Retriever != evalRetrieverRawSessions || summary.RunConfig == nil {
		t.Fatalf("summary missing retriever/run_config: %+v", summary)
	}
	if summary.RunConfig.TasksSHA256 == "" || summary.RunConfig.BrainManifestSHA256 == "" {
		t.Fatalf("summary must retain tasks and brain hashes: %+v", summary.RunConfig)
	}
	if summary.RunConfig.RawSessionIDScheme != "raw:sha256(session_id)[:16]:start-end" {
		t.Fatalf("raw session id scheme not recorded: %+v", summary.RunConfig)
	}
	if !summary.RunConfig.IncludeIDs {
		t.Fatalf("include ids flag not recorded: %+v", summary.RunConfig)
	}
	if !strings.Contains(summary.RunConfig.TurnSigningLimitation, "turn-level") {
		t.Fatalf("turn-signing limitation not recorded: %+v", summary.RunConfig)
	}
	if len(summary.Results) != 1 || summary.Results[0].RelevanceSource != evalRelevanceSourceMatch || summary.Results[0].Labeled {
		t.Fatalf("raw-session source match should be proxy, not labeled proof: %+v", summary.Results)
	}
	if len(summary.Results[0].RetrievedIDs) != 1 || !strings.HasPrefix(summary.Results[0].RetrievedIDs[0], "raw:") {
		t.Fatalf("raw-session retrieved ids missing: %+v", summary.Results[0].RetrievedIDs)
	}
}

func TestFactsEvalCLICompareUsesProofLabeledRealSummaries(t *testing.T) {
	f := newVerifyFixture(t)
	transcriptRel := "sessions/main/session-one.jsonl"
	var needles []string
	for i := 1; i <= 12; i++ {
		needles = append(needles, fmt.Sprintf("needle-%02d", i))
	}
	f.writeBrainFile(t, transcriptRel, strings.Join(needles, " ")+" compact shared raw session context "+strings.Repeat("padding ", 160)+"\n")
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   f.now,
		RepoRoot:      f.repoDir,
		RepoKey:       f.storage.Key,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: f.now, DefaultBranch: "main", Sessions: []exportSession{{
				SessionID:        "session-one",
				Branch:           "main",
				TranscriptPath:   transcriptRel,
				LatestCheckpoint: "cp1",
				CreatedAt:        f.now,
			}}},
		},
	}
	if err := writeBrainManifestAndReadme(f.brainDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	rawID := rawSessionChunkID(exportSession{SessionID: "session-one"}, transcriptChunk{StartLine: 1, EndLine: 1})
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	var facts []factRecord
	var tasks []evalTask
	for i, needle := range needles {
		text := needle + " compact fact"
		fact := factRecord{
			ID:        factRecordID(text, paths),
			Paths:     paths,
			Text:      text,
			Branch:    "main",
			Origin:    factOriginDistilled,
			Status:    factStatusActive,
			UpdatedAt: f.now,
		}
		facts = append(facts, fact)
		tasks = append(tasks, evalTask{
			ID:          fmt.Sprintf("task-%02d", i+1),
			Task:        needle + " compact",
			Branch:      "main",
			K:           1,
			Relevant:    []string{fact.ID, rawID},
			LabelSource: evalLabelSourceHuman,
		})
	}
	f.writeFacts(t, "main", facts)
	tasksPath := filepath.Join(t.TempDir(), "tasks.json")
	data, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tasksPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	factsOut, err := execute(t, NewRootCommand(f.opts), "facts", "eval", "--tasks", tasksPath, "--retriever", evalRetrieverFacts, "--k", "1", "--include-ids", "--json")
	if err != nil {
		t.Fatalf("facts eval facts: %v\n%s", err, factsOut)
	}
	rawOut, err := execute(t, NewRootCommand(f.opts), "facts", "eval", "--tasks", tasksPath, "--retriever", evalRetrieverRawSessions, "--k", "1", "--include-ids", "--json")
	if err != nil {
		t.Fatalf("facts eval raw-sessions: %v\n%s", err, rawOut)
	}
	var factsSummary, rawSummary evalSummary
	if err := json.Unmarshal([]byte(factsOut), &factsSummary); err != nil {
		t.Fatalf("parse facts summary: %v\n%s", err, factsOut)
	}
	if err := json.Unmarshal([]byte(rawOut), &rawSummary); err != nil {
		t.Fatalf("parse raw summary: %v\n%s", err, rawOut)
	}
	if factsSummary.Retriever != evalRetrieverFacts || factsSummary.RunConfig == nil || !factsSummary.RunConfig.IncludeIDs || factsSummary.RunConfig.BrainManifestSHA256 == "" {
		t.Fatalf("facts summary missing proof config: %+v", factsSummary)
	}
	if rawSummary.Retriever != evalRetrieverRawSessions || rawSummary.RunConfig == nil || !rawSummary.RunConfig.IncludeIDs {
		t.Fatalf("raw summary missing proof config: %+v", rawSummary)
	}
	if factsSummary.RunConfig.BrainManifestSHA256 != rawSummary.RunConfig.BrainManifestSHA256 {
		t.Fatalf("brain hash mismatch: facts=%s raw=%s", factsSummary.RunConfig.BrainManifestSHA256, rawSummary.RunConfig.BrainManifestSHA256)
	}
	for _, summary := range []evalSummary{factsSummary, rawSummary} {
		if len(summary.Results) != len(tasks) {
			t.Fatalf("%s result count = %d, want %d", summary.Retriever, len(summary.Results), len(tasks))
		}
		for _, result := range summary.Results {
			if !result.Labeled || result.RelevanceSource != evalRelevanceExplicitLabel || result.LabelSource != evalLabelSourceHuman || len(result.RetrievedIDs) != 1 {
				t.Fatalf("%s result is not retained proof-labeled output: %+v", summary.Retriever, result)
			}
		}
	}
	dir := t.TempDir()
	factsPath := filepath.Join(dir, "facts.json")
	rawPath := filepath.Join(dir, "raw.json")
	if err := os.WriteFile(factsPath, []byte(factsOut), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawPath, []byte(rawOut), 0o600); err != nil {
		t.Fatal(err)
	}
	compareOut, err := execute(t, NewRootCommand(f.opts), "facts", "eval-compare", "--a", rawPath, "--b", factsPath, "--json")
	if err != nil {
		t.Fatalf("facts eval-compare: %v\n%s", err, compareOut)
	}
	var compare struct {
		N                   int                `json:"n"`
		ARetriever          string             `json:"a_retriever"`
		BRetriever          string             `json:"b_retriever"`
		ReleasePairingReady bool               `json:"release_pairing_ready"`
		ReleaseClaimable    bool               `json:"release_claimable"`
		Metrics             []metricComparison `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(compareOut), &compare); err != nil {
		t.Fatalf("parse compare: %v\n%s", err, compareOut)
	}
	if compare.N != len(tasks) || compare.ARetriever != evalRetrieverRawSessions || compare.BRetriever != evalRetrieverFacts || !compare.ReleasePairingReady || !compare.ReleaseClaimable {
		t.Fatalf("compare summary not release-ready: %+v\n%s", compare, compareOut)
	}
	var useful *metricComparison
	for i := range compare.Metrics {
		if compare.Metrics[i].Metric == "useful_per_1k" {
			useful = &compare.Metrics[i]
			break
		}
	}
	if useful == nil || useful.EvidenceBasis != evalMetricEvidenceProofLabels || useful.Winner != "b" || !useful.ReleaseClaimable {
		t.Fatalf("useful_per_1k should be a proof-labeled facts win: %+v\n%s", useful, compareOut)
	}
}

func TestPrintEvalSummaryShowsRelevanceSourceWarning(t *testing.T) {
	summary := evalSummary{
		Retriever: evalRetrieverRawSessions,
		Results: []evalTaskResult{
			{
				ID:               "raw-one",
				Task:             "alpha",
				Retriever:        evalRetrieverRawSessions,
				RelevanceSource:  evalRelevanceSourceMatch,
				Surfaced:         1,
				Tokens:           10,
				RelevantSurfaced: 1,
				Precision:        1,
				UsefulPer1k:      100,
			},
			{
				ID:              "fact-one",
				Task:            "alpha",
				Retriever:       evalRetrieverFacts,
				RelevanceSource: evalRelevanceExplicitLabel,
				LabelSource:     evalLabelSourceHuman,
				Surfaced:        1,
				Tokens:          8,
				Precision:       1,
				UsefulPer1k:     125,
				Labeled:         true,
			},
		},
	}
	summary = summarizeEval(summary.Results)
	cmd := &cobra.Command{Use: "eval"}
	var out strings.Builder
	cmd.SetOut(&out)
	printEvalSummary(cmd, summary)
	got := out.String()
	if !strings.Contains(got, "relevance") || !strings.Contains(got, evalRelevanceSourceMatch) {
		t.Fatalf("human summary should show relevance source:\n%s", got)
	}
	if !strings.Contains(got, "label") || !strings.Contains(got, evalLabelSourceHuman) {
		t.Fatalf("human summary should show label source:\n%s", got)
	}
	if !strings.Contains(got, "non-proof relevance rows") {
		t.Fatalf("human summary should warn for proxy rows:\n%s", got)
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
	os.WriteFile(path, []byte(`[{"id":"t1","task":"do X","relevant":["fact:a"],"label_source":"human"}]`), 0o600)
	tasks, err := loadEvalTasks(path)
	if err != nil || len(tasks) != 1 || tasks[0].ID != "t1" {
		t.Fatalf("loadEvalTasks: %v %+v", err, tasks)
	}
	os.WriteFile(path, []byte(`[{"id":"t1","task":"do X","relevant":["fact:a"]}]`), 0o600)
	if _, err := loadEvalTasks(path); err == nil || !strings.Contains(err.Error(), "label_source") {
		t.Fatalf("missing label_source should be rejected, got %v", err)
	}
	os.WriteFile(path, []byte(`[{"id":"t1","task":"do X","relevant":["fact:a"],"label_source":"typo"}]`), 0o600)
	if _, err := loadEvalTasks(path); err == nil || !strings.Contains(err.Error(), "label_source") {
		t.Fatalf("bad label_source should be rejected, got %v", err)
	}
	if _, err := loadEvalTasks(filepath.Join(dir, "missing.json")); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestFactsEvalHelpShowsValidLabeledTaskExample(t *testing.T) {
	cmd := newFactsEvalCommand(Options{})
	out, err := execute(t, cmd, "--help")
	if err != nil {
		t.Fatalf("facts eval help: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"relevant":["fact:abc","fact:def"],"label_source":"human"`) {
		t.Fatalf("help should show label_source with relevant labels:\n%s", out)
	}
}
