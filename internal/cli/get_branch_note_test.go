package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// `get fact:<id>` is usually the exact moment someone meets branch scoping, and
// it printed a bare "not found" -- making the same claim the empty-result note
// makes, that the item was never there, when the fact may be sitting on another
// branch. That is a different answer, and the only one with something the
// caller can act on.
//
// runGet checked ONLY the integrity path on a miss, so the branch choke point
// was unreachable from this surface.
func TestGetOnAMissNamesTheBranchHoldingFacts(t *testing.T) {
	f := newVerifyFixture(t)
	now := f.now
	// declareFacts edits an existing manifest, so the brain needs one.
	writeDistillFixtureAt(t, f.brainDir, now)

	// The fact lives on a feature branch; the query runs from the fixture's
	// branch and must miss.
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFacts(t, f.brainDir, now, 1, "feature/deploy")

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "fact:doesnotexist"})
	_ = cmd.Execute() // a miss exits 1 by design; the OUTPUT is the subject

	got := out.String()
	if !strings.Contains(got, "not found") {
		t.Fatalf("a miss must still say so:\n%s", got)
	}
	if !strings.Contains(got, "feature/deploy") {
		t.Errorf("the branch holding facts must be named on a fact miss, so the caller has something to retype:\n%s", got)
	}
}

// Gate 1: this is a UNIFIED surface. A conversation id missing has nothing to
// do with which branch holds facts, and a note about facts there is noise
// attached to an unrelated answer.
func TestGetDoesNotTalkAboutFactBranchesForANonFactID(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFacts(t, f.brainDir, f.now, 1, "feature/deploy")

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "conversation:nope"})
	_ = cmd.Execute()

	if got := out.String(); strings.Contains(got, "feature/deploy") {
		t.Errorf("a conversation miss must not carry a note about fact branches:\n%s", got)
	}
}

// Gate 2: the id-shape test itself. A bare id with no prefix resolves as a fact
// everywhere else in this codebase, so it counts here too.
func TestMissingFactIDShapes(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		ids  []string
		want bool
	}{
		"prefixed fact":      {[]string{"fact:abc"}, true},
		"bare id":            {[]string{"abc123"}, true},
		"conversation":       {[]string{"conversation:abc"}, false},
		"history":            {[]string{"history:abc"}, false},
		"raw has a colon":    {[]string{"raw:abc"}, false},
		"mixed, one fact":    {[]string{"conversation:a", "fact:b"}, true},
		"mixed, no fact":     {[]string{"conversation:a", "doc:b"}, false},
		"empty slice":        {nil, false},
		"blank entries only": {[]string{"", "   "}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := missingFactIDs(tc.ids); got != tc.want {
				t.Errorf("missingFactIDs(%v) = %v, want %v", tc.ids, got, tc.want)
			}
		})
	}
}

// Gate 3: integrity still wins. A store that lost facts must never be reported
// as merely the wrong branch -- the branch note would send the caller hunting
// for a fact that is actually gone.
func TestIntegrityWarningStillBeatsTheBranchNoteOnAMiss(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)

	// Declare more facts than exist anywhere: the store cannot produce what the
	// manifest claims.
	seedFactBranch(t, f.brainDir, "feature/deploy", "aaaa1111")
	declareFactsAt(t, f.brainDir, f.now, f.now, 99, "feature/deploy")

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"get", "fact:doesnotexist"})
	_ = cmd.Execute()

	got := out.String()
	if !strings.Contains(got, "NOT evidence of absence") {
		t.Fatalf("a store that lost facts must say so on a miss:\n%s", got)
	}
	if strings.Contains(got, "feature/deploy") {
		t.Errorf("a lost-facts store must not be reported as merely the wrong branch:\n%s", got)
	}
	_ = time.Now
}
