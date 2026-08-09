//go:build brain_cgo

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMultiConceptHybridIncludesShortTermLexicalEvidence(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_CONVERSATION_FUSION", "1")
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	longTerm := historyRecord{
		ID: conversationIDPrefix + "zz-long", Kind: conversationKind,
		Path: "sessions/main/20260808T000000Z_split.jsonl", Line: 1, EndLine: 2,
		TurnOrdinal: 1, SessionID: "split-session", Branch: "main", Agent: "Codex",
		CreatedAt: "2026-08-08T00:00:00Z", ContentRole: conversationContentRole,
		Summary: "alpha rollout plan for the ingest service",
	}
	index := historyIndex{GeneratedAt: now, Records: []historyRecord{longTerm}}
	indexBytes, err := json.MarshalIndent(index, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	if err := os.WriteFile(indexPath, indexBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	indexDigest := fileSHA256Hex(indexPath)
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/split",
		Sources: &brainSources{History: &historySourceManifest{
			GeneratedAt: now, IndexPath: historyIndexPath, IndexDigest: indexDigest,
			PrivacyIdentity: "absent", Records: 1, Exchanges: 1,
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	e := &fakeFusionEmbedder{vecs: map[string][]float32{
		"alpha rollout":                     {0, 1},
		"beta cache eviction":               {1, 0},
		conversationEmbeddingText(longTerm): {0, 1},
	}}
	store, ok := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim())
	if !ok {
		t.Fatal("conversation store unavailable on brain_cgo build")
	}
	if _, _, _, err := syncConversationVectors(store, index, e, nil); err != nil {
		t.Fatalf("sync conversation vectors: %v", err)
	}
	if err := saveMemoryVectorProgress(brainDir, memoryVectorProgress{
		ModelID: e.ID(), SourceDigest: indexDigest, CompleteSourceDigest: indexDigest,
		ResetComplete: true, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	overlayRecord := historyRecord{
		ID: conversationIDPrefix + "aa-overlay", Kind: conversationKind,
		Path: "sessions/main/20260809T000000Z_split-delta.jsonl", Line: 1, EndLine: 2,
		TurnOrdinal: 2, SessionID: "split-session", Branch: "main", Agent: "Codex",
		CreatedAt: "2026-08-09T00:00:00Z", ContentRole: conversationContentRole,
		Summary: "beta cache eviction completed after the ingest rollout",
	}
	overlayBytes, err := json.MarshalIndent(shortTermIndex{
		Version: historyShortTermVersion, ReconcilerVersion: historyShortTermReconcilerVersion,
		BaseGeneratedAt: now, GeneratedAt: now.Add(time.Hour),
		Files: map[string]shortTermFile{overlayRecord.Path: {Records: []historyRecord{overlayRecord}}},
	}, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)), overlayBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	originalEmbedder := conversationMultiConceptEmbedder
	conversationMultiConceptEmbedder = func() Embedder { return e }
	t.Cleanup(func() { conversationMultiConceptEmbedder = originalEmbedder })
	results, err := retrieveConversationMultiConcept(brainDir, "alpha rollout", 10, modeHybrid, retrievalOptions{
		Source: retrievalSourceConversation, Concepts: []string{"beta cache eviction"},
	})
	if err != nil {
		t.Fatalf("hybrid multi-concept retrieval: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results=%d, want split session: %+v", len(results), results)
	}
	if !results[0].Approximate || len(results[0].ConceptMatches) != 2 {
		t.Fatalf("hybrid coverage envelope: %+v", results[0])
	}
	if got := results[0].ConceptMatches[1]; got.ConversationID != overlayRecord.ID || got.Arm != "fused" {
		t.Fatalf("overlay lexical evidence was dropped from fused coverage: %+v", got)
	}
}

func markVectorGenerationCurrentForTest(t *testing.T, brainDir string, e Embedder) {
	t.Helper()
	digest := "sha256:" + strings.Repeat("1", 64)
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/vector", Sources: &brainSources{History: &historySourceManifest{IndexDigest: digest, PrivacyIdentity: "absent"}}}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if err := saveMemoryVectorProgress(brainDir, memoryVectorProgress{ModelID: e.ID(), SourceDigest: digest, CompleteSourceDigest: digest, ResetComplete: true, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
}

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
	markVectorGenerationCurrentForTest(t, brainDir, e)

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

func TestHistorySemanticScoresUseStableCalibrationNeighborhood(t *testing.T) {
	brainDir := t.TempDir()
	e := &fakeFusionEmbedder{vecs: map[string][]float32{"calibration query": {1, 0}}}
	store, ok := newHistoryVectorStore(brainDir, e.ID(), e.Dim())
	if !ok {
		t.Fatal("store unavailable")
	}
	const records = 100
	vectors := make(map[string][]float32, records)
	for i := 0; i < records; i++ {
		vectors[fmt.Sprintf("r%03d", i)] = []float32{1, float32(i + 1)}
	}
	if err := store.upsert(vectors, nil); err != nil {
		t.Fatal(err)
	}
	markVectorGenerationCurrentForTest(t, brainDir, e)
	rawScores := historySemanticScores(brainDir, e, "calibration query", 1, false)
	if len(rawScores) != 4 {
		t.Fatalf("explicit display limit 1 produced %d scores, want the 4x raw-search budget", len(rawScores))
	}
	scores := historySemanticScores(brainDir, e, "calibration query", 1, true)
	if len(scores) != records {
		t.Fatalf("display limit 1 produced %d calibration scores, want all %d stored rows", len(scores), records)
	}
}

// TestConversationVecStoreIsolatedFromHistoryStore proves the conversation
// vector store is a distinct vec0 file with its own identity: writes to one
// are invisible to the other, so general history KNN can never surface
// exchange vectors and vice versa.
func TestConversationVecStoreIsolatedFromHistoryStore(t *testing.T) {
	brainDir := t.TempDir()
	hist, ok := newHistoryVectorStore(brainDir, "model-a", 2)
	if !ok {
		t.Fatal("history store unavailable on brain_cgo build")
	}
	conv, ok := newConversationVectorStore(brainDir, "model-a", 2)
	if !ok {
		t.Fatal("conversation store unavailable on brain_cgo build")
	}
	if err := hist.upsert(map[string][]float32{"history:d1": {1, 0}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := conv.upsert(map[string][]float32{conversationIDPrefix + "e1": {0, 1}}, nil); err != nil {
		t.Fatal(err)
	}
	histIDs, ok := hist.ids()
	if !ok || len(histIDs) != 1 {
		t.Fatalf("history ids: ok=%v %v", ok, histIDs)
	}
	if _, leaked := histIDs[conversationIDPrefix+"e1"]; leaked {
		t.Fatal("conversation vector leaked into the history store")
	}
	convIDs, ok := conv.ids()
	if !ok || len(convIDs) != 1 {
		t.Fatalf("conversation ids: ok=%v %v", ok, convIDs)
	}
	if _, leaked := convIDs["history:d1"]; leaked {
		t.Fatal("history vector leaked into the conversation store")
	}
	// KNN on each store sees only its own rows.
	scores, ok := conv.knnCos([]float32{0, 1}, 10)
	if !ok || len(scores) != 1 {
		t.Fatalf("conversation knn: ok=%v %v", ok, scores)
	}
	if _, leaked := scores["history:d1"]; leaked {
		t.Fatal("conversation KNN returned a history row")
	}
}

// TestRankConversationFusedSemanticArmEndToEnd exercises the OPEN fused path
// against the real vec0 store with a deterministic fusion-eligible embedder:
// a term-disjoint exchange (no lexical overlap with the query) must be
// reachable through the semantic arm, and lexical hits must survive fusion.
func TestRankConversationFusedSemanticArmEndToEnd(t *testing.T) {
	brainDir := t.TempDir()
	semanticSummary := "term disjoint note about connection pooling"
	lexicalSummary := "deploy pipeline alpha rollback rationale"
	index := historyIndex{
		GeneratedAt: time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			{ID: conversationIDPrefix + "lex", Kind: conversationKind, SessionID: "s1",
				Path: "sessions/main/a.jsonl", Line: 1, Summary: lexicalSummary},
			{ID: conversationIDPrefix + "sem", Kind: conversationKind, SessionID: "s2",
				Path: "sessions/main/b.jsonl", Line: 3, Summary: semanticSummary},
			{ID: conversationIDPrefix + "far", Kind: conversationKind, SessionID: "s3",
				Path: "sessions/main/c.jsonl", Line: 5, Summary: "unrelated chatter"},
			{ID: "history:d1", Kind: "decision", Summary: "deploy pipeline decision must stay out"},
		},
	}
	query := "deploy pipeline alpha"
	e := &fakeFusionEmbedder{vecs: map[string][]float32{
		query:               {0, 1},
		semanticSummary:     {0, 1}, // cosine 1.0 with the query, zero lexical overlap
		lexicalSummary:      {1, 0}, // lexically strong, semantically orthogonal
		"unrelated chatter": {1, 0},
	}}
	// The store identity carries the embedding-text scheme version (reqw1);
	// query-time lookups use the same versioned id.
	store, ok := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim())
	if !ok {
		t.Fatal("conversation store unavailable on brain_cgo build")
	}
	if _, _, _, err := syncConversationVectors(store, index, e, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
	markVectorGenerationCurrentForTest(t, brainDir, e)

	fused, _, ok := rankConversationFused(brainDir, index, query, 10, e, nil)
	if !ok || len(fused) == 0 {
		t.Fatalf("fused: ok=%v len=%d", ok, len(fused))
	}
	got := map[string]bool{}
	for _, s := range fused {
		got[s.Record.ID] = true
		if s.Record.Kind != conversationKind {
			t.Fatalf("non-exchange leaked through fusion: %+v", s.Record)
		}
	}
	if !got[conversationIDPrefix+"lex"] {
		t.Fatalf("lexical hit lost in fusion: %v", got)
	}
	if !got[conversationIDPrefix+"sem"] {
		t.Fatalf("term-disjoint semantic hit not reachable through fusion: %v", got)
	}

	// Explicit vector mode over the same store: semantic-only, best-first.
	scores := conversationSemanticScores(brainDir, e, query, 10, false)
	if len(scores) == 0 {
		t.Fatal("conversationSemanticScores returned nothing with a populated store")
	}
	ranked := rankConversationSemantic(index, scores, 10)
	if len(ranked) == 0 || ranked[0].Record.ID != conversationIDPrefix+"sem" {
		t.Fatalf("vector mode order wrong: %+v", ranked)
	}
}

// TestRankConversationFusedFilteredCompleteness proves R0-3 on the semantic
// and fused arms against the real vec0 store: an in-scope exchange that both
// arms rank far below a wall of out-of-scope candidates is still returned
// when a structured filter selects it, because filters reach candidate
// generation instead of trimming an already-bounded window.
func TestRankConversationFusedFilteredCompleteness(t *testing.T) {
	brainDir := t.TempDir()
	query := "lock timeout retry storm"
	vecs := map[string][]float32{query: {0, 1}}
	records := make([]historyRecord, 0, 41)
	for i := 0; i < 40; i++ {
		summary := fmt.Sprintf("lock timeout lock timeout retry storm attempt %02d", i)
		vecs[summary] = []float32{0, 1} // cosine 1.0 with the query
		records = append(records, historyRecord{
			ID: fmt.Sprintf("%snoise%02d", conversationIDPrefix, i), Kind: conversationKind,
			SessionID: "noise-sess", Path: "sessions/main/noise.jsonl", Line: i*2 + 1,
			Summary: summary,
		})
	}
	targetSummary := "one lock timeout observed in an unrelated renderer warmup trace"
	vecs[targetSummary] = []float32{1, 0} // cosine 0 with the query
	records = append(records, historyRecord{
		ID: conversationIDPrefix + "target", Kind: conversationKind,
		SessionID: "target-sess", Path: "sessions/release/target.jsonl", Line: 1,
		Summary: targetSummary,
	})
	index := historyIndex{GeneratedAt: time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC), Records: records}
	e := &fakeFusionEmbedder{vecs: vecs}
	store, ok := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim())
	if !ok {
		t.Fatal("conversation store unavailable on brain_cgo build")
	}
	if _, _, _, err := syncConversationVectors(store, index, e, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
	markVectorGenerationCurrentForTest(t, brainDir, e)

	pred := func(r historyRecord) bool { return r.SessionID == "target-sess" }

	// Adversarial property: without the filter, the target is nowhere near
	// the top of the fused list at this limit.
	unfiltered, _, ok := rankConversationFused(brainDir, index, query, 2, e, nil)
	if !ok {
		t.Fatal("unfiltered fused ranking unavailable")
	}
	for _, s := range unfiltered {
		if s.Record.ID == conversationIDPrefix+"target" {
			t.Fatal("fixture not adversarial: target ranked into unfiltered fused top-k")
		}
	}

	fused, complete, ok := rankConversationFused(brainDir, index, query, 2, e, pred)
	if !ok || !complete {
		t.Fatalf("filtered fused: ok=%v complete=%v", ok, complete)
	}
	if len(fused) != 1 || fused[0].Record.ID != conversationIDPrefix+"target" {
		t.Fatalf("filtered fused must return exactly the in-scope hit: %+v", fused)
	}

	// The exhaustive semantic neighborhood covers every stored vector, so the
	// filtered vector arm also reaches the target despite its cosine 0.
	scores := conversationSemanticScoresExhaustive(brainDir, e, query, false)
	if _, ok := scores[conversationIDPrefix+"target"]; !ok {
		t.Fatalf("exhaustive scores missed the in-scope vector: %d scores", len(scores))
	}
	semIndex := historyIndex{GeneratedAt: index.GeneratedAt, Records: filterHistoryRecords(index.Records, pred)}
	ranked := rankConversationSemantic(semIndex, scores, 2)
	if len(ranked) != 1 || ranked[0].Record.ID != conversationIDPrefix+"target" {
		t.Fatalf("filtered vector arm must return the in-scope hit: %+v", ranked)
	}
}

// TestExclusionCleanupDeletesPopulatedConversationVectorStore is the
// vector-store canary (R0-1/R0-2): a real vec0 store holding embeddings for a
// tombstoned session's exchanges is deleted by the shared cleanup and the
// operation still verifies clean.
func TestExclusionCleanupDeletesPopulatedConversationVectorStore(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	e := &fakeFusionEmbedder{vecs: map[string][]float32{}}
	store, ok := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim())
	if !ok {
		t.Fatal("conversation store unavailable on brain_cgo build")
	}
	if _, _, _, err := syncConversationVectors(store, index, e, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(historyDirName), embedStoreDirName, conversationVecStoreFileNamePortable)
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("populated store must exist before cleanup: %v", err)
	}

	var plan sessionPurgePlan
	if err := withBrainWriteLock(brainDir, func() error {
		var planErr error
		plan, planErr = buildSessionPurgePlan(brainDir, "secret-sess")
		if planErr != nil {
			return planErr
		}
		return executeSessionCleanup(brainDir, "secret-sess", plan, time.Now().UTC(), "test", true)
	}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	listed := false
	for _, artifact := range plan.DerivedStores {
		if strings.HasSuffix(artifact.Path, conversationVecStoreFileNamePortable) {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("vector store missing from the cleanup inventory: %+v", plan.DerivedStores)
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("vector store must be deleted by exclusion cleanup: %v", err)
	}
}
