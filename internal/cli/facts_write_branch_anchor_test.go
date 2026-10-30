package cli

import (
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/facts"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
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

	facts, err := factstore.Load(fixture.storage.BrainDir, "release")
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

	facts, err := factstore.Load(fixture.storage.BrainDir, "main")
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

// TestRememberStampsSessionID pins COR-2126's session dimension: --session
// wins, $ENTIRE_BRAIN_SESSION_ID is the fallback, and neither leaves the
// anchor's SessionID empty — valid for a human at a terminal.
func TestRememberStampsSessionID(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: entityFixtureCommitB + "\n"}
	t.Setenv(facts.RememberSessionEnv, "env-session-id")

	fixture.run(t, "remember", "--session", "flag-session-id", "--path", "architecture.data.flow", "Session flag wins over the environment.")
	fixture.run(t, "remember", "--path", "architecture.data.flow", "The environment supplies the session id.")

	t.Setenv(facts.RememberSessionEnv, "")
	fixture.run(t, "remember", "--path", "architecture.data.flow", "No session anywhere stays valid.")

	facts, err := factstore.Load(fixture.storage.BrainDir, "main")
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	bySession := map[string]string{}
	for _, fact := range facts {
		if len(fact.Provenance) != 1 {
			t.Fatalf("provenance = %+v", fact.Provenance)
		}
		bySession[fact.Text] = fact.Provenance[0].SessionID
	}
	want := map[string]string{
		"Session flag wins over the environment.":  "flag-session-id",
		"The environment supplies the session id.": "env-session-id",
		"No session anywhere stays valid.":         "",
	}
	for text, session := range want {
		if got, ok := bySession[text]; !ok || got != session {
			t.Fatalf("fact %q session = %q (present=%v), want %q", text, got, ok, session)
		}
	}
}
