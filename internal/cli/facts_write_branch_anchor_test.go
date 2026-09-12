package cli

import (
	"testing"
)

// releaseBranchTip is a commit that exists only on `release`.
const releaseBranchTip = "cccc333333333333333333333333333333333333"

// TestRememberAnchorsToTheBranchItFilesTheFactOn pins the branch dimension of
// an AUTHORED fact's anchor.
//
// `remember --branch <other>` files the fact on another branch without moving
// the working tree. The anchor commit was taken from `rev-parse HEAD` — the
// CURRENT branch's tip — so the stored fact reads "on release, evidenced by
// <a commit that is only on main>". `verify` cannot catch it: verifyCommitUncached
// accepts reachability from ANY local ref, so the fabricated pairing is
// reported as verified forever.
func TestRememberAnchorsToTheBranchItFilesTheFactOn(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	// The working tree is on `main`, whose tip is commit B.
	fixture.runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: entityFixtureCommitB + "\n"}
	// `release` is a real local branch pointing somewhere else entirely.
	fixture.runner.responses[fakeCommandKey("git", "rev-parse", "--verify", "release^{commit}")] = fakeCommandResponse{stdout: releaseBranchTip + "\n"}

	fixture.run(t, "remember", "--branch", "release", "--path", "architecture.data.flow", "The release branch pins the v1 payload schema.")

	facts, err := loadFacts(fixture.storage.BrainDir, "release")
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if len(facts) != 1 || len(facts[0].Provenance) != 1 {
		t.Fatalf("stored facts = %+v", facts)
	}
	got := facts[0].Provenance[0].Commit
	if got == entityFixtureCommitB {
		t.Fatalf("a fact filed on `release` cites %s, the tip of the CURRENT branch (main)", got)
	}
	if got != releaseBranchTip {
		t.Fatalf("anchor commit = %q, want the tip of `release` (%s)", got, releaseBranchTip)
	}
}

// TestRememberKeepsHeadWhenNoBranchOverrideIsGiven pins the unchanged default:
// with no --branch the branch was derived FROM HEAD, so HEAD stays the honest
// evidence — including on a detached HEAD, where no branch exists to resolve.
func TestRememberKeepsHeadWhenNoBranchOverrideIsGiven(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: entityFixtureCommitB + "\n"}

	fixture.run(t, "remember", "--path", "architecture.data.flow", "The payload schema is versioned.")

	facts, err := loadFacts(fixture.storage.BrainDir, "main")
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	if len(facts) != 1 || len(facts[0].Provenance) != 1 {
		t.Fatalf("stored facts = %+v", facts)
	}
	if got := facts[0].Provenance[0].Commit; got != entityFixtureCommitB {
		t.Fatalf("anchor commit = %q, want HEAD (%s)", got, entityFixtureCommitB)
	}
}
