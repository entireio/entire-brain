package cli

import (
	"strings"
	"testing"
)

// Two branches rewrote this refusal independently, each fixing a different half,
// and merging them naively produced a duplicate declaration that would not
// compile. The reconciliation keeps BOTH halves, and this pins them together so
// a later edit cannot quietly drop one and still look correct.
//
//	UX-08 (gate exemption branch): say HOW MANY files and WHICH, with correct
//	      singular/plural, and summarise a long list instead of printing it.
//	UX-05 (runnable advice branch): name `.brainignore`, the only escape hatch
//	      that was documented in no error string and no README line.
func TestTheRefusalKeepsBothBranchesContributions(t *testing.T) {
	t.Parallel()
	message := dirtyWorktreeRefusal("index", []string{"src/a.go", "src/b.go"}).Error()

	for _, want := range []string{
		".brainignore",         // UX-05: the undocumented escape hatch
		"2 uncommitted paths",  // UX-08: the scale
		"src/a.go", "src/b.go", // UX-08: which files
		"--worktree",           // both: the flag
		"commit or stash",      // both: the other way out
		dirtyWorktreeErrorCode, // the machine-readable prefix callers match on
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the reconciled refusal lost %q: %s", want, message)
		}
	}
}

func TestTheRefusalStaysReadableOnAHugeDirtyTree(t *testing.T) {
	t.Parallel()
	many := make([]string, 0, dirtyWorktreeRefusalPathLimit+3)
	for i := 0; i < cap(many); i++ {
		many = append(many, "src/file"+string(rune('a'+i))+".go")
	}
	message := dirtyWorktreeRefusal("index", many).Error()

	if !strings.Contains(message, "and 3 more") {
		t.Errorf("a long list must be summarised, not printed: %s", message)
	}
	if strings.Contains(message, many[len(many)-1]) {
		t.Errorf("every path was listed instead of a sample: %s", message)
	}
	// The escape hatch must survive truncation -- it is most useful precisely
	// when the tree is full of files the reader does not want indexed.
	if !strings.Contains(message, ".brainignore") {
		t.Errorf("the escape hatch was lost on a long list: %s", message)
	}
}

func TestTheRefusalCountsOneFileAsOne(t *testing.T) {
	t.Parallel()
	if message := dirtyWorktreeRefusal("seed", []string{"src/a.go"}).Error(); !strings.Contains(message, "1 uncommitted path:") {
		t.Errorf(`"1 uncommitted paths" is how a reader learns the tool is not paying attention: %s`, message)
	}
}

// The "same workspace, different tuning" case this file's own header describes:
// a second repo adopting an existing workspace without naming --interval etc.
// arrives with those fields empty, and without a fallback it blanks tuning the
// first repo deliberately set. Raised as a finding on this PR, and correct for
// three of its four fields.
func TestASecondRepoDoesNotBlankTheWorkspaceTuning(t *testing.T) {
	t.Parallel()
	existing := setupWatchPlanEntry{
		Workspace:    "default",
		Agent:        "claude",
		Model:        "sonnet",
		Interval:     "5m0s",
		DistillEvery: "24h0m0s",
		Effort:       "low",
		MaxSessions:  25,
	}
	// A bare re-run from another repo: no tuning flags named, so these are the
	// fields setupWatchPlanEntryFor leaves empty.
	incoming := setupWatchPlanEntry{Workspace: "default", MaxSessions: 10}

	merged := mergeSetupWatchPlanEntry(existing, incoming)

	for _, c := range []struct{ name, got, want string }{
		{"Interval", merged.Interval, existing.Interval},
		{"DistillEvery", merged.DistillEvery, existing.DistillEvery},
		{"Effort", merged.Effort, existing.Effort},
		{"Agent", merged.Agent, existing.Agent},
		{"Model", merged.Model, existing.Model},
	} {
		if c.got != c.want {
			t.Errorf("%s was blanked by a run that did not name it: got %q want %q", c.name, c.got, c.want)
		}
	}
	// MaxSessions deliberately does NOT fall back: it carries the budget THIS
	// run showed the user and paid the foreground backfill with. Inheriting a
	// previous repo's cap would let the daemon spend against a number nobody saw.
	if merged.MaxSessions != incoming.MaxSessions {
		t.Errorf("MaxSessions must carry this run's budget, not the previous one: got %d want %d", merged.MaxSessions, incoming.MaxSessions)
	}
}

// A run that DOES name tuning must still win, or the fallback becomes a freeze.
func TestNamedTuningStillOverridesTheExistingEntry(t *testing.T) {
	t.Parallel()
	existing := setupWatchPlanEntry{Workspace: "default", Interval: "5m0s", Effort: "low"}
	incoming := setupWatchPlanEntry{Workspace: "default", Interval: "30m0s", Effort: "high"}

	merged := mergeSetupWatchPlanEntry(existing, incoming)
	if merged.Interval != "30m0s" || merged.Effort != "high" {
		t.Fatalf("an explicit value was overridden by the fallback: %+v", merged)
	}
}
