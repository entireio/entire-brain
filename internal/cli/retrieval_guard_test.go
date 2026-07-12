package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestGuardUnifiedFactResultsCollapsesReviewAndBackfills(t *testing.T) {
	repoDir := t.TempDir()
	facts := []factRecord{
		{ID: "fact:candidate", Text: "Use the new cache.", Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cache.go"}, Status: factStatusActive},
		{ID: "fact:target", Text: "Do not use the new cache.", Paths: []string{"architecture.data.flow"}, Status: factStatusActive},
		{ID: "fact:current", Text: "Requests are idempotent.", Paths: []string{"constraints.invariants.general"}, Status: factStatusActive},
	}
	proposal := factProposal{Action: factActionSupersede, CandidateID: "fact:candidate", TargetID: "fact:target", Confidence: 0.61}
	ranked := []unifiedResult{
		{Source: "fact", ID: "fact:candidate", Text: facts[0].Text, Score: 0.9},
		{Source: "fact", ID: "fact:target", Text: facts[1].Text, Score: 0.8},
		{Source: "fact", ID: "fact:current", Text: facts[2].Text, Score: 0.7},
		{Source: "doc", ID: "doc:runbook", Text: "Runbook", Score: 0.6},
	}

	got := guardUnifiedFactResults(repoDir, facts, []factProposal{proposal}, ranked, 3)
	if len(got) != 3 {
		t.Fatalf("collapse should backfill to the requested limit, got %d: %+v", len(got), got)
	}
	if got[0].Source != "fact-review" || !strings.HasPrefix(got[0].ID, "review:") {
		t.Fatalf("pending participants should collapse into one review result, got %+v", got[0])
	}
	if got[0].Score != 0.9 {
		t.Fatalf("review should retain the best participant score, got %v", got[0].Score)
	}
	if !got[0].VerificationRequired || !hasRetrievalCaveat(got[0], retrievalCaveatUnresolvedReview) || !hasRetrievalCaveat(got[0], retrievalCaveatStaleLocus) {
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
	facts := []factRecord{
		{ID: "fact:a", Status: factStatusActive},
		{ID: "fact:b", Status: factStatusActive},
		{ID: "fact:c", Status: factStatusActive},
		{ID: "fact:d", Status: factStatusActive},
		{ID: "fact:e", Status: factStatusActive},
	}
	if got := guardedFactCandidateLimit(facts, nil, 2); got != 2 {
		t.Fatalf("no-review candidate limit = %d, want 2", got)
	}
	proposals := []factProposal{
		{Action: factActionMerge, CandidateID: "fact:a", TargetID: "fact:b"},
		{Action: factActionSupersede, CandidateID: "fact:c", TargetID: "fact:b"},
		{Action: factActionMerge, CandidateID: "fact:d", TargetID: "fact:missing"},
	}
	if got := guardedFactCandidateLimit(facts, proposals, 2); got != 4 {
		t.Fatalf("three-fact review candidate limit = %d, want 4", got)
	}
	if got := guardedFactCandidateLimit(facts, proposals, 10); got != len(facts) {
		t.Fatalf("oversized requested limit = %d, want %d", got, len(facts))
	}

	ranked := factsToUnified(facts[:guardedFactCandidateLimit(facts, proposals, 2)])
	guarded := guardUnifiedFactResults("", facts, proposals, ranked, 2)
	if len(guarded) != 2 || guarded[0].Source != "fact-review" || guarded[1].ID != "fact:d" {
		t.Fatalf("collapse reserve did not backfill the guarded top-2: %+v", guarded)
	}
}

func TestFactReviewIDStableAcrossInputOrder(t *testing.T) {
	facts := []factRecord{
		{ID: "fact:a", Text: "A", Status: factStatusActive},
		{ID: "fact:b", Text: "B", Status: factStatusActive},
		{ID: "fact:c", Text: "C", Status: factStatusActive},
	}
	p1 := factProposal{Action: factActionMerge, CandidateID: "fact:a", TargetID: "fact:b", Confidence: 0.6}
	p2 := factProposal{Action: factActionSupersede, CandidateID: "fact:c", TargetID: "fact:b", Confidence: 0.7}
	left := buildFactReviewGroups(facts, []factProposal{p1, p2})
	right := buildFactReviewGroups(facts, []factProposal{p2, p1, p1})
	if len(left) != 1 || len(right) != 1 || left[0].ID != right[0].ID {
		t.Fatalf("review id must be stable and duplicate-insensitive: left=%+v right=%+v", left, right)
	}
}

func TestFactReviewAliasSurvivesGroupGrowth(t *testing.T) {
	facts := []factRecord{
		{ID: "fact:a", Text: "A", Status: factStatusActive},
		{ID: "fact:b", Text: "B", Status: factStatusActive},
		{ID: "fact:c", Text: "C", Status: factStatusActive},
	}
	p1 := factProposal{Action: factActionMerge, CandidateID: "fact:a", TargetID: "fact:b", Confidence: 0.6}
	p2 := factProposal{Action: factActionSupersede, CandidateID: "fact:c", TargetID: "fact:b", Confidence: 0.7}
	original := buildFactReviewGroups(facts, []factProposal{p1})
	if len(original) != 1 {
		t.Fatalf("expected one original group, got %+v", original)
	}
	oldID := original[0].ID
	byID, _ := indexFactReviewGroups(buildFactReviewGroups(facts, []factProposal{p1, p2}))
	if _, ok := byID[oldID]; !ok {
		t.Fatalf("existing proposal id %s stopped resolving after its group grew", oldID)
	}
}

func TestPendingMergeMessageDoesNotInventLowConfidence(t *testing.T) {
	group := buildFactReviewGroups(
		[]factRecord{{ID: "fact:a", Status: factStatusActive}, {ID: "fact:b", Status: factStatusActive}},
		[]factProposal{{Action: factActionMerge, CandidateID: "fact:a", TargetID: "fact:b", Confidence: 0.97}},
	)
	if len(group) != 1 {
		t.Fatalf("expected one group, got %+v", group)
	}
	caveat := factReviewCaveat(group[0])
	if strings.Contains(strings.ToLower(caveat.Message), "low-confidence") {
		t.Fatalf("caveat contradicted stored confidence %.2f: %q", caveat.Confidence, caveat.Message)
	}
}

func TestGetUnifiedBatchAddressesReviewAndAnnotatesFacts(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "internal", "current.go"), []byte("package internal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts := []factRecord{
		{ID: "fact:candidate", Text: "Use the new cache.", Locus: []string{"internal/removed.go"}, Status: factStatusActive},
		{ID: "fact:target", Text: "Do not use the new cache.", Status: factStatusActive},
		{ID: "fact:current", Text: "Current implementation fact.", Locus: []string{"internal/current.go"}, Status: factStatusActive},
	}
	proposal := factProposal{Action: factActionSupersede, CandidateID: "fact:candidate", TargetID: "fact:target", Confidence: 0.55, Branch: "main"}
	if err := writeFacts(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	if err := writeFactProposals(brainDir, "main", []factProposal{proposal}); err != nil {
		t.Fatal(err)
	}
	groups := buildFactReviewGroups(facts, []factProposal{proposal})
	if len(groups) != 1 {
		t.Fatalf("expected one review group, got %+v", groups)
	}

	found, missing, err := getUnifiedBatch(repoDir, brainDir, "main", []string{groups[0].ID, "fact:candidate", "fact:current"})
	if err != nil {
		t.Fatalf("getUnifiedBatch: %v", err)
	}
	if len(missing) != 0 || len(found) != 3 {
		t.Fatalf("found=%+v missing=%v", found, missing)
	}
	if found[0].Source != "fact-review" || !found[0].VerificationRequired || !hasRetrievalCaveat(found[0], retrievalCaveatStaleLocus) {
		t.Fatalf("addressed review did not carry trust metadata: %+v", found[0])
	}
	if !found[1].VerificationRequired || !hasRetrievalCaveat(found[1], retrievalCaveatUnresolvedReview) || !hasRetrievalCaveat(found[1], retrievalCaveatStaleLocus) {
		t.Fatalf("explicit pending fact did not carry both caveats: %+v", found[1])
	}
	if found[2].VerificationRequired || len(found[2].Caveats) != 0 {
		t.Fatalf("current independent fact should remain uncaveated: %+v", found[2])
	}
}

func TestInvalidProposalDoesNotRelabelIndependentFact(t *testing.T) {
	facts := []factRecord{{ID: "fact:a", Text: "A", Status: factStatusActive}}
	proposals := []factProposal{{Action: factActionSupersede, CandidateID: "fact:a", TargetID: "fact:missing", Confidence: 0.5}}
	got := guardUnifiedFactResults("", facts, proposals, []unifiedResult{{Source: "fact", ID: "fact:a", Text: "A", Score: 1}}, 1)
	if len(got) != 1 || got[0].Source != "fact" || got[0].VerificationRequired {
		t.Fatalf("stale proposal queue entry should not relabel a fact: %+v", got)
	}
}

func TestEmptyGuardPreservesArrayShape(t *testing.T) {
	ranked := make([]unifiedResult, 0)
	got := guardUnifiedFactResults("", nil, nil, ranked, 10)
	if got == nil {
		t.Fatal("empty ranked results must remain a non-nil slice for JSON array output")
	}
}

func TestMalformedProposalQueueCaveatsFactsButKeepsGetAvailable(t *testing.T) {
	brainDir := t.TempDir()
	fact := factRecord{ID: "fact:a", Text: "A", Status: factStatusActive}
	if err := writeFacts(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	proposalPath := filepath.Join(brainDir, filepath.FromSlash(factsProposalsRelPath("main")))
	if err := os.WriteFile(proposalPath, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	found, missing, err := getUnifiedBatch(t.TempDir(), brainDir, "main", []string{fact.ID})
	if err != nil {
		t.Fatalf("ordinary fact get should degrade with a caveat, not fail: %v", err)
	}
	if len(missing) != 0 || len(found) != 1 || !hasRetrievalCaveat(found[0], retrievalCaveatProposalStateUnavailable) {
		t.Fatalf("malformed proposal queue did not produce an explicit caveat: found=%+v missing=%v", found, missing)
	}
	if _, _, err := getUnifiedBatch("", brainDir, "main", []string{"review:unknown"}); err == nil {
		t.Fatal("an explicit review lookup must report an unreadable proposal queue")
	}
}

func TestCurrentCodeUnavailableCaveatsOnlyMemoryFacts(t *testing.T) {
	got := annotateCurrentCodeUnavailable([]unifiedResult{
		{Source: "fact", ID: "fact:a"},
		{Source: "fact-review", ID: "review:a"},
		{Source: "history", ID: "history:a"},
		{Source: "doc", ID: "doc:a"},
	})
	for i := 0; i < 2; i++ {
		if !got[i].VerificationRequired || !hasRetrievalCaveat(got[i], retrievalCaveatCurrentCodeUnavailable) {
			t.Fatalf("memory result %d was not caveated: %+v", i, got[i])
		}
	}
	for i := 2; i < len(got); i++ {
		if got[i].VerificationRequired || len(got[i].Caveats) != 0 {
			t.Fatalf("non-fact result %d was incorrectly caveated: %+v", i, got[i])
		}
	}
}

func TestSearchCollapsesPendingFactsAndGetAddressesReview(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	facts := []factRecord{
		{ID: "fact:new", Text: "cache contract uses the new implementation", Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/removed.go"}, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
		{ID: "fact:old", Text: "cache contract uses the old implementation", Paths: []string{"architecture.data.flow"}, Branch: "feature", Status: factStatusActive, UpdatedAt: now.Add(-time.Hour)},
	}
	proposal := factProposal{Action: factActionSupersede, CandidateID: "fact:new", TargetID: "fact:old", Confidence: 0.62, Branch: "feature"}
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}
	if err := writeFactProposals(storage.BrainDir, "feature", []factProposal{proposal}); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "search", "cache contract", "--format", "json", "--limit", "5")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, out)
	}
	var searchPayload struct {
		Results []unifiedResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &searchPayload); err != nil {
		t.Fatalf("decode search: %v\n%s", err, out)
	}
	if len(searchPayload.Results) != 1 || searchPayload.Results[0].Source != "fact-review" {
		t.Fatalf("search did not collapse pending facts: %+v", searchPayload.Results)
	}
	review := searchPayload.Results[0]
	if !review.VerificationRequired || !hasRetrievalCaveat(review, retrievalCaveatUnresolvedReview) || !hasRetrievalCaveat(review, retrievalCaveatStaleLocus) {
		t.Fatalf("search review omitted trust metadata: %+v", review)
	}

	getOut, err := execute(t, NewRootCommand(opts), "get", review.ID, "--format", "json")
	if err != nil {
		t.Fatalf("get %s: %v\n%s", review.ID, err, getOut)
	}
	var getPayload struct {
		Results []unifiedResult `json:"results"`
		Missing []string        `json:"missing"`
	}
	if err := json.Unmarshal([]byte(getOut), &getPayload); err != nil {
		t.Fatalf("decode get: %v\n%s", err, getOut)
	}
	if len(getPayload.Results) != 1 || getPayload.Results[0].ID != review.ID || len(getPayload.Missing) != 0 {
		t.Fatalf("review id was not addressable: %+v", getPayload)
	}
}

func hasRetrievalCaveat(result unifiedResult, kind string) bool {
	for _, caveat := range result.Caveats {
		if caveat.Kind == kind {
			return true
		}
	}
	return false
}
