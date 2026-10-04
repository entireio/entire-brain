package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// `recall --evidence` returns at facts_read_cmd.go:259, BEFORE the blind-spot
// checks the other recall paths run at :412 and :430. So the surface said
// nothing at all when a scan found no sessions on the queried branch -- the
// wrong-branch trap presented as a genuine miss.
//
// The note is in the result's OWN unit. Evidence scans canonical SESSIONS and
// returns spans; attaching "N active fact(s) on M other branch(es)" to a result
// that holds no facts is the shape mismatch the review flagged on the unified
// surfaces, and it would apply here too.
func TestEvidenceRecallNamesOtherBranchesHoldingSessions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	// The fixture captures s1 on main and s2 on feature. A scan of a third
	// branch has no sessions of its own while both others hold one.
	note := otherBranchSessionBlindSpot(brainDir, "release/1.0")
	for _, want := range []string{"no sessions were captured on release/1.0", "2 session(s)", "2 other branch(es)", "feature", "main", "--branch"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note must contain %q, got %q", want, note)
		}
	}
}

// Sessions on THIS branch mean the scan had material, so an empty result is
// about the query and a branch note would misdirect.
func TestEvidenceBranchNoteIsSilentWhenTheBranchHasSessions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	if note := otherBranchSessionBlindSpot(brainDir, "main"); note != "" {
		t.Errorf("main holds a session, so an empty result is about the query, not the branch: %q", note)
	}
	if note := otherBranchSessionBlindSpot(brainDir, ""); note != "" {
		t.Errorf("a caller that did not name a branch gets no note: %q", note)
	}
	if note := otherBranchSessionBlindSpot(t.TempDir(), "main"); note != "" {
		t.Errorf("an unreadable manifest is not evidence of anything: %q", note)
	}
}

// Branch names come out of a map and Go randomises map iteration, so an
// unsorted list would differ between identical runs.
func TestEvidenceBranchNoteIsDeterministic(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)

	first := otherBranchSessionBlindSpot(brainDir, "release/1.0")
	for i := 0; i < 24; i++ {
		if got := otherBranchSessionBlindSpot(brainDir, "release/1.0"); got != first {
			t.Fatalf("run %d differed:\n %q\n %q", i, first, got)
		}
	}
	if !strings.Contains(first, "feature, main") {
		t.Errorf("branches must be sorted, got %q", first)
	}
}

// END-TO-END. The three tests above call otherBranchSessionBlindSpot directly,
// and all three stayed green when the CALL SITE was removed -- the defect class
// where the bug lives in the wiring between two correct functions rather than
// in either of them. Removing `out.BlindSpot = ...` left the helper perfect and
// the surface silent, which is the exact bug being fixed.
//
// This drives the real command and asserts the note reaches the output.
func TestEvidenceRecallCommandEmitsTheBranchNote(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// A branch with no captured sessions, while the fixture holds main+feature.
	cmd.SetArgs([]string{"recall", "nothing matches this", "--evidence", "--branch", "release/1.0", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("recall --evidence: %v\n%s", err, out.String())
	}

	var got struct {
		ReturnedCount int    `json:"returned_count"`
		BlindSpot     string `json:"blind_spot"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if got.ReturnedCount != 0 {
		t.Fatalf("fixture must produce an empty result for this guard to mean anything, got %d spans", got.ReturnedCount)
	}
	if !strings.Contains(got.BlindSpot, "release/1.0") || !strings.Contains(got.BlindSpot, "other branch(es)") {
		t.Errorf("the command must emit the branch note on an empty evidence result, got %q", got.BlindSpot)
	}
}
