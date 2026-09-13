package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitKnowsBranches teaches a fixture runner that these branch names resolve, so
// gitRefExists answers true for them and false for everything else — which is
// exactly the shape of a typo.
func gitKnowsBranches(f verifyFixture, branches ...string) {
	for _, branch := range branches {
		f.runner.responses[fakeCommandKey("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)] = fakeCommandResponse{}
	}
}

func promoteFixtureWithSourceFacts(t *testing.T) (verifyFixture, factRecord) {
	t.Helper()
	f := newVerifyFixture(t)
	gitKnowsBranches(f, "main", "feature", "empty")
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	fact := factRecord{
		ID: factRecordID("deploys use blue-green cutover", paths), Paths: paths,
		Text: "deploys use blue-green cutover", Branch: "feature", Origin: factOriginDistilled,
		Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "feature", []factRecord{fact})
	return f, fact
}

// TestFactsPromoteValidatesBothBranchEndpoints is the regression for a promote
// that checked --strategy, a missing --from and self-promotion but never once
// asked whether the branch names it was handed meant anything.
//
// Both halves failed silently, and the --into half destroyed data:
// writeFacts creates the target store on demand, so a typo forked the fact set
// into a directory keyed to a branch that does not exist. No branch can reach
// it, no command names it, and promote reported success — the facts were gone.
func TestFactsPromoteValidatesBothBranchEndpoints(t *testing.T) {
	t.Run("unknown --from is refused", func(t *testing.T) {
		f, _ := promoteFixtureWithSourceFacts(t)
		out, err := execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "no-such-branch", "--strategy", "keep-both")
		if err == nil {
			t.Fatalf("promoting from a branch that does not exist succeeded:\n%s", out)
		}
		if !strings.Contains(err.Error(), "--from") || !strings.Contains(err.Error(), "no-such-branch") {
			t.Fatalf("refusal should name --from and the branch, got: %v", err)
		}
	})

	t.Run("unknown --into is refused and strands no store", func(t *testing.T) {
		f, _ := promoteFixtureWithSourceFacts(t)
		const bogus = "totally/bogus/branch"

		out, err := execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "feature", "--into", bogus, "--strategy", "prefer-target")
		if err == nil {
			t.Fatalf("promoting into a branch that does not exist succeeded:\n%s", out)
		}
		if !strings.Contains(err.Error(), "--into") || !strings.Contains(err.Error(), bogus) {
			t.Fatalf("refusal should name --into and the branch, got: %v", err)
		}

		// The orphan store is the damage: it must never have been written.
		if factBranchStoreExists(f.storage.BrainDir, bogus) {
			t.Fatalf("refused promote still wrote a fact store for %q", bogus)
		}
		orphanDir := filepath.Join(f.storage.BrainDir, filepath.FromSlash(factsBranchRelDir(bogus)))
		if _, statErr := os.Stat(orphanDir); statErr == nil {
			t.Fatalf("refused promote still created the branch directory %s", orphanDir)
		}
	})

	t.Run("a real --from with no facts says so instead of reporting success", func(t *testing.T) {
		f, _ := promoteFixtureWithSourceFacts(t)
		out, err := execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "empty", "--strategy", "keep-both")
		if err == nil {
			t.Fatalf("promoting from a branch with no facts reported success:\n%s", out)
		}
		if !strings.Contains(err.Error(), "no facts to promote") {
			t.Fatalf("an empty source should be named as such, got: %v", err)
		}
	})

	t.Run("a valid promote still works", func(t *testing.T) {
		f, fact := promoteFixtureWithSourceFacts(t)
		out, err := execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "feature", "--strategy", "keep-both")
		if err != nil {
			t.Fatalf("valid promote was refused: %v\n%s", err, out)
		}
		if !strings.Contains(out, "promoted 1 fact(s) from feature into main") {
			t.Fatalf("unexpected promote summary:\n%s", out)
		}
		facts, loadErr := loadFacts(f.storage.BrainDir, "main")
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if indexOfFact(facts, fact.ID) < 0 {
			t.Fatalf("promoted fact did not land on main: %+v", facts)
		}
	})
}

// TestFactsPromoteAcceptsADeletedBranchThatStillHasFacts pins the reason the
// endpoint check is "git knows it OR the brain has facts for it" rather than
// git alone: carrying a merged-and-deleted feature branch's facts forward is a
// normal use of promote, and a git-only check would have broken it.
func TestFactsPromoteAcceptsADeletedBranchThatStillHasFacts(t *testing.T) {
	f := newVerifyFixture(t)
	gitKnowsBranches(f, "main") // "shipped" was deleted after its merge
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	fact := factRecord{
		ID: factRecordID("deploys use blue-green cutover", paths), Paths: paths,
		Text: "deploys use blue-green cutover", Branch: "shipped", Origin: factOriginDistilled,
		Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "shipped", []factRecord{fact})

	out, err := execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "shipped", "--strategy", "keep-both")
	if err != nil {
		t.Fatalf("promote from a deleted branch that still has facts was refused: %v\n%s", err, out)
	}
	facts, loadErr := loadFacts(f.storage.BrainDir, "main")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if indexOfFact(facts, fact.ID) < 0 {
		t.Fatalf("fact from the deleted branch did not land on main: %+v", facts)
	}
}

// TestFactsReviewRefusesContradictoryFlags is the regression for a resolver
// whose switch tested apply before reject: asking it to both apply and reject
// the same proposal settled the apply half, dropped the reject half without a
// word, and reported "resolved 1 proposal(s)". The user was told a decision
// landed while the opposite one actually did.
func TestFactsReviewRefusesContradictoryFlags(t *testing.T) {
	newFixture := func(t *testing.T) (verifyFixture, factProposal) {
		t.Helper()
		f := newVerifyFixture(t)
		paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
		target := factRecord{
			ID: factRecordID("deploys use blue-green cutover", paths), Paths: paths,
			Text: "deploys use blue-green cutover", Branch: "main", Origin: factOriginDistilled,
			Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now,
		}
		candidate := factRecord{
			ID: factRecordID("deploys use in-place rolling restart", paths), Paths: paths,
			Text: "deploys use in-place rolling restart", Branch: "main", Origin: factOriginDistilled,
			Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now,
		}
		f.writeFacts(t, "main", []factRecord{target, candidate})
		proposal := factProposal{
			Action: factActionSupersede, CandidateID: candidate.ID, TargetID: target.ID, Branch: "main",
		}
		if err := writeFactProposals(f.storage.BrainDir, "main", []factProposal{proposal}); err != nil {
			t.Fatal(err)
		}
		return f, proposal
	}

	// One proposal, and every way of asking for it to be settled two ways at once.
	contradictions := func(id string) map[string][]string {
		return map[string][]string{
			"apply and reject the same id": {"--apply", id, "--reject", id},
			"apply-all and reject-all":     {"--apply-all", "--reject-all"},
			"apply-all and reject an id":   {"--apply-all", "--reject", id},
			"reject-all and apply an id":   {"--reject-all", "--apply", id},
		}
	}

	f, proposal := newFixture(t)
	for name, flags := range contradictions(proposal.CandidateID) {
		t.Run(name, func(t *testing.T) {
			f, proposal := newFixture(t)
			args := append([]string{"facts", "review"}, flags...)
			out, err := execute(t, NewRootCommand(f.opts), args...)
			if err == nil {
				t.Fatalf("contradictory flags were accepted:\n%s", out)
			}

			// A refusal must be total: the queue and the facts stay as they were,
			// so the caller can re-run with the half they meant.
			queued, qErr := loadFactProposals(f.storage.BrainDir, "main")
			if qErr != nil {
				t.Fatal(qErr)
			}
			if len(queued) != 1 || queued[0].CandidateID != proposal.CandidateID {
				t.Fatalf("refused review still settled the queue: %+v", queued)
			}
			facts, loadErr := loadFacts(f.storage.BrainDir, "main")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			for _, fact := range facts {
				if fact.Status != factStatusActive {
					t.Fatalf("refused review still mutated facts: %+v", fact)
				}
			}
		})
	}

	// The control: one side alone still settles, so the refusal is about the
	// contradiction and not about the flags themselves.
	out, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--apply", proposal.CandidateID)
	if err != nil {
		t.Fatalf("a single --apply should still work: %v\n%s", err, out)
	}
	if !strings.Contains(out, "resolved 1 proposal(s)") {
		t.Fatalf("unexpected review summary:\n%s", out)
	}
}
