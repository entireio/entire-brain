package cli

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScoreFixture(t *testing.T, corpusDir string, sessionIDs []string, rawLines, traceLines string) (runDir, corpusPath string) {
	t.Helper()
	var entries []distillEvalCorpusEntry
	for _, id := range sessionIDs {
		entries = append(entries, distillEvalCorpusEntry{SessionID: id, Branch: "main", Golden: id == sessionIDs[0]})
	}
	corpus := distillEvalCorpus{RepoKey: "test/repo", CreatedAt: time.Now(), CorpusSHA256: "corpus-sha", Sessions: entries}
	corpusPath = filepath.Join(corpusDir, "corpus.json")
	data, _ := json.Marshal(corpus)
	if err := os.WriteFile(corpusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	runDir = filepath.Join(corpusDir, "runs", "base")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := distillEvalRunMeta{
		Variant:      distillEvalVariant{Name: "base", Agent: "claude-code", Model: "m", CapPerChunk: 2, MaxChunkBytes: 1024, TemplateSHA256: "tsha"},
		CorpusSHA256: "corpus-sha",
		StartedAt:    time.Now(),
		WallSeconds:  1.5,
	}
	metaData, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), metaData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "raw-facts.jsonl"), []byte(rawLines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "call-trace.jsonl"), []byte(traceLines), 0o600); err != nil {
		t.Fatal(err)
	}
	return runDir, corpusPath
}

func loadScoreSummary(t *testing.T, path string) distillEvalSummary {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s distillEvalSummary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func almostEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDistillEvalScoreIncludesZeroFactSessions(t *testing.T) {
	raw := `{"type":"chunk","session_id":"s1","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":1,"cap":2}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f1","kind":"gotcha","paths":["a.b.c"],"text":"alpha beta","checkpoint_id":"cp"}
{"type":"chunk","session_id":"s2","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":1,"cap":2}
{"type":"fact","session_id":"s2","branch":"main","chunk_index":1,"fact_id":"f2","kind":"gotcha","paths":["a.b.c"],"text":"gamma delta","checkpoint_id":"cp"}
`
	trace := `{"session":"s1","chunk":0,"input_tokens":100,"output_tokens":10}
{"session":"s2","chunk":0,"input_tokens":200,"output_tokens":20}
`
	runDir, corpusPath := writeScoreFixture(t, t.TempDir(), []string{"s1", "s2", "s3"}, raw, trace)
	out := filepath.Join(runDir, "summary.json")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", runDir, "--corpus", corpusPath, "--out", out); err != nil {
		t.Fatalf("score: %v", err)
	}
	s := loadScoreSummary(t, out)
	if len(s.Sessions) != 3 {
		t.Fatalf("got %d session results, want 3 (silent included): %+v", len(s.Sessions), s.Sessions)
	}
	byID := map[string]distillEvalSessionResult{}
	for _, r := range s.Sessions {
		byID[r.SessionID] = r
	}
	if byID["s3"].Facts != 0 || byID["s3"].Chunks != 0 {
		t.Errorf("silent session wrong: %+v", byID["s3"])
	}
	if byID["s1"].Facts != 1 || byID["s1"].Tokens != 110 || byID["s1"].AgentCalls != 1 {
		t.Errorf("s1 result wrong: %+v", byID["s1"])
	}
	if s.Facts != 2 || s.Cost.ExtractionCalls != 2 || s.Cost.Tokens != 330 || !almostEq(s.Cost.WallSeconds, 1.5) {
		t.Errorf("totals wrong: facts=%d cost=%+v", s.Facts, s.Cost)
	}
	if s.CorpusSHA256 != "corpus-sha" || s.Variant.Name != "base" {
		t.Errorf("metadata not carried: %+v", s)
	}
	if s.Precision.Golden != nil || s.Precision.Judge != nil || s.Precision.Combined != nil {
		t.Errorf("precision must be absent without labels/judge: %+v", s.Precision)
	}
}

func TestDistillEvalScorePooledPrecisionGoldenOverridesJudge(t *testing.T) {
	raw := `{"type":"chunk","session_id":"s1","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":2,"cap":6}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f1","kind":"gotcha","paths":["a.b.c"],"text":"alpha beta","checkpoint_id":"cp"}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f2","kind":"gotcha","paths":["a.b.c"],"text":"gamma delta","checkpoint_id":"cp"}
`
	runDir, corpusPath := writeScoreFixture(t, t.TempDir(), []string{"s1"}, raw, "")
	labelsPath := filepath.Join(runDir, "labels.jsonl")
	if err := os.WriteFile(labelsPath, []byte(`{"fact_id":"f1","verdict":"kill"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	judgePath := filepath.Join(runDir, "judge.jsonl")
	judge := `{"fact_id":"f1","verdict":"keep","judge_agent":"claude-code","judge_model":"claude-fable-5"}
{"fact_id":"f2","verdict":"keep","judge_agent":"claude-code","judge_model":"claude-fable-5"}
`
	if err := os.WriteFile(judgePath, []byte(judge), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(runDir, "summary.json")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", runDir, "--corpus", corpusPath, "--labels", labelsPath, "--judge", judgePath, "--out", out); err != nil {
		t.Fatalf("score: %v", err)
	}
	s := loadScoreSummary(t, out)
	if s.Precision.GoldenN != 1 || s.Precision.Golden == nil || !almostEq(*s.Precision.Golden, 0.0) {
		t.Errorf("golden precision wrong: %+v", s.Precision)
	}
	if s.Precision.JudgeN != 2 || s.Precision.Judge == nil || !almostEq(*s.Precision.Judge, 1.0) {
		t.Errorf("judge precision wrong: %+v", s.Precision)
	}
	// Combined: f1 kill (golden overrides judge keep), f2 keep → 0.5.
	if s.Precision.Combined == nil || !almostEq(*s.Precision.Combined, 0.5) {
		t.Errorf("combined precision wrong: %+v", s.Precision)
	}
	if s.JudgeConfig != "claude-code/claude-fable-5" {
		t.Errorf("judge config = %q", s.JudgeConfig)
	}
}

func TestDistillEvalScoreGoldenOnlyRestrictsSessions(t *testing.T) {
	raw := `{"type":"chunk","session_id":"s1","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":1,"cap":6}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f1","kind":"gotcha","paths":["a.b.c"],"text":"alpha beta","checkpoint_id":"cp"}
`
	runDir, corpusPath := writeScoreFixture(t, t.TempDir(), []string{"s1", "s2", "s3"}, raw, "")
	// Mark the run golden-only (writeScoreFixture marks only s1 golden in the corpus).
	metaPath := filepath.Join(runDir, "run.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta distillEvalRunMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta.GoldenOnly = true
	data, _ = json.Marshal(meta)
	if err := os.WriteFile(metaPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(runDir, "summary.json")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", runDir, "--corpus", corpusPath, "--out", out); err != nil {
		t.Fatalf("score: %v", err)
	}
	s := loadScoreSummary(t, out)
	if !s.GoldenOnly {
		t.Error("summary must be stamped golden_only")
	}
	if len(s.Sessions) != 1 || s.Sessions[0].SessionID != "s1" {
		t.Errorf("golden-only score must cover only golden sessions, got %+v", s.Sessions)
	}
}

func TestDistillEvalScoreRefusesIncompleteSessions(t *testing.T) {
	// s1: chunk_count says 2, only one chunk line arrived (a failed chunk).
	// s2: chunk line claims 2 facts, only one fact line survived.
	raw := `{"type":"chunk","session_id":"s1","branch":"main","chunk_index":1,"chunk_count":2,"facts_emitted":1,"cap":6}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"f1","kind":"gotcha","paths":["a.b.c"],"text":"alpha beta","checkpoint_id":"cp"}
{"type":"chunk","session_id":"s2","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":2,"cap":6}
{"type":"fact","session_id":"s2","branch":"main","chunk_index":1,"fact_id":"f2","kind":"gotcha","paths":["a.b.c"],"text":"gamma delta","checkpoint_id":"cp"}
`
	runDir, corpusPath := writeScoreFixture(t, t.TempDir(), []string{"s1", "s2"}, raw, "")
	out := filepath.Join(runDir, "summary.json")
	_, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", runDir, "--corpus", corpusPath, "--out", out)
	if err == nil || !strings.Contains(err.Error(), "s1") || !strings.Contains(err.Error(), "s2") {
		t.Fatalf("expected incomplete-session error naming s1 and s2, got %v", err)
	}
	if _, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", runDir, "--corpus", corpusPath, "--out", out, "--allow-incomplete"); err != nil {
		t.Fatalf("--allow-incomplete should pass: %v", err)
	}
	s := loadScoreSummary(t, out)
	if len(s.IncompleteSessions) != 2 {
		t.Errorf("summary must list incomplete sessions, got %+v", s.IncompleteSessions)
	}
}

func TestDistillEvalScoreCapShareAndRedundancy(t *testing.T) {
	// s1: chunk1 capped (2/2) with a near-dup pair, chunk2 under cap.
	// s2: one capped chunk whose two facts share an id (exact dup pair).
	raw := `{"type":"chunk","session_id":"s1","branch":"main","chunk_index":1,"chunk_count":2,"facts_emitted":2,"cap":2}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"fA","kind":"gotcha","paths":["a.b.c"],"text":"alpha beta gamma delta","checkpoint_id":"cp"}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":1,"fact_id":"fB","kind":"gotcha","paths":["a.b.c"],"text":"alpha beta gamma epsilon","checkpoint_id":"cp"}
{"type":"chunk","session_id":"s1","branch":"main","chunk_index":2,"chunk_count":2,"facts_emitted":1,"cap":2}
{"type":"fact","session_id":"s1","branch":"main","chunk_index":2,"fact_id":"fC","kind":"gotcha","paths":["a.b.c"],"text":"zeta eta theta iota","checkpoint_id":"cp"}
{"type":"chunk","session_id":"s2","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":2,"cap":2}
{"type":"fact","session_id":"s2","branch":"main","chunk_index":1,"fact_id":"fX","kind":"gotcha","paths":["a.b.c"],"text":"kappa lambda mu","checkpoint_id":"cp"}
{"type":"fact","session_id":"s2","branch":"main","chunk_index":1,"fact_id":"fX","kind":"gotcha","paths":["a.b.c"],"text":"kappa lambda mu","checkpoint_id":"cp"}
`
	runDir, corpusPath := writeScoreFixture(t, t.TempDir(), []string{"s1", "s2"}, raw, "")
	out := filepath.Join(runDir, "summary.json")
	if _, err := runDistillEvalCommand(t, t.TempDir(), "score", "--run", runDir, "--corpus", corpusPath, "--out", out); err != nil {
		t.Fatalf("score: %v", err)
	}
	s := loadScoreSummary(t, out)
	// Chunks: 3 total, 2 at cap → 2/3.
	if !almostEq(s.CapShare, 2.0/3.0) {
		t.Errorf("cap share = %v, want 2/3", s.CapShare)
	}
	// 5 fact lines; exact pairs: fX-fX = 1 → 0.2; near pairs: fA-fB (Jaccard 3/5) → 1 → 0.2.
	if s.Facts != 5 || !almostEq(s.Redundancy.ExactDupRate, 0.2) || !almostEq(s.Redundancy.NearDupRate, 0.2) {
		t.Errorf("redundancy wrong: facts=%d %+v", s.Facts, s.Redundancy)
	}
	byID := map[string]distillEvalSessionResult{}
	for _, r := range s.Sessions {
		byID[r.SessionID] = r
	}
	if byID["s1"].NearDupPairs != 1 || byID["s2"].NearDupPairs != 0 {
		t.Errorf("per-session near dup pairs wrong: %+v", s.Sessions)
	}
	if byID["s1"].Chunks != 2 || byID["s2"].Chunks != 1 {
		t.Errorf("per-session chunks wrong: %+v", s.Sessions)
	}
}
