package cli

import (
	"math"
	"sort"
	"strings"
)

// rrfK is the Reciprocal Rank Fusion constant. 60 is the canonical value from
// the original RRF paper and what hybrid-search systems (e.g. qmd) use; it
// damps the influence of a single retriever's top ranks so neither the lexical
// nor the semantic list can unilaterally dominate the fusion.
const (
	rrfK                     = 60.0
	semanticFusionOversample = 2
)

// vectorStore persists fact vectors across CLI invocations. Two
// implementations exist, selected by build tag: the pure-Go vectors.bin flat
// file (embedStore, the default build) and the Stage 1b sqlite-vec vec0 store
// (vecStore, brain_cgo builds). Both are regenerable derived artifacts keyed
// by embedder model id + dim; a mismatch loads empty and triggers a clean
// rebuild.
type vectorStore interface {
	load() map[string][]float32
	save(map[string][]float32) error
	savePresent(map[string][]float32, map[string]struct{}) error
}

// knnVectorStore is the optional vectorStore upgrade the brain_cgo build
// provides: one vec0 MATCH query returns cosine similarity for every stored
// fact, replacing the per-fact brute-force loop in the semantic arm. ok=false
// (store absent, model mismatch, empty) sends the caller to the brute-force
// fallback, so a degraded store never breaks ranking. Implementations may
// return scores only for validated, non-zero stored vectors; the ranker also
// cross-checks each score against its independently loaded vector cache.
type knnVectorStore interface {
	knnCos(qvec []float32) (map[string]float64, bool)
}

// semanticReranker holds the embedder and a cache of fact vectors. Fact ids hash
// normalized text plus sorted paths, so a vector is valid for the life of the
// fact. The in-memory cache makes an eval over many queries embed each fact at
// most once, and an optional disk store (recall/brief) carries vectors across
// CLI invocations.
type semanticReranker struct {
	e       Embedder
	cache   map[string][]float32
	store   vectorStore     // nil => in-memory only (eval, tests)
	touched map[string]bool // ids seen this run; nil unless disk-backed
	// foreign holds ids that are ranked here but do not belong to this store.
	// Global facts are the case: the store is keyed by (brainDir, branch), so a
	// fact that belongs to every repository must not be filed under one
	// repository's key — it would be written into that repo's cache and pruned
	// out of it again the moment the same query runs with global facts off.
	// They are embedded per run and ranked; they are never cached or retained.
	foreign map[string]bool
	dirty   bool // a new vector was embedded this run
	loaded  int  // full-dimension vectors loaded from the store

	// lastRun is observation-only instrumentation for callers that must report
	// the effective retrieval engine. It records what rankFactsFused actually
	// used; it never participates in ranking decisions.
	lastRun semanticRerankTrace
}

// semanticRerankTrace describes the most recent rankFactsFused call. In
// particular, Applied follows the existing len(qvec)>0 behavior, while
// QueryVectorValid and ValidCandidateVectors let identity reporting fail closed
// when a backend returns a malformed/partial result instead of claiming a
// healthy semantic arm from configuration intent alone.
type semanticRerankTrace struct {
	Attempted             bool
	Applied               bool
	QueryVectorValid      bool
	CandidateCount        int
	ValidCandidateVectors int
	BM25Used              bool
}

// newSemanticReranker returns nil when no embedder is available, so callers can
// pass the result straight to rankFactsFused and get the pure-lexical path.
// This variant is in-memory only — used by eval and tests, which should not
// read or write a persistent cache.
func newSemanticReranker(e Embedder) *semanticReranker {
	if e == nil {
		return nil
	}
	return &semanticReranker{e: e, cache: map[string][]float32{}}
}

// newSemanticRerankerForBranch adds a disk-backed vector cache under
// facts/<branch>/embeddings/ so recall and brief reuse vectors across
// invocations instead of re-embedding the branch each time.
func newSemanticRerankerForBranch(e Embedder, brainDir, branch string) *semanticReranker {
	rr := newSemanticReranker(e)
	if rr == nil {
		return nil
	}
	rr.store = newVectorStore(brainDir, branch, factEmbeddingModelID(e.ID()), e.Dim())
	rr.cache = rr.store.load()
	rr.loaded = len(rr.cache)
	rr.touched = map[string]bool{}
	return rr
}

// markTouched records an id as present this run without forcing an embed —
// the KNN path serves cosines straight from the store, bypassing factVector,
// but flush must still know the fact is alive or it would prune its vector.
func (s *semanticReranker) markTouched(id string) {
	if s.foreign[id] {
		return
	}
	if s.touched != nil {
		s.touched[id] = true
	}
}

// markForeign records ids that are ranked through this reranker but belong to
// another store. Safe on a nil reranker so callers do not have to guard.
func (s *semanticReranker) markForeign(ids map[string]bool) {
	if s == nil || len(ids) == 0 {
		return
	}
	if s.foreign == nil {
		s.foreign = make(map[string]bool, len(ids))
	}
	for id := range ids {
		if ids[id] {
			s.foreign[id] = true
		}
	}
}

func (s *semanticReranker) factVector(f factRecord) []float32 {
	if s.foreign[f.ID] {
		// Ranked, never persisted: embedding it each run costs one call and
		// keeps a fact that belongs everywhere out of one repository's store.
		return s.e.Embed(factEmbeddingText(f))
	}
	if s.touched != nil {
		s.touched[f.ID] = true
	}
	if v, ok := s.cache[f.ID]; ok {
		if validSemanticEmbedding(v, s.e.Dim()) {
			return v
		}
		// Keep a tombstone until flush. savePresent merges with the latest disk
		// state under the write lock, so deleting only from this stale in-memory
		// snapshot could otherwise resurrect the invalid persisted vector.
		s.cache[f.ID] = nil
		s.dirty = true
	}
	v := s.e.Embed(factEmbeddingText(f))
	// Cache only a full-dimension vector with actual numeric signal. A transient
	// embed failure (nil/short/zero/non-finite)
	// must not be cached, or every later call this process would reuse the empty
	// result and never retry; returning it uncached lets a later call re-embed.
	if validSemanticEmbedding(v, s.e.Dim()) {
		s.cache[f.ID] = v
		s.dirty = true
	}
	return v
}

// queryEmbedder is implemented by embedders (e.g. EmbeddingGemma) that embed a
// search query with a different instruction prefix than a document. Embedders
// without the asymmetry (the Model2Vec static model) simply omit it.
type queryEmbedder interface {
	EmbedQuery(text string) []float32
}

// embedQuery embeds the query, using the embedder's query-specific prefix when it
// has one, so a document/query asymmetry (EmbeddingGemma) is honored without
// changing the symmetric Model2Vec path.
func (s *semanticReranker) embedQuery(query string) []float32 {
	return embedQueryWith(s.e, query)
}

// retain marks ids as present this run for prune purposes, without forcing an
// embed. Callers pass the full branch fact set (before scope/status filtering)
// so flush keeps every still-present fact's cached vector and prunes only facts
// that genuinely left the branch — otherwise a scoped recall, which ranks only a
// subset, would prune the rest and thrash the cache on the next unscoped run.
func (s *semanticReranker) retain(facts []factRecord) {
	if s == nil || s.touched == nil {
		return
	}
	for _, f := range facts {
		if s.foreign[f.ID] {
			continue
		}
		s.touched[f.ID] = true
	}
}

// flush persists vectors when disk-backed. It writes only the ids touched this
// run (embedded via factVector or marked present via retain), so facts removed
// or superseded since the last run are pruned from the cache on rewrite.
// Best-effort: a cache is never load-bearing.
func (s *semanticReranker) flush() error {
	if s == nil || s.store == nil {
		return nil
	}
	out := make(map[string][]float32, len(s.touched))
	for id := range s.touched {
		if v, ok := s.cache[id]; ok {
			out[id] = v
		}
	}
	// Rewrite when a new vector was embedded (dirty) or when the loaded cache
	// holds vectors for facts no longer present this run (out ⊂ cache → stale
	// entries to prune). Skipping both means the on-disk file already matches —
	// so a pure cache-hit run with no departures does no I/O, but a removal-only
	// run still prunes even though nothing new was embedded.
	if !s.dirty && len(out) == len(s.cache) {
		return nil
	}
	present := make(map[string]struct{}, len(s.touched))
	for id := range s.touched {
		present[id] = struct{}{}
	}
	return s.store.savePresent(out, present)
}

// rankFactsFused ranks facts by Reciprocal Rank Fusion of a lexical list and a
// semantic (cosine) list. When rr is nil it is exactly rankFacts (pure
// lexical), so callers without an embedder are unaffected.
//
// The fusion is what attacks both measured failure modes at once:
//   - Reachability (the 0.667 lexical ceiling): the semantic list lets a relevant
//     fact that shares no query term surface — something no lexical weighting
//     can do.
//   - Ranking within reach: a fact strong in both lists fuses above one strong
//     in only one, reordering the lexically-reachable set by meaning.
//
// An empty (or whitespace-only) query keeps the lexical path's recency listing
// (no query vector to compare against), matching rankFacts exactly.
func rankFactsFused(facts []factRecord, query string, limit int, includeAll bool, rr *semanticReranker) []factRecord {
	return rankFactsFusedWithSemanticDepthMultiplier(facts, query, limit, includeAll, rr, semanticFusionOversample)
}

// rankFactsFusedWithSemanticDepthMultiplier is the implementation seam used to
// compare semantic oversampling depths in deterministic retrieval tests. A
// multiplier <= 0 preserves the historical full-candidate contribution.
func rankFactsFusedWithSemanticDepthMultiplier(facts []factRecord, query string, limit int, includeAll bool, rr *semanticReranker, semanticDepthMultiplier int) []factRecord {
	if rr == nil {
		return rankFacts(facts, query, limit, includeAll)
	}
	rr.lastRun = semanticRerankTrace{}
	if strings.TrimSpace(query) == "" {
		return rankFacts(facts, query, limit, includeAll)
	}
	if limit <= 0 {
		limit = 10
	}
	type cand struct {
		rec           factRecord
		lex           float64
		lexHit        bool // retrieved by the lexical arm (distinguishes a 0-score BM25 hit from a miss)
		cos           float64
		semHit        bool // backed by a valid candidate vector, not failure-shaped zero noise
		semanticValid bool
		semantic      bool
	}
	queryLocus := factLocus(query)
	rr.lastRun.Attempted = true
	qvec := rr.embedQuery(query)
	// No query embedding (e.g. the embedder is unavailable) → fall back cleanly to
	// lexical-only ranking. Otherwise every cosine is 0 and the semantic arm would
	// still add an RRF term, reordering results by the UpdatedAt tiebreaker.
	queryNorm, haveSemantic := semanticEmbeddingSquaredNorm(qvec, rr.e.Dim())
	rr.lastRun.Applied = haveSemantic
	rr.lastRun.QueryVectorValid = haveSemantic
	candidates := make([]factRecord, 0, len(facts))
	for _, f := range facts {
		if !includeAll && f.Status != factStatusActive {
			continue
		}
		candidates = append(candidates, f)
	}
	rr.lastRun.CandidateCount = len(candidates)
	if len(candidates) == 0 {
		return nil
	}
	// Lexical arm: the hand-rolled token-overlap scorer (plus the code locus
	// boost) by default. The opt-in FTS5 BM25 arm (ENTIRE_BRAIN_FACTS_BM25=1) is
	// IDF-weighted with no coverage gate; it measured at parity on the facts
	// baseline, so it stays off by default and falls back here if its in-memory
	// index can't be built.
	var bm25 map[string]float64
	haveBM25 := false
	if factsBM25Enabled() {
		bm25, haveBM25 = factsFTSScores(candidates, query)
	}
	rr.lastRun.BM25Used = haveBM25
	// Semantic arm engine: under brain_cgo the disk store is a sqlite-vec vec0
	// table, and one KNN MATCH query returns the cosine for every stored fact —
	// replacing the per-fact brute-force loop below. Facts not yet in the store
	// (new this run, or any store failure) fall back to embed + Go cosine, so
	// the two paths always agree on coverage.
	var storeCos map[string]float64
	if haveSemantic && rr.store != nil {
		if ks, ok := rr.store.(knnVectorStore); ok {
			storeCos, _ = ks.knnCos(qvec)
		}
	}
	cands := make([]cand, 0, len(candidates))
	for _, f := range candidates {
		var lex float64
		var lexHit bool
		if haveBM25 {
			// factsFTSScores only contains matched ids, so presence — not a positive
			// score — is the hit signal. A missing key (no match) returns 0, which
			// must not be confused with a real hit that happens to score 0.
			lex, lexHit = bm25[f.ID]
		} else {
			lex = float64(factLexicalScore(f, query, queryLocus))
			lexHit = lex > 0
		}
		cos := 0.0
		semHit := false
		if haveSemantic {
			if c, ok := storeCos[f.ID]; ok && !math.IsNaN(c) && !math.IsInf(c, 0) {
				if _, exists := rr.cache[f.ID]; exists {
					cos = c
					semHit = true
					rr.markTouched(f.ID)
					rr.lastRun.ValidCandidateVectors++
				}
			}
			if !semHit {
				fvec := rr.factVector(f)
				if candidateCos, ok := cosineFloat32WithQueryNorm(qvec, queryNorm, fvec); ok {
					cos = candidateCos
					semHit = true
					rr.lastRun.ValidCandidateVectors++
				}
			}
		}
		cands = append(cands, cand{rec: f, lex: lex, lexHit: lexHit, cos: cos, semHit: semHit, semanticValid: semHit})
	}
	if haveSemantic {
		cosines := make([]float64, len(cands))
		needsCalibration := false
		for index := range cands {
			cosines[index] = cands[index].cos
			needsCalibration = needsCalibration || (cands[index].semanticValid && !cands[index].lexHit)
		}
		var backgrounds []float64
		if needsCalibration {
			vectors := make([][]float32, len(cands))
			for index := range cands {
				if cands[index].semanticValid {
					vectors[index] = rr.factVector(cands[index].rec)
				}
			}
			backgrounds = semanticLeaveOneOutBackgrounds(qvec, vectors)
		}
		mask := semanticResultMask(cosines, needsCalibration, backgrounds)
		for index := range cands {
			cands[index].semantic = mask[index]
		}
	}

	// Lexical ranks: only facts retrieved lexically (lexHit) contribute a lexical
	// RRF term. Hits sort above misses so a BM25 hit that happens to score 0 still
	// earns a rank instead of being lumped in with the non-matches.
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if cands[ia].lexHit != cands[ib].lexHit {
			return cands[ia].lexHit
		}
		if cands[ia].lex != cands[ib].lex {
			return cands[ia].lex > cands[ib].lex
		}
		return cands[ia].rec.UpdatedAt.After(cands[ib].rec.UpdatedAt)
	})
	fused := make([]float64, len(cands))
	for rank, idx := range order {
		if cands[idx].lexHit {
			fused[idx] += 1.0 / (rrfK + float64(rank+1))
		}
	}

	// Semantic ranks: semantic-only candidates must clear the corpus-relative
	// gate. Lexical hits retain their semantic rank. The oversampled depth keeps
	// the weak tail of a small static model from displacing a top lexical match.
	if haveSemantic {
		semanticOrder := make([]int, 0, len(cands))
		for index := range cands {
			if !cands[index].semanticValid {
				continue
			}
			if cands[index].semantic || cands[index].lexHit {
				semanticOrder = append(semanticOrder, index)
			}
		}
		sort.SliceStable(semanticOrder, func(a, b int) bool {
			ia, ib := semanticOrder[a], semanticOrder[b]
			if cands[ia].semHit != cands[ib].semHit {
				return cands[ia].semHit
			}
			if cands[ia].cos != cands[ib].cos {
				return cands[ia].cos > cands[ib].cos
			}
			return cands[ia].rec.UpdatedAt.After(cands[ib].rec.UpdatedAt)
		})
		semanticDepth := semanticFusionDepth(limit, semanticDepthMultiplier, len(semanticOrder))
		semanticOrder = semanticOrder[:semanticDepth]
		for rank, idx := range semanticOrder {
			fused[idx] += 1.0 / (rrfK + float64(rank+1))
		}
		calibratedRank := 0
		for _, idx := range semanticOrder {
			if !cands[idx].semantic || cands[idx].lexHit {
				continue
			}
			fused[idx] += 1.0 / (rrfK + float64(calibratedRank+1))
			calibratedRank++
		}
	}

	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if fused[ia] != fused[ib] {
			return fused[ia] > fused[ib]
		}
		if !cands[ia].rec.UpdatedAt.Equal(cands[ib].rec.UpdatedAt) {
			return cands[ia].rec.UpdatedAt.After(cands[ib].rec.UpdatedAt)
		}
		if cands[ia].semanticValid != cands[ib].semanticValid {
			return cands[ia].semanticValid
		}
		if cands[ia].semanticValid && cands[ia].cos != cands[ib].cos {
			return cands[ia].cos > cands[ib].cos
		}
		return cands[ia].rec.ID < cands[ib].rec.ID
	})
	// Only facts that earned a retrieval signal (fused > 0) are returned. The
	// oversampled semantic window and every lexical hit are eligible; when
	// embeddings are unavailable and nothing matched lexically, every fused score
	// is 0 — return nothing rather than an arbitrary recency-ordered top-N
	// (matches rankFacts's score>0 filter).
	out := make([]factRecord, 0, min(limit, len(order)))
	for _, idx := range order {
		if fused[idx] <= 0 {
			break // order is sorted by fused desc, so the rest are 0 too
		}
		out = append(out, cands[idx].rec)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// semanticFusionDepth computes limit*multiplier capped to the candidate count
// without overflowing int. A non-positive multiplier means the full candidate
// set, which is retained as the comparison arm in tests.
func semanticFusionDepth(limit, multiplier, candidates int) int {
	if candidates <= 0 {
		return 0
	}
	if limit <= 0 {
		limit = 10
	}
	if multiplier <= 0 || limit > candidates/multiplier {
		return candidates
	}
	return limit * multiplier
}

// validSemanticEmbedding rejects failure-shaped vectors before they can earn
// an RRF rank. Length alone is insufficient: a backend can return the expected
// number of zero or non-finite values while still providing no semantic signal.
func validSemanticEmbedding(v []float32, dim int) bool {
	_, valid := semanticEmbeddingSquaredNorm(v, dim)
	return valid
}

func semanticEmbeddingSquaredNorm(v []float32, dim int) (float64, bool) {
	if dim <= 0 || len(v) != dim {
		return 0, false
	}
	norm := 0.0
	for _, value := range v {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		norm += f * f
	}
	return norm, norm > 0 && !math.IsInf(norm, 0)
}

// cosineFloat32WithQueryNorm validates the candidate while computing its
// cosine in one pass. rankFactsFused has already validated the query and keeps
// its exact squared norm, avoiding two redundant vector scans per candidate:
// candidate validation and recomputing the same query norm for every cosine.
func cosineFloat32WithQueryNorm(query []float32, queryNorm float64, candidate []float32) (float64, bool) {
	if len(query) == 0 || len(query) != len(candidate) || queryNorm <= 0 || math.IsInf(queryNorm, 0) || math.IsNaN(queryNorm) {
		return 0, false
	}
	var dot, candidateNorm float64
	for i, value := range candidate {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		dot += float64(query[i]) * f
		candidateNorm += f * f
	}
	if candidateNorm <= 0 || math.IsInf(candidateNorm, 0) {
		return 0, false
	}
	return dot / (math.Sqrt(queryNorm) * math.Sqrt(candidateNorm)), true
}

// cosineFloat32 is the cosine similarity of two vectors. Vectors from the
// static embedder are already unit length, but guarding the norms keeps it
// correct for the zero vector (text that tokenizes to nothing, e.g.
// empty/whitespace-only) — which yields 0.
func cosineFloat32(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
