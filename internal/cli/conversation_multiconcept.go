package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// conversation_multiconcept.go is C2 of the conversational-memory plan:
// session-scoped AND coverage over two to five concepts. A session matches
// only when EVERY concept has at least one matching exchange; the result is
// the session's virtual identity plus the exact supporting exchanges, so the
// caller keeps the deterministic query-then-get workflow.

const (
	// conversationConceptsMaxTotal bounds primary query plus extra concepts.
	conversationConceptsMaxTotal = 5
	conversationConceptMaxBytes  = 512
	// conversationConceptScanCeiling is the per-concept complete-enumeration
	// bound for lexical mode; past it the query is memory_query_too_broad
	// rather than silently partial AND semantics.
	conversationConceptScanCeiling = 10000
	// conversationConceptResponseMaxBytes caps the complete multi-concept
	// response; whole tail results drop, never a cut record.
	conversationConceptResponseMaxBytes = 128 * 1024
)

// conceptMatch names the best supporting exchange for one concept inside a
// matching session.
type conceptMatch struct {
	Concept        string   `json:"concept"`
	ConversationID string   `json:"conversation_id"`
	Rank           int      `json:"rank"` // 1-based rank in the concept's own list
	MatchedTerms   []string `json:"matched_terms,omitempty"`
	Arm            string   `json:"arm"` // lexical | semantic | fused
}

// normalizeConversationConcepts merges the primary query with the extra
// concepts: whitespace-normalized, case-insensitively deduplicated, bounded,
// two to five total. Every violation is a structured input error.
func normalizeConversationConcepts(query string, extras []string) ([]string, error) {
	all := append([]string{query}, extras...)
	seen := map[string]bool{}
	out := make([]string, 0, len(all))
	for _, concept := range all {
		concept = strings.Join(strings.Fields(concept), " ")
		if concept == "" {
			return nil, fmt.Errorf("concepts must be non-empty")
		}
		if len(concept) > conversationConceptMaxBytes {
			return nil, fmt.Errorf("each concept is capped at %d bytes after normalization", conversationConceptMaxBytes)
		}
		key := strings.ToLower(concept)
		if seen[key] {
			return nil, fmt.Errorf("duplicate concept %q", concept)
		}
		seen[key] = true
		out = append(out, concept)
	}
	if len(out) < 2 || len(out) > conversationConceptsMaxTotal {
		return nil, fmt.Errorf("multi-concept search takes two to %d concepts including the query (got %d)", conversationConceptsMaxTotal, len(out))
	}
	return out, nil
}

// conceptRankList is one concept's complete (or explicitly approximate)
// in-scope rank list.
type conceptRankList struct {
	concept     string
	arm         string
	ranked      []scoredHistoryRecord
	approximate bool
}

// retrieveConversationMultiConcept executes the C2 contract. Filters and the
// exclusion guard reach candidate generation exactly like single-concept
// retrieval (R0-3/R0-1); lexical mode enumerates each concept's complete
// in-scope match set up to the safety ceiling and fails structured
// (memory_query_too_broad) beyond it; vector and hybrid modes are explicitly
// approximate but never violate filters.
func retrieveConversationMultiConcept(brainDir, query string, limit int, mode retrievalMode, opts retrievalOptions) ([]unifiedResult, error) {
	concepts, err := normalizeConversationConcepts(query, opts.Concepts)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
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
	guard := loadSessionReadGuard(brainDir, manifest)
	winners := fresh.duplicateIDWinners()
	filtered := opts.hasConversationOnlyFilters() || strings.TrimSpace(opts.Branch) != ""
	// The predicate is always non-nil here: complete enumeration rides the
	// same exhaustive-scan machinery as filtered single-concept search.
	pred := func(r historyRecord) bool {
		if guard.blocksRecord(r) {
			return false
		}
		if w, ok := winners[r.ID]; ok && !sameRecordCopy(w, r) {
			return false
		}
		if !filtered {
			return true
		}
		return conversationRecordMatchesFilters(r, opts)
	}

	semanticAvailable := func() (map[string]float64, bool) {
		scores := conversationSemanticScoresExhaustive(brainDir, historySemanticEmbedder(defaultEmbedder()), concepts[0], mode == modeHybrid)
		return scores, len(scores) > 0
	}

	rankOne := func(concept string) (conceptRankList, error) {
		list := conceptRankList{concept: concept}
		switch mode {
		case modeVector:
			scores := conversationSemanticScoresExhaustive(brainDir, historySemanticEmbedder(defaultEmbedder()), concept, false)
			if len(scores) == 0 {
				return list, errConversationVectorUnsupported
			}
			semIndex := fresh.longTermReconciled()
			semIndex = historyIndex{GeneratedAt: semIndex.GeneratedAt, Records: filterHistoryRecords(semIndex.Records, pred)}
			list.ranked = rankConversationSemantic(semIndex, scores, conversationConceptScanCeiling)
			list.arm = "semantic"
			list.approximate = true
			return list, nil
		case modeHybrid:
			if envBool("ENTIRE_BRAIN_CONVERSATION_FUSION") {
				if _, ok := semanticAvailable(); ok {
					fused, complete, ok := rankConversationFused(brainDir, fresh.index, concept, conversationConceptScanCeiling/4, defaultEmbedder(), pred)
					if ok {
						if !complete {
							return list, errConversationConceptTooBroad(concept)
						}
						list.ranked = filterScoredByPathReplacement(fused, fresh)
						list.arm = "fused"
						list.approximate = true
						return list, nil
					}
				}
			}
			fallthrough
		default:
			complete := true
			list.ranked = rankFreshHistory(fresh, conversationKind, concept, conversationConceptScanCeiling, pred, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
				lex, lexComplete, ok := rankHistoryViaFTSFiltered(brainDir, longTerm, conversationKind, concept, conversationConceptScanCeiling, 0, pred)
				if !lexComplete {
					complete = false
				}
				return lex, ok
			})
			if !complete {
				return list, errConversationConceptTooBroad(concept)
			}
			list.arm = "lexical"
			return list, nil
		}
	}

	lists := make([]conceptRankList, 0, len(concepts))
	for _, concept := range concepts {
		list, err := rankOne(concept)
		if err != nil {
			return nil, err
		}
		lists = append(lists, list)
	}

	// Session-scoped AND: every concept needs at least one exchange in the
	// session; one exchange may satisfy several concepts.
	type sessionCoverage struct {
		ref      string
		matches  []conceptMatch
		worst    int
		sum      int
		approx   bool
		evidence []string
	}
	perConcept := make([]map[string]conceptMatch, len(lists))
	for i, list := range lists {
		best := map[string]conceptMatch{}
		for rank, scored := range list.ranked {
			ref, _ := recordSessionRef(scored.Record, manifest.RepoKey, manifest)
			if _, ok := best[ref]; ok {
				continue // ranked order: the first hit per session is its best
			}
			best[ref] = conceptMatch{
				Concept:        list.concept,
				ConversationID: scored.Record.ID,
				Rank:           rank + 1,
				MatchedTerms:   historyRecordMatchedTerms(scored.Record, list.concept),
				Arm:            list.arm,
			}
		}
		perConcept[i] = best
	}
	var sessions []sessionCoverage
	for ref, first := range perConcept[0] {
		coverage := sessionCoverage{ref: ref, matches: []conceptMatch{first}}
		complete := true
		for _, best := range perConcept[1:] {
			match, ok := best[ref]
			if !ok {
				complete = false
				break
			}
			coverage.matches = append(coverage.matches, match)
		}
		if !complete {
			continue
		}
		seenEvidence := map[string]bool{}
		for _, match := range coverage.matches {
			if match.Rank > coverage.worst {
				coverage.worst = match.Rank
			}
			coverage.sum += match.Rank
			if !seenEvidence[match.ConversationID] {
				seenEvidence[match.ConversationID] = true
				coverage.evidence = append(coverage.evidence, match.ConversationID)
			}
		}
		for _, list := range lists {
			if list.approximate {
				coverage.approx = true
			}
		}
		sessions = append(sessions, coverage)
	}
	// Deterministic order: lowest worst per-concept rank, then lowest rank
	// sum, then session reference. Raw lexical/vector scores are never mixed.
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].worst != sessions[j].worst {
			return sessions[i].worst < sessions[j].worst
		}
		if sessions[i].sum != sessions[j].sum {
			return sessions[i].sum < sessions[j].sum
		}
		return sessions[i].ref < sessions[j].ref
	})
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}

	out := make([]unifiedResult, 0, len(sessions))
	totalBytes := 0
	truncated := false
	for _, session := range sessions {
		result := unifiedResult{
			Source:               retrievalSourceConversation,
			ID:                   session.ref,
			Heading:              "session_coverage",
			SessionRef:           session.ref,
			Text:                 fmt.Sprintf("session covers all %d concepts (worst rank %d)", len(concepts), session.worst),
			VerificationRequired: true,
			Caveats:              []retrievalCaveat{conversationHistoricalEvidenceCaveat()},
			Concepts:             concepts,
			ConceptMatches:       session.matches,
			EvidenceIDs:          session.evidence,
			WorstRank:            session.worst,
			RankSum:              session.sum,
			Approximate:          session.approx,
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		if totalBytes+len(encoded) > conversationConceptResponseMaxBytes {
			truncated = true
			break
		}
		totalBytes += len(encoded)
		out = append(out, result)
	}
	if truncated && len(out) > 0 {
		out[len(out)-1].ResponseTruncated = true
	}
	return out, nil
}

func errConversationConceptTooBroad(concept string) error {
	return fmt.Errorf("memory_query_too_broad: concept %q matched more than %d in-scope exchanges; narrow the concept or the filters", concept, conversationConceptScanCeiling)
}

// filterScoredByPathReplacement drops fused hits whose file the short-term
// overlay superseded, mirroring rankFreshHistory's replaced-path rule for the
// fused arm that ranked the on-disk long-term index directly.
func filterScoredByPathReplacement(scored []scoredHistoryRecord, fresh freshHistory) []scoredHistoryRecord {
	if len(fresh.replaced) == 0 {
		return scored
	}
	kept := scored[:0:0]
	for _, s := range scored {
		if fresh.replaced[s.Record.Path] {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}
