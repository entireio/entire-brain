package cli

import (
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

// conversation_multiconcept.go implements multi-concept session recall:
// session-scoped AND coverage over two to five concepts. A session matches
// only when EVERY concept has at least one matching exchange; the result is
// the session's virtual identity plus the exact supporting exchanges, so the
// caller keeps the deterministic query-then-get workflow.

// retrieval.ConversationConceptsMaxTotal and retrieval.ConversationConceptResultMax moved to
// retrieval (payload.go) with the extracted command surface.
const (
	conversationConceptMaxBytes = 512
	// conversationConceptResponseMaxBytes caps the complete multi-concept
	// response; whole tail results drop, never a cut record.
	conversationConceptResponseMaxBytes = 128 * 1024
)

// conversationConceptScanCeiling is a var so focused tests can prove the
// overflow contract without building ten thousand-record fixtures.
var conversationConceptScanCeiling = 10000

// normalizeConversationConcepts merges the primary query with the extra
// concepts: whitespace-normalized, case-insensitively deduplicated, bounded,
// two to five total. Every violation is a structured input error.
func normalizeConversationConcepts(query string, extras []string) ([]string, error) {
	if len(extras) > retrieval.ConversationConceptsMaxTotal-1 {
		return nil, fmt.Errorf("multi-concept search takes at most %d additional concepts (got %d)", retrieval.ConversationConceptsMaxTotal-1, len(extras))
	}
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
	if len(out) < 2 || len(out) > retrieval.ConversationConceptsMaxTotal {
		return nil, fmt.Errorf("multi-concept search takes two to %d concepts including the query (got %d)", retrieval.ConversationConceptsMaxTotal, len(out))
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

// retrieveConversationMultiConcept executes the multi-concept contract. Filters and the
// exclusion guard reach candidate generation exactly like single-concept
// retrieval; lexical mode enumerates each concept's complete
// in-scope match set up to the safety ceiling and fails structured
// (memory_query_too_broad) beyond it; vector and hybrid modes are explicitly
// approximate but never violate filters.
func retrieveConversationMultiConcept(brainDir, query string, limit int, mode retrievalMode, opts retrievalOptions) ([]unifiedResult, error) {
	if err := retrieval.ValidateRetrievalLimit(limit); err != nil {
		return nil, err
	}
	concepts, err := normalizeConversationConcepts(query, opts.Concepts)
	if err != nil {
		return nil, err
	}
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
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return nil, err
	}
	winners := fresh.duplicateRecordWinners()
	filtered := opts.HasConversationOnlyFilters() || strings.TrimSpace(opts.Branch) != ""
	// The predicate is always non-nil here: complete enumeration rides the
	// same exhaustive-scan machinery as filtered single-concept search.
	pred := func(r historyRecord) bool {
		if historystore.GuardBlocksRecord(guard, r) {
			return false
		}
		if w, ok := winners[recordReplacementKey(r)]; ok && !sameRecordCopy(w, r) {
			return false
		}
		if !filtered {
			return true
		}
		return conversationRecordMatchesFilters(r, opts)
	}

	rankOne := func(concept string) (conceptRankList, error) {
		list := conceptRankList{concept: concept}
		switch mode {
		case retrieval.ModeVector:
			return list, errConversationVectorUnsupported
		case retrieval.ModeHybrid:
			fallthrough
		default:
			var exhaustiveState historyExhaustiveRankState
			list.ranked, exhaustiveState = rankFreshHistoryExhaustive(fresh, conversationKind, concept, conversationConceptScanCeiling, pred, func(longTerm historyIndex, longTermPred func(historyRecord) bool) ([]scoredHistoryRecord, historyExhaustiveRankState, bool, int) {
				return rankHistoryViaFTSExhaustiveFiltered(brainDir, longTerm, conversationKind, concept, conversationConceptScanCeiling, longTermPred)
			})
			if exhaustiveState == historyExhaustiveRankCandidateOverflow {
				return list, errConversationConceptTooBroad(concept)
			}
			if exhaustiveState == historyExhaustiveRankRawScanOverflow {
				return list, errConversationConceptRawScanTooBroad(concept)
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
	for _, session := range sessions {
		result := unifiedResult{
			Source:               brainstore.RetrievalSourceConversation,
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
		out = append(out, result)
	}
	return out, nil
}

func errConversationConceptTooBroad(concept string) error {
	return fmt.Errorf("memory_query_too_broad: concept %q matched more than %d in-scope exchanges; narrow the concept or the filters", concept, conversationConceptScanCeiling)
}

func errConversationConceptRawScanTooBroad(concept string) error {
	return fmt.Errorf("memory_query_too_broad: concept %q requires scanning more than %d lexical candidates; narrow the concept or the filters", concept, historyFTSExhaustiveRawScanCeiling)
}
