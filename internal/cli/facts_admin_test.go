package cli

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

func TestApplyProposalMerge(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	target := factFor(t, "target fact", []string{"project.tooling.stack"}, now)
	target.Provenance = []factAnchor{{SessionID: "s1"}}
	cand := factFor(t, "candidate fact", []string{"project.tooling.stack"}, now)
	cand.Provenance = []factAnchor{{SessionID: "s2"}}
	cand.RelatedIDs = []string{target.ID}
	target.RelatedIDs = []string{cand.ID}

	facts := []factRecord{target, cand}
	p := factProposal{Action: factActionMerge, CandidateID: cand.ID, TargetID: target.ID}
	out, err := applyProposal(facts, p, now)
	if err != nil {
		t.Fatalf("applyProposal: %v", err)
	}
	if len(out) != 1 || out[0].ID != target.ID {
		t.Fatalf("merge should leave only the target, got %+v", out)
	}
	if len(out[0].Provenance) != 2 {
		t.Fatalf("merge should union provenance, got %d", len(out[0].Provenance))
	}
	if contains(out[0].RelatedIDs, cand.ID) {
		t.Fatalf("conflict link should be cleared")
	}
}

func TestApplyProposalSupersede(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	target := factFor(t, "old", []string{"project.tooling.stack"}, now)
	cand := factFor(t, "new", []string{"project.tooling.stack"}, now)
	out, err := applyProposal([]factRecord{target, cand}, factProposal{Action: factActionSupersede, CandidateID: cand.ID, TargetID: target.ID}, now)
	if err != nil {
		t.Fatalf("applyProposal: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("supersede retains both, got %d", len(out))
	}
	ti := indexOfFact(out, target.ID)
	if out[ti].Status != factStatusSuperseded || out[ti].SupersededBy != cand.ID {
		t.Fatalf("target not superseded: %+v", out[ti])
	}
}

func TestApplyProposalSupersedeReactivatesHistoricalCandidate(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	candidate := factFor(t, "old truth reasserted", []string{"project.tooling.stack"}, now)
	target := factFor(t, "current truth", []string{"project.tooling.stack"}, now.Add(time.Hour))
	candidate.Status = factStatusRetracted
	candidate.SupersededBy = target.ID

	out, err := applyProposal(
		[]factRecord{candidate, target},
		factProposal{Action: factActionSupersede, CandidateID: candidate.ID, TargetID: target.ID},
		now.Add(2*time.Hour),
	)
	if err != nil {
		t.Fatalf("applyProposal: %v", err)
	}
	ci := indexOfFact(out, candidate.ID)
	ti := indexOfFact(out, target.ID)
	if ci < 0 || out[ci].Status != factStatusActive || out[ci].SupersededBy != "" {
		t.Fatalf("review path did not reactivate the approved candidate: %+v", out)
	}
	if ti < 0 || out[ti].Status != factStatusSuperseded || out[ti].SupersededBy != candidate.ID {
		t.Fatalf("review path did not supersede the target: %+v", out)
	}
}

func TestApplyProposalStale(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	cand := factFor(t, "c", []string{"project.tooling.stack"}, now)
	if _, err := applyProposal([]factRecord{cand}, factProposal{Action: factActionMerge, CandidateID: cand.ID, TargetID: "fact:gone"}, now); err == nil {
		t.Fatalf("expected stale-proposal error")
	}
}

func TestFactsReviewInvalidSelfTargetPreservesState(t *testing.T) {
	for _, action := range []string{factActionMerge, factActionSupersede} {
		t.Run(action, func(t *testing.T) {
			f := newVerifyFixture(t)
			fact := factFor(t, "only fact", []string{"project.tooling.stack"}, f.now)
			facts := []factRecord{fact}
			proposals := []factProposal{{
				Action:      action,
				CandidateID: fact.ID,
				TargetID:    fact.ID,
				Branch:      "main",
			}}
			if err := writeFacts(f.brainDir, "main", facts); err != nil {
				t.Fatalf("write facts: %v", err)
			}
			if err := writeFactProposals(f.brainDir, "main", proposals); err != nil {
				t.Fatalf("write proposals: %v", err)
			}

			_, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--apply", fact.ID)
			if !errors.Is(err, factmerge.ErrInvalidProposal) {
				t.Fatalf("facts review error = %v, want ErrInvalidProposal", err)
			}
			gotFacts, err := loadFacts(f.brainDir, "main")
			if err != nil {
				t.Fatalf("load facts: %v", err)
			}
			gotProposals, err := loadFactProposals(f.brainDir, "main")
			if err != nil {
				t.Fatalf("load proposals: %v", err)
			}
			if !reflect.DeepEqual(gotFacts, facts) || !reflect.DeepEqual(gotProposals, proposals) {
				t.Fatalf("invalid review changed state: facts=%+v proposals=%+v", gotFacts, gotProposals)
			}
		})
	}
}

func TestFactsReviewInvalidActionPreservesState(t *testing.T) {
	f := newVerifyFixture(t)
	target := factFor(t, "target fact", []string{"project.tooling.stack"}, f.now)
	candidate := factFor(t, "candidate fact", []string{"project.tooling.stack"}, f.now)
	target.RelatedIDs = []string{candidate.ID}
	candidate.RelatedIDs = []string{target.ID}
	facts := []factRecord{target, candidate}
	proposals := []factProposal{{
		Action:      "merg",
		CandidateID: candidate.ID,
		TargetID:    target.ID,
		Branch:      "main",
	}}
	if err := writeFacts(f.brainDir, "main", facts); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := writeFactProposals(f.brainDir, "main", proposals); err != nil {
		t.Fatalf("write proposals: %v", err)
	}

	_, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--apply", candidate.ID)
	if !errors.Is(err, factmerge.ErrInvalidProposal) {
		t.Fatalf("facts review error = %v, want ErrInvalidProposal", err)
	}
	gotFacts, err := loadFacts(f.brainDir, "main")
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	gotProposals, err := loadFactProposals(f.brainDir, "main")
	if err != nil {
		t.Fatalf("load proposals: %v", err)
	}
	if !reflect.DeepEqual(gotFacts, facts) || !reflect.DeepEqual(gotProposals, proposals) {
		t.Fatalf("invalid review changed state: facts=%+v proposals=%+v", gotFacts, gotProposals)
	}
}

func TestRejectProposalKeepsBothActive(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "a", []string{"project.tooling.stack"}, now)
	b := factFor(t, "b", []string{"project.tooling.stack"}, now)
	a.RelatedIDs = []string{b.ID}
	b.RelatedIDs = []string{a.ID}
	out := rejectProposal([]factRecord{a, b}, factProposal{Action: factActionSupersede, CandidateID: b.ID, TargetID: a.ID})
	if len(out) != 2 {
		t.Fatalf("reject keeps both, got %d", len(out))
	}
	for _, f := range out {
		if f.Status != factStatusActive {
			t.Errorf("reject should not change status: %+v", f)
		}
		if len(f.RelatedIDs) != 0 {
			t.Errorf("conflict link should be cleared: %+v", f.RelatedIDs)
		}
	}
}

func TestPromoteFactsStrategies(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// Source and target each have a different fact at the same path (conflict),
	// plus the source has a non-conflicting fact at another path.
	srcConflict := factFor(t, "source: use MySQL", []string{"project.tooling.stack"}, now)
	srcConflict.Branch = "feature"
	srcUnique := factFor(t, "source: prefers tabs", []string{"preferences.coding.style"}, now)
	srcUnique.Branch = "feature"
	tgtConflict := factFor(t, "target: use Postgres", []string{"project.tooling.stack"}, now)
	tgtConflict.Branch = "main"

	source := []factRecord{srcConflict, srcUnique}
	target := []factRecord{tgtConflict}

	t.Run("keep-both", func(t *testing.T) {
		merged, proposals, promoted := promoteFacts(source, target, "keep-both", "main", now)
		if promoted != 2 {
			t.Fatalf("expected 2 promoted, got %d", promoted)
		}
		if len(proposals) != 1 {
			t.Fatalf("conflict should queue 1 proposal, got %d", len(proposals))
		}
		for _, f := range merged {
			if f.ID == tgtConflict.ID && f.Status != factStatusActive {
				t.Errorf("keep-both should keep target active")
			}
		}
		// Promoted facts must be re-stamped onto the target branch.
		for _, f := range merged {
			if f.Branch != "main" {
				t.Errorf("promoted fact not re-stamped to main: %+v", f)
			}
		}
	})

	t.Run("prefer-source", func(t *testing.T) {
		merged, proposals, _ := promoteFacts(source, target, "prefer-source", "main", now)
		if len(proposals) != 0 {
			t.Fatalf("prefer-source should not queue proposals")
		}
		ti := indexOfFact(merged, tgtConflict.ID)
		if merged[ti].Status != factStatusSuperseded {
			t.Fatalf("prefer-source should supersede the conflicting target fact")
		}
	})

	t.Run("prefer-target", func(t *testing.T) {
		merged, _, promoted := promoteFacts(source, target, "prefer-target", "main", now)
		// The conflicting source fact is dropped; only the unique one promotes.
		if promoted != 1 {
			t.Fatalf("prefer-target should promote only the non-conflicting fact, got %d", promoted)
		}
		if indexOfFact(merged, srcConflict.ID) >= 0 {
			t.Fatalf("conflicting source fact should be dropped under prefer-target")
		}
	})
}

func TestPromoteFactsIdenticalUnionsProvenance(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	f := factFor(t, "shared fact", []string{"project.tooling.stack"}, now)
	src := f
	src.Branch = "feature"
	src.Provenance = []factAnchor{{SessionID: "s2"}}
	tgt := f
	tgt.Branch = "main"
	tgt.Provenance = []factAnchor{{SessionID: "s1"}}

	merged, proposals, promoted := promoteFacts([]factRecord{src}, []factRecord{tgt}, "keep-both", "main", now)
	if promoted != 0 || len(proposals) != 0 {
		t.Fatalf("identical fact should not be promoted as new or conflict")
	}
	if len(merged) != 1 || len(merged[0].Provenance) != 2 {
		t.Fatalf("identical fact should union provenance, got %+v", merged)
	}
}

func TestRetractFact(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "active fact", []string{"project.tooling.stack"}, now)
	facts := []factRecord{a}

	found, changed := retractFact(facts, a.ID, now.Add(time.Hour))
	if !found || !changed {
		t.Fatalf("expected found+changed, got found=%v changed=%v", found, changed)
	}
	if facts[0].Status != factStatusRetracted || !facts[0].UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("fact not retracted/stamped: %+v", facts[0])
	}
	// Retracting again is a no-op (found, not changed).
	found, changed = retractFact(facts, a.ID, now.Add(2*time.Hour))
	if !found || changed {
		t.Fatalf("re-retract should be found-but-unchanged, got found=%v changed=%v", found, changed)
	}
	// Unknown id.
	if found, _ := retractFact(facts, "fact:nope", now); found {
		t.Fatalf("unknown id should not be found")
	}
	// A retracted fact drops out of active recall and gc prunes it.
	if got := rankFacts(facts, "", 10, false); len(got) != 0 {
		t.Fatalf("retracted fact should not surface in active recall")
	}
	res := gcFacts(facts, defaultFactTaxonomy(now), now, defaultFactRetention)
	if len(res.Pruned) != 1 || len(res.Kept) != 0 {
		t.Fatalf("gc should prune the retracted fact: %+v", res)
	}
}

func TestGCFacts(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	active := factFor(t, "active", []string{"project.tooling.stack"}, now)
	retracted := factFor(t, "retracted", []string{"project.tooling.stack"}, now)
	retracted.Status = factStatusRetracted
	oldSuperseded := factFor(t, "old superseded", []string{"project.tooling.stack"}, now.Add(-60*24*time.Hour))
	oldSuperseded.Status = factStatusSuperseded
	recentSuperseded := factFor(t, "recent superseded", []string{"project.tooling.stack"}, now.Add(-1*24*time.Hour))
	recentSuperseded.Status = factStatusSuperseded
	orphan := factFor(t, "orphan", []string{"project.tooling.stack"}, now)
	orphan.Paths = []string{"gone.sub.type"} // top-level not in taxonomy

	tax := defaultFactTaxonomy(now)
	result := gcFacts([]factRecord{active, retracted, oldSuperseded, recentSuperseded, orphan}, tax, now, defaultFactRetention)

	if len(result.Pruned) != 2 {
		t.Fatalf("expected retracted + old-superseded pruned (2), got %d", len(result.Pruned))
	}
	if len(result.Orphans) != 1 || result.Orphans[0].Text != "orphan" {
		t.Fatalf("expected 1 reported orphan, got %d", len(result.Orphans))
	}
	// Orphan is active and not old → kept (orphans are never auto-deleted).
	if indexOfFact(result.Kept, orphan.ID) < 0 {
		t.Fatalf("active orphan should be kept, not pruned")
	}
	if indexOfFact(result.Kept, recentSuperseded.ID) < 0 {
		t.Fatalf("recent superseded should be retained within the window")
	}
}
