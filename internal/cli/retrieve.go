package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// retrieve.go is the unified retrieval layer behind the qmd-inspired verbs
// (search/vsearch/query). It ranks across the brain's text layers — facts,
// history, docs — and merges the per-source ranked lists with RRF. Each layer
// keeps its own best ranker; RRF fuses them by rank so their incomparable raw
// scores don't fight.

type unifiedResult struct {
	Source               string            `json:"source"` // fact | fact-review | history | conversation | doc | consolidation | theme | workspace_pattern | workspace_graph
	ID                   string            `json:"id"`     // prefixed, addressable by get/multi-get
	Path                 string            `json:"path,omitempty"`
	Heading              string            `json:"heading,omitempty"`
	Line                 int               `json:"line,omitempty"`
	Text                 string            `json:"text"`
	Score                float64           `json:"score,omitempty"` // omitted for unranked results (get/multi-get); RRF scores are always > 0
	VerificationRequired bool              `json:"verification_required,omitempty"`
	Caveats              []retrievalCaveat `json:"caveats,omitempty"`
	RelatedIDs           []string          `json:"related_ids,omitempty"`

	// Conversation-exchange provenance (experimental, additive; empty for every
	// other source).
	EndLine   int    `json:"end_line,omitempty"` // inclusive 1-based source range end
	Branch    string `json:"branch,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Truncated bool   `json:"truncated,omitempty"` // expanded/projected text is bounded, not complete
	// MatchedTerms lists the query tokens that actually hit this record, so a
	// weak match is diagnosable instead of opaque (conversation search only).
	MatchedTerms []string `json:"matched_terms,omitempty"`
}

type retrievalMode int

const (
	modeLexical retrievalMode = iota // search
	modeVector                       // vsearch
	modeHybrid                       // query
)

// Retrieval source selectors. Phase 1 of the conversational-memory plan:
// "all" retains the pre-Phase-1 source set (facts + classified history + docs);
// conversation exchanges are returned only when explicitly selected.
const (
	retrievalSourceAll          = "all"
	retrievalSourceFact         = "fact"
	retrievalSourceHistory      = "history"
	retrievalSourceConversation = "conversation"
	retrievalSourceDoc          = "doc"
)

// retrievalOptions is the shared CLI/MCP retrieval-options contract; extend it
// rather than widening positional signatures (cross-cutting rule of the
// conversational-memory plan).
type retrievalOptions struct {
	// Source selects the layer(s) to rank: "" or "all" is the default set.
	Source string
	// Structured conversation-source filters (Phase 2). Zero values mean no
	// filter. After/Before bound the session time (After inclusive, Before
	// exclusive); records without a session time are excluded whenever a time
	// filter is set, so a filter can never leak an unprovable record into
	// scope. SessionID and Agent are exact (Agent case-insensitive) matches.
	// Branch filters on the captured branch. All five apply only when
	// Source == "conversation"; supplying After/Before/SessionID/Agent with
	// another source is a structured error, never silently ignored.
	After     time.Time
	Before    time.Time
	SessionID string
	Agent     string
	Branch    string
}

// hasConversationOnlyFilters reports filters that have no meaning outside the
// conversation source. Branch is excluded: it is a long-standing facts-branch
// selector on every retrieval surface and doubles as the conversation branch
// filter when that source is selected.
func (o retrievalOptions) hasConversationOnlyFilters() bool {
	return !o.After.IsZero() || !o.Before.IsZero() ||
		strings.TrimSpace(o.SessionID) != "" || strings.TrimSpace(o.Agent) != ""
}

// parseRetrievalTimeFilter parses a CLI/MCP time filter: RFC3339 or a plain
// YYYY-MM-DD day (interpreted as UTC midnight). Empty means unset.
func parseRetrievalTimeFilter(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("time filter must be RFC3339 or YYYY-MM-DD (got %q)", value)
}

// parseRetrievalSource validates a user/client-supplied source selector,
// normalizing "" to "all". CLI and MCP share these semantics.
func parseRetrievalSource(value string) (string, error) {
	source := strings.ToLower(strings.TrimSpace(value))
	switch source {
	case "":
		return retrievalSourceAll, nil
	case retrievalSourceAll, retrievalSourceFact, retrievalSourceHistory, retrievalSourceConversation, retrievalSourceDoc:
		return source, nil
	default:
		return "", fmt.Errorf("source must be one of all, fact, history, conversation, doc (got %q)", value)
	}
}

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
	return retrieveUnifiedWithOptions(repoDir, brainDir, branch, query, limit, mode, retrievalOptions{})
}

// errConversationVectorUnsupported is the structured answer for explicit
// vector search over the conversation source when the semantic arm is
// unavailable: the arm requires the fusion-eligible embedder opt-in
// (ENTIRE_BRAIN_EMBEDDER with a Gemma-class server), the brain_cgo build's
// vec0 store, and refresh-built conversation vectors. Lexical search and query
// keep working without any of that.
var errConversationVectorUnsupported = errors.New(`source "conversation" vector search is unavailable: it requires a fusion-eligible embedder (ENTIRE_BRAIN_EMBEDDER), the brain_cgo build, and conversation vectors built by refresh; use search or query with --source conversation for lexical recall`)

func retrieveUnifiedWithOptions(repoDir, brainDir, branch, query string, limit int, mode retrievalMode, opts retrievalOptions) ([]unifiedResult, error) {
	if limit <= 0 {
		limit = 10
	}
	source := opts.Source
	if source == "" {
		source = retrievalSourceAll
	}
	if source == retrievalSourceConversation {
		return retrieveConversation(brainDir, query, limit, mode, opts)
	}
	if opts.hasConversationOnlyFilters() {
		return nil, fmt.Errorf(`after/before/session/agent filters require source "conversation" (got %q)`, source)
	}
	includeFacts := source == retrievalSourceAll || source == retrievalSourceFact
	includeHistory := source == retrievalSourceAll || source == retrievalSourceHistory
	includeDocs := source == retrievalSourceAll || source == retrievalSourceDoc
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
	var all []factRecord
	var active []factRecord
	var proposals []factProposal
	var proposalsErr error
	if includeFacts {
		var err error
		all, err = loadFacts(brainDir, branch)
		if err != nil {
			return nil, err
		}
		active = make([]factRecord, 0, len(all))
		for _, f := range all {
			if f.Status == factStatusActive {
				active = append(active, f)
			}
		}
		proposals, proposalsErr = loadFactProposals(brainDir, branch)
	}
	var e Embedder
	if mode != modeLexical {
		e = defaultEmbedder()
	}

	var lists [][]unifiedResult

	// Facts.
	if includeFacts && len(active) > 0 {
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
	if includeHistory && (mode != modeVector || historySem != nil) {
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return nil, err
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
			if err != nil {
				return nil, fmt.Errorf("load history index: %w", err)
			}
			// Semantic arms rank the long-term records minus files the
			// short-term overlay superseded (overlay records have no vectors
			// until consolidation; the lexical tier carries their freshness).
			index := fresh.longTermActive()
			var lexicalHistoryIDs map[string]struct{}
			if mode != modeVector {
				historyCandidateLimit := candidateLimit * 3
				if historyCandidateLimit < candidateLimit {
					historyCandidateLimit = candidateLimit
				}
				scored := rankFreshHistory(fresh, "history", query, historyCandidateLimit, nil, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
					return rankHistoryViaFTS(brainDir, longTerm, "history", query, historyCandidateLimit)
				})
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
	var docIdx docIndex
	var derr error
	if includeDocs {
		docIdx, derr = loadDocIndex(brainDir)
	} else {
		derr = os.ErrNotExist
	}
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
		left := docTrustAdjustedScore(scored[a].cos, index.Records[scored[a].i].Historical)
		right := docTrustAdjustedScore(scored[b].cos, index.Records[scored[b].i].Historical)
		if left != right {
			return left > right
		}
		return index.Records[scored[a].i].ID < index.Records[scored[b].i].ID
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

// conversationSessionTopKDivisor sets the default per-session share of a
// conversation result list: one session may hold at most limit/2 (min 1) of
// the returned results while other sessions still have candidates. Dogfooding
// measured single sessions holding 29–84% of a brain's exchanges; without a
// cap one long session crowds out every other trajectory.
const conversationSessionTopKDivisor = 2

// retrieveConversation is the explicit conversation-source arm over exchange
// records only, then structured filters and a per-session diversity cap. It
// never consults facts, docs, or classified history, and every result carries
// the historical-evidence contract. Ranking by mode: search is BM25 (substring
// scorer as fallback); query fuses BM25 with calibrated conversation vectors
// when the gated semantic arm is available and degrades to exactly the lexical
// ranking otherwise; vsearch is semantic-only and returns a structured
// unavailable error when the arm is closed.
func retrieveConversation(brainDir, query string, limit int, mode retrievalMode, opts retrievalOptions) ([]unifiedResult, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		if mode == modeVector {
			return nil, errConversationVectorUnsupported
		}
		return nil, nil
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		return nil, fmt.Errorf("load history index: %w", err)
	}
	filtered := opts.hasConversationOnlyFilters() || strings.TrimSpace(opts.Branch) != ""
	// Structured filters are pushed into candidate generation (R0-3): every
	// arm ranks only in-scope records, so a valid session/agent/time/branch
	// match can never be displaced out of a bounded candidate window by
	// higher-ranked out-of-scope rows. The over-fetch below is dedup/diversity
	// headroom only.
	var pred func(historyRecord) bool
	if filtered {
		pred = func(r historyRecord) bool { return conversationRecordMatchesFilters(r, opts) }
	}
	candidateLimit := limit * 4
	if candidateLimit < limit { // overflow guard
		candidateLimit = limit
	}
	scanComplete := true
	var scored []scoredHistoryRecord
	switch mode {
	case modeVector:
		var scores map[string]float64
		if filtered {
			scores = conversationSemanticScoresExhaustive(brainDir, historySemanticEmbedder(defaultEmbedder()), query, false)
		} else {
			scores = conversationSemanticScores(brainDir, historySemanticEmbedder(defaultEmbedder()), query, candidateLimit, false)
		}
		if len(scores) == 0 {
			return nil, errConversationVectorUnsupported
		}
		// Semantic-only ranks long-term vectors; short-term records have no
		// vectors until consolidation and are deliberately absent here.
		semIndex := fresh.longTermActive()
		if pred != nil {
			semIndex = historyIndex{GeneratedAt: semIndex.GeneratedAt, Records: filterHistoryRecords(semIndex.Records, pred)}
		}
		scored = rankConversationSemantic(semIndex, scores, candidateLimit)
	case modeHybrid:
		// Conversation fusion is OFF by default pending a validated positive:
		// the 2026-08-07 calibration on the entire-brain corpus (22-task exact
		// pack + 10-task paraphrase stratum, EmbeddingGemma) measured fusion
		// trading exact-match precision (R@1 0.864→0.773, one R@5 loss, one
		// paraphrase dropped from rank 1 to unranked) for +1 paraphrase hit;
		// the same displacement failure mode that closed Model2Vec history
		// fusion. Same discipline as historyFusionEligible: the fused arm
		// ships dark behind a development flag until an eval-ledger row
		// validates it (see docs/eval_ledger.md).
		scored = rankFreshHistory(fresh, conversationKind, query, candidateLimit, pred, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
			if envBool("ENTIRE_BRAIN_CONVERSATION_FUSION") {
				fused, complete, ok := rankConversationFused(brainDir, longTerm, query, candidateLimit, defaultEmbedder(), pred)
				if !complete {
					scanComplete = false
				}
				return fused, ok
			}
			lex, complete, ok := rankHistoryViaFTSFiltered(brainDir, longTerm, conversationKind, query, candidateLimit, historyFTSRelevanceCutoff, pred)
			if !complete {
				scanComplete = false
			}
			return lex, ok
		})
	default:
		scored = rankFreshHistory(fresh, conversationKind, query, candidateLimit, pred, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
			lex, complete, ok := rankHistoryViaFTSFiltered(brainDir, longTerm, conversationKind, query, candidateLimit, historyFTSRelevanceCutoff, pred)
			if !complete {
				scanComplete = false
			}
			return lex, ok
		})
	}
	if !scanComplete {
		return nil, errConversationFilterScanExceeded()
	}
	// Defense in depth: every arm already generated in-scope candidates; this
	// re-check keeps the contract obvious and cheap.
	kept := make([]scoredHistoryRecord, 0, len(scored))
	for _, s := range scored {
		if conversationRecordMatchesFilters(s.Record, opts) {
			kept = append(kept, s)
		}
	}
	kept = capConversationSessionShare(kept, limit, opts)
	if len(kept) > limit {
		kept = kept[:limit]
	}
	out := make([]unifiedResult, len(kept))
	for i, s := range kept {
		out[i] = conversationToUnified(s.Record)
		// Explainability: which query tokens actually hit this record, so a
		// thin result is debuggable ("only 'bug' matched") instead of opaque.
		out[i].MatchedTerms = historyRecordMatchedTerms(s.Record, query)
		// Preserve list order for callers that read Score; RRF-scale for
		// consistency with single-list merges.
		out[i].Score = 1.0 / (rrfK + float64(i+1))
	}
	return out, nil
}

// errConversationFilterScanExceeded is the structured degraded state for a
// filtered conversation scan that hit its candidate ceiling before filling the
// requested limit: completeness cannot be proven, so the caller gets an
// explicit error rather than a silently partial (possibly false-empty) result.
func errConversationFilterScanExceeded() error {
	return fmt.Errorf("conversation filter scan exceeded %d candidates before the requested limit was met; results would be incomplete, narrow the query or the filters", historyFTSFilteredScanCeiling)
}

// conversationRecordMatchesFilters applies the structured conversation filters.
// Scope safety over recall: a record that cannot prove it is inside a time
// filter (no session time) is excluded when one is set.
func conversationRecordMatchesFilters(record historyRecord, opts retrievalOptions) bool {
	if session := strings.TrimSpace(opts.SessionID); session != "" && record.SessionID != session {
		return false
	}
	if agent := strings.TrimSpace(opts.Agent); agent != "" && !strings.EqualFold(record.Agent, agent) {
		return false
	}
	if branch := strings.TrimSpace(opts.Branch); branch != "" && record.Branch != branch {
		return false
	}
	if !opts.After.IsZero() || !opts.Before.IsZero() {
		created, err := time.Parse(time.RFC3339, record.CreatedAt)
		if err != nil {
			return false
		}
		if !opts.After.IsZero() && created.Before(opts.After) {
			return false
		}
		if !opts.Before.IsZero() && !created.Before(opts.Before) {
			return false
		}
	}
	return true
}

// capConversationSessionShare enforces the per-session diversity cap: at most
// max(1, limit/conversationSessionTopKDivisor) results per session while other
// sessions still have candidates. If the cap leaves the list short and only
// capped sessions have candidates left, they backfill in rank order; the cap
// prevents crowding out, it does not hide the only matching session. An
// explicit session filter disables the cap entirely.
func capConversationSessionShare(scored []scoredHistoryRecord, limit int, opts retrievalOptions) []scoredHistoryRecord {
	if strings.TrimSpace(opts.SessionID) != "" || len(scored) <= 1 {
		return scored
	}
	perSession := limit / conversationSessionTopKDivisor
	if perSession < 1 {
		perSession = 1
	}
	counts := map[string]int{}
	kept := make([]scoredHistoryRecord, 0, min(limit, len(scored)))
	var overflow []scoredHistoryRecord
	for _, s := range scored {
		key := s.Record.SessionID
		if key == "" {
			key = s.Record.Path
		}
		if counts[key] >= perSession {
			overflow = append(overflow, s)
			continue
		}
		counts[key]++
		kept = append(kept, s)
		if len(kept) >= limit {
			return kept
		}
	}
	for _, s := range overflow {
		if len(kept) >= limit {
			break
		}
		kept = append(kept, s)
	}
	return kept
}

// conversationToUnified projects an exchange record for search results: the
// bounded search projection plus provenance and the historical-evidence
// contract. Full bounded content is get's job.
func conversationToUnified(record historyRecord) unifiedResult {
	return unifiedResult{
		Source:               retrievalSourceConversation,
		ID:                   record.ID,
		Path:                 record.Path,
		Heading:              conversationKind,
		Line:                 record.Line,
		EndLine:              record.EndLine,
		Branch:               record.Branch,
		SessionID:            record.SessionID,
		Agent:                record.Agent,
		CreatedAt:            record.CreatedAt,
		Text:                 record.Summary,
		Truncated:            record.ProjectionTruncated,
		VerificationRequired: true,
		Caveats:              []retrievalCaveat{conversationHistoricalEvidenceCaveat()},
	}
}

// conversationGetResult is the bounded ID-based expansion: re-parse the exact
// indexed range from the canonical transcript when its digest still matches;
// otherwise degrade to the stored projection with an explicit source-integrity
// caveat instead of presenting it as faithful full content.
func conversationGetResult(brainDir string, record historyRecord) unifiedResult {
	result := conversationToUnified(record)
	expansion, err := expandConversationExchange(brainDir, record)
	if err != nil {
		result.Caveats = append(result.Caveats, conversationSourceStaleCaveat(record.Path))
		return result
	}
	result.Text = conversationExpansionText(expansion)
	result.Truncated = expansion.Truncated
	return result
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
	var wantFact, wantReview, wantHistory, wantConversation, wantDoc bool
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "fact:"):
			wantFact = true
		case strings.HasPrefix(id, "review:"):
			wantReview = true
		case strings.HasPrefix(id, "history:"):
			wantHistory = true
		case strings.HasPrefix(id, conversationIDPrefix):
			wantConversation = true
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
	convByID := map[string]historyRecord{}
	if wantHistory || wantConversation {
		// A corrupt manifest, or a history index the manifest declares but that is
		// missing/unreadable, is a real storage problem — surface it rather than
		// reporting every history:* id as "not found". A brain with no history source
		// (nil) legitimately has no such records, so leave histByID empty.
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return nil, nil, err
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
			if err != nil {
				return nil, nil, fmt.Errorf("load history index: %w", err)
			}
			// mergedRecords appends short-term records last, so for a duplicate
			// id the fresher short-term copy wins the map insert.
			for _, r := range fresh.mergedRecords() {
				if r.Kind == conversationKind {
					convByID[r.ID] = r
					continue
				}
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
		case strings.HasPrefix(id, conversationIDPrefix):
			// The transcript path is resolved from the indexed record only;
			// a client-supplied id can never choose a filesystem path.
			if r, ok := convByID[id]; ok {
				found = append(found, conversationGetResult(brainDir, r))
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
