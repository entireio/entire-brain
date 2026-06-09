package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// retrieve.go is the unified retrieval layer behind the qmd-aligned verbs
// (search/vsearch/query). It ranks across the brain's text layers — facts,
// history, docs — and merges the per-source ranked lists with RRF. Each layer
// keeps its own best ranker; RRF fuses them by rank so their incomparable raw
// scores don't fight.

type unifiedResult struct {
	Source  string  `json:"source"` // fact | history | doc
	ID      string  `json:"id"`     // prefixed, addressable by get/multi-get
	Path    string  `json:"path,omitempty"`
	Heading string  `json:"heading,omitempty"`
	Line    int     `json:"line,omitempty"`
	Text    string  `json:"text"`
	Score   float64 `json:"score,omitempty"` // omitted for unranked results (get/multi-get); RRF scores are always > 0
}

type retrievalMode int

const (
	modeLexical retrievalMode = iota // search
	modeVector                       // vsearch
	modeHybrid                       // query
)

func embedQueryWith(e Embedder, q string) []float32 {
	if qe, ok := e.(queryEmbedder); ok {
		return qe.EmbedQuery(q)
	}
	return e.Embed(q)
}

// retrieveUnified ranks across facts + history + docs. Lexical (search) and
// hybrid (query) include all three; vector (vsearch) covers facts + docs —
// history vectors are Model2Vec noise and embedding tens of thousands of records
// per query is too slow, so they join the vector arm once Stage 1b's embedder
// lands. Missing layers are skipped, not errors.
func retrieveUnified(brainDir, branch, query string, limit int, mode retrievalMode) ([]unifiedResult, error) {
	if limit <= 0 {
		limit = 10
	}
	// loadFacts surfaces corrupt NDJSON as a hard error; propagate it rather than
	// presenting a broken store as "no results".
	all, err := loadFacts(brainDir, branch)
	if err != nil {
		return nil, err
	}
	active := make([]factRecord, 0, len(all))
	for _, f := range all {
		if f.Status == factStatusActive {
			active = append(active, f)
		}
	}
	var e Embedder
	if mode != modeLexical {
		e = defaultEmbedder()
	}

	var lists [][]unifiedResult

	// Facts.
	if len(active) > 0 {
		switch mode {
		case modeLexical:
			lists = append(lists, factsToUnified(rankFacts(active, query, limit*2, false)))
		case modeVector:
			if e != nil {
				// Pass the full set: factsVectorRanked ranks active facts but caches
				// (and prunes) every present fact, matching the reranker's shared store.
				lists = append(lists, factsToUnified(factsVectorRanked(brainDir, branch, all, query, e, limit*2)))
			}
		case modeHybrid:
			var rr *semanticReranker
			if e != nil {
				rr = newSemanticRerankerForBranch(e, brainDir, branch)
			}
			lists = append(lists, factsToUnified(rankFactsFused(active, query, limit*2, false, rr)))
			if rr != nil {
				rr.retain(all) // keep every present fact's vector; prune only departed facts (matches recall/brief)
				_ = rr.flush()
			}
		}
	}

	// History — BM25 (lexical / hybrid only). The FTS index is an optimization,
	// never load-bearing (see history_fts.go): on a build/open/query failure fall
	// back to the in-memory substring scorer. But a corrupt manifest, or a history
	// index the manifest declares yet is missing/unreadable, is a real storage
	// problem — surface it rather than returning silently-incomplete results. A
	// brain with no history source (nil) is legitimately skipped.
	if mode != modeVector {
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return nil, err
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
			if err != nil {
				return nil, fmt.Errorf("load history index: %w", err)
			}
			scored, ok := rankHistoryViaFTS(brainDir, index, "history", query, limit*2)
			if !ok {
				scored = rankHistoryRecordsScored(index, "history", query, limit*2, 0)
			}
			if len(scored) > 0 {
				lists = append(lists, historyToUnified(scored))
			}
		}
	}

	// Docs — lexical (search/query) and/or vector (vsearch/query). In hybrid mode
	// docs join BOTH arms, mirroring facts. A never-built doc index is skipped; a
	// corrupt/unreadable one is a real storage problem and is surfaced.
	docIdx, derr := loadDocIndex(brainDir)
	switch {
	case derr == nil && len(docIdx.Records) > 0:
		if mode != modeVector {
			// FTS is an optimization, never load-bearing: fall back to the in-memory
			// lexical scorer so docs don't vanish when the doc FTS index can't open.
			scored, ok := rankDocsViaFTS(brainDir, docIdx, query, limit*2)
			if !ok {
				scored = rankDocsLexical(docIdx, query, limit*2)
			}
			if len(scored) > 0 {
				lists = append(lists, docsToUnified(scored))
			}
		}
		if mode != modeLexical && e != nil {
			lists = append(lists, docsVectorRanked(brainDir, docIdx, query, e, limit*2))
		}
	case derr != nil && !os.IsNotExist(derr):
		return nil, fmt.Errorf("load doc index: %w", derr)
	}

	return rrfMergeUnified(lists, limit), nil
}

// factsVectorRanked ranks active facts by cosine. facts is the full present set
// (all statuses): active facts are ranked, but every present fact is embedded and
// retained in the shared cache so this path keeps the same vectors the recall/brief
// reranker does — and departed facts are pruned so the on-disk cache stays bounded.
func factsVectorRanked(brainDir, branch string, facts []factRecord, query string, e Embedder, limit int) []factRecord {
	// An empty query vector means the embedder is unavailable (e.g. Ollama down).
	// Return no semantic results rather than an arbitrary top-N: every cosine
	// would be 0 and the sort would just echo input order.
	qv := embedQueryWith(e, query)
	if len(qv) == 0 {
		return nil
	}
	store := newEmbedStore(brainDir, branch, e.ID(), e.Dim())
	cache := store.load()
	dirty := false
	present := make(map[string]struct{}, len(facts))
	type sc struct {
		rec factRecord
		cos float64
	}
	scored := make([]sc, 0, len(facts))
	for _, f := range facts {
		present[f.ID] = struct{}{}
		v, ok := cache[f.ID]
		if !ok {
			v = e.Embed(f.Text)
			if len(v) == len(qv) {
				cache[f.ID] = v // never cache empty/mismatched vectors
				dirty = true
			}
		}
		if f.Status != factStatusActive || len(v) != len(qv) {
			continue // rank active facts only; skip degenerate vectors
		}
		scored = append(scored, sc{f, cosineFloat32(qv, v)})
	}
	if saved := pruneToPresent(cache, present); dirty || saved {
		_ = store.save(cache)
	}
	sort.Slice(scored, func(a, b int) bool { return scored[a].cos > scored[b].cos })
	out := make([]factRecord, 0, min(limit, len(scored)))
	for _, s := range scored {
		out = append(out, s.rec)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// pruneToPresent drops cache entries whose id is no longer in present, keeping the
// on-disk vector cache bounded to the current corpus. Reports whether it removed
// anything (so callers save even when no new vector was embedded).
func pruneToPresent(cache map[string][]float32, present map[string]struct{}) bool {
	removed := false
	for id := range cache {
		if _, ok := present[id]; !ok {
			delete(cache, id)
			removed = true
		}
	}
	return removed
}

func docsVectorRanked(brainDir string, index docIndex, query string, e Embedder, limit int) []unifiedResult {
	// Same guard as factsVectorRanked: no query vector → no doc semantic hits,
	// not arbitrary docs ranked by all-zero cosines.
	qv := embedQueryWith(e, query)
	if len(qv) == 0 {
		return nil
	}
	store := newDocEmbedStore(brainDir, e.ID(), e.Dim())
	cache := store.load()
	dirty := false
	present := make(map[string]struct{}, len(index.Records))
	type sc struct {
		i   int
		cos float64
	}
	scored := make([]sc, 0, len(index.Records))
	for i, r := range index.Records {
		present[r.ID] = struct{}{}
		v, ok := cache[r.ID]
		if !ok {
			v = e.Embed(r.Text)
			if len(v) == len(qv) {
				cache[r.ID] = v
				dirty = true
			}
		}
		if len(v) != len(qv) {
			continue
		}
		scored = append(scored, sc{i, cosineFloat32(qv, v)})
	}
	// Doc ids are content-derived, so any rebuild churns them; prune departed ids
	// so the cache stays bounded to the current doc corpus.
	if saved := pruneToPresent(cache, present); dirty || saved {
		_ = store.save(cache)
	}
	sort.Slice(scored, func(a, b int) bool { return scored[a].cos > scored[b].cos })
	out := make([]unifiedResult, 0, min(limit, len(scored)))
	for _, s := range scored {
		out = append(out, docToUnified(index.Records[s.i]))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func factsToUnified(facts []factRecord) []unifiedResult {
	// fact ids are already prefixed "fact:" (factRecordID); history likewise.
	out := make([]unifiedResult, len(facts))
	for i, f := range facts {
		out[i] = unifiedResult{Source: "fact", ID: f.ID, Path: strings.Join(f.Paths, ","), Text: f.Text}
	}
	return out
}

func historyToUnified(scored []scoredHistoryRecord) []unifiedResult {
	out := make([]unifiedResult, len(scored))
	for i, s := range scored {
		out[i] = unifiedResult{Source: "history", ID: s.Record.ID, Path: s.Record.Path, Line: s.Record.Line, Heading: s.Record.Kind, Text: s.Record.Summary}
	}
	return out
}

func docToUnified(r docRecord) unifiedResult {
	return unifiedResult{Source: "doc", ID: "doc:" + r.ID, Path: r.Path, Line: r.Line, Heading: r.Heading, Text: r.Text}
}

func docsToUnified(scored []scoredDocRecord) []unifiedResult {
	out := make([]unifiedResult, len(scored))
	for i, s := range scored {
		out[i] = docToUnified(s.Record)
	}
	return out
}

func rrfMergeUnified(lists [][]unifiedResult, limit int) []unifiedResult {
	fused := map[string]float64{}
	rec := map[string]unifiedResult{}
	for _, list := range lists {
		for rank, r := range list {
			fused[r.ID] += 1.0 / (rrfK + float64(rank+1))
			if _, ok := rec[r.ID]; !ok {
				rec[r.ID] = r
			}
		}
	}
	out := make([]unifiedResult, 0, len(rec))
	for id, r := range rec {
		r.Score = fused[id]
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].ID < out[b].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// getUnifiedBatch resolves prefixed ids (fact:/history:/doc:) to full records,
// loading each corpus at most once and indexing it by id. get/multi-get (and the
// MCP brain_get/brain_multi_get) route through here so resolving N ids is O(corpus
// + N), not O(N × corpus) — the latter rescans the full history per id and is
// pathological on large brains. Results preserve input order.
func getUnifiedBatch(brainDir, branch string, ids []string) (found []unifiedResult, missing []string, err error) {
	var wantFact, wantHistory, wantDoc bool
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "fact:"):
			wantFact = true
		case strings.HasPrefix(id, "history:"):
			wantHistory = true
		case strings.HasPrefix(id, "doc:"):
			wantDoc = true
		}
	}
	factByID := map[string]factRecord{}
	if wantFact {
		// Surface a corrupt facts store as an error, not a misleading "not found".
		facts, ferr := loadFacts(brainDir, branch)
		if ferr != nil {
			return nil, nil, ferr
		}
		for _, f := range facts {
			factByID[f.ID] = f
		}
	}
	histByID := map[string]historyRecord{}
	if wantHistory {
		// A corrupt manifest, or a history index the manifest declares but that is
		// missing/unreadable, is a real storage problem — surface it rather than
		// reporting every history:* id as "not found". A brain with no history source
		// (nil) legitimately has no such records, so leave histByID empty.
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return nil, nil, err
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
			if err != nil {
				return nil, nil, fmt.Errorf("load history index: %w", err)
			}
			for _, r := range index.Records {
				histByID[r.ID] = r
			}
		}
	}
	docByID := map[string]docRecord{}
	if wantDoc {
		// A never-built doc index is not an error (doc:* ids just report "not
		// found"); a corrupt/unreadable one is — surface it.
		index, err := loadDocIndex(brainDir)
		switch {
		case err == nil:
			for _, r := range index.Records {
				docByID[r.ID] = r
			}
		case !os.IsNotExist(err):
			return nil, nil, fmt.Errorf("load doc index: %w", err)
		}
	}
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "fact:"):
			if f, ok := factByID[id]; ok {
				found = append(found, factsToUnified([]factRecord{f})[0])
				continue
			}
		case strings.HasPrefix(id, "history:"):
			if r, ok := histByID[id]; ok {
				found = append(found, historyToUnified([]scoredHistoryRecord{{Record: r}})[0])
				continue
			}
		case strings.HasPrefix(id, "doc:"):
			if r, ok := docByID[strings.TrimPrefix(id, "doc:")]; ok {
				found = append(found, docToUnified(r))
				continue
			}
		}
		missing = append(missing, id)
	}
	return found, missing, nil
}
