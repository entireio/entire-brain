package cli

import (
	"regexp"
	"strings"
	"testing"
)

// A flag a user can type, that the binary accepts, and that changes nothing is
// a promise the program does not keep. These two were the surviving examples.

// TestSeedHasNoDeadInteractiveFlag: `refresh seed --interactive` was
// registered and parsed into seedOpts.interactive, which NOTHING ever read --
// promptSeedTimeoutAction gates on noInteractive and a TTY, so prompts were
// already on by default and the flag could only ask for what was already
// happening. It is gone, and its working counterpart is not.
func TestSeedHasNoDeadInteractiveFlag(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	seed, _, err := root.Find([]string{"refresh", "seed"})
	if err != nil {
		t.Fatalf("find refresh seed: %v", err)
	}
	if flag := seed.Flags().Lookup("interactive"); flag != nil {
		t.Fatalf("`refresh seed --interactive` is back (%q) -- nothing reads it, so it cannot do what it says", flag.Usage)
	}
	// The flag that DOES something is untouched.
	if seed.Flags().Lookup("no-interactive") == nil {
		t.Fatal("--no-interactive is the one that is actually read; it must stay")
	}
	// And a user who types the dead flag is told, rather than silently obeyed.
	out, err := execute(t, NewRootCommand(Options{Version: "test"}), "refresh", "seed", "--interactive")
	if err == nil || !isUnknownCommandOrFlag(err.Error()) {
		t.Fatalf("`refresh seed --interactive` = %v, want an unknown-flag error\n%s", err, out)
	}
}

// TestJobsIsOneFlagUnderTwoSpellings: `--jobs` was described as a
// "Compatibility alias for --concurrency" and was not one. It was its own
// IntVar, so `--jobs 4 --concurrency 4` was accepted, `--jobs 4 --concurrency
// 8` was an error telling you to use the flag you had just used, `--jobs 0`
// was rejected while the identical default of 0 was fine, and
// distillRequestedJobs silently preferred jobs over concurrency. It is now
// normalized onto --concurrency: one value, so none of those states exist.
func TestJobsIsOneFlagUnderTwoSpellings(t *testing.T) {
	cmd := newDistillCommand(Options{Version: "test"})
	jobs, concurrency := cmd.Flags().Lookup("jobs"), cmd.Flags().Lookup("concurrency")
	if jobs == nil || concurrency == nil {
		t.Fatalf("both spellings must resolve: jobs=%v concurrency=%v", jobs, concurrency)
	}
	if jobs != concurrency {
		t.Fatalf("--jobs and --concurrency are still two flags (%p vs %p); an alias is one flag with two names", jobs, concurrency)
	}

	// The old spelling still sets the value the pipeline reads.
	cmd = newDistillCommand(Options{Version: "test"})
	if err := cmd.Flags().Parse([]string{"--jobs", "4"}); err != nil {
		t.Fatalf("--jobs 4: %v", err)
	}
	if got := cmd.Flags().Lookup("concurrency").Value.String(); got != "4" {
		t.Fatalf("--jobs 4 left concurrency at %s", got)
	}
	if !cmd.Flags().Changed("jobs") || !cmd.Flags().Changed("concurrency") {
		t.Fatal("a flag set under either spelling must read as Changed under both")
	}

	// The self-conflict is structurally gone: a flag cannot disagree with
	// itself, so the two spellings together are last-wins, not an error.
	cmd = newDistillCommand(Options{Version: "test"})
	if err := cmd.Flags().Parse([]string{"--jobs", "4", "--concurrency", "8"}); err != nil {
		t.Fatalf("one flag under two names must not conflict with itself: %v", err)
	}
	if got := cmd.Flags().Lookup("concurrency").Value.String(); got != "8" {
		t.Fatalf("last spelling must win, got concurrency=%s", got)
	}

	// And it is not advertised as a flag of its own: help lists --concurrency
	// once, naming the other spelling, rather than two entries to choose between.
	usage := cmd.Flags().FlagUsages()
	if regexp.MustCompile(`(?m)^\s+--jobs\b`).MatchString(usage) {
		t.Fatalf("--jobs is listed as its own flag again:\n%s", usage)
	}
	if !strings.Contains(usage, "--jobs") {
		t.Fatalf("the surviving spelling must still be discoverable from --concurrency:\n%s", usage)
	}
}

// TestDistillRequestedJobsReadsOneValue: the in-process callers (the watcher's
// gated distill, `hook session-end`) build options in Go, where no flag
// normalization runs. They used to have to set `jobs` AND `concurrency` in
// step; there is one field now, so there is nothing to get out of step.
func TestDistillRequestedJobsReadsOneValue(t *testing.T) {
	if got := distillRequestedJobs(distillCommandOptions{concurrency: 6}); got != 6 {
		t.Fatalf("requested jobs = %d, want the concurrency the caller set", got)
	}
	if got := distillRequestedJobs(distillCommandOptions{}); got != 1 {
		t.Fatalf("an unset concurrency must fall back to 1, got %d", got)
	}
	if got := watchDistillOptions(defaultWatchOptions()); got.concurrency <= 0 {
		t.Fatalf("the watcher must still set the one value runDistill guards: %+v", got)
	}
}
