package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const (
	retrievalCaveatStaleLocus               = "stale_locus"
	retrievalCaveatUnresolvedReview         = "unresolved_fact_proposal"
	retrievalCaveatProposalStateUnavailable = "proposal_state_unavailable"
	retrievalCaveatCurrentCodeUnavailable   = "current_code_unavailable"
	retrievalCaveatHistoricalDocument       = "historical_document"
	retrievalCaveatHistoricalConversation   = "historical_conversation"
	retrievalCaveatConversationSourceStale  = "conversation_source_stale"
	// Distinct expansion failure states (R0-7): a too-large or unreadable
	// source is not the same contract failure as a changed digest.
	retrievalCaveatConversationSourceTooLarge   = "conversation_source_too_large"
	retrievalCaveatConversationSourceUnreadable = "conversation_source_unreadable"
)

// retrievalCaveat is a machine-readable reason an agent must verify a memory
// result against current evidence before relying on it.
type retrievalCaveat struct {
	Kind       string   `json:"kind"`
	Message    string   `json:"message"`
	Paths      []string `json:"paths,omitempty"`
	ReviewID   string   `json:"review_id,omitempty"`
	Action     string   `json:"action,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
}

type factReviewGroup struct {
	ID        string
	Facts     []factRecord
	Proposals []factProposal
}

// buildFactReviewGroups turns the pending proposal graph into stable connected
// components. Only valid proposals between two active facts affect retrieval;
// stale queue entries remain available to the review command but cannot hide or
// relabel an otherwise independent fact.
func buildFactReviewGroups(facts []factRecord, proposals []factProposal) []factReviewGroup {
	active := make(map[string]factRecord, len(facts))
	for _, fact := range facts {
		if fact.Status == factStatusActive {
			active[fact.ID] = fact
		}
	}

	parent := make(map[string]string)
	var findRoot func(string) string
	findRoot = func(id string) string {
		p, ok := parent[id]
		if !ok {
			parent[id] = id
			return id
		}
		if p != id {
			parent[id] = findRoot(p)
		}
		return parent[id]
	}
	union := func(a, b string) {
		ra, rb := findRoot(a), findRoot(b)
		if ra == rb {
			return
		}
		if ra < rb {
			parent[rb] = ra
		} else {
			parent[ra] = rb
		}
	}

	validByIdentity := make(map[string]factProposal, len(proposals))
	for _, proposal := range proposals {
		if proposal.Action != factActionMerge && proposal.Action != factActionSupersede {
			continue
		}
		if proposal.CandidateID == proposal.TargetID {
			continue
		}
		if _, ok := active[proposal.CandidateID]; !ok {
			continue
		}
		if _, ok := active[proposal.TargetID]; !ok {
			continue
		}
		key := factProposalIdentityKey(proposal)
		if prior, ok := validByIdentity[key]; !ok || proposal.Confidence > prior.Confidence {
			validByIdentity[key] = proposal
		}
		union(proposal.CandidateID, proposal.TargetID)
	}
	valid := make([]factProposal, 0, len(validByIdentity))
	for _, proposal := range validByIdentity {
		valid = append(valid, proposal)
	}
	if len(valid) == 0 {
		return nil
	}

	idsByRoot := make(map[string][]string)
	for id := range parent {
		root := findRoot(id)
		idsByRoot[root] = append(idsByRoot[root], id)
	}
	proposalsByRoot := make(map[string][]factProposal)
	for _, proposal := range valid {
		root := findRoot(proposal.CandidateID)
		proposalsByRoot[root] = append(proposalsByRoot[root], proposal)
	}

	groups := make([]factReviewGroup, 0, len(idsByRoot))
	for root, ids := range idsByRoot {
		sort.Strings(ids)
		groupProposals := proposalsByRoot[root]
		sort.Slice(groupProposals, func(i, j int) bool {
			return factProposalIdentityKey(groupProposals[i]) < factProposalIdentityKey(groupProposals[j])
		})
		group := factReviewGroup{ID: factReviewID(groupProposals[0]), Proposals: groupProposals}
		for _, id := range ids {
			group.Facts = append(group.Facts, active[id])
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups
}

func factProposalIdentityKey(proposal factProposal) string {
	return proposal.Action + "\x00" + proposal.CandidateID + "\x00" + proposal.TargetID
}

func factReviewID(proposal factProposal) string {
	sum := sha256.Sum256([]byte(factProposalIdentityKey(proposal)))
	return "review:" + hex.EncodeToString(sum[:8])
}

func indexFactReviewGroups(groups []factReviewGroup) (map[string]factReviewGroup, map[string]factReviewGroup) {
	byID := make(map[string]factReviewGroup, len(groups))
	byFactID := make(map[string]factReviewGroup)
	for _, group := range groups {
		for _, proposal := range group.Proposals {
			byID[factReviewID(proposal)] = group
		}
		for _, fact := range group.Facts {
			byFactID[fact.ID] = group
		}
	}
	return byID, byFactID
}

// guardedFactCandidateLimit reserves exactly enough ranked rows to backfill the
// requested limit if every valid pending review component collapses. Ranking
// more rows cannot improve the guarded top-N, and ranking the whole fact corpus
// creates avoidable conversions and allocations when no review is pending.
func guardedFactCandidateLimit(facts []factRecord, proposals []factProposal, limit int) int {
	if limit <= 0 || len(facts) == 0 {
		return 0
	}
	if limit >= len(facts) {
		return len(facts)
	}
	reserve := 0
	for _, group := range buildFactReviewGroups(facts, proposals) {
		reserve += len(group.Facts) - 1
		if reserve >= len(facts)-limit {
			return len(facts)
		}
	}
	return limit + reserve
}

// guardUnifiedFactResults collapses every pending proposal component into one
// explicit review result and annotates current-code drift. It preserves the
// first ranked position and best score of each component, then backfills from
// the expanded candidate set up to limit.
func guardUnifiedFactResults(repoDir string, facts []factRecord, proposals []factProposal, ranked []unifiedResult, limit int) []unifiedResult {
	if limit <= 0 || len(ranked) == 0 {
		return ranked
	}
	factByID := make(map[string]factRecord, len(facts))
	for _, fact := range facts {
		factByID[fact.ID] = fact
	}
	_, reviewByFactID := indexFactReviewGroups(buildFactReviewGroups(facts, proposals))
	bestReviewScore := make(map[string]float64)
	for _, result := range ranked {
		if group, ok := reviewByFactID[result.ID]; ok && result.Score > bestReviewScore[group.ID] {
			bestReviewScore[group.ID] = result.Score
		}
	}

	emittedReviews := make(map[string]bool)
	out := make([]unifiedResult, 0, min(limit, len(ranked)))
	for _, result := range ranked {
		if result.Source == "fact" {
			if group, ok := reviewByFactID[result.ID]; ok {
				if emittedReviews[group.ID] {
					continue
				}
				emittedReviews[group.ID] = true
				review := factReviewToUnified(repoDir, group)
				review.Score = bestReviewScore[group.ID]
				out = append(out, review)
			} else if fact, ok := factByID[result.ID]; ok {
				out = append(out, annotateFactLocusTrust(repoDir, fact, result))
			} else {
				out = append(out, result)
			}
		} else {
			out = append(out, result)
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func factReviewToUnified(repoDir string, group factReviewGroup) unifiedResult {
	paths := make(map[string]struct{})
	relatedIDs := make([]string, 0, len(group.Facts))
	statements := make([]string, 0, len(group.Facts))
	var drift []string
	for _, fact := range group.Facts {
		relatedIDs = append(relatedIDs, fact.ID)
		statements = append(statements, fmt.Sprintf("%s says %q", fact.ID, fact.Text))
		for _, path := range fact.Paths {
			paths[path] = struct{}{}
		}
		drift = append(drift, factLocusDrift(repoDir, fact)...)
	}
	sort.Strings(relatedIDs)
	sort.Strings(statements)

	actions := make([]string, 0, len(group.Proposals))
	for _, proposal := range group.Proposals {
		actions = append(actions, fmt.Sprintf("%s %s -> %s (confidence %.2f)", proposal.Action, proposal.CandidateID, proposal.TargetID, proposal.Confidence))
	}
	caveat := factReviewCaveat(group)
	text := "Pending fact review. " + caveat.Message + " Facts: " + strings.Join(statements, "; ") +
		". Proposed actions: " + strings.Join(actions, "; ") + "."
	result := unifiedResult{
		Source:               "fact-review",
		ID:                   group.ID,
		Path:                 strings.Join(sortedStringSet(paths), ","),
		Heading:              "pending fact review",
		Text:                 text,
		VerificationRequired: true,
		Caveats:              []retrievalCaveat{caveat},
		RelatedIDs:           relatedIDs,
	}
	if drift = uniqueSortedStrings(drift); len(drift) > 0 {
		result.Caveats = append(result.Caveats, staleLocusCaveat(drift))
	}
	return result
}

func annotateExplicitFactReview(result unifiedResult, group factReviewGroup) unifiedResult {
	result.VerificationRequired = true
	// group.Facts is already in ascending fact-ID order (buildFactReviewGroups
	// sorts the component's ids first), so filtering out the current fact leaves
	// related sorted without an extra sort — same invariant factsPendingReview
	// relies on.
	related := make([]string, 0, len(group.Facts)-1)
	for _, fact := range group.Facts {
		if fact.ID != result.ID {
			related = append(related, fact.ID)
		}
	}
	result.RelatedIDs = related
	result.Caveats = append(result.Caveats, factReviewCaveat(group))
	return result
}

func factReviewCaveat(group factReviewGroup) retrievalCaveat {
	message := "A pending merge proposal may represent duplicate or distinct facts; verify the relationship before treating the statements as independent."
	for _, proposal := range group.Proposals {
		if proposal.Action == factActionSupersede {
			message = "A pending supersede proposal indicates a potential contradiction; verify the related statements before relying on either one."
			break
		}
	}
	caveat := retrievalCaveat{
		Kind:     retrievalCaveatUnresolvedReview,
		Message:  message,
		ReviewID: group.ID,
	}
	if len(group.Proposals) == 1 {
		caveat.Action = group.Proposals[0].Action
		caveat.Confidence = group.Proposals[0].Confidence
	}
	return caveat
}

// factReviewNotice is the per-fact trust annotation for surfaces whose results
// are factRecords rather than unifiedResults (recall, brief). Those listings
// keep their record shape — no collapse into a fact-review row — so the pending
// state rides alongside, keyed by fact id like locus drift.
type factReviewNotice struct {
	ReviewID   string   `json:"review_id"`
	Action     string   `json:"action,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
	Message    string   `json:"message"`
	RelatedIDs []string `json:"related_ids,omitempty"`
}

// factReviewQueueUnavailableWarning mirrors the unified path's
// proposal_state_unavailable caveat for the facts-shaped surfaces.
const factReviewQueueUnavailableWarning = "fact review queue unreadable; verify fact consistency before relying on these facts"

// factsPendingReview maps each surfaced fact that participates in a pending
// review component to its review notice. facts is the full branch set (group
// membership must not depend on the caller's scope/kind/locus filters), so
// building the review groups is O(facts + proposals); only the final
// annotation loop over surfaced is O(page). The early return keeps that full
// scan off the hot path whenever nothing was surfaced or no proposals exist.
func factsPendingReview(facts []factRecord, proposals []factProposal, surfaced []factRecord) map[string]factReviewNotice {
	if len(surfaced) == 0 || len(proposals) == 0 {
		return nil
	}
	_, byFactID := indexFactReviewGroups(buildFactReviewGroups(facts, proposals))
	if len(byFactID) == 0 {
		return nil
	}
	out := map[string]factReviewNotice{}
	for _, fact := range surfaced {
		group, ok := byFactID[fact.ID]
		if !ok {
			continue
		}
		caveat := factReviewCaveat(group)
		// group.Facts is already in ascending fact-ID order (buildFactReviewGroups
		// sorts the component's ids before materializing facts), so filtering out
		// the current fact leaves related sorted without an extra sort.
		related := make([]string, 0, len(group.Facts)-1)
		for _, other := range group.Facts {
			if other.ID != fact.ID {
				related = append(related, other.ID)
			}
		}
		out[fact.ID] = factReviewNotice{
			ReviewID:   group.ID,
			Action:     caveat.Action,
			Confidence: caveat.Confidence,
			Message:    caveat.Message,
			RelatedIDs: related,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// factReviewNoticeLine renders a notice for the human-readable fact listings,
// shared by recall and brief so the two surfaces stay in lockstep.
func factReviewNoticeLine(notice factReviewNotice) string {
	line := "⚠ pending fact review " + notice.ReviewID
	if notice.Action != "" {
		line += fmt.Sprintf(" (%s, confidence %.2f)", notice.Action, notice.Confidence)
	}
	if len(notice.RelatedIDs) > 0 {
		line += ": verify against " + strings.Join(notice.RelatedIDs, ", ")
	}
	return line
}

func annotateProposalStateUnavailable(results []unifiedResult) []unifiedResult {
	return annotateFactResultCaveat(results, retrievalCaveat{
		Kind:    retrievalCaveatProposalStateUnavailable,
		Message: "The pending fact-review queue could not be read; verify fact consistency before relying on these results.",
	})
}

func annotateCurrentCodeUnavailable(results []unifiedResult) []unifiedResult {
	return annotateFactResultCaveat(results, retrievalCaveat{
		Kind:    retrievalCaveatCurrentCodeUnavailable,
		Message: "The repository worktree could not be safely resolved, so current-code locus verification was not performed.",
	})
}

func annotateFactResultCaveat(results []unifiedResult, caveat retrievalCaveat) []unifiedResult {
	for i := range results {
		if results[i].Source != "fact" && results[i].Source != "fact-review" {
			continue
		}
		results[i].VerificationRequired = true
		results[i].Caveats = append(results[i].Caveats, caveat)
	}
	return results
}

func annotateFactLocusTrust(repoDir string, fact factRecord, result unifiedResult) unifiedResult {
	drift := factLocusDrift(repoDir, fact)
	if len(drift) == 0 {
		return result
	}
	result.VerificationRequired = true
	result.Caveats = append(result.Caveats, staleLocusCaveat(uniqueSortedStrings(drift)))
	return result
}

func staleLocusCaveat(paths []string) retrievalCaveat {
	return retrievalCaveat{
		Kind:    retrievalCaveatStaleLocus,
		Message: "One or more referenced code files are absent from the current worktree; verify whether the fact was invalidated or moved.",
		Paths:   paths,
	}
}

func sortedStringSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func uniqueSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
