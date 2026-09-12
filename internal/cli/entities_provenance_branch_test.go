package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// entityFixtureRevListKey is the reachability walk the entity index already
// runs for `entities history` (branchReachableCommits).
func entityFixtureRevListKey(branch string) string {
	return fakeCommandKey("git", "rev-list", fmt.Sprintf("--max-count=%d", entityBranchRevWalkMax), branch)
}

// TestEntityProvenanceRefusesACommitThatIsNotOnTheFactsBranch pins the branch
// dimension of an anchor.
//
// The reverse index is PROJECT-scoped: `brain:entity:<key>` lists every commit
// that ever touched an entity, on every branch, plus commits a rebase or a
// force-push has since orphaned. Rewriting history (rebase, amend, cherry-pick
// onto another branch) duplicates a checkpoint's `Entire-Checkpoint:` trailer
// onto a SECOND commit, so one checkpoint id resolves to two commits and the
// newer committer date wins.
//
// Nothing in the resolver checks that the commit it picks is on the branch the
// fact was distilled on, so a fact on `main` is silently anchored to a commit
// `git log main` will never show. When that commit is a live copy on another
// branch, `verify` passes it: verifyCommitUncached (verify_cmd.go:392) accepts
// reachability from ANY local ref, so nothing downstream ever objects.
func TestEntityProvenanceRefusesACommitThatIsNotOnTheFactsBranch(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)
	fixture.writeLocalCheckpoints(t)
	fixture.run(t, "entities", "backfill", "--json")

	// Commit B is not on `main`: it lives on a feature branch, or it is the
	// pre-rewrite copy a rebase left behind. The object still exists and still
	// carries the session's checkpoint trailer, so the index still lists it.
	fixture.runner.responses[entityFixtureRevListKey("main")] = fakeCommandResponse{stdout: entityFixtureCommitA + "\n"}

	resolver := newEntityProvenanceResolver(context.Background(), fixture.opts, fixture.repoDir)
	if resolver == nil {
		t.Fatal("resolver is nil despite a populated index")
	}

	// Distill's coarse anchor: the session's latest checkpoint and NO commit.
	fallback := factAnchor{SessionID: entityFixtureSession, CheckpointID: entityFixtureCkptB, Transcript: "sessions/s.jsonl", Line: 1}

	// RefundCard was only ever touched by commit B — the commit that is not on
	// `main`. The honest answer is the coarse anchor, not a commit off-branch.
	records := []factRecord{{
		Text:       "The `RefundCard` helper must never be called twice for one charge.",
		Branch:     "main",
		Locus:      []string{"refundcard"},
		Provenance: []factAnchor{fallback},
	}}
	got := applyEntityProvenance(records, resolver)[0].Provenance[0]
	if got.Commit == entityFixtureCommitB {
		t.Fatalf("a fact on main was anchored to %s, a commit that is not reachable from main", got.Commit)
	}
	if got != fallback {
		t.Fatalf("anchor = %+v, want the untouched fallback %+v", got, fallback)
	}

	// Control: an entity whose commit IS on the branch must still sharpen, so
	// the guard cannot pass by disabling the feature.
	records = []factRecord{{
		Text:       "`AuthorizeCard` must run before a charge is captured.",
		Branch:     "main",
		Locus:      []string{"authorizecard"},
		Provenance: []factAnchor{fallback},
	}}
	if got := applyEntityProvenance(records, resolver)[0].Provenance[0]; got.CheckpointID != entityFixtureCkptA || got.Commit != entityFixtureCommitA {
		t.Fatalf("an on-branch anchor stopped sharpening: %+v", got)
	}
}

// TestEntityProvenanceKeepsSharpeningWhenReachabilityIsUnknown pins the safe
// direction of the guard: a branch git cannot walk (deleted locally, a detached
// export, a repository the walk bound cannot cover) must NOT silently downgrade
// every fact in the run to the coarse anchor.
func TestEntityProvenanceKeepsSharpeningWhenReachabilityIsUnknown(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)
	fixture.writeLocalCheckpoints(t)
	fixture.run(t, "entities", "backfill", "--json")

	// `git rev-list main` fails: reachability is unknown, not "empty".
	fixture.runner.responses[entityFixtureRevListKey("main")] = fakeCommandResponse{err: errors.New("unknown revision")}

	resolver := newEntityProvenanceResolver(context.Background(), fixture.opts, fixture.repoDir)
	if resolver == nil {
		t.Fatal("resolver is nil despite a populated index")
	}
	fallback := factAnchor{SessionID: entityFixtureSession, CheckpointID: entityFixtureCkptB}
	records := []factRecord{{
		Text:       "`AuthorizeCard` must run before a charge is captured.",
		Branch:     "main",
		Locus:      []string{"authorizecard"},
		Provenance: []factAnchor{fallback},
	}}
	if got := applyEntityProvenance(records, resolver)[0].Provenance[0]; got.CheckpointID != entityFixtureCkptA || got.Commit != entityFixtureCommitA {
		t.Fatalf("unknown reachability disabled sharpening: %+v", got)
	}
}

// TestEntityProvenanceTreatsATruncatedReachabilityWalkAsUnknown pins the
// deep-repository case. branchReachableCommits is bounded, so on a history
// longer than the bound it returns the branch's NEWEST commits, not its
// commits. Reading that as the reachability set would call every older indexed
// commit off-branch and silently stop sharpening the older half of the
// repository — a bounded walk is unknown reachability, not an empty one.
func TestEntityProvenanceTreatsATruncatedReachabilityWalkAsUnknown(t *testing.T) {
	fixture := newEntityIndexFixture(t)
	fixture.writeSessionManifest(t)
	fixture.writeLocalCheckpoints(t)
	fixture.run(t, "entities", "backfill", "--json")

	// A full walk: exactly the bound's worth of commits, none of them the
	// fixture's — i.e. the answer got clipped before reaching them.
	var walk strings.Builder
	for i := 0; i < entityBranchRevWalkMax; i++ {
		walk.WriteString(fmt.Sprintf("%040x\n", i+1))
	}
	fixture.runner.responses[entityFixtureRevListKey("main")] = fakeCommandResponse{stdout: walk.String()}

	resolver := newEntityProvenanceResolver(context.Background(), fixture.opts, fixture.repoDir)
	if resolver == nil {
		t.Fatal("resolver is nil despite a populated index")
	}
	fallback := factAnchor{SessionID: entityFixtureSession, CheckpointID: entityFixtureCkptB}
	records := []factRecord{{
		Text:       "`AuthorizeCard` must run before a charge is captured.",
		Branch:     "main",
		Locus:      []string{"authorizecard"},
		Provenance: []factAnchor{fallback},
	}}
	if got := applyEntityProvenance(records, resolver)[0].Provenance[0]; got.CheckpointID != entityFixtureCkptA || got.Commit != entityFixtureCommitA {
		t.Fatalf("a truncated walk disabled sharpening: %+v", got)
	}
}
