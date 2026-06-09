package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// emptyEmbedder stands in for an unavailable backend (e.g. Ollama down): every
// call returns a nil vector, the failure mode the vector-rank guards must handle.
type emptyEmbedder struct{}

func (emptyEmbedder) Embed(string) []float32 { return nil }
func (emptyEmbedder) Dim() int               { return 768 }
func (emptyEmbedder) ID() string             { return "empty-embedder" }

func TestVectorRankedReturnsNothingWhenEmbedderUnavailable(t *testing.T) {
	dir := t.TempDir()
	facts := []factRecord{{ID: "fact:a", Text: "alpha"}, {ID: "fact:b", Text: "beta"}}
	if out := factsVectorRanked(dir, "main", facts, "q", emptyEmbedder{}, 10); len(out) != 0 {
		t.Fatalf("facts: empty embedder must yield no semantic results, got %d (arbitrary top-N)", len(out))
	}
	idx := docIndex{Records: []docRecord{{ID: "d1", Text: "x"}, {ID: "d2", Text: "y"}}}
	if out := docsVectorRanked(dir, idx, "q", emptyEmbedder{}, 10); len(out) != 0 {
		t.Fatalf("docs: empty embedder must yield no semantic results, got %d", len(out))
	}
}

func TestGetUnifiedBatchPreservesOrderAndReportsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, docDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	idx := docIndex{Records: []docRecord{{ID: "aaa", Text: "alpha"}, {ID: "bbb", Text: "beta"}}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(docIndexPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	found, missing, err := getUnifiedBatch(dir, "main", []string{"doc:bbb", "doc:nope", "doc:aaa"})
	if err != nil {
		t.Fatalf("getUnifiedBatch: %v", err)
	}
	if len(found) != 2 || found[0].ID != "doc:bbb" || found[1].ID != "doc:aaa" {
		t.Fatalf("expected input-order [doc:bbb doc:aaa], got %+v", found)
	}
	if len(missing) != 1 || missing[0] != "doc:nope" {
		t.Fatalf("expected missing [doc:nope], got %v", missing)
	}
}

func TestUnifiedIDsNotDoublePrefixed(t *testing.T) {
	// fact/history ids are already source-prefixed; docs are not.
	f := factsToUnified([]factRecord{{ID: "fact:abc", Text: "x"}})
	if f[0].ID != "fact:abc" {
		t.Fatalf("fact id double-prefixed: %s", f[0].ID)
	}
	h := historyToUnified([]scoredHistoryRecord{{Record: historyRecord{ID: "history:def", Summary: "y"}}})
	if h[0].ID != "history:def" {
		t.Fatalf("history id double-prefixed: %s", h[0].ID)
	}
	if d := docToUnified(docRecord{ID: "ghi", Text: "z"}); d.ID != "doc:ghi" {
		t.Fatalf("doc id: %s", d.ID)
	}
}

func TestRRFMergeUnifiedFavorsCrossListMatches(t *testing.T) {
	lists := [][]unifiedResult{
		{{ID: "a"}, {ID: "b"}},
		{{ID: "b"}, {ID: "c"}},
	}
	out := rrfMergeUnified(lists, 10)
	if len(out) != 3 || out[0].ID != "b" {
		t.Fatalf("expected b (in both lists) ranked first, got %v", out)
	}
}

func TestFactsVectorRankedRanksActiveButCachesAll(t *testing.T) {
	dir := t.TempDir()
	e := defaultEmbedder()
	facts := []factRecord{
		{ID: "fact:act", Text: "active checkpoint logic", Status: factStatusActive},
		{ID: "fact:sup", Text: "superseded note", Status: factStatusSuperseded},
	}
	out := factsVectorRanked(dir, "main", facts, "checkpoint", e, 10)
	// vsearch ranks active facts only.
	if len(out) != 1 || out[0].ID != "fact:act" {
		t.Fatalf("expected only the active fact ranked, got %v", out)
	}
	// But every present fact is cached on the shared store, matching the reranker's
	// retain-all convention (so vsearch doesn't churn the recall/brief cache).
	cache := newEmbedStore(dir, "main", e.ID(), e.Dim()).load()
	if _, ok := cache["fact:act"]; !ok {
		t.Fatal("active fact vector should be cached")
	}
	if _, ok := cache["fact:sup"]; !ok {
		t.Fatal("superseded fact vector should still be cached, not evicted")
	}
}

func TestPruneToPresentDropsDeparted(t *testing.T) {
	cache := map[string][]float32{"a": {1}, "b": {2}, "gone": {3}}
	present := map[string]struct{}{"a": {}, "b": {}}
	if !pruneToPresent(cache, present) {
		t.Fatal("expected pruneToPresent to report a removal")
	}
	if _, ok := cache["gone"]; ok {
		t.Fatal("departed id should be pruned")
	}
	if len(cache) != 2 {
		t.Fatalf("expected 2 entries after prune, got %d", len(cache))
	}
}

func TestQMDOutputFormatAlias(t *testing.T) {
	got, err := outputWantsJSON(false, "json")
	if err != nil || !got {
		t.Fatalf("--format json should request JSON, got %t err=%v", got, err)
	}
	got, err = outputWantsJSON(true, "cli")
	if err != nil || got {
		t.Fatalf("--format cli should force CLI output, got %t err=%v", got, err)
	}
	if _, err := outputWantsJSON(false, "xml"); err == nil {
		t.Fatal("unknown --format should error")
	}
}
