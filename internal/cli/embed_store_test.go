package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEmbedStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newEmbedStore(dir, "main", "test-model", 3)
	want := map[string][]float32{
		"fact:a": {0.1, 0.2, 0.3},
		"fact:b": {-1, 0, 1},
	}
	if err := s.save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := newEmbedStore(dir, "main", "test-model", 3).load()
	if len(got) != len(want) {
		t.Fatalf("loaded %d vectors, want %d", len(got), len(want))
	}
	for id, vec := range want {
		for d := range vec {
			if got[id][d] != vec[d] {
				t.Errorf("%s[%d] = %v, want %v", id, d, got[id][d], vec[d])
			}
		}
	}
}

// TestEmbedStoreSkipsWrongDimWithoutCorruptingCount guards the header-count fix:
// a wrong-dim vector is skipped on write, and the header count must reflect only
// the entries actually written so load() does not see the file as truncated.
func TestEmbedStoreSkipsWrongDimWithoutCorruptingCount(t *testing.T) {
	dir := t.TempDir()
	s := newEmbedStore(dir, "main", "test-model", 3)
	if err := s.save(map[string][]float32{
		"fact:ok":  {0.1, 0.2, 0.3},
		"fact:bad": {1, 2}, // wrong dim: skipped, must not be counted
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := newEmbedStore(dir, "main", "test-model", 3).load()
	if len(got) != 1 {
		t.Fatalf("loaded %d vectors, want 1 (wrong-dim skipped, count intact)", len(got))
	}
	if _, ok := got["fact:ok"]; !ok {
		t.Errorf("valid vector missing after round-trip: %v", got)
	}
}

func TestEmbedStoreInvalidatesOnModelOrDimChange(t *testing.T) {
	dir := t.TempDir()
	if err := newEmbedStore(dir, "main", "model-A", 3).save(map[string][]float32{"fact:a": {1, 2, 3}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := newEmbedStore(dir, "main", "model-B", 3).load(); len(got) != 0 {
		t.Errorf("different model should invalidate, got %d vectors", len(got))
	}
	if got := newEmbedStore(dir, "main", "model-A", 4).load(); len(got) != 0 {
		t.Errorf("different dim should invalidate, got %d vectors", len(got))
	}
	if got := newEmbedStore(dir, "main", "model-A", 3).load(); len(got) != 1 {
		t.Errorf("matching model+dim should load, got %d vectors", len(got))
	}
}

func TestEmbedStoreMissingFileIsEmpty(t *testing.T) {
	if got := newEmbedStore(t.TempDir(), "main", "m", 3).load(); len(got) != 0 {
		t.Errorf("absent cache should load empty, got %d", len(got))
	}
}

// TestRerankerDiskCacheReusesVectors verifies the disk-backed reranker persists
// embedded vectors and a fresh reranker reads them back (no re-embed needed),
// and that flush prunes a fact that disappears.
func TestRerankerDiskCacheReusesVectors(t *testing.T) {
	e := defaultEmbedder()
	if e == nil {
		t.Skip("embedding backend unavailable")
	}
	dir := t.TempDir()
	now := time.Now()
	facts := []factRecord{
		{ID: "fact:a", Text: "indentation uses spaces", Status: factStatusActive, UpdatedAt: now},
		{ID: "fact:b", Text: "tokens are validated per request", Status: factStatusActive, UpdatedAt: now},
	}
	rr := newSemanticRerankerForBranch(e, dir, "main")
	_ = rankFactsFused(facts, "whitespace policy", 5, false, rr)
	if err := rr.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	path := filepath.Join(dir, filepath.FromSlash(factsBranchRelDir("main")), embedStoreDirName, embedStoreFileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected persisted cache at %s: %v", path, err)
	}

	// A fresh reranker should load both vectors from disk (no dirty embed).
	rr2 := newSemanticRerankerForBranch(e, dir, "main")
	if len(rr2.cache) != 2 {
		t.Fatalf("expected 2 cached vectors loaded, got %d", len(rr2.cache))
	}
	_ = rankFactsFused(facts[:1], "whitespace policy", 5, false, rr2) // touch only fact:a
	if rr2.dirty {
		t.Errorf("ranking cached facts should not mark dirty")
	}
	// Force a prune: only fact:a was touched, so a dirty save would drop fact:b.
	rr2.dirty = true
	if err := rr2.flush(); err != nil {
		t.Fatalf("flush2: %v", err)
	}
	if got := newEmbedStore(dir, "main", e.ID(), e.Dim()).load(); len(got) != 1 {
		t.Errorf("expected prune to 1 touched vector, got %d", len(got))
	}
}

// TestRerankerRetainPreservesOutOfScopeVectors guards the scope-flush fix: a
// scoped run that ranks only a subset must not prune the cached vectors of the
// facts it didn't rank, as long as those facts still exist in the branch.
func TestRerankerRetainPreservesOutOfScopeVectors(t *testing.T) {
	e := defaultEmbedder()
	if e == nil {
		t.Skip("embedding backend unavailable")
	}
	dir := t.TempDir()
	now := time.Now()
	facts := []factRecord{
		{ID: "fact:a", Text: "indentation uses spaces", Status: factStatusActive, UpdatedAt: now},
		{ID: "fact:b", Text: "tokens are validated per request", Status: factStatusActive, UpdatedAt: now},
	}
	// Seed the cache with both vectors.
	rr := newSemanticRerankerForBranch(e, dir, "main")
	_ = rankFactsFused(facts, "whitespace policy", 5, false, rr)
	if err := rr.flush(); err != nil {
		t.Fatalf("seed flush: %v", err)
	}

	// A scoped run ranks only fact:a but retains the full branch set, then
	// embeds a new fact so the flush actually rewrites the file.
	rr2 := newSemanticRerankerForBranch(e, dir, "main")
	scoped := facts[:1] // simulate --scope dropping fact:b
	_ = rankFactsFused(scoped, "whitespace policy", 5, false, rr2)
	rr2.retain(facts)                                              // full branch fact set
	_ = rr2.factVector(factRecord{ID: "fact:c", Text: "new fact"}) // force dirty
	if err := rr2.flush(); err != nil {
		t.Fatalf("scoped flush: %v", err)
	}
	got := newEmbedStore(dir, "main", e.ID(), e.Dim()).load()
	if _, ok := got["fact:b"]; !ok {
		t.Errorf("retain should preserve out-of-scope fact:b across flush; cache has %d entries: %v", len(got), keysOf(got))
	}
}

// TestRerankerFlushPrunesWithoutDirtyEmbed guards the prune-without-embed fix: a
// run that embeds nothing new (no dirty) but sees fewer facts (one departed the
// branch) must still rewrite the cache to drop the stale vector.
func TestRerankerFlushPrunesWithoutDirtyEmbed(t *testing.T) {
	e := defaultEmbedder()
	if e == nil {
		t.Skip("embedding backend unavailable")
	}
	dir := t.TempDir()
	now := time.Now()
	facts := []factRecord{
		{ID: "fact:a", Text: "indentation uses spaces", Status: factStatusActive, UpdatedAt: now},
		{ID: "fact:b", Text: "tokens are validated per request", Status: factStatusActive, UpdatedAt: now},
	}
	// Seed both vectors on disk.
	rr := newSemanticRerankerForBranch(e, dir, "main")
	_ = rankFactsFused(facts, "whitespace policy", 5, false, rr)
	if err := rr.flush(); err != nil {
		t.Fatalf("seed flush: %v", err)
	}

	// fact:b leaves the branch. This run loads the cache, retains only the
	// remaining fact, and embeds nothing new — so dirty stays false.
	rr2 := newSemanticRerankerForBranch(e, dir, "main")
	rr2.retain(facts[:1])
	if rr2.dirty {
		t.Fatalf("no embed happened; dirty should be false")
	}
	if err := rr2.flush(); err != nil {
		t.Fatalf("prune flush: %v", err)
	}
	got := newEmbedStore(dir, "main", e.ID(), e.Dim()).load()
	if _, ok := got["fact:b"]; ok {
		t.Errorf("departed fact:b should be pruned even without a dirty embed; have %v", keysOf(got))
	}
	if len(got) != 1 {
		t.Errorf("expected 1 vector after prune, got %d", len(got))
	}
}

func keysOf(m map[string][]float32) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
