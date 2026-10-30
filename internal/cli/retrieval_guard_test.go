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

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

func TestFactReviewIDStableAcrossInputOrder(t *testing.T) {
	facts := []factRecord{
		{ID: "fact:a", Text: "A", Status: factStatusActive},
		{ID: "fact:b", Text: "B", Status: factStatusActive},
		{ID: "fact:c", Text: "C", Status: factStatusActive},
	}
	p1 := factProposal{Action: factActionMerge, CandidateID: "fact:a", TargetID: "fact:b", Confidence: 0.6}
	p2 := factProposal{Action: factActionSupersede, CandidateID: "fact:c", TargetID: "fact:b", Confidence: 0.7}
	left := retrieval.BuildFactReviewGroups(facts, []factProposal{p1, p2})
	right := retrieval.BuildFactReviewGroups(facts, []factProposal{p2, p1, p1})
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
	original := retrieval.BuildFactReviewGroups(facts, []factProposal{p1})
	if len(original) != 1 {
		t.Fatalf("expected one original group, got %+v", original)
	}
	oldID := original[0].ID
	byID, _ := retrieval.IndexFactReviewGroups(retrieval.BuildFactReviewGroups(facts, []factProposal{p1, p2}))
	if _, ok := byID[oldID]; !ok {
		t.Fatalf("existing proposal id %s stopped resolving after its group grew", oldID)
	}
	brainDir := t.TempDir()
	if err := factstore.Write(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	if err := writeFactProposals(brainDir, "main", []factProposal{p1, p2}); err != nil {
		t.Fatal(err)
	}
	found, missing, err := getUnifiedBatch(t.TempDir(), brainDir, "main", []string{oldID})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 || len(found) != 1 || found[0].ID != oldID {
		t.Fatalf("review alias did not resolve consistently: found=%+v missing=%v", found, missing)
	}
	foundReviewCaveat := false
	for _, caveat := range found[0].Caveats {
		if caveat.Kind == retrieval.CaveatUnresolvedReview && caveat.ReviewID != oldID {
			t.Fatalf("review alias caveat points at %s, want %s", caveat.ReviewID, oldID)
		}
		foundReviewCaveat = foundReviewCaveat || caveat.Kind == retrieval.CaveatUnresolvedReview
	}
	if !foundReviewCaveat {
		t.Fatalf("review alias omitted its unresolved-review caveat: %+v", found[0])
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
	if err := factstore.Write(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	if err := writeFactProposals(brainDir, "main", []factProposal{proposal}); err != nil {
		t.Fatal(err)
	}
	groups := retrieval.BuildFactReviewGroups(facts, []factProposal{proposal})
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
	if found[0].Source != "fact-review" || !found[0].VerificationRequired || !hasRetrievalCaveat(found[0], retrieval.CaveatStaleLocus) {
		t.Fatalf("addressed review did not carry trust metadata: %+v", found[0])
	}
	if !found[1].VerificationRequired || !hasRetrievalCaveat(found[1], retrieval.CaveatUnresolvedReview) || !hasRetrievalCaveat(found[1], retrieval.CaveatStaleLocus) {
		t.Fatalf("explicit pending fact did not carry both caveats: %+v", found[1])
	}
	if found[2].VerificationRequired || len(found[2].Caveats) != 0 {
		t.Fatalf("current independent fact should remain uncaveated: %+v", found[2])
	}
}

func TestMalformedProposalQueueCaveatsFactsButKeepsGetAvailable(t *testing.T) {
	brainDir := t.TempDir()
	fact := factRecord{ID: "fact:a", Text: "A", Status: factStatusActive}
	if err := factstore.Write(brainDir, "main", []factRecord{fact}); err != nil {
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
	if len(missing) != 0 || len(found) != 1 || !hasRetrievalCaveat(found[0], retrieval.CaveatProposalStateUnavailable) {
		t.Fatalf("malformed proposal queue did not produce an explicit caveat: found=%+v missing=%v", found, missing)
	}
	if _, _, err := getUnifiedBatch("", brainDir, "main", []string{"review:unknown"}); err == nil {
		t.Fatal("an explicit review lookup must report an unreadable proposal queue")
	}
}

func TestCurrentCodeUnavailableCaveatsOnlyMemoryFacts(t *testing.T) {
	got := retrieval.AnnotateCurrentCodeUnavailable([]unifiedResult{
		{Source: "fact", ID: "fact:a"},
		{Source: "fact-review", ID: "review:a"},
		{Source: "history", ID: "history:a"},
		{Source: "doc", ID: "doc:a"},
	})
	for i := 0; i < 2; i++ {
		if !got[i].VerificationRequired || !hasRetrievalCaveat(got[i], retrieval.CaveatCurrentCodeUnavailable) {
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
	if err := factstore.Write(storage.BrainDir, "feature", facts); err != nil {
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
	if !review.VerificationRequired || !hasRetrievalCaveat(review, retrieval.CaveatUnresolvedReview) || !hasRetrievalCaveat(review, retrieval.CaveatStaleLocus) {
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

func TestFactsPendingReviewAnnotatesOnlySurfacedParticipants(t *testing.T) {
	facts := []factRecord{
		{ID: "fact:new", Text: "new contract", Status: factStatusActive},
		{ID: "fact:old", Text: "old contract", Status: factStatusActive},
		{ID: "fact:independent", Text: "unrelated", Status: factStatusActive},
	}
	proposals := []factProposal{{Action: factActionSupersede, CandidateID: "fact:new", TargetID: "fact:old", Confidence: 0.62}}

	// Surface one participant and the independent fact; the filtered-out
	// participant must still appear as a related id, not vanish from the group.
	got := retrieval.FactsPendingReview(facts, proposals, []factRecord{facts[0], facts[2]})
	if len(got) != 1 {
		t.Fatalf("expected exactly the surfaced participant annotated, got %+v", got)
	}
	notice, ok := got["fact:new"]
	if !ok {
		t.Fatalf("surfaced participant missing from notices: %+v", got)
	}
	if !strings.HasPrefix(notice.ReviewID, "review:") || notice.Action != factActionSupersede || notice.Confidence != 0.62 {
		t.Fatalf("notice lost proposal identity: %+v", notice)
	}
	if !reflect.DeepEqual(notice.RelatedIDs, []string{"fact:old"}) {
		t.Fatalf("notice related ids = %v, want the other participant", notice.RelatedIDs)
	}
	if !strings.Contains(notice.Message, "supersede") && !strings.Contains(notice.Message, "contradiction") {
		t.Fatalf("supersede notice should flag a potential contradiction: %q", notice.Message)
	}
	if line := retrieval.FactReviewNoticeLine(notice); !strings.Contains(line, notice.ReviewID) || !strings.Contains(line, "fact:old") {
		t.Fatalf("human-readable notice line lost review id or related fact: %q", line)
	}

	if got := retrieval.FactsPendingReview(facts, nil, facts); got != nil {
		t.Fatalf("no proposals must yield no notices, got %+v", got)
	}
	if got := retrieval.FactsPendingReview(facts, proposals, []factRecord{facts[2]}); got != nil {
		t.Fatalf("independent surfaced fact must not be annotated, got %+v", got)
	}
}

func TestRecallAnnotatesPendingFactReview(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	facts := []factRecord{
		{ID: "fact:new", Text: "cache contract uses the new implementation", Paths: []string{"architecture.data.flow"}, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
		{ID: "fact:old", Text: "cache contract uses the old implementation", Paths: []string{"architecture.data.flow"}, Branch: "feature", Status: factStatusActive, UpdatedAt: now.Add(-time.Hour)},
	}
	proposal := factProposal{Action: factActionSupersede, CandidateID: "fact:new", TargetID: "fact:old", Confidence: 0.62, Branch: "feature"}
	if err := factstore.Write(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}
	if err := writeFactProposals(storage.BrainDir, "feature", []factProposal{proposal}); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "recall", "cache contract", "--no-semantic", "--json")
	if err != nil {
		t.Fatalf("recall: %v\n%s", err, out)
	}
	var payload struct {
		Facts          []factRecord                `json:"facts"`
		PendingReviews map[string]factReviewNotice `json:"pending_reviews"`
		Warnings       []string                    `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode recall: %v\n%s", err, out)
	}
	if len(payload.Facts) != 2 {
		t.Fatalf("recall must keep its record shape (no collapse): %+v", payload.Facts)
	}
	if len(payload.Warnings) != 0 {
		t.Fatalf("readable queue must not warn: %v", payload.Warnings)
	}
	for _, id := range []string{"fact:new", "fact:old"} {
		notice, ok := payload.PendingReviews[id]
		if !ok {
			t.Fatalf("pending participant %s missing trust annotation: %+v", id, payload.PendingReviews)
		}
		if !strings.HasPrefix(notice.ReviewID, "review:") || notice.Action != factActionSupersede {
			t.Fatalf("notice for %s lost proposal identity: %+v", id, notice)
		}
	}

	textOut, err := execute(t, NewRootCommand(opts), "recall", "cache contract", "--no-semantic")
	if err != nil {
		t.Fatalf("recall text: %v\n%s", err, textOut)
	}
	if !strings.Contains(textOut, "pending fact review review:") {
		t.Fatalf("text recall omitted the pending-review line:\n%s", textOut)
	}
}

func TestRecallWarnsWhenProposalQueueUnreadable(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	fact := factRecord{ID: "fact:a", Text: "cache contract fact", Branch: "feature", Status: factStatusActive, UpdatedAt: now}
	if err := factstore.Write(storage.BrainDir, "feature", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	proposalPath := filepath.Join(storage.BrainDir, filepath.FromSlash(factsProposalsRelPath("feature")))
	if err := os.WriteFile(proposalPath, []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "recall", "cache contract", "--no-semantic", "--json")
	if err != nil {
		t.Fatalf("recall must degrade with a warning, not fail: %v\n%s", err, out)
	}
	var payload struct {
		Facts    []factRecord `json:"facts"`
		Warnings []string     `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode recall: %v\n%s", err, out)
	}
	if len(payload.Facts) != 1 {
		t.Fatalf("facts must still be returned: %+v", payload.Facts)
	}
	if len(payload.Warnings) != 1 || payload.Warnings[0] != retrieval.FactReviewQueueUnavailableWarning {
		t.Fatalf("unreadable queue must surface the verify warning: %v", payload.Warnings)
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
