package cli

import (
	"math"
	"sort"
)

// rrfK is the Reciprocal Rank Fusion constant. 60 is the canonical value from
// the original RRF paper and what hybrid-search systems (e.g. qmd) use; it
// damps the influence of a single retriever's top ranks so neither the lexical
// nor the semantic list can unilaterally dominate the fusion.
const rrfK = 60.0

// semanticReranker holds the embedder and a cache of fact vectors. Fact ids are
// content-derived, so a vector is valid for the life of the fact: the in-memory
// cache makes an eval over many queries embed each fact at most once, and an
// optional disk store (recall/brief) carries vectors across CLI invocations.
type semanticReranker struct {
	e       Embedder
	cache   map[string][]float32
	store   *embedStore     // nil => in-memory only (eval, tests)
	touched map[string]bool // ids seen this run; nil unless disk-backed
	dirty   bool            // a new vector was embedded this run
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
	rr.store = newEmbedStore(brainDir, branch, e.ID(), e.Dim())
	rr.cache = rr.store.load()
	rr.touched = map[string]bool{}
	return rr
}

func (s *semanticReranker) factVector(f factRecord) []float32 {
	if s.touched != nil {
		s.touched[f.ID] = true
	}
	if v, ok := s.cache[f.ID]; ok {
		return v
	}
	v := s.e.Embed(f.Text)
	s.cache[f.ID] = v
	s.dirty = true
	return v
}

// flush persists vectors embedded this run when disk-backed. It writes only the
// ids touched this run, so facts removed or superseded since the last run are
// pruned from the cache on rewrite. Best-effort: a cache is never load-bearing.
func (s *semanticReranker) flush() error {
	if s == nil || s.store == nil || !s.dirty {
		return nil
	}
	out := make(map[string][]float32, len(s.touched))
	for id := range s.touched {
		if v, ok := s.cache[id]; ok {
			out[id] = v
		}
	}
	return s.store.save(out)
}

// rankFactsFused ranks facts by Reciprocal Rank Fusion of a lexical list and a
// semantic (cosine) list. When rr is nil it is exactly rankFacts (pure
// lexical), so callers without an embedder are unaffected.
//
// The fusion is what attacks both measured failure modes at once:
//   - Reachability (the 0.667 lexical ceiling): the semantic list ranks the
//     *entire* active candidate set, so a relevant fact that shares no query
//     term still gets a rank and can surface — something no lexical weighting
//     can do.
//   - Ranking within reach: a fact strong in both lists fuses above one strong
//     in only one, reordering the lexically-reachable set by meaning.
//
// An empty query keeps the lexical path's recency listing (no query vector to
// compare against).
func rankFactsFused(facts []factRecord, query string, limit int, includeAll bool, rr *semanticReranker) []factRecord {
	if rr == nil || query == "" {
		return rankFacts(facts, query, limit, includeAll)
	}
	if limit <= 0 {
		limit = 10
	}
	type cand struct {
		rec factRecord
		lex int
		cos float64
	}
	queryLocus := factLocus(query)
	qvec := rr.e.Embed(query)
	cands := make([]cand, 0, len(facts))
	for _, f := range facts {
		if !includeAll && f.Status != factStatusActive {
			continue
		}
		score := factQueryScore(f, query)
		if overlap := locusOverlap(queryLocus, f.Text); overlap > 0 {
			score += overlap * factLocusBoost
		}
		cands = append(cands, cand{rec: f, lex: score, cos: cosineFloat32(qvec, rr.factVector(f))})
	}
	if len(cands) == 0 {
		return nil
	}

	// Lexical ranks: only facts with a positive lexical score are "retrieved"
	// lexically, so only they contribute a lexical RRF term.
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if cands[ia].lex != cands[ib].lex {
			return cands[ia].lex > cands[ib].lex
		}
		return cands[ia].rec.UpdatedAt.After(cands[ib].rec.UpdatedAt)
	})
	fused := make([]float64, len(cands))
	for rank, idx := range order {
		if cands[idx].lex > 0 {
			fused[idx] += 1.0 / (rrfK + float64(rank+1))
		}
	}

	// Semantic ranks: the full candidate set is ranked by cosine, so a
	// term-disjoint but semantically-near fact still earns a rank.
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if cands[ia].cos != cands[ib].cos {
			return cands[ia].cos > cands[ib].cos
		}
		return cands[ia].rec.UpdatedAt.After(cands[ib].rec.UpdatedAt)
	})
	for rank, idx := range order {
		fused[idx] += 1.0 / (rrfK + float64(rank+1))
	}

	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if fused[ia] != fused[ib] {
			return fused[ia] > fused[ib]
		}
		return cands[ia].rec.UpdatedAt.After(cands[ib].rec.UpdatedAt)
	})
	if len(order) > limit {
		order = order[:limit]
	}
	out := make([]factRecord, len(order))
	for i, idx := range order {
		out[i] = cands[idx].rec
	}
	return out
}

// cosineFloat32 is the cosine similarity of two vectors. Vectors from the
// static embedder are already unit length, but guarding the norms keeps it
// correct for the zero vector (empty/all-unknown text) — which yields 0.
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
