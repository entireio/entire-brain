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
