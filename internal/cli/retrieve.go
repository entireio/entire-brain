package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// retrieve.go is the unified retrieval layer behind the qmd-inspired verbs
// (search/vsearch/query). It ranks across the brain's text layers — facts,
// history, docs — and merges the per-source ranked lists with RRF. Each layer
// keeps its own best ranker; RRF fuses them by rank so their incomparable raw
// scores don't fight.

type unifiedResult struct {
	Source               string            `json:"source"` // fact | fact-review | history | doc | consolidation | theme | workspace_pattern | workspace_graph
	ID                   string            `json:"id"`     // prefixed, addressable by get/multi-get
	Path                 string            `json:"path,omitempty"`
	Heading              string            `json:"heading,omitempty"`
	Line                 int               `json:"line,omitempty"`
	Text                 string            `json:"text"`
	Score                float64           `json:"score,omitempty"` // omitted for unranked results (get/multi-get); RRF scores are always > 0
	VerificationRequired bool              `json:"verification_required,omitempty"`
	Caveats              []retrievalCaveat `json:"caveats,omitempty"`
	RelatedIDs           []string          `json:"related_ids,omitempty"`
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
// hybrid (query) include all three; vector (vsearch) covers facts + docs always,
// and history only behind the fusion gate (a fusion-eligible embedder plus the
// brain_cgo vec0 store with refresh-built vectors — see history_vec.go). The
// gate exists because Model2Vec on history is a measured closed negative and
// embedding hundreds of thousands of records per query is hours of work that
// belongs in refresh. Missing layers are skipped, not errors.
func retrieveUnified(repoDir, brainDir, branch, query string, limit int, mode retrievalMode) ([]unifiedResult, error) {
	if limit <= 0 {
		limit = 10
	}
	// Preserve the existing 2x per-layer candidate budget. The facts arm adds a
	// bounded collapse reserve before applying trust state, so unrelated
	// history/doc RRF candidates never change merely because a proposal entered
	// the review queue without converting the entire fact corpus on every query.
	candidateLimit := limit * 2
	if candidateLimit < limit { // integer overflow guard for unreasonable inputs
		candidateLimit = limit
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
	proposals, proposalsErr := loadFactProposals(brainDir, branch)
	var e Embedder
	if mode != modeLexical {
		e = defaultEmbedder()
	}

	var lists [][]unifiedResult

	// Facts.
	if len(active) > 0 {
		factLimit := min(len(active), candidateLimit)
		if proposalsErr == nil {
			factLimit = guardedFactCandidateLimit(active, proposals, candidateLimit)
		}
		var factResults []unifiedResult
		switch mode {
		case modeLexical:
			factResults = factsToUnified(rankFacts(active, query, factLimit, false))
		case modeVector:
			if e != nil {
				// Pass the full set: factsVectorRanked ranks active facts but caches
				// (and prunes) every present fact, matching the reranker's shared store.
				factResults = factsToUnified(factsVectorRanked(
					brainDir, branch, all, query, e, factLimit,
				))
			}
		case modeHybrid:
			var rr *semanticReranker
			if e != nil {
				rr = newSemanticRerankerForBranch(e, brainDir, branch)
			}
			factResults = factsToUnified(rankFactsFused(active, query, factLimit, false, rr))
			if rr != nil {
				rr.retain(all) // keep every present fact's vector; prune only departed facts (matches recall/brief)
				_ = rr.flush()
			}
		}
		if proposalsErr == nil {
			factResults = guardUnifiedFactResults(repoDir, all, proposals, factResults, candidateLimit)
		} else {
			factResults = guardUnifiedFactResults(repoDir, all, nil, factResults, candidateLimit)
			factResults = annotateProposalStateUnavailable(factResults)
		}
		if len(factResults) > 0 {
			lists = append(lists, factResults)
		}
	}

	// History — BM25 (lexical / hybrid), plus the gated semantic arm (hybrid /
	// vector; see history_vec.go). The FTS index is an optimization, never
	// load-bearing (see history_fts.go): on a build/open/query failure fall
	// back to the in-memory substring scorer. But a corrupt manifest, or a history
	// index the manifest declares yet is missing/unreadable, is a real storage
	// problem — surface it rather than returning silently-incomplete results. A
	// brain with no history source (nil) is legitimately skipped.
	historySem := historySemanticEmbedder(e)
	if mode != modeVector || historySem != nil {
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return nil, err
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
			if err != nil {
				return nil, fmt.Errorf("load history index: %w", err)
			}
			var lexicalHistoryIDs map[string]struct{}
			if mode != modeVector {
				historyCandidateLimit := candidateLimit * 3
				if historyCandidateLimit < candidateLimit {
					historyCandidateLimit = candidateLimit
				}
				scored, ok := rankHistoryViaFTS(brainDir, index, "history", query, historyCandidateLimit)
				if !ok {
					scored = rankHistoryRecordsScored(index, "history", query, historyCandidateLimit, 0)
				}
				scored = filterHistoryRetrievalSelfEchoes(scored, query)
				if len(scored) > candidateLimit {
					scored = scored[:candidateLimit]
				}
				if len(scored) > 0 {
					lexicalHistoryIDs = make(map[string]struct{}, len(scored))
					for _, record := range scored {
						lexicalHistoryIDs[record.Record.ID] = struct{}{}
					}
					lists = append(lists, historyToUnified(scored))
				}
			}
			if mode != modeLexical && historySem != nil {
				// Independently ranked history lists: the global RRF merge fuses
				// BM25 and cosine, then gives calibrated term-disjoint evidence a
				// second vote without triple-counting lexical hits.
				scores := historySemanticScores(
					brainDir, historySem, query, candidateLimit, mode == modeHybrid,
				)
				var sem historyVectorRanks
				if mode == modeVector {
					sem.ranked = rankHistorySemantic(index, scores, candidateLimit)
				} else {
					sem = rankHistorySemanticHybridRanks(index, scores, candidateLimit, lexicalHistoryIDs)
				}
				if len(sem.ranked) > 0 {
					lists = append(lists, historyToUnified(sem.ranked))
				}
				if len(sem.calibratedSemanticOnly) > 0 {
					lists = append(lists, historyToUnified(sem.calibratedSemanticOnly))
				}
			}
		}
	}

	// Docs — lexical (search/query) and/or vector (vsearch/query). In hybrid mode
	// docs join BOTH arms, mirroring facts. A never-built doc index is skipped; a
	// corrupt/unreadable one is a real storage problem and is surfaced.
	docIdx, derr := loadDocIndex(brainDir)
	switch {
	case derr == nil && len(docIdx.Records) > 0:
		var lexicalDocIDs map[string]struct{}
		if mode != modeVector {
			// FTS is an optimization, never load-bearing: fall back to the in-memory
			// lexical scorer so docs don't vanish when the doc FTS index can't open.
			scored, ok := rankDocsViaFTS(brainDir, docIdx, query, candidateLimit)
			if !ok {
				scored = rankDocsLexical(docIdx, query, candidateLimit)
			}
			if len(scored) > 0 {
				lexicalDocIDs = make(map[string]struct{}, len(scored))
				for _, record := range scored {
					lexicalDocIDs[record.Record.ID] = struct{}{}
				}
				lists = append(lists, docsToUnified(scored))
			}
		}
		if mode != modeLexical && e != nil {
			vectorRanks := docsVectorRanked(
				brainDir, docIdx, query, e, candidateLimit, mode == modeHybrid, lexicalDocIDs,
			)
			if len(vectorRanks.ranked) > 0 {
				lists = append(lists, vectorRanks.ranked)
			}
			if len(vectorRanks.calibratedSemanticOnly) > 0 {
				lists = append(lists, vectorRanks.calibratedSemanticOnly)
			}
		}
	case derr != nil && !os.IsNotExist(derr):
		return nil, fmt.Errorf("load doc index: %w", derr)
	}

	return rrfMergeUnified(lists, limit), nil
}

func filterHistoryRetrievalSelfEchoes(scored []scoredHistoryRecord, query string) []scoredHistoryRecord {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return scored
	}
	out := scored[:0]
	for _, item := range scored {
		kind := strings.ToLower(strings.TrimSpace(item.Record.Kind))
		summary := strings.ToLower(item.Record.Summary)
		brainInvocation := strings.Contains(summary, "entire brain search") ||
			strings.Contains(summary, "entire brain query") ||
			strings.Contains(summary, "entire brain brief") ||
			strings.Contains(summary, "brain_search") ||
			strings.Contains(summary, "brain_query") ||
			strings.Contains(summary, "brain_brief")
		if kind == "tool_call" && brainInvocation && strings.Contains(summary, needle) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// factsVectorRanked ranks active facts by cosine. facts is the full present set
// (all statuses): active facts are ranked, but every present fact is embedded and
// retained in the shared cache so this path keeps the same vectors the recall/brief
// reranker does — and departed facts are pruned so the on-disk cache stays bounded.
func factsVectorRanked(
	brainDir, branch string,
	facts []factRecord,
	query string,
	e Embedder,
	limit int,
) []factRecord {
	// An empty query vector means the embedder is unavailable (e.g. Ollama down).
	// Return no semantic results rather than an arbitrary top-N: every cosine
	// would be 0 and the sort would just echo input order. A wrong-dimension
	// query also fails closed before the cache is inspected: it must not make
	// valid document vectors look corrupt and trigger a destructive rebuild.
	qv := embedQueryWith(e, query)
	if e.Dim() <= 0 || len(qv) != e.Dim() || !vectorHasMagnitude(qv) {
		return nil
	}
	store := newVectorStore(brainDir, branch, factEmbeddingModelID(e.ID()), e.Dim())
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
		if ok && (len(v) != len(qv) || !vectorHasMagnitude(v)) {
			// A nil value is a deletion tombstone for savePresent: it overrides
			// any invalid entry reloaded under the write lock, then is omitted
			// from the rewritten store if re-embedding fails.
			cache[f.ID] = nil
			dirty = true
			ok = false
		}
		if !ok {
			v = e.Embed(factEmbeddingText(f))
			if len(v) == len(qv) && vectorHasMagnitude(v) {
				cache[f.ID] = v // never cache empty/mismatched vectors
				dirty = true
			}
		}
		if f.Status != factStatusActive || len(v) != len(qv) {
			continue // rank active facts only; skip degenerate vectors
		}
		if !vectorHasMagnitude(v) {
			continue
		}
		scored = append(scored, sc{rec: f, cos: cosineFloat32(qv, v)})
	}
	if saved := pruneToPresent(cache, present); dirty || saved {
		_ = store.savePresent(cache, present)
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

type documentVectorRanks struct {
	ranked                 []unifiedResult
	calibratedSemanticOnly []unifiedResult
}

func docsVectorRanked(
	brainDir string,
	index docIndex,
	query string,
	e Embedder,
	limit int,
	requireRelevance bool,
	lexicalDocIDs map[string]struct{},
) documentVectorRanks {
	// Same guard as factsVectorRanked: no query vector → no doc semantic hits,
	// not arbitrary docs ranked by all-zero cosines.
	qv := embedQueryWith(e, query)
	if e.Dim() <= 0 || len(qv) != e.Dim() || !vectorHasMagnitude(qv) {
		return documentVectorRanks{}
	}
	store := newDocEmbedStore(brainDir, e.ID(), e.Dim())
	cache := store.load()
	dirty := false
	present := make(map[string]struct{}, len(index.Records))
	type sc struct {
		i      int
		cos    float64
		vector []float32
		keep   bool
	}
	scored := make([]sc, 0, len(index.Records))
	for i, r := range index.Records {
		present[r.ID] = struct{}{}
		v, ok := cache[r.ID]
		if ok && (len(v) != len(qv) || !vectorHasMagnitude(v)) {
			cache[r.ID] = nil // persist deletion if repair fails
			dirty = true
			ok = false
		}
		if !ok {
			v = e.Embed(r.Text)
			if len(v) == len(qv) && vectorHasMagnitude(v) {
				cache[r.ID] = v
				dirty = true
			}
		}
		if len(v) != len(qv) {
			continue
		}
		if !vectorHasMagnitude(v) {
			continue
		}
		entry := sc{i: i, cos: cosineFloat32(qv, v)}
		if requireRelevance {
			entry.vector = v
		}
		scored = append(scored, entry)
	}
	// Doc ids are content-derived, so any rebuild churns them; prune departed ids
	// so the cache stays bounded to the current doc corpus.
	if saved := pruneToPresent(cache, present); dirty || saved {
		_ = store.savePresent(cache, present)
	}
	cosines := make([]float64, len(scored))
	vectors := make([][]float32, len(scored))
	for i := range scored {
		cosines[i] = scored[i].cos
		vectors[i] = scored[i].vector
	}
	var backgrounds []float64
	if requireRelevance {
		backgrounds = semanticLeaveOneOutBackgrounds(qv, vectors)
	}
	mask := semanticResultMask(cosines, requireRelevance, backgrounds)
	for i := range scored {
		scored[i].keep = mask[i]
	}
	sort.Slice(scored, func(a, b int) bool {
		if scored[a].cos != scored[b].cos {
			return scored[a].cos > scored[b].cos
		}
		return index.Records[scored[a].i].ID < index.Records[scored[b].i].ID
	})
	// Keep old plans discoverable, but exhaust current material before returning
	// explicitly historical chunks. Similarity order is preserved within each
	// class, and historical results are labeled by docToUnified below.
	sort.SliceStable(scored, func(a, b int) bool {
		return !index.Records[scored[a].i].Historical && index.Records[scored[b].i].Historical
	})
	out := documentVectorRanks{
		ranked:                 make([]unifiedResult, 0, min(limit, len(scored))),
		calibratedSemanticOnly: make([]unifiedResult, 0),
	}
	for _, s := range scored {
		_, lexical := lexicalDocIDs[index.Records[s.i].ID]
		if !s.keep && !(requireRelevance && lexical) {
			continue
		}
		result := docToUnified(index.Records[s.i])
		if len(out.ranked) < limit {
			out.ranked = append(out.ranked, result)
		}
		if requireRelevance && s.keep && !lexical && len(out.calibratedSemanticOnly) < limit {
			out.calibratedSemanticOnly = append(out.calibratedSemanticOnly, result)
		}
		if len(out.ranked) >= limit && (!requireRelevance || len(out.calibratedSemanticOnly) >= limit) {
			break
		}
	}
	return out
}

func factsToUnified(facts []factRecord) []unifiedResult {
	// fact ids are already prefixed "fact:" (factRecordID); history likewise.
	out := make([]unifiedResult, len(facts))
	for i, f := range facts {
		out[i] = unifiedResult{Source: "fact", ID: f.ID, Path: strings.Join(f.Paths, ","), Heading: factKindOrInferred(f), Text: f.Text}
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
	result := unifiedResult{Source: "doc", ID: "doc:" + r.ID, Path: r.Path, Line: r.Line, Heading: r.Heading, Text: r.Text}
	if r.Historical {
		result.VerificationRequired = true
		result.Caveats = []retrievalCaveat{{
			Kind:    retrievalCaveatHistoricalDocument,
			Message: "This document is explicitly historical or superseded; prefer current operational documentation.",
			Paths:   []string{r.Path},
			Action:  "Verify against the current README and active implementation before relying on it.",
		}}
	}
	return result
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

// getUnifiedBatch resolves prefixed ids (fact:/review:/history:/doc:/pattern:/theme:) to full records,
// loading each corpus at most once and indexing it by id. get/multi-get (and the
// MCP brain_get/brain_multi_get) route through here so resolving N ids is O(corpus
// + N), not O(N × corpus) — the latter rescans the full history per id and is
// pathological on large brains. Results preserve input order.
func getUnifiedBatch(repoDir, brainDir, branch string, ids []string) (found []unifiedResult, missing []string, err error) {
	var wantFact, wantReview, wantHistory, wantDoc bool
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "fact:"):
			wantFact = true
		case strings.HasPrefix(id, "review:"):
			wantReview = true
		case strings.HasPrefix(id, "history:"):
			wantHistory = true
		case strings.HasPrefix(id, "doc:"):
			wantDoc = true
		}
	}
	factByID := map[string]factRecord{}
	var reviewByID map[string]factReviewGroup
	var reviewByFactID map[string]factReviewGroup
	var proposalStateUnavailable bool
	if wantFact || wantReview {
		// Surface a corrupt facts store as an error, not a misleading "not found".
		facts, ferr := loadFacts(brainDir, branch)
		if ferr != nil {
			return nil, nil, ferr
		}
		for _, f := range facts {
			factByID[f.ID] = f
		}
		proposals, perr := loadFactProposals(brainDir, branch)
		if perr != nil && wantReview {
			return nil, nil, perr
		}
		if perr == nil {
			reviewByID, reviewByFactID = indexFactReviewGroups(buildFactReviewGroups(facts, proposals))
		} else {
			proposalStateUnavailable = true
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
				r := factsToUnified([]factRecord{f})[0]
				if group, pending := reviewByFactID[id]; pending {
					r = annotateExplicitFactReview(r, group)
				}
				r = annotateFactLocusTrust(repoDir, f, r)
				if proposalStateUnavailable {
					r = annotateProposalStateUnavailable([]unifiedResult{r})[0]
				}
				found = append(found, r)
				continue
			}
		case strings.HasPrefix(id, "review:"):
			if group, ok := reviewByID[id]; ok {
				r := factReviewToUnified(repoDir, group)
				// Preserve an addressable proposal alias if the group grew, including
				// the machine-readable caveat that tells clients what to review.
				r.ID = id
				for i := range r.Caveats {
					if r.Caveats[i].Kind == retrievalCaveatUnresolvedReview {
						r.Caveats[i].ReviewID = id
					}
				}
				found = append(found, r)
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
		case strings.HasPrefix(id, "pattern:"):
			// Consolidation dossier addressed by its pattern id (v2). Corpus is
			// rebuildable/optional, so a missing corpus is "not found", not an error.
			if r, ok := getCorpusConsolidation(brainDir, id); ok {
				found = append(found, r)
				continue
			}
		case strings.HasPrefix(id, "theme:"):
			if r, ok := getCorpusTheme(brainDir, id); ok {
				found = append(found, r)
				continue
			}
		}
		missing = append(missing, id)
	}
	return found, missing, nil
}
