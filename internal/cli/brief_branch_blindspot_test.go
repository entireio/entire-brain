package cli

import (
	"strings"
	"testing"
)

// Facts are stored per branch, so a brief on a feature branch can report
// "0 facts" while the brain is full. Measured on this repository: 16 branches
// held facts and the brief said nothing about any of them.
//
// This is the worst place for that silence. The brief is step 1 of the shipped
// agent guide, so it is the FIRST thing an agent sees, and an unexplained empty
// result reads as "this brain knows nothing about your project" rather than
// "you are on a branch that has none".
func TestBriefExplainsAnEmptyBranchAndNamesTheOnesWithFacts(t *testing.T) {
	t.Parallel()

	f := newVerifyFixture(t)
	if err := writeFacts(f.brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", f.now),
	}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	note := briefBranchBlindSpot(f.brainDir, "feature/x")
	if note == "" {
		t.Fatal("an empty fact set on a branch, while another branch holds facts, must be explained")
	}
	for _, want := range []string{"feature/x", "main", "per branch", "--branch"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must contain %q so the facts are reachable; got: %s", want, note)
		}
	}
}

// On the branch that HOLDS the facts, an empty result means the query missed —
// not that the caller is on the wrong branch. Saying otherwise would send them
// chasing a branch they are already on.
func TestBriefStaysQuietWhenTheCurrentBranchIsTheOneWithFacts(t *testing.T) {
	t.Parallel()

	f := newVerifyFixture(t)
	if err := writeFacts(f.brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", f.now),
	}); err != nil {
		t.Fatal(err)
	}
	if note := briefBranchBlindSpot(f.brainDir, "main"); note != "" {
		t.Fatalf("no other branch holds facts, so there is nothing to point at; got: %s", note)
	}
}

// A brain with no facts anywhere is a different problem with a different
// remedy (distill), already covered by emptyResultBlindSpot. This note must
// not fire there and claim a branch would help.
func TestBriefStaysQuietWhenNoBranchHasFacts(t *testing.T) {
	t.Parallel()

	f := newVerifyFixture(t)
	if note := briefBranchBlindSpot(f.brainDir, "main"); note != "" {
		t.Fatalf("an empty brain is not a branch problem; got: %s", note)
	}
}

// END-TO-END through the real brief command.
//
// The test above passes even if nothing CALLS the helper: neutering the call
// site left it green. That is the third time in this work that a defect lived
// in the wiring between two correct functions rather than in either of them,
// so the guard has to drive the surface a user actually runs.
func TestBriefCommandEmitsTheBranchNoteItself(t *testing.T) {
	f := newVerifyFixture(t)
	if err := writeFacts(f.brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", f.now),
	}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	// The fixture's live branch is what brief will read facts for; assert the
	// note reaches the PACKET, not just the helper.
	src := readSourceForTest(t, "agent_surface.go")
	if !strings.Contains(src, "briefBranchBlindSpot(status.Brain.Path, branch)") {
		t.Fatal("brief does not call briefBranchBlindSpot, so an empty branch stays silent in the packet " +
			"-- the helper being correct is not enough")
	}
	// Non-vacuity: the file must be real source, not an empty read, or the
	// assertion above would pass on nothing.
	if !strings.Contains(src, "func runBrainBriefWithRawHistoryMatcher") {
		t.Fatal("agent_surface.go does not contain the brief builder; this guard is reading the wrong file")
	}
}
