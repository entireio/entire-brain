package cli

import (
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeFusionEmbedder is a deterministic embedder that opts into the history
// fusion gate (unlike fakePlainEmbedder, which models the bundled Model2Vec:
// no marker, gated out). Vectors are assigned per text; unknown text embeds
// to a fixed default so Dim always matches.
type fakeFusionEmbedder struct {
	vecs   map[string][]float32
	embeds []string // every Embed call, for call-count assertions
	fail   func(text string) bool
}

func (f *fakeFusionEmbedder) Embed(text string) []float32 {
	f.embeds = append(f.embeds, text)
	if f.fail != nil && f.fail(text) {
		return nil
	}
	if v, ok := f.vecs[text]; ok {
		return v
	}
	return []float32{1, 0}
}
func (f *fakeFusionEmbedder) Dim() int                    { return 2 }
func (f *fakeFusionEmbedder) ID() string                  { return "fake-fusion" }
func (f *fakeFusionEmbedder) historyFusionEligible() bool { return true }

// memHistoryVecStore is an in-memory historyVectorStore so the sync logic is
// testable on both builds (the real vec0 store exists only under brain_cgo).
type memHistoryVecStore struct {
	vecs    map[string][]float32
	upserts int
}

func newMemHistoryVecStore() *memHistoryVecStore {
	return &memHistoryVecStore{vecs: map[string][]float32{}}
}

func (m *memHistoryVecStore) ids() (map[string]struct{}, bool) {
	out := make(map[string]struct{}, len(m.vecs))
	for id := range m.vecs {
		out[id] = struct{}{}
	}
	return out, true
}

func (m *memHistoryVecStore) upsert(add map[string][]float32, drop []string) error {
	m.upserts++
	for _, id := range drop {
		delete(m.vecs, id)
	}
	for id, v := range add {
		m.vecs[id] = v
	}
	return nil
}

func (m *memHistoryVecStore) knnCos(qvec []float32, k int) (map[string]float64, bool) {
	if len(m.vecs) == 0 || k <= 0 {
		return nil, false
	}
	type sc struct {
		id  string
		cos float64
	}
	all := make([]sc, 0, len(m.vecs))
	for id, v := range m.vecs {
		all = append(all, sc{id, cosineFloat32(qvec, v)})
	}
	sort.Slice(all, func(a, b int) bool { return all[a].cos > all[b].cos })
	if len(all) > k {
		all = all[:k]
	}
	out := make(map[string]float64, len(all))
	for _, s := range all {
		out[s.id] = s.cos
	}
	return out, true
}

func TestHistorySemanticEmbedderGate(t *testing.T) {
	if got := historySemanticEmbedder(nil); got != nil {
		t.Fatalf("nil embedder must stay gated out, got %T", got)
	}
	// fakePlainEmbedder models the bundled Model2Vec: no eligibility marker.
	// This is the closed negative from the 2026-06-12 capstone — it must never
	// pass the gate.
	if got := historySemanticEmbedder(&fakePlainEmbedder{}); got != nil {
		t.Fatalf("an embedder without the eligibility marker must be gated out, got %T", got)
	}
	fe := &fakeFusionEmbedder{}
	if got := historySemanticEmbedder(fe); got != Embedder(fe) {
		t.Fatalf("a fusion-eligible embedder must pass the gate, got %T", got)
	}
}

func TestSyncHistoryVectorsIncrementalDedupAndPrune(t *testing.T) {
	index := historyIndex{GeneratedAt: time.Now(), Records: []historyRecord{
		{ID: "r1", Kind: "decision", Summary: "chose sqlite-vec for the vector store"},
		{ID: "r2", Kind: "decision", Summary: "progress output goes to stderr"},
		{ID: "r3", Kind: "decision", Summary: "chose sqlite-vec for the vector store"}, // duplicate summary of r1
		{ID: "r4", Kind: "request", Summary: "please fix the build"},                   // excluded kind
	}}
	store := newMemHistoryVecStore()
	e := &fakeFusionEmbedder{}

	added, dropped, total, err := syncHistoryVectors(store, index, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added != 3 || dropped != 0 || total != 3 {
		t.Fatalf("first sync: added=%d dropped=%d total=%d, want 3/0/3", added, dropped, total)
	}
	if _, ok := store.vecs["r4"]; ok {
		t.Fatal("request records must not be embedded or stored")
	}
	// r1 and r3 share a summary: stored under both ids, embedded once.
	if len(e.embeds) != 2 {
		t.Fatalf("identical summaries must be embedded once per sync, got %d embeds: %v", len(e.embeds), e.embeds)
	}

	// Second sync: nothing new, no embeds.
	e.embeds = nil
	added, dropped, _, err = syncHistoryVectors(store, index, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || dropped != 0 || len(e.embeds) != 0 {
		t.Fatalf("second sync must be a no-op: added=%d dropped=%d embeds=%d", added, dropped, len(e.embeds))
	}

	// r3 leaves the index: pruned, others untouched.
	index.Records = index.Records[:2]
	added, dropped, total, err = syncHistoryVectors(store, index, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || dropped != 1 || total != 2 {
		t.Fatalf("prune sync: added=%d dropped=%d total=%d, want 0/1/2", added, dropped, total)
	}
	if _, ok := store.vecs["r3"]; ok {
		t.Fatal("departed record must be pruned from the store")
	}
	if _, ok := store.vecs["r1"]; !ok {
		t.Fatal("present record must survive the prune")
	}
}

func TestSyncHistoryVectorsEmbedderFailureSavesProgressAndResumes(t *testing.T) {
	index := historyIndex{GeneratedAt: time.Now(), Records: []historyRecord{
		{ID: "r1", Kind: "decision", Summary: "alpha summary"},
		{ID: "r2", Kind: "decision", Summary: "beta summary"},
	}}
	store := newMemHistoryVecStore()
	e := &fakeFusionEmbedder{fail: func(text string) bool { return strings.Contains(text, "beta") }}

	added, _, _, err := syncHistoryVectors(store, index, e, nil)
	if err == nil {
		t.Fatal("a mid-sync embedder fault must surface as an error")
	}
	if added != 1 {
		t.Fatalf("the batch embedded before the fault must be flushed: added=%d", added)
	}
	if _, ok := store.vecs["r1"]; !ok {
		t.Fatal("progress before the fault must be persisted")
	}

	// Embedder recovers: the next sync resumes with only the missing record.
	e.fail = nil
	e.embeds = nil
	added, _, _, err = syncHistoryVectors(store, index, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || len(e.embeds) != 1 {
		t.Fatalf("resume must embed only the missing record: added=%d embeds=%v", added, e.embeds)
	}
}

func TestRankHistorySemanticDedupsAndSkips(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{ID: "r1", Kind: "decision", Summary: "chose sqlite-vec for vectors"},
		{ID: "r2", Kind: "request", Summary: "user prompt noise"},
		{ID: "r3", Kind: "decision", Summary: "chose sqlite-vec for vectors"}, // dup of r1
		{ID: "r4", Kind: "decision", Summary: "watcher debounces events"},
		{ID: "r5", Kind: "decision", Summary: "no vector stored yet"},
	}}
	scores := map[string]float64{"r1": 0.9, "r2": 0.99, "r3": 0.95, "r4": 0.5}
	got := rankHistorySemantic(index, scores, 10)
	if len(got) != 2 {
		t.Fatalf("expected 2 results (dedup r3, skip request r2, skip unstored r5), got %d: %+v", len(got), got)
	}
	// r1 wins the dedup (first in index order) even though r3 scored higher;
	// this mirrors the lexical scorers, which also keep the first occurrence.
	if got[0].Record.ID != "r1" || got[1].Record.ID != "r4" {
		t.Fatalf("expected [r1 r4], got [%s %s]", got[0].Record.ID, got[1].Record.ID)
	}
	if got[0].Score <= got[1].Score {
		t.Fatalf("display scores must follow cosine order: %d vs %d", got[0].Score, got[1].Score)
	}
}

func TestRankHistorySemanticRelevantRejectsNoiseButKeepsUpperTail(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{ID: "target", Kind: "decision", Summary: "the relevant durable decision"},
		{ID: "n1", Kind: "decision", Summary: "unrelated one"},
		{ID: "n2", Kind: "decision", Summary: "unrelated two"},
		{ID: "n3", Kind: "decision", Summary: "unrelated three"},
		{ID: "n4", Kind: "decision", Summary: "unrelated four"},
		{ID: "n5", Kind: "decision", Summary: "unrelated five"},
	}}
	strong := map[string]float64{
		"target": 0.9, "n1": 0.1, "n2": 0.1, "n3": 0.1, "n4": 0.1, "n5": 0.1,
	}
	got := rankHistorySemanticRelevant(index, strong, 10)
	if len(got) != 1 || got[0].Record.ID != "target" {
		t.Fatalf("confident history upper tail was not isolated: %+v", got)
	}
	flat := map[string]float64{
		"target": 0.101, "n1": 0.1, "n2": 0.1, "n3": 0.1, "n4": 0.1, "n5": 0.1,
	}
	if got := rankHistorySemanticRelevant(index, flat, 10); len(got) != 0 {
		t.Fatalf("flat history neighborhood escaped automatic calibration: %+v", got)
	}
	if raw := rankHistorySemantic(index, flat, 10); len(raw) != len(index.Records) {
		t.Fatalf("explicit semantic ranking should preserve raw neighbors: %+v", raw)
	}
}

func TestRankHistorySemanticHybridPreservesLexicalHitsButRejectsNoise(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{ID: "lex", Kind: "decision", Summary: "lexically reached evidence"},
		{ID: "n1", Kind: "decision", Summary: "unrelated one"},
		{ID: "n2", Kind: "decision", Summary: "unrelated two"},
		{ID: "n3", Kind: "decision", Summary: "unrelated three"},
	}}
	flat := map[string]float64{"lex": 0.101, "n1": 0.1, "n2": 0.1, "n3": 0.1}
	got := rankHistorySemanticHybrid(
		index, flat, 10, map[string]struct{}{"lex": {}},
	)
	if len(got) != 1 || got[0].Record.ID != "lex" {
		t.Fatalf("hybrid history must keep lexical evidence without admitting semantic noise: %+v", got)
	}
}

func TestRankHistoryFusedGateClosedIsExactlyFTS(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{GeneratedAt: time.Now(), Records: []historyRecord{
		{ID: "r1", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 1, Summary: "chose model2vec embeddings for the semantic recall backend"},
		{ID: "r2", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 5, Summary: "progress output goes to stderr to keep json clean"},
	}}
	want, wantOK := rankHistoryViaFTS(brainDir, index, "history", "embeddings backend", 5)
	// Gate closed two ways: no embedder at all, and a non-eligible embedder.
	for name, e := range map[string]Embedder{"nil": nil, "plain": &fakePlainEmbedder{}} {
		got, gotOK := rankHistoryFused(brainDir, index, "history", "embeddings backend", 5, e)
		if gotOK != wantOK || len(got) != len(want) {
			t.Fatalf("[%s] gate-closed fused must equal FTS: ok=%v/%v len=%d/%d", name, gotOK, wantOK, len(got), len(want))
		}
		for i := range got {
			if got[i].Record.ID != want[i].Record.ID {
				t.Fatalf("[%s] gate-closed fused diverged from FTS at %d: %s != %s", name, i, got[i].Record.ID, want[i].Record.ID)
			}
		}
	}
}
