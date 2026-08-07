//go:build brain_cgo

package cli

import (
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type countingEmbedder struct {
	Embedder
	calls int
}

func (e *countingEmbedder) Embed(text string) []float32 {
	e.calls++
	return e.Embedder.Embed(text)
}

func testVecStore(t *testing.T, modelID string, dim int) *vecStore {
	t.Helper()
	s := newVectorStore(t.TempDir(), "main", modelID, dim)
	vs, ok := s.(*vecStore)
	if !ok {
		t.Fatalf("brain_cgo newVectorStore returned %T, want *vecStore", s)
	}
	return vs
}

func TestVecStoreRoundtripAndPrune(t *testing.T) {
	s := testVecStore(t, "m1", 3)
	if got := s.load(); len(got) != 0 {
		t.Fatalf("missing store should load empty, got %d", len(got))
	}
	vecs := map[string][]float32{
		"f1": {1, 0, 0},
		"f2": {0, 0.5, 0.5},
		"f3": {0.2, 0.3, 0.4},
	}
	if err := s.save(vecs); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := s.load()
	if len(got) != 3 {
		t.Fatalf("load returned %d vectors, want 3", len(got))
	}
	for id, want := range vecs {
		for d := range want {
			if got[id][d] != want[d] {
				t.Fatalf("vector %s[%d] = %v, want %v", id, d, got[id][d], want[d])
			}
		}
	}
	// A rewrite with a subset prunes departed facts (embedStore.save semantics).
	if err := s.save(map[string][]float32{"f1": {1, 0, 0}}); err != nil {
		t.Fatalf("prune save: %v", err)
	}
	if got := s.load(); len(got) != 1 || got["f1"] == nil {
		t.Fatalf("pruned store should hold only f1, got %v", got)
	}
	// Wrong-dim vectors are skipped on save, not persisted.
	if err := s.save(map[string][]float32{"ok": {1, 2, 3}, "short": {1}}); err != nil {
		t.Fatalf("save with wrong-dim entry: %v", err)
	}
	if got := s.load(); len(got) != 1 || got["ok"] == nil {
		t.Fatalf("wrong-dim vector should be skipped, got %v", got)
	}
}

func TestVecStoreRejectsFailureShapedVectorsBeforeKNN(t *testing.T) {
	s := testVecStore(t, "m1", 2)
	if err := s.save(map[string][]float32{
		"valid": {1, 0},
		"zero":  {0, 0},
		"nan":   {float32(math.NaN()), 1},
		"inf":   {float32(math.Inf(1)), 1},
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.load(); len(got) != 1 || got["valid"] == nil {
		t.Fatalf("save persisted failure-shaped vectors: %v", got)
	}
	for name, query := range map[string][]float32{
		"zero": {0, 0},
		"nan":  {float32(math.NaN()), 1},
		"inf":  {float32(math.Inf(1)), 1},
	} {
		t.Run("query-"+name, func(t *testing.T) {
			if _, ok := s.knnCos(query); ok {
				t.Fatal("KNN served a failure-shaped query vector")
			}
		})
	}

	// Simulate a legacy/corrupt same-dimension zero vector. sqlite-vec can
	// produce a finite distance for it, so KNN must validate source blobs rather
	// than treating a finite score as proof of a valid embedding.
	db, err := s.open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE vec_facts SET embedding = ?`, make([]byte, 2*4)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := &vecStore{path: s.path, modelID: s.modelID, dim: s.dim}
	if _, ok := fresh.knnCos([]float32{1, 0}); ok {
		t.Fatal("KNN served a failure-shaped persisted vector")
	}
	if got := fresh.load(); len(got) != 0 {
		t.Fatalf("failure-shaped vec0 cache must rebuild, got %v", got)
	}
}

func TestVecStoreModelAndDimMismatchRebuild(t *testing.T) {
	dir := t.TempDir()
	s := newVectorStore(dir, "main", "m1", 3).(*vecStore)
	if err := s.save(map[string][]float32{"f1": {1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	// A different model id over the same file is a cache miss, never an error.
	other := newVectorStore(dir, "main", "m2", 3).(*vecStore)
	if got := other.load(); len(got) != 0 {
		t.Fatalf("model mismatch should load empty, got %v", got)
	}
	if _, ok := other.knnCos([]float32{1, 0, 0}); ok {
		t.Fatal("model mismatch must not serve KNN")
	}
	// A dim change rewrites the vec0 schema in place (save is the migration).
	wider := newVectorStore(dir, "main", "m1", 4).(*vecStore)
	if got := wider.load(); len(got) != 0 {
		t.Fatalf("dim mismatch should load empty, got %v", got)
	}
	if err := wider.save(map[string][]float32{"f1": {1, 0, 0, 0}}); err != nil {
		t.Fatalf("save across dim change: %v", err)
	}
	if got := wider.load(); len(got) != 1 || len(got["f1"]) != 4 {
		t.Fatalf("store after dim change should hold one 4-dim vector, got %v", got)
	}
}

// TestVecStoreKNNMatchesBruteForce is the engine-parity check: the vec0 cosine
// KNN must agree with cosineFloat32 — the pure-Go brute-force arm it replaces —
// on deliberately non-normalized vectors (a metric mismatch, e.g. L2 instead
// of cosine, fails loudly here).
func TestVecStoreKNNMatchesBruteForce(t *testing.T) {
	const dim, n = 16, 50
	rng := rand.New(rand.NewSource(1))
	vecs := make(map[string][]float32, n)
	for i := 0; i < n; i++ {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()*4 - 2 // unnormalized on purpose
		}
		vecs[string(rune('a'+i%26))+string(rune('0'+i/26))] = v
	}
	s := testVecStore(t, "m1", dim)
	if err := s.save(vecs); err != nil {
		t.Fatal(err)
	}
	qvec := make([]float32, dim)
	for d := range qvec {
		qvec[d] = rng.Float32()*4 - 2
	}
	got, ok := s.knnCos(qvec)
	if !ok {
		t.Fatal("knnCos not served from a populated, matching store")
	}
	if len(got) != n {
		t.Fatalf("knnCos returned %d facts, want all %d raw neighbors", len(got), n)
	}
	for id, vec := range vecs {
		want := cosineFloat32(qvec, vec)
		if math.Abs(got[id]-want) > 1e-4 {
			t.Fatalf("cosine for %s: knn %v vs brute-force %v", id, got[id], want)
		}
	}
	// Wrong query dim falls back rather than erroring.
	if _, ok := s.knnCos(make([]float32, dim+1)); ok {
		t.Fatal("wrong-dim query must not be served")
	}
}

func TestVecStoreKNNClampsCorpusAboveSQLiteLimit(t *testing.T) {
	const dim = 2
	query := "exclusive missing vector marker"
	e := &fixedEmbedder{dim: dim, vecs: map[string][]float32{query: {1, 0}}}
	vecs := make(map[string][]float32, vec0KnnMaxK+1)
	for i := 0; i < vec0KnnMaxK+1; i++ {
		vecs[fmt.Sprintf("fact:%05d", i)] = []float32{1, 0}
	}
	s := testVecStore(t, factEmbeddingModelID(e.ID()), dim)
	if err := s.save(vecs); err != nil {
		t.Fatal(err)
	}
	got, ok := s.knnCos([]float32{1, 0})
	if !ok {
		t.Fatal("large fact store should use a clamped KNN instead of falling back wholesale")
	}
	if len(got) != vec0KnnMaxK {
		t.Fatalf("large KNN returned %d rows, want sqlite limit %d", len(got), vec0KnnMaxK)
	}
	missing := ""
	for id := range vecs {
		if _, served := got[id]; !served {
			missing = id
			break
		}
	}
	if missing == "" {
		t.Fatal("clamped KNN did not leave a fallback candidate")
	}
	facts := make([]factRecord, 0, len(vecs))
	for id := range vecs {
		text := "neutral background record"
		if id == missing {
			text = query
		}
		facts = append(facts, factRecord{ID: id, Text: text, Status: factStatusActive})
	}
	ranked := rankFactsFused(
		facts, query, 1, false,
		newSemanticRerankerForBranch(e, s.brainDir, "main"),
	)
	if len(ranked) != 1 || ranked[0].ID != missing {
		t.Fatalf("candidate outside the clamped KNN was not recovered from the vector cache: %+v", ranked)
	}
}

func TestVecStoreConcurrentSavePresentMergesUnderBrainLock(t *testing.T) {
	s := testVecStore(t, "m-concurrent", 2)
	present := map[string]struct{}{"fact:a": {}, "fact:b": {}}
	start := make(chan struct{})
	var wait sync.WaitGroup
	for id, vector := range map[string][]float32{
		"fact:a": {1, 0},
		"fact:b": {0, 1},
	} {
		wait.Add(1)
		go func(id string, vector []float32) {
			defer wait.Done()
			<-start
			if err := s.savePresent(map[string][]float32{id: vector}, present); err != nil {
				t.Errorf("save %s: %v", id, err)
			}
		}(id, vector)
	}
	close(start)
	wait.Wait()
	got := s.load()
	if len(got) != 2 {
		t.Fatalf("concurrent merge lost vectors: %v", got)
	}
}

// TestRankFactsFusedVecStoreParity is the Stage 1b exit-gate check in
// miniature: ranking through the vec0 KNN path must reproduce the in-memory
// brute-force ranking on the same facts and query.
func TestRankFactsFusedVecStoreParity(t *testing.T) {
	e := &countingEmbedder{Embedder: testEmbedder(t)}
	now := time.Now()
	facts := []factRecord{
		{ID: "a", Text: "indentation uses space characters rather than tab stops", Status: factStatusActive, UpdatedAt: now},
		{ID: "b", Text: "the nightly deployment pipeline publishes release artifacts", Status: factStatusActive, UpdatedAt: now},
		{ID: "c", Text: "code review requires two approvals before merge", Status: factStatusActive, UpdatedAt: now},
		{ID: "d", Text: "retry backoff doubles on each failed attempt", Status: factStatusActive, UpdatedAt: now},
	}
	query := "what whitespace convention do we follow"
	want := rankFactsFused(facts, query, 10, false, newSemanticReranker(e))

	brainDir := t.TempDir()
	// First disk-backed run embeds and persists (store empty: brute-force path).
	rr1 := newSemanticRerankerForBranch(e, brainDir, "main")
	rankFactsFused(facts, query, 10, false, rr1)
	rr1.retain(facts)
	if err := rr1.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	store := newVectorStore(brainDir, "main", factEmbeddingModelID(e.ID()), e.Dim()).(*vecStore)
	cos, ok := store.knnCos(e.Embed(query))
	if !ok || len(cos) != len(facts) {
		t.Fatalf("after flush the vec0 store must serve all %d facts, got %d (ok=%v)", len(facts), len(cos), ok)
	}
	if _, err := filepath.Glob(store.path); err != nil {
		t.Fatal(err)
	}

	// Second run is served by the vec0 KNN; order must match brute force.
	e.calls = 0
	rr2 := newSemanticRerankerForBranch(e, brainDir, "main")
	got := rankFactsFused(facts, query, 10, false, rr2)
	if e.calls != 1 {
		t.Fatalf("KNN calibration should embed only the query, got %d embed calls", e.calls)
	}
	if len(got) != len(want) {
		t.Fatalf("KNN-served ranking returned %d facts, brute force %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Fatalf("order diverged at %d: knn %s vs brute force %s", i, got[i].ID, want[i].ID)
		}
	}
	// The KNN path must keep served facts alive through flush (markTouched):
	// a second flush must not prune vectors the ranking never re-embedded.
	rr2.retain(facts)
	if err := rr2.flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if after := store.load(); len(after) != len(facts) {
		t.Fatalf("flush after KNN-served run pruned vectors: %d left, want %d", len(after), len(facts))
	}
}
