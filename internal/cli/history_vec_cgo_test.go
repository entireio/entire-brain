//go:build brain_cgo

package cli

import (
	"testing"
	"time"
)

func TestHistoryVecStoreRoundtrip(t *testing.T) {
	brainDir := t.TempDir()
	store, ok := newHistoryVectorStore(brainDir, "model-a", 2)
	if !ok {
		t.Fatal("brain_cgo build must provide the history vector store")
	}

	// Fresh store: no ids yet.
	if _, ok := store.ids(); ok {
		t.Fatal("a store that was never written must report ok=false")
	}

	if err := store.upsert(map[string][]float32{
		"r1": {1, 0},
		"r2": {0, 1},
	}, nil); err != nil {
		t.Fatal(err)
	}
	ids, ok := store.ids()
	if !ok || len(ids) != 2 {
		t.Fatalf("ids after upsert: ok=%v len=%d", ok, len(ids))
	}

	// KNN: query along r1's direction must score r1 ~1 and r2 ~0.
	scores, ok := store.knnCos([]float32{1, 0}, 10)
	if !ok {
		t.Fatal("knnCos must succeed on a populated store")
	}
	if scores["r1"] < 0.99 || scores["r2"] > 0.01 {
		t.Fatalf("cosine scores wrong: %v", scores)
	}

	// Re-adding a known id is a no-op; dropping removes both row and vector.
	if err := store.upsert(map[string][]float32{"r1": {0, 1}}, []string{"r2"}); err != nil {
		t.Fatal(err)
	}
	ids, _ = store.ids()
	if len(ids) != 1 {
		t.Fatalf("after drop: %v", ids)
	}
	scores, ok = store.knnCos([]float32{1, 0}, 10)
	if !ok || scores["r1"] < 0.99 {
		t.Fatalf("re-add of a known id must not overwrite its vector: %v (ok=%v)", scores, ok)
	}
	if _, present := scores["r2"]; present {
		t.Fatal("dropped id must leave the KNN result")
	}

	// A different model/dim sees a mismatched store: ids ok=false (sync treats
	// as empty and rebuilds), and the first upsert resets the schema.
	other, _ := newHistoryVectorStore(brainDir, "model-b", 3)
	if _, ok := other.ids(); ok {
		t.Fatal("model mismatch must read as no store")
	}
	if err := other.upsert(map[string][]float32{"r9": {1, 0, 0}}, nil); err != nil {
		t.Fatal(err)
	}
	ids, ok = other.ids()
	if !ok || len(ids) != 1 {
		t.Fatalf("after reset+upsert: ok=%v ids=%v", ok, ids)
	}
	if _, stale := ids["r1"]; stale {
		t.Fatal("schema reset must not carry old-model vectors over")
	}
}

// TestRankHistoryFusedSemanticRescue is the capstone result in miniature: a
// record that shares no usable lexical term with the query is unreachable for
// BM25 but lives next to the query in vector space; with the gate open and
// vectors synced, the fused ranking surfaces both it and the lexical hit.
func TestRankHistoryFusedSemanticRescue(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{GeneratedAt: time.Now(), Records: []historyRecord{
		{ID: "lex", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 1, Summary: "embeddings backend uses model2vec"},
		{ID: "sem", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 5, Summary: "vector similarity drives recall ranking"},
		{ID: "off", Kind: "decision", Path: "sessions/main/s2.jsonl", Line: 2, Summary: "progress output goes to stderr"},
	}}
	e := &fakeFusionEmbedder{vecs: map[string][]float32{
		"embeddings backend uses model2vec":       {1, 0},
		"vector similarity drives recall ranking": {0.1, 0.995},
		"progress output goes to stderr":          {1, 0},
		"embeddings backend":                      {0.1, 0.995}, // query lands on "sem"
	}}
	store, ok := newHistoryVectorStore(brainDir, e.ID(), e.Dim())
	if !ok {
		t.Fatal("store unavailable")
	}
	if _, _, _, err := syncHistoryVectors(store, index, e, nil); err != nil {
		t.Fatal(err)
	}

	// Sanity: plain FTS does not surface "sem" for this query.
	ftsOnly, ok := rankHistoryViaFTS(brainDir, index, "history", "embeddings backend", 5)
	if !ok {
		t.Fatal("FTS ranking unavailable")
	}
	for _, s := range ftsOnly {
		if s.Record.ID == "sem" {
			t.Fatal("fixture broken: the semantic record must be lexically unreachable")
		}
	}

	fused, ok := rankHistoryFused(brainDir, index, "history", "embeddings backend", 5, e)
	if !ok {
		t.Fatal("fused ranking unavailable with gate open and vectors synced")
	}
	got := map[string]bool{}
	for _, s := range fused {
		got[s.Record.ID] = true
	}
	if !got["sem"] {
		t.Fatalf("fusion must rescue the semantically-near record; got %v", fused)
	}
	if !got["lex"] {
		t.Fatalf("fusion must keep the lexical hit; got %v", fused)
	}
}
