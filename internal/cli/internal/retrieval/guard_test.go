package retrieval

import (
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

func TestGuardUnifiedFactResultsCollapsesReviewAndBackfills(t *testing.T) {
	repoDir := t.TempDir()
	facts := []factmerge.Record{
		{ID: "fact:candidate", Text: "Use the new cache.", Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cache.go"}, Status: factmerge.StatusActive},
		{ID: "fact:target", Text: "Do not use the new cache.", Paths: []string{"architecture.data.flow"}, Status: factmerge.StatusActive},
		{ID: "fact:current", Text: "Requests are idempotent.", Paths: []string{"constraints.invariants.general"}, Status: factmerge.StatusActive},
	}
	proposal := factmerge.Proposal{Action: factmerge.ActionSupersede, CandidateID: "fact:candidate", TargetID: "fact:target", Confidence: 0.61}
	ranked := []Result{
		{Source: "fact", ID: "fact:candidate", Text: facts[0].Text, Score: 0.9},
		{Source: "fact", ID: "fact:target", Text: facts[1].Text, Score: 0.8},
		{Source: "fact", ID: "fact:current", Text: facts[2].Text, Score: 0.7},
		{Source: "doc", ID: "doc:runbook", Text: "Runbook", Score: 0.6},
	}

	got := guardUnifiedFactResults(repoDir, facts, []factmerge.Proposal{proposal}, ranked, 3)
	if len(got) != 3 {
		t.Fatalf("collapse should backfill to the requested limit, got %d: %+v", len(got), got)
	}
	if got[0].Source != "fact-review" || !strings.HasPrefix(got[0].ID, "review:") {
		t.Fatalf("pending participants should collapse into one review result, got %+v", got[0])
	}
	if got[0].Score != 0.9 {
		t.Fatalf("review should retain the best participant score, got %v", got[0].Score)
	}
	if !got[0].VerificationRequired || !hasRetrievalCaveat(got[0], CaveatUnresolvedReview) || !hasRetrievalCaveat(got[0], CaveatStaleLocus) {
		t.Fatalf("review must expose proposal and current-code caveats: %+v", got[0])
	}
	if !reflect.DeepEqual(got[0].RelatedIDs, []string{"fact:candidate", "fact:target"}) {
		t.Fatalf("review related ids = %v", got[0].RelatedIDs)
	}
	if got[1].ID != "fact:current" || got[2].ID != "doc:runbook" {
		t.Fatalf("independent candidates were not backfilled in rank order: %+v", got)
	}
}

func TestGuardedFactCandidateLimitAddsOnlyCollapseReserve(t *testing.T) {
	facts := []factmerge.Record{
		{ID: "fact:a", Status: factmerge.StatusActive},
		{ID: "fact:b", Status: factmerge.StatusActive},
		{ID: "fact:c", Status: factmerge.StatusActive},
		{ID: "fact:d", Status: factmerge.StatusActive},
		{ID: "fact:e", Status: factmerge.StatusActive},
	}
	if got := guardedFactCandidateLimit(facts, nil, 2); got != 2 {
		t.Fatalf("no-review candidate limit = %d, want 2", got)
	}
	proposals := []factmerge.Proposal{
		{Action: factmerge.ActionMerge, CandidateID: "fact:a", TargetID: "fact:b"},
		{Action: factmerge.ActionSupersede, CandidateID: "fact:c", TargetID: "fact:b"},
		{Action: factmerge.ActionMerge, CandidateID: "fact:d", TargetID: "fact:missing"},
	}
	if got := guardedFactCandidateLimit(facts, proposals, 2); got != 4 {
		t.Fatalf("three-fact review candidate limit = %d, want 4", got)
	}
	if got := guardedFactCandidateLimit(facts, proposals, 10); got != len(facts) {
		t.Fatalf("oversized requested limit = %d, want %d", got, len(facts))
	}

	ranked := FactsToUnified(facts[:guardedFactCandidateLimit(facts, proposals, 2)])
	guarded := guardUnifiedFactResults("", facts, proposals, ranked, 2)
	if len(guarded) != 2 || guarded[0].Source != "fact-review" || guarded[1].ID != "fact:d" {
		t.Fatalf("collapse reserve did not backfill the guarded top-2: %+v", guarded)
	}
}

func TestPendingMergeMessageDoesNotInventLowConfidence(t *testing.T) {
	group := BuildFactReviewGroups(
		[]factmerge.Record{{ID: "fact:a", Status: factmerge.StatusActive}, {ID: "fact:b", Status: factmerge.StatusActive}},
		[]factmerge.Proposal{{Action: factmerge.ActionMerge, CandidateID: "fact:a", TargetID: "fact:b", Confidence: 0.97}},
	)
	if len(group) != 1 {
		t.Fatalf("expected one group, got %+v", group)
	}
	caveat := factReviewCaveat(group[0])
	if strings.Contains(strings.ToLower(caveat.Message), "low-confidence") {
		t.Fatalf("caveat contradicted stored confidence %.2f: %q", caveat.Confidence, caveat.Message)
	}
}

func TestInvalidProposalDoesNotRelabelIndependentFact(t *testing.T) {
	facts := []factmerge.Record{{ID: "fact:a", Text: "A", Status: factmerge.StatusActive}}
	proposals := []factmerge.Proposal{{Action: factmerge.ActionSupersede, CandidateID: "fact:a", TargetID: "fact:missing", Confidence: 0.5}}
	got := guardUnifiedFactResults("", facts, proposals, []Result{{Source: "fact", ID: "fact:a", Text: "A", Score: 1}}, 1)
	if len(got) != 1 || got[0].Source != "fact" || got[0].VerificationRequired {
		t.Fatalf("stale proposal queue entry should not relabel a fact: %+v", got)
	}
}

func TestEmptyGuardPreservesArrayShape(t *testing.T) {
	ranked := make([]Result, 0)
	got := guardUnifiedFactResults("", nil, nil, ranked, 10)
	if got == nil {
		t.Fatal("empty ranked results must remain a non-nil slice for JSON array output")
	}
}

func hasRetrievalCaveat(result Result, kind string) bool {
	for _, caveat := range result.Caveats {
		if caveat.Kind == kind {
			return true
		}
	}
	return false
}
