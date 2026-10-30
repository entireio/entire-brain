package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestStagedReconcilePreservesDistinctClaims(t *testing.T) {
	cases := []struct{ name, candidate, target string }{
		{"recorded_manifest", "Seed and export write to a shared brain manifest and README through a unified atomic writer (schema 2, Phase 1) to prevent last-writer-wins data loss. Seed owns `seed/*`, export owns `sessions/*`. The writer must deduplicate warnings from merged sources to avoid stale warnings accumulating on subsequent runs.", "Manifest uses a multi-source structure with separate seed and sessions sections, allowing deterministic repo analysis and real Entire session logs to coexist independently. Export cleanup removes only stale session files, never deletes seed/ or the seed manifest section. Brain README describes both sources and the time gap between oldest commit and oldest session, ensuring baseline context persists as session history grows."},
		{"overlap_is_not_entailment", "All writes use atomic rename; retry failed writes with exponential backoff.", "All writes use atomic rename."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
			target := stagedTestFact(tc.target, []string{"architecture.boundaries.rationale"}, factStatusActive, now.Add(-time.Hour))
			candidate := stagedTestFact(tc.candidate, target.Paths, factStatusCandidate, now)
			run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
				return "1 merge 1 1.0", nil
			}
			got, proposals, _, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{target, candidate}, nil, ".", time.Minute, 4, now)
			ci, ti := indexOfFact(got, candidate.ID), indexOfFact(got, target.ID)
			if ci < 0 || ti < 0 || got[ci].Text != tc.candidate || got[ci].Status != factStatusCandidate || got[ti].Status != factStatusActive || len(proposals) != 1 {
				t.Fatalf("must preserve original and unpublished candidate for review: facts=%+v proposals=%+v", got, proposals)
			}
			noCall := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
				t.Error("pending review was re-adjudicated")
				return "1 new", nil
			}
			resumed, _, stats, _ := reconcileStagedCandidates(context.Background(), noCall, nil, got, proposals, ".", time.Minute, 4, now)
			if stats.Candidates != 0 || resumed[indexOfFact(resumed, candidate.ID)].Status != factStatusCandidate {
				t.Fatalf("resume published pending review: %+v", resumed)
			}
		})
	}
}

func TestStagedReconcileMalformedStaysUnpublished(t *testing.T) {
	for _, output := range []string{"", "1 unknown", "1 merge 99 1", "1 merge", "1 new garbage", "1 new - NaN", "1 merge 1 1\n1 new", "1 new\n1 merge 1 1"} {
		t.Run(output, func(t *testing.T) {
			now := time.Now().UTC()
			target := stagedTestFact("All writes use atomic rename.", []string{"workflow.testing.rules"}, factStatusActive, now)
			candidate := stagedTestFact("All writes use atomic rename and retry failures.", target.Paths, factStatusCandidate, now)
			run := func(context.Context, string, []string, []byte, time.Duration) (string, error) { return output, nil }
			got, _, _, warnings := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{target, candidate}, nil, ".", time.Minute, 2, now)
			if len(got) != 2 || got[indexOfFact(got, candidate.ID)].Status != factStatusCandidate || len(warnings) == 0 {
				t.Fatalf("malformed result published candidate: %+v %v", got, warnings)
			}
		})
	}
}

func TestStagedReviewResolution(t *testing.T) {
	for _, kind := range []string{factActionMerge, factActionSupersede} {
		for _, accept := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/accept=%v", kind, accept), func(t *testing.T) {
				now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
				dir := writeSingleSessionFixture(t, now, "some transcript\n")
				target := stagedTestFact("All writes use atomic rename.", []string{"workflow.testing.rules"}, factStatusActive, now)
				target.Provenance = []factAnchor{{SessionID: "target"}}
				candidate := stagedTestFact("All writes use atomic rename and retry failures.", target.Paths, factStatusCandidate, now)
				candidate.Provenance = []factAnchor{{SessionID: "candidate"}}
				p := factProposal{Action: kind, CandidateID: candidate.ID, TargetID: target.ID, Branch: "main", Confidence: 1}
				if err := factstore.Write(dir, "main", []factRecord{target, candidate}); err != nil {
					t.Fatal(err)
				}
				if err := writeFactProposals(dir, "main", []factProposal{p}); err != nil {
					t.Fatal(err)
				}
				cmd := &cobra.Command{}
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				opts := Options{Now: func() time.Time { return now }}
				if err := runFactsReview(cmd, opts, dir, "main", factsReviewActions{}); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), candidate.Text) || !strings.Contains(out.String(), "candidate (candidate)") {
					t.Fatalf("review hides full pending text: %s", out.String())
				}
				act := factsReviewActions{reject: candidate.ID}
				if accept {
					act = factsReviewActions{apply: candidate.ID}
				}
				if err := runFactsReview(cmd, opts, dir, "main", act); err != nil {
					t.Fatal(err)
				}
				got, err := factstore.Load(dir, "main")
				if err != nil {
					t.Fatal(err)
				}
				queue, err := loadFactProposals(dir, "main")
				if err != nil || len(queue) != 0 {
					t.Fatalf("queue not resolved: %+v %v", queue, err)
				}
				if accept && kind == factActionMerge {
					if len(got) != 1 || got[0].ID != target.ID || len(got[0].Provenance) != 2 {
						t.Fatalf("merge review: %+v", got)
					}
					return
				}
				if len(got) != 2 || got[indexOfFact(got, candidate.ID)].Status != factStatusActive {
					t.Fatalf("review did not publish candidate: %+v", got)
				}
				want := factStatusActive
				if accept {
					want = factStatusSuperseded
				}
				if got[indexOfFact(got, target.ID)].Status != want {
					t.Fatalf("target status: %+v", got)
				}
			})
		}
	}
}

func TestStagedReviewCannotPublishBeforeTargetResolved(t *testing.T) {
	now := time.Now().UTC()
	target := stagedTestFact("target", []string{"workflow.testing.rules"}, factStatusCandidate, now)
	candidate := stagedTestFact("candidate", target.Paths, factStatusCandidate, now)
	got, err := applyDistillReviewProposal([]factRecord{target, candidate}, factProposal{Action: factActionMerge, CandidateID: candidate.ID, TargetID: target.ID}, now)
	if err == nil || len(got) != 2 || got[1].Status != factStatusCandidate {
		t.Fatalf("unresolved target published candidate: %+v, %v", got, err)
	}
	dir := writeSingleSessionFixture(t, now, "some transcript\n")
	p := factProposal{Action: factActionMerge, CandidateID: candidate.ID, TargetID: target.ID, Branch: "main"}
	if err := factstore.Write(dir, "main", []factRecord{target, candidate}); err != nil {
		t.Fatal(err)
	}
	if err := writeFactProposals(dir, "main", []factProposal{p}); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := runFactsReview(cmd, Options{Now: func() time.Time { return now }}, dir, "main", factsReviewActions{apply: candidate.ID}); err != nil {
		t.Fatal(err)
	}
	queue, err := loadFactProposals(dir, "main")
	if err != nil || len(queue) != 1 || !strings.Contains(out.String(), "1 remaining") {
		t.Fatalf("deferred review lost queue or misreported it: %+v %v %s", queue, err, out.String())
	}
}

func TestStagedReconcileForcePreservesPendingReview(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	dir := writeSingleSessionFixture(t, now, "some transcript\n")
	target := stagedTestFact("All writes use atomic rename.", []string{"workflow.testing.rules"}, factStatusActive, now)
	candidate := stagedTestFact("All writes use atomic rename and retry failures.", target.Paths, factStatusCandidate, now)
	p := factProposal{Action: factActionMerge, CandidateID: candidate.ID, TargetID: target.ID, Branch: "main"}
	if err := factstore.Write(dir, "main", []factRecord{target, candidate}); err != nil {
		t.Fatal(err)
	}
	if err := writeFactProposals(dir, "main", []factProposal{p}); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, _ string, _ []string, input []byte, _ time.Duration) (string, error) {
		if strings.HasPrefix(string(input), "CANDIDATES\n") {
			t.Error("pending review sent to model")
		}
		return "", nil
	}
	_, err := runDistillForBrain(context.Background(), t.TempDir(), dir, distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, force: true, timeout: time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := factstore.Load(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	queue, err := loadFactProposals(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(queue) != 1 || got[indexOfFact(got, candidate.ID)].Status != factStatusCandidate || got[indexOfFact(got, target.ID)].Status != factStatusActive {
		t.Fatalf("force lost pending review: %+v %+v", got, queue)
	}
}

func TestStagedForceResumeKeepsCacheBeforeExtraction(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	dir := writeSingleSessionFixture(t, now, "some transcript\n")
	leftover := stagedTestFact("leftover standalone fact", []string{"workflow.testing.rules"}, factStatusCandidate, now)
	if err := factstore.Write(dir, "main", []factRecord{leftover}); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillCache(dir, distillCache{Version: distillCacheVersion, Sessions: map[string]string{"unchanged-session": "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		if cache := loadDistillCache(dir); cache.Sessions["unchanged-session"] != "fingerprint" {
			t.Error("resume flush erased cache before force extraction")
		}
		return "", fmt.Errorf("extraction interrupted")
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), dir, distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, force: true, timeout: time.Minute}, now); err == nil {
		t.Fatal("expected interrupted extraction")
	}
	if cache := loadDistillCache(dir); cache.Sessions["unchanged-session"] != "fingerprint" {
		t.Fatal("interrupted resume lost unchanged session cache")
	}
}

func TestStagedExactIdentityPreservesStatusAndProvenance(t *testing.T) {
	for _, status := range []string{factStatusActive, factStatusRetracted, factStatusSuperseded} {
		t.Run(status, func(t *testing.T) {
			now := time.Now().UTC()
			original := stagedTestFact("All writes use atomic rename.", []string{"workflow.testing.rules"}, status, now)
			original.Provenance = []factAnchor{{SessionID: "original"}}
			duplicate := original
			duplicate.Status = factStatusCandidate
			duplicate.Provenance = []factAnchor{{SessionID: "duplicate"}}
			staged := upsertFact([]factRecord{original}, duplicate)
			run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
				t.Error("exact identity should not call the model")
				return "", nil
			}
			got, proposals, _, _ := reconcileStagedCandidates(context.Background(), run, nil, staged, nil, ".", time.Minute, 2, now)
			if len(got) != 1 || got[0].Status != status || len(got[0].Provenance) != 2 || len(proposals) != 0 {
				t.Fatalf("identity upsert changed status or lost provenance: %+v", got)
			}
		})
	}
}
