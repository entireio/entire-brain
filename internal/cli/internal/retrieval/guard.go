package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
)

const (
	CaveatStaleLocus               = "stale_locus"
	CaveatUnresolvedReview         = "unresolved_fact_proposal"
	CaveatProposalStateUnavailable = "proposal_state_unavailable"
	CaveatCurrentCodeUnavailable   = "current_code_unavailable"
	CaveatHistoricalDocument       = "historical_document"
	CaveatHistoricalConversation   = "historical_conversation"
	CaveatConversationSourceStale  = "conversation_source_stale"
	// Distinct expansion failure states: a too-large or unreadable
	// source is not the same contract failure as a changed digest.
	CaveatConversationSourceTooLarge   = "conversation_source_too_large"
	CaveatConversationSourceUnreadable = "conversation_source_unreadable"
)

// Caveat is a machine-readable reason an agent must verify a memory
// result against current evidence before relying on it.
type Caveat struct {
	Kind       string   `json:"kind"`
	Message    string   `json:"message"`
	Paths      []string `json:"paths,omitempty"`
	ReviewID   string   `json:"review_id,omitempty"`
	Action     string   `json:"action,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
}

type FactReviewGroup struct {
	ID        string
	Facts     []factmerge.Record
	Proposals []factmerge.Proposal
}

// BuildFactReviewGroups turns the pending proposal graph into stable connected
// components. Only valid proposals between two active facts affect retrieval;
// stale queue entries remain available to the review command but cannot hide or
// relabel an otherwise independent fact.
func BuildFactReviewGroups(facts []factmerge.Record, proposals []factmerge.Proposal) []FactReviewGroup {
	if len(proposals) == 0 {
		return nil
	}
	active := make(map[string]factmerge.Record, len(facts))
	for _, fact := range facts {
		if fact.Status == factmerge.StatusActive {
			active[fact.ID] = fact
		}
	}

	parent := make(map[string]string)
	// ITERATIVE path compression. The recursive form recursed once per link in
	// the parent chain, so a long chain was bounded only by the stack. Union by
	// smaller id keeps chains short in practice, which is why this has not
	// bitten -- but "short in practice" is not a bound, and the iterative form
	// costs nothing and has one.
	findRoot := func(id string) string {
		p, ok := parent[id]
		if !ok {
			parent[id] = id
			return id
		}
		// Walk to the root, remembering the path.
		root := id
		var path []string
		for {
			p, ok = parent[root]
			if !ok || p == root {
				if !ok {
					parent[root] = root
				}
				break
			}
			path = append(path, root)
			root = p
		}
		// Point every node on the path straight at the root.
		for _, node := range path {
			parent[node] = root
		}
		return root
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

	validByIdentity := make(map[string]factmerge.Proposal, len(proposals))
	for _, proposal := range proposals {
		if proposal.Action != factmerge.ActionMerge && proposal.Action != factmerge.ActionSupersede {
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
	valid := make([]factmerge.Proposal, 0, len(validByIdentity))
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
	proposalsByRoot := make(map[string][]factmerge.Proposal)
	for _, proposal := range valid {
		root := findRoot(proposal.CandidateID)
		proposalsByRoot[root] = append(proposalsByRoot[root], proposal)
	}

	groups := make([]FactReviewGroup, 0, len(idsByRoot))
	for root, ids := range idsByRoot {
		sort.Strings(ids)
		groupProposals := proposalsByRoot[root]
		sort.Slice(groupProposals, func(i, j int) bool {
			return factProposalIdentityKey(groupProposals[i]) < factProposalIdentityKey(groupProposals[j])
		})
		group := FactReviewGroup{ID: factReviewID(groupProposals[0]), Proposals: groupProposals}
		for _, id := range ids {
			group.Facts = append(group.Facts, active[id])
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups
}

func factProposalIdentityKey(proposal factmerge.Proposal) string {
	return proposal.Action + "\x00" + proposal.CandidateID + "\x00" + proposal.TargetID
}

func factReviewID(proposal factmerge.Proposal) string {
	sum := sha256.Sum256([]byte(factProposalIdentityKey(proposal)))
	return "review:" + hex.EncodeToString(sum[:8])
}

func IndexFactReviewGroups(groups []FactReviewGroup) (map[string]FactReviewGroup, map[string]FactReviewGroup) {
	byID := make(map[string]FactReviewGroup, len(groups))
	byFactID := make(map[string]FactReviewGroup)
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
func guardedFactCandidateLimit(facts []factmerge.Record, proposals []factmerge.Proposal, limit int) int {
	return guardedFactCandidateLimitForGroups(len(facts), BuildFactReviewGroups(facts, proposals), limit)
}

func guardedFactCandidateLimitForGroups(factCount int, groups []FactReviewGroup, limit int) int {
	if limit <= 0 || factCount == 0 {
		return 0
	}
	if limit >= factCount {
		return factCount
	}
	reserve := 0
	for _, group := range groups {
		reserve += len(group.Facts) - 1
		if reserve >= factCount-limit {
			return factCount
		}
	}
	return limit + reserve
}

// guardUnifiedFactResults collapses every pending proposal component into one
// explicit review result and annotates current-code drift. It preserves the
// first ranked position and best score of each component, then backfills from
// the expanded candidate set up to limit.
func guardUnifiedFactResults(repoDir string, facts []factmerge.Record, proposals []factmerge.Proposal, ranked []Result, limit int) []Result {
	return guardUnifiedFactResultsWithGroups(repoDir, facts, BuildFactReviewGroups(facts, proposals), ranked, limit)
}

func guardUnifiedFactResultsWithGroups(repoDir string, facts []factmerge.Record, groups []FactReviewGroup, ranked []Result, limit int) []Result {
	if limit <= 0 || len(ranked) == 0 {
		return ranked
	}
	factByID := make(map[string]factmerge.Record, len(facts))
	for _, fact := range facts {
		factByID[fact.ID] = fact
	}
	reviewByFactID := make(map[string]FactReviewGroup)
	for _, group := range groups {
		for _, fact := range group.Facts {
			reviewByFactID[fact.ID] = group
		}
	}
	bestReviewScore := make(map[string]float64)
	for _, result := range ranked {
		if group, ok := reviewByFactID[result.ID]; ok && result.Score > bestReviewScore[group.ID] {
			bestReviewScore[group.ID] = result.Score
		}
	}

	emittedReviews := make(map[string]bool)
	out := make([]Result, 0, min(limit, len(ranked)))
	for _, result := range ranked {
		if result.Source == "fact" {
			if group, ok := reviewByFactID[result.ID]; ok {
				if emittedReviews[group.ID] {
					continue
				}
				emittedReviews[group.ID] = true
				review := FactReviewToUnified(repoDir, group)
				review.Score = bestReviewScore[group.ID]
				out = append(out, review)
			} else if fact, ok := factByID[result.ID]; ok {
				out = append(out, AnnotateFactLocusTrust(repoDir, fact, result))
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

func FactReviewToUnified(repoDir string, group FactReviewGroup) Result {
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
		drift = append(drift, FactLocusDrift(repoDir, fact)...)
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
	result := Result{
		Source:               "fact-review",
		ID:                   group.ID,
		Path:                 strings.Join(sortedStringSet(paths), ","),
		Heading:              "pending fact review",
		Text:                 text,
		VerificationRequired: true,
		Caveats:              []Caveat{caveat},
		RelatedIDs:           relatedIDs,
	}
	if drift = uniqueSortedStrings(drift); len(drift) > 0 {
		result.Caveats = append(result.Caveats, staleLocusCaveat(drift))
	}
	return result
}

func AnnotateExplicitFactReview(result Result, group FactReviewGroup) Result {
	result.VerificationRequired = true
	// group.Facts is already in ascending fact-ID order (BuildFactReviewGroups
	// sorts the component's ids first), so filtering out the current fact leaves
	// related sorted without an extra sort — same invariant FactsPendingReview
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

func factReviewCaveat(group FactReviewGroup) Caveat {
	message := "A pending merge proposal may represent duplicate or distinct facts; verify the relationship before treating the statements as independent."
	for _, proposal := range group.Proposals {
		if proposal.Action == factmerge.ActionSupersede {
			message = "A pending supersede proposal indicates a potential contradiction; verify the related statements before relying on either one."
			break
		}
	}
	caveat := Caveat{
		Kind:     CaveatUnresolvedReview,
		Message:  message,
		ReviewID: group.ID,
	}
	if len(group.Proposals) == 1 {
		caveat.Action = group.Proposals[0].Action
		caveat.Confidence = group.Proposals[0].Confidence
	}
	return caveat
}

// FactReviewNotice is the per-fact trust annotation for surfaces whose results
// are factRecords rather than unifiedResults (recall, brief). Those listings
// keep their record shape — no collapse into a fact-review row — so the pending
// state rides alongside, keyed by fact id like locus drift.
type FactReviewNotice struct {
	ReviewID   string   `json:"review_id"`
	Action     string   `json:"action,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
	Message    string   `json:"message"`
	RelatedIDs []string `json:"related_ids,omitempty"`
}

// FactReviewQueueUnavailableWarning mirrors the unified path's
// proposal_state_unavailable caveat for the facts-shaped surfaces.
const FactReviewQueueUnavailableWarning = "fact review queue unreadable; verify fact consistency before relying on these facts"

// FactsPendingReview maps each surfaced fact that participates in a pending
// review component to its review notice. facts is the full branch set (group
// membership must not depend on the caller's scope/kind/locus filters), so
// building the review groups is O(facts + proposals); only the final
// annotation loop over surfaced is O(page). The early return keeps that full
// scan off the hot path whenever nothing was surfaced or no proposals exist.
func FactsPendingReview(facts []factmerge.Record, proposals []factmerge.Proposal, surfaced []factmerge.Record) map[string]FactReviewNotice {
	if len(surfaced) == 0 || len(proposals) == 0 {
		return nil
	}
	_, byFactID := IndexFactReviewGroups(BuildFactReviewGroups(facts, proposals))
	if len(byFactID) == 0 {
		return nil
	}
	out := map[string]FactReviewNotice{}
	for _, fact := range surfaced {
		group, ok := byFactID[fact.ID]
		if !ok {
			continue
		}
		caveat := factReviewCaveat(group)
		// group.Facts is already in ascending fact-ID order (BuildFactReviewGroups
		// sorts the component's ids before materializing facts), so filtering out
		// the current fact leaves related sorted without an extra sort.
		related := make([]string, 0, len(group.Facts)-1)
		for _, other := range group.Facts {
			if other.ID != fact.ID {
				related = append(related, other.ID)
			}
		}
		out[fact.ID] = FactReviewNotice{
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

// FactReviewNoticeLine renders a notice for the human-readable fact listings,
// shared by recall and brief so the two surfaces stay in lockstep.
func FactReviewNoticeLine(notice FactReviewNotice) string {
	line := "⚠ pending fact review " + notice.ReviewID
	if notice.Action != "" {
		line += fmt.Sprintf(" (%s, confidence %.2f)", notice.Action, notice.Confidence)
	}
	if len(notice.RelatedIDs) > 0 {
		line += ": verify against " + strings.Join(notice.RelatedIDs, ", ")
	}
	return line
}

func AnnotateProposalStateUnavailable(results []Result) []Result {
	return annotateFactResultCaveat(results, Caveat{
		Kind:    CaveatProposalStateUnavailable,
		Message: "The pending fact-review queue could not be read; verify fact consistency before relying on these results.",
	})
}

func AnnotateCurrentCodeUnavailable(results []Result) []Result {
	return annotateFactResultCaveat(results, Caveat{
		Kind:    CaveatCurrentCodeUnavailable,
		Message: "The repository worktree could not be safely resolved, so current-code locus verification was not performed.",
	})
}

func annotateFactResultCaveat(results []Result, caveat Caveat) []Result {
	for i := range results {
		if results[i].Source != "fact" && results[i].Source != "fact-review" {
			continue
		}
		results[i].VerificationRequired = true
		results[i].Caveats = append(results[i].Caveats, caveat)
	}
	return results
}

func AnnotateFactLocusTrust(repoDir string, fact factmerge.Record, result Result) Result {
	drift := FactLocusDrift(repoDir, fact)
	if len(drift) == 0 {
		return result
	}
	result.VerificationRequired = true
	result.Caveats = append(result.Caveats, staleLocusCaveat(uniqueSortedStrings(drift)))
	return result
}

func staleLocusCaveat(paths []string) Caveat {
	return Caveat{
		Kind:    CaveatStaleLocus,
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
