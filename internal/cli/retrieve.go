package cli

import (
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"os"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/docstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

// retrieve.go wires the extracted unified retrieval engine (see
// internal/retrieval) to its cli-private seams — guard composition, the store
// rankers, declared-index inspection, the conversation arms — and keeps the
// surfaces that bind session views and the pattern corpus: the conversation
// source arm and the get/multi-get batch resolver.

// retrievalEngineDeps binds the engine's injected seams to their cli homes:
// guard composition (session_privacy.go), the store rankers, declared-index
// inspection, and the conversation arms.
func retrievalEngineDeps() retrieval.Deps {
	return retrieval.Deps{
		LoadSessionReadGuard:             bmanifest.LoadSessionReadGuard,
		GuardFactRecords:                 factstore.GuardRecords,
		GuardBlocksRecord:                historystore.GuardBlocksRecord,
		LoadFactProposals:                loadFactProposals,
		RankFacts:                        rankFacts,
		RankHistoryLexical:               rankHistoryLexicalForRetrieval,
		RankGuardedHistory:               rankGuardedHistoryForRetrieval,
		RankDocs:                         rankDocsForRetrieval,
		InspectDeclaredIndex:             inspectBrainDeclaredIndex,
		RetrieveConversation:             retrieveConversation,
		RetrieveConversationMultiConcept: retrieveConversationMultiConcept,
	}
}

// retrieveUnified ranks across facts + history + docs through the extracted
// engine, binding the cli-private dependencies above. See retrieval.Unified.
func retrieveUnified(repoDir, brainDir, branch, query string, limit int, mode retrievalMode) ([]unifiedResult, error) {
	return retrieval.Unified(retrievalEngineDeps(), repoDir, brainDir, branch, query, limit, mode)
}

func retrieveUnifiedWithOptions(repoDir, brainDir, branch, query string, limit int, mode retrievalMode, opts retrievalOptions) ([]unifiedResult, error) {
	return retrieval.UnifiedWithOptions(retrievalEngineDeps(), repoDir, brainDir, branch, query, limit, mode, opts)
}

// rankHistoryLexicalForRetrieval adapts rankFreshHistoryLexicalFromSource's
// diagnostic-rich signature to the engine seam, which needs only the scored
// window.
func rankHistoryLexicalForRetrieval(brainDir string, source *historySourceManifest, kind, query string, limit int) ([]scoredHistoryRecord, error) {
	scored, _, _, rankErr := rankFreshHistoryLexicalFromSource(brainDir, source, kind, query, limit)
	return scored, rankErr
}

// rankGuardedHistoryForRetrieval is the tombstone-guarded history arm: the
// FTS-payload fast path cannot apply an exclusion predicate during candidate
// generation, so a brain with live tombstones ranks through the full fresh
// view here.
func rankGuardedHistoryForRetrieval(brainDir string, source *historySourceManifest, kind, query string, limit int, guardPred func(historyRecord) bool) ([]scoredHistoryRecord, error) {
	fresh, loadErr := loadFreshHistory(brainDir, source)
	if loadErr != nil {
		return nil, loadErr
	}
	// Semantic arms rank the long-term records minus files the
	// short-term overlay superseded (overlay records have no vectors
	// until consolidation; the lexical tier carries their freshness)
	// and minus copies a newer overlay copy supersedes. The
	// exclusion guard filters both arms; it already carries
	// the manifest-derived transcript paths, resolved before the
	// facts arm ran, so records that lost their session id block by
	// path here too.
	index := fresh.longTermReconciled()
	if guardPred != nil {
		index = historyIndex{GeneratedAt: index.GeneratedAt, Records: historystore.FilterRecords(index.Records, guardPred)}
	}
	scored := rankFreshHistory(fresh, kind, query, limit, guardPred, func(longTerm historyIndex, eligible func(historyRecord) bool) ([]scoredHistoryRecord, bool) {
		lex, complete, ok := rankHistoryViaFTSFiltered(brainDir, longTerm, kind, query, limit, historyFTSRelevanceCutoff, eligible)
		return lex, ok && complete
	})
	return scored, nil
}

// rankDocsForRetrieval ranks doc chunks for the engine.
// FTS is an optimization, never load-bearing: fall back to the in-memory
// lexical scorer so docs don't vanish when the doc FTS index can't open.
func rankDocsForRetrieval(brainDir string, index docIndex, query string, limit int) []scoredDocRecord {
	scored, ok := rankDocsViaFTS(brainDir, index, query, limit)
	if !ok {
		scored = rankDocsLexical(index, query, limit)
	}
	return scored
}

// errConversationVectorUnsupported answers a direct modeVector call on the
// conversation source. No surface produces modeVector anymore (the
// verbs alias to the lexical ranking); this guards internal callers.
var errConversationVectorUnsupported = errors.New(`vector search was removed with semantic retrieval (trail #278); use query --source conversation for lexical recall`)

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
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		if mode == retrieval.ModeVector {
			return nil, errConversationVectorUnsupported
		}
		return nil, nil
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		return nil, fmt.Errorf("load history index: %w", err)
	}
	filtered := opts.HasConversationOnlyFilters() || strings.TrimSpace(opts.Branch) != ""
	// Structured filters are pushed into candidate generation: every
	// arm ranks only in-scope records, so a valid session/agent/time/branch
	// match can never be displaced out of a bounded candidate window by
	// higher-ranked out-of-scope rows. The over-fetch below is dedup/diversity
	// headroom only. The exclusion guard joins the same predicate: a
	// tombstoned session is unreadable here even while its derived cleanup is
	// still running. Superseded duplicate copies join it too: a stale
	// copy must not even consume a candidate slot.
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return nil, err
	}
	winners := fresh.duplicateRecordWinners()
	var pred func(historyRecord) bool
	if filtered || !guard.Empty() || len(winners) > 0 {
		pred = func(r historyRecord) bool {
			if historystore.GuardBlocksRecord(guard, r) {
				return false
			}
			if w, ok := winners[recordReplacementKey(r)]; ok && !sameRecordCopy(w, r) {
				return false
			}
			return !filtered || conversationRecordMatchesFilters(r, opts)
		}
	}
	candidateLimit := limit * 4
	if candidateLimit < limit { // overflow guard
		candidateLimit = limit
	}
	scanComplete := true
	var scored []scoredHistoryRecord
	switch mode {
	case retrieval.ModeVector:
		return nil, errConversationVectorUnsupported
	case retrieval.ModeHybrid:
		scored = rankFreshHistory(fresh, conversationKind, query, candidateLimit, pred, func(longTerm historyIndex, eligible func(historyRecord) bool) ([]scoredHistoryRecord, bool) {
			lex, complete, ok := rankHistoryViaFTSFiltered(brainDir, longTerm, conversationKind, query, candidateLimit, historyFTSRelevanceCutoff, eligible)
			if ok && !complete {
				scanComplete = false
			}
			return lex, ok
		})
	default:
		scored = rankFreshHistory(fresh, conversationKind, query, candidateLimit, pred, func(longTerm historyIndex, eligible func(historyRecord) bool) ([]scoredHistoryRecord, bool) {
			lex, complete, ok := rankHistoryViaFTSFiltered(brainDir, longTerm, conversationKind, query, candidateLimit, historyFTSRelevanceCutoff, eligible)
			if ok && !complete {
				scanComplete = false
			}
			return lex, ok
		})
	}
	// complete is only meaningful when the arm actually ran. On ok == false the
	// FTS/fused arm was UNAVAILABLE (no match expression, open/query failure),
	// and rankFreshHistory falls back to the in-memory scorer, which filters
	// the complete long-term record slice and therefore cannot be partial.
	// Honoring a stale complete=false there turned every FTS outage into a
	// bogus "filter scan exceeded" error on the default (unfiltered) path,
	// contradicting this file's own contract that FTS is an optimization and
	// never load-bearing.
	if !scanComplete {
		return nil, errConversationFilterScanExceeded()
	}
	// Defense in depth: every arm already generated in-scope candidates; this
	// re-check keeps the contract obvious and cheap.
	kept := make([]scoredHistoryRecord, 0, len(scored))
	for _, s := range scored {
		if historystore.GuardBlocksRecord(guard, s.Record) {
			continue
		}
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
		// Session navigation identity: every hit names its virtual
		// session so callers can fetch the outline or adjacent context.
		out[i].SessionRef, _ = recordSessionRef(s.Record, manifest.RepoKey, manifest)
		// Explainability: which query tokens actually hit this record, so a
		// thin result is debuggable ("only 'bug' matched") instead of opaque.
		out[i].MatchedTerms = historyRecordMatchedTerms(s.Record, query)
		// Preserve list order for callers that read Score; RRF-scale for
		// consistency with single-list merges.
		out[i].Score = 1.0 / (retrieval.RRFK + float64(i+1))
	}
	return out, nil
}

// maxGetBatchIDs caps one get/multi-get request across CLI and
// workspace surfaces (the plan's shared result-limit ceiling).
const maxGetBatchIDs = 50

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
		Source:               brainstore.RetrievalSourceConversation,
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
// otherwise degrade to the stored projection with a caveat naming WHICH
// contract failed. Too-large, stale, and unreadable sources are distinct
// states, never one collapsed "stale" answer.
func conversationGetResult(brainDir string, record historyRecord) unifiedResult {
	result := conversationToUnified(record)
	expansion, err := expandConversationExchange(brainDir, record)
	switch {
	case err == nil:
		result.Text = conversationExpansionText(expansion)
		result.Truncated = expansion.Truncated
	case errors.Is(err, errConversationSourceTooLarge):
		result.Caveats = append(result.Caveats, conversationSourceTooLargeCaveat(record.Path))
	case errors.Is(err, errConversationSourceStale):
		result.Caveats = append(result.Caveats, conversationSourceStaleCaveat(record.Path))
	default:
		result.Caveats = append(result.Caveats, conversationSourceUnreadableCaveat(record.Path))
	}
	return result
}

// getUnifiedBatch resolves prefixed ids (fact:/review:/history:/doc:/pattern:/theme:) to full records,
// loading each corpus at most once and indexing it by id. get/multi-get
// route through here so resolving N ids is O(corpus
// + N), not O(N × corpus) — the latter rescans the full history per id and is
// pathological on large brains. Results preserve input order.
func getUnifiedBatch(repoDir, brainDir, branch string, ids []string) (found []unifiedResult, missing []string, err error) {
	return getUnifiedBatchOptions(repoDir, brainDir, branch, ids, getOptions{})
}

func getUnifiedBatchOptions(repoDir, brainDir, branch string, ids []string, gopts getOptions) (found []unifiedResult, missing []string, err error) {
	// One request fans out to at most this many record resolutions; each
	// conversation id triggers a bounded transcript read, so the batch size
	// itself must be bounded too.
	if len(ids) > maxGetBatchIDs {
		return nil, nil, fmt.Errorf("at most %d ids per get/multi-get request (got %d)", maxGetBatchIDs, len(ids))
	}
	if err := validateGetOptions(gopts); err != nil {
		return nil, nil, err
	}
	var wantFact, wantReview, wantHistory, wantConversation, wantSession, wantDoc, wantPatternDerived bool
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "fact:"):
			wantFact = true
		case strings.HasPrefix(id, "review:"):
			wantReview = true
		case strings.HasPrefix(id, "history:"):
			wantHistory = true
		case strings.HasPrefix(id, brainstore.ConversationSessionIDPrefix):
			wantSession = true
		case strings.HasPrefix(id, conversationIDPrefix):
			wantConversation = true
		case strings.HasPrefix(id, "doc:"):
			wantDoc = true
		case strings.HasPrefix(id, "pattern:"), strings.HasPrefix(id, "theme:"):
			wantPatternDerived = true
		}
	}
	if wantPatternDerived {
		if err := requirePrivacyDerivedRead(brainDir); err != nil {
			return nil, nil, err
		}
	}
	// Navigation arguments are type-specific: context counts belong to a
	// single conversation: target, outline cursor/limit to a single
	// conversation-session: target. Anything else is a structured
	// invalid-argument error, never an ignored option.
	if gopts.Navigation() && len(ids) != 1 {
		return nil, nil, fmt.Errorf("navigation options require exactly one target id (got %d)", len(ids))
	}
	if gopts.ContextSet && !wantConversation {
		return nil, nil, fmt.Errorf("context-before/context-after are valid only for a conversation: target")
	}
	if gopts.OutlineSet && !wantSession {
		return nil, nil, fmt.Errorf("after-turn/limit are valid only for a conversation-session: target")
	}
	if wantSession && len(ids) != 1 {
		return nil, nil, fmt.Errorf("a conversation-session: outline requires its own single-id request")
	}
	// Exclusion guard: a tombstoned session's records resolve as "not
	// found" on every get surface immediately, even before cleanup finishes.
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, nil)
	if err != nil {
		return nil, nil, err
	}
	if !guard.Empty() {
		// Resolve the manifest-derived transcript paths BEFORE the facts arm
		// filters below, so a fact whose only anchor lost its session id blocks
		// by path here too. The history arm's later re-resolution ran after
		// factstore.GuardRecords and therefore never protected `get fact:`.
		manifest, err := bmanifest.Load(brainDir)
		if err != nil {
			return nil, nil, err
		}
		guard, err = bmanifest.LoadSessionReadGuard(brainDir, manifest)
		if err != nil {
			return nil, nil, err
		}
	}
	factByID := map[string]factRecord{}
	var globalGetIDs map[string]bool
	var reviewByID map[string]factReviewGroup
	var reviewByFactID map[string]factReviewGroup
	var proposalStateUnavailable bool
	if wantFact || wantReview {
		// Surface a corrupt facts store as an error, not a misleading "not found".
		facts, ferr := factstore.Load(brainDir, branch)
		if ferr != nil {
			return nil, nil, ferr
		}
		if len(gopts.GlobalFacts) > 0 {
			// query and search return global facts, so their ids have to
			// resolve here too. The repository's copy still wins on a collision.
			facts, globalGetIDs = retrieval.MergeGlobalFacts(facts, gopts.GlobalFacts)
		}
		facts = factstore.GuardRecords(guard, facts)
		for _, f := range facts {
			factByID[f.ID] = f
		}
		proposals, perr := loadFactProposals(brainDir, branch)
		if perr != nil && wantReview {
			return nil, nil, perr
		}
		if perr == nil {
			localFacts := retrieval.FactsEligibleForLocusDrift(facts, globalGetIDs)
			reviewByID, reviewByFactID = retrieval.IndexFactReviewGroups(retrieval.BuildFactReviewGroups(localFacts, proposals))
		} else {
			proposalStateUnavailable = true
		}
	}
	histByID := map[string]historyRecord{}
	convByID := map[string]historyRecord{}
	var sessionViews map[string]conversationSessionView
	var abstractResolver *sessionAbstractResolver
	// convScopes maps a conversation id to its session scopes (ref -> newest
	// record in that scope), so a legacy id colliding across scopes is a
	// structured ambiguity, never a silent last-write-wins pick.
	convScopes := map[string]map[string]historyRecord{}
	if wantHistory || wantConversation || wantSession {
		// A corrupt manifest, or a history index the manifest declares but that is
		// missing/unreadable, is a real storage problem — surface it rather than
		// reporting every history:* id as "not found". A brain with no history source
		// (nil) legitimately has no such records, so leave histByID empty.
		manifest, err := bmanifest.Load(brainDir)
		if err != nil {
			return nil, nil, err
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
			if err != nil {
				return nil, nil, fmt.Errorf("load history index: %w", err)
			}
			// The guard already carries the manifest-derived transcript paths
			// (resolved above, before the facts arm), so records that lost
			// their session id block by path on every surface here.
			if wantConversation || wantSession {
				sessionViews = buildConversationSessionViews(fresh, manifest.RepoKey, manifest, guard)
				for ref, view := range sessionViews {
					for _, r := range view.Records {
						scopes := convScopes[r.ID]
						if scopes == nil {
							scopes = map[string]historyRecord{}
							convScopes[r.ID] = scopes
						}
						if prev, ok := scopes[ref]; ok {
							scopes[ref] = newestHistoryRecord(prev, r)
						} else {
							scopes[ref] = r
						}
					}
				}
			}
			// reconciledRecords collapses duplicate stable IDs to the same
			// newest-copy winner ranking uses, so get expands exactly the
			// record search returned.
			for _, r := range fresh.reconciledRecords() {
				if historystore.GuardBlocksRecord(guard, r) {
					continue
				}
				if r.Kind == conversationKind {
					convByID[r.ID] = r
					continue
				}
				histByID[r.ID] = r
			}
		}
	}
	if wantSession {
		abstractResolver = newSessionAbstractResolver(brainDir)
	}
	docByID := map[string]docRecord{}
	if wantDoc {
		// A never-built doc index is not an error (doc:* ids just report "not
		// found"); a corrupt/unreadable one is — surface it.
		index, err := docstore.Load(brainDir)
		switch {
		case err == nil:
			for _, r := range index.Records {
				docByID[r.ID] = r
			}
		case os.IsNotExist(err):
			if defect := inspectBrainDeclaredIndex(brainDir, brainstore.DeclaredIndexDocs); !defect.OK() {
				return nil, nil, defect
			}
		case !os.IsNotExist(err):
			return nil, nil, fmt.Errorf("load doc index: %w", err)
		}
	}
	for _, id := range ids {
		switch {
		case strings.HasPrefix(id, "fact:"):
			if f, ok := factByID[id]; ok {
				r := retrieval.FactsToUnified([]factRecord{f})[0]
				if group, pending := reviewByFactID[id]; pending {
					r = retrieval.AnnotateExplicitFactReview(r, group)
				}
				if globalGetIDs[f.ID] {
					// Not scoped to this repository, so locus drift cannot
					// answer for it; labelled the way the ranked surfaces are.
					r.Heading += " (global)"
				} else {
					r = retrieval.AnnotateFactLocusTrust(repoDir, f, r)
				}
				if proposalStateUnavailable {
					r = retrieval.AnnotateProposalStateUnavailable([]unifiedResult{r})[0]
				}
				found = append(found, r)
				continue
			}
		case strings.HasPrefix(id, "review:"):
			if group, ok := reviewByID[id]; ok {
				r := retrieval.FactReviewToUnified(repoDir, group)
				// Preserve an addressable proposal alias if the group grew, including
				// the machine-readable caveat that tells clients what to review.
				r.ID = id
				for i := range r.Caveats {
					if r.Caveats[i].Kind == retrieval.CaveatUnresolvedReview {
						r.Caveats[i].ReviewID = id
					}
				}
				found = append(found, r)
				continue
			}
		case strings.HasPrefix(id, "history:"):
			if r, ok := histByID[id]; ok {
				found = append(found, retrieval.HistoryToUnified([]scoredHistoryRecord{{Record: r}})[0])
				continue
			}
		case strings.HasPrefix(id, brainstore.ConversationSessionIDPrefix):
			if view, ok := sessionViews[id]; ok {
				outline := conversationSessionOutline(view, gopts.AfterTurn, gopts.OutlineLimit)
				outline.AbstractStatus, outline.Abstract = abstractResolver.status(view)
				outline = boundSessionAbstractEnvelope(outline)
				if outline.AbstractStatus == abstractStatusStale || outline.AbstractStatus == abstractStatusMissing {
					if err := enqueueAutomaticAbstractReconciliationHook(brainDir, "session_get", time.Now().UTC()); err != nil {
						// Automatic generation is an optional acceleration. Exact
						// session recall remains valid even when its durable work
						// enqueue is temporarily unavailable; expose the issue instead
						// of converting a successful get into a failure.
						outline.AbstractIssue = memoryOperationalError(err)
					}
				}
				found = append(found, outline)
				continue
			}
		case strings.HasPrefix(id, conversationIDPrefix):
			// The transcript path is resolved from the indexed record only;
			// a client-supplied id can never choose a filesystem path.
			scopes := convScopes[id]
			if len(scopes) > 1 {
				// The existing branch selector may disambiguate a legacy id
				// that collides across session scopes; a silent winner never
				// may.
				matching := map[string]historyRecord{}
				for ref, record := range scopes {
					if record.Branch == branch {
						matching[ref] = record
					}
				}
				if len(matching) != 1 {
					return nil, nil, fmt.Errorf("memory_identity_ambiguous: %s resolves to %d session scopes; pass --branch to select one", id, len(scopes))
				}
				scopes = matching
			}
			if len(scopes) == 1 {
				for ref, record := range scopes {
					if gopts.ContextSet {
						found = append(found, conversationContextPacket(brainDir, sessionViews[ref], record, gopts.ContextBefore, gopts.ContextAfter))
					} else {
						result := conversationGetResult(brainDir, record)
						result.SessionRef = ref
						found = append(found, result)
					}
				}
				continue
			}
			if r, ok := convByID[id]; ok {
				found = append(found, conversationGetResult(brainDir, r))
				continue
			}
		case strings.HasPrefix(id, "doc:"):
			if r, ok := docByID[strings.TrimPrefix(id, "doc:")]; ok {
				found = append(found, retrieval.DocToUnified(r))
				continue
			}
		case strings.HasPrefix(id, "pattern:"):
			// Consolidation dossier addressed by its pattern id (v2). Corpus is
			// rebuildable/optional, so a missing corpus is "not found", not an error.
			if r, ok, err := getCorpusConsolidationChecked(brainDir, id); err != nil {
				return nil, nil, err
			} else if ok {
				found = append(found, r)
				continue
			}
		case strings.HasPrefix(id, "theme:"):
			if r, ok, err := getCorpusThemeChecked(brainDir, id); err != nil {
				return nil, nil, err
			} else if ok {
				found = append(found, r)
				continue
			}
		}
		missing = append(missing, id)
	}
	return found, missing, nil
}
