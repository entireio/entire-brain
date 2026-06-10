package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// historyEvalFixture returns a brain dir with two session transcripts and the
// manifest that names them, plus an in-memory history index whose records
// point at those transcripts (the provenance link eval-gen labels through).
func historyEvalFixture(t *testing.T) (string, *exportManifest, historyIndex) {
	t.Helper()
	brainDir := t.TempDir()
	transcripts := map[string]string{
		"sessions/main/s1.jsonl": `{"type":"user","message":{"content":"add retry backoff to the fetcher"}}` + "\n",
		"sessions/main/s2.jsonl": `{"type":"user","message":{"content":"why does the exporter skip unchanged sessions"}}` + "\n",
	}
	for rel, content := range transcripts {
		p := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	manifest := &exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "session-aaaa-1111", Branch: "main", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-2 * time.Hour)},
			{SessionID: "session-bbbb-2222", Branch: "main", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	index := historyIndex{GeneratedAt: now, Records: []historyRecord{
		{ID: "r1", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 3, Summary: "decided to use exponential backoff with jitter for fetch retries"},
		{ID: "r2", Kind: "learning", Path: "sessions/main/s1.jsonl", Line: 9, Summary: "the fetcher must cap retry attempts at five to bound latency"},
		{ID: "r3", Kind: "request", Path: "sessions/main/s1.jsonl", Line: 1, Summary: "add retry backoff to the fetcher"},
		{ID: "r4", Kind: "decision", Path: "sessions/main/s2.jsonl", Line: 4, Summary: "the export cursor reuses unchanged transcripts so refresh stays incremental"},
		{ID: "r5", Kind: "validation", Path: "sessions/main/s2.jsonl", Line: 8, Summary: "verified the exporter cache hit rate on the live brain"},
	}}
	return brainDir, manifest, index
}

func TestGenerateHistoryEvalTasksLabelsByTranscript(t *testing.T) {
	brainDir, manifest, index := historyEvalFixture(t)
	tasks := generateHistoryEvalTasks(brainDir, manifest, index, "", 2, 0, 0)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d: %+v", len(tasks), tasks)
	}
	byTask := map[string]evalTask{}
	for _, task := range tasks {
		byTask[task.Task] = task
	}
	s1, ok := byTask["add retry backoff to the fetcher"]
	if !ok {
		t.Fatalf("s1's opening request not recovered as a task: %+v", tasks)
	}
	// r3 is kind=request: the general arm can never surface it, so labeling it
	// would cap recall at an unreachable target.
	if len(s1.Relevant) != 2 || s1.Relevant[0] != "r1" || s1.Relevant[1] != "r2" {
		t.Fatalf("s1 labels = %v, want [r1 r2] (request-kind excluded)", s1.Relevant)
	}
	if s1.QueryType != queryTypeHowto {
		t.Errorf("s1 query type = %q, want howto", s1.QueryType)
	}

	// min-records: raising the floor above s2's 2 rankable records drops it.
	tasks = generateHistoryEvalTasks(brainDir, manifest, index, "", 3, 0, 0)
	if len(tasks) != 0 {
		t.Fatalf("min-records=3 should disqualify both sessions, got %d", len(tasks))
	}
}

// TestHistoryEvalBM25SurfacesWhatSubstringMisses is the matches-null defect in
// miniature, as an eval measurement: a paraphrase query sharing one
// high-value term with the relevant record scores zero on the coverage-gated
// substring arm but is surfaced (and measured) by the BM25 arm.
func TestHistoryEvalBM25SurfacesWhatSubstringMisses(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{GeneratedAt: time.Now(), Records: []historyRecord{
		{ID: "r1", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 1, Summary: "chose model2vec embeddings for the semantic recall backend"},
		{ID: "r2", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 5, Summary: "progress output goes to stderr to keep json clean"},
		{ID: "r3", Kind: "decision", Path: "sessions/main/s2.jsonl", Line: 2, Summary: "the watcher debounces filesystem events before refresh"},
	}}
	tasks := []evalTask{{ID: "t1", Task: "what did we pick for embeddings and what were the alternatives", Relevant: []string{"r1"}}}

	subArm, err := selectHistoryEvalArm("substring", brainDir, index, historyFTSRelevanceCutoff)
	if err != nil {
		t.Fatal(err)
	}
	subRes, err := runHistoryEval(tasks, 5, subArm)
	if err != nil {
		t.Fatal(err)
	}
	bmArm, err := selectHistoryEvalArm("bm25", brainDir, index, historyFTSRelevanceCutoff)
	if err != nil {
		t.Fatal(err)
	}
	bmRes, err := runHistoryEval(tasks, 5, bmArm)
	if err != nil {
		t.Fatal(err)
	}
	if bmRes[0].RelevantSurfaced != 1 || bmRes[0].Recall != 1 {
		t.Fatalf("bm25 arm should surface r1: %+v", bmRes[0])
	}
	if subRes[0].RelevantSurfaced >= bmRes[0].RelevantSurfaced {
		t.Fatalf("expected the coverage-gated substring arm to miss what bm25 finds (the defect being measured); substring=%d bm25=%d",
			subRes[0].RelevantSurfaced, bmRes[0].RelevantSurfaced)
	}
	if bmRes[0].Tokens <= 0 || bmRes[0].UsefulPer1k <= 0 {
		t.Fatalf("metrics not computed: %+v", bmRes[0])
	}
}

// TestHistoryEvalCutoffSweep proves --cutoff is a real lever: the weak
// one-term tail survives cutoff 0 and is trimmed at a high cutoff.
func TestHistoryEvalCutoffSweep(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{GeneratedAt: time.Now(), Records: []historyRecord{
		{ID: "strong", Kind: "decision", Path: "p1", Line: 1, Summary: "embedding cache invalidation uses the model fingerprint"},
		{ID: "weak", Kind: "decision", Path: "p2", Line: 1, Summary: "the cache directory is created lazily"},
	}}
	query := "embedding cache invalidation fingerprint"
	loose, ok := rankHistoryViaFTSCutoff(brainDir, index, "history", query, 10, 0)
	if !ok || len(loose) != 2 {
		t.Fatalf("cutoff 0 should keep the weak one-term tail, got %d (ok=%v)", len(loose), ok)
	}
	tight, ok := rankHistoryViaFTSCutoff(brainDir, index, "history", query, 10, 0.99)
	if !ok || len(tight) != 1 || tight[0].Record.ID != "strong" {
		t.Fatalf("cutoff 0.99 should trim to the top match, got %+v (ok=%v)", tight, ok)
	}
}

// fixedEmbedder is a deterministic stub: texts map to fixed vectors, unknown
// text to the zero vector. The optional query prefix mirrors queryEmbedder.
type fixedEmbedder struct {
	vecs map[string][]float32
	dim  int
}

func (f *fixedEmbedder) Embed(text string) []float32 {
	if v, ok := f.vecs[text]; ok {
		return v
	}
	return make([]float32, f.dim)
}
func (f *fixedEmbedder) Dim() int   { return f.dim }
func (f *fixedEmbedder) ID() string { return "fixed-test" }

// TestHistoryFusedReachesTermDisjoint: the reachability claim for the
// prospective fused arm — a record sharing no term with the query is
// unreachable by BM25 but surfaces through the semantic side of the fusion.
func TestHistoryFusedReachesTermDisjoint(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{GeneratedAt: time.Now(), Records: []historyRecord{
		{ID: "target", Kind: "decision", Path: "p1", Line: 1, Summary: "indentation uses spaces, never tabs"},
		{ID: "noise1", Kind: "decision", Path: "p2", Line: 1, Summary: "release artifacts publish nightly"},
		{ID: "noise2", Kind: "decision", Path: "p3", Line: 1, Summary: "reviews need two approvals"},
	}}
	query := "whitespace convention"
	e := &fixedEmbedder{dim: 3, vecs: map[string][]float32{
		query:                                 {1, 0, 0},
		"indentation uses spaces, never tabs": {0.95, 0.05, 0},
		"release artifacts publish nightly":   {0, 1, 0},
		"reviews need two approvals":          {0, 0, 1},
	}}

	// BM25 alone cannot reach the term-disjoint record.
	if scored, ok := rankHistoryViaFTSCutoff(brainDir, index, "history", query, 5, historyFTSRelevanceCutoff); ok {
		for _, s := range scored {
			if s.Record.ID == "target" {
				t.Fatalf("test premise broken: bm25 reached the term-disjoint record")
			}
		}
	}
	got, err := newHistoryFusedRanker(brainDir, index, e, historyFTSRelevanceCutoff).rank(query, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "target" {
		t.Fatalf("fused arm should surface the term-disjoint record first, got %+v", got)
	}
}
