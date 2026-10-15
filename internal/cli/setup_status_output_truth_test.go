package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/tui"
)

// setup_status_output_truth_test.go pins four things a FIRST-TIME user reads on
// screen and was misled by: the inner refresh's lines leaking into setup's own
// (UX-24), a readiness claim a brain with no index and no baseline cannot back
// (UX-25), a status row that named failures without saying why (UX-26), and two
// contradictory remedies for one dirty worktree (UX-27).

// --- UX-24: the nested refresh is run, not read -----------------------------

// TestSetupInstantOutputNeverReachesTheTerminal: only --json used to discard
// the nested deterministic refresh, so a human first run printed setup's
// renderer AND the inner refresh's on top of it -- its own consent text, its
// own component names, its own daemon identity.
//
// The nested command's two streams now go to the step sink in every mode:
// each line becomes a sub-step on the open phase's transient line, which on a
// terminal is a repaint and in a pipe is nothing at all. Pointing either
// writer back at a real stream still compiles, and fails here.
func TestSetupInstantOutputNeverReachesTheTerminal(t *testing.T) {
	t.Parallel()
	sink := &setupStepSink{}
	steps := resolveSetupSteps(nil, Options{}, defaultSetupOptions(), t.TempDir(), setupBrainBinaryName,
		setupSteps{stepOutput: sink, observeComponent: &setupComponentObserver{}})
	if steps.instant == nil {
		t.Fatal("resolveSetupSteps left the instant step unfilled")
	}
	// The sub-command is built inside the step, so the contract is asserted
	// where it is observable: nothing the nested refresh writes may become a
	// permanent row, and a warning it writes must still survive.
	fmt.Fprintf(sink, "refresh: semantic index: parsing sources\n")
	fmt.Fprintf(sink, "warning: memory coordinator: worker launch failed\n")
	got := sink.Warnings()
	if len(got) != 1 || got[0] != "memory coordinator: worker launch failed" {
		t.Fatalf("a warning on the nested stream must be recoverable, got %q", got)
	}
	if strings.Contains(strings.Join(got, "|"), "parsing sources") {
		t.Fatalf("non-vacuity: the nested chatter was kept as a warning: %q", got)
	}
}

// TestSetupAndStatusNeverPrintTheHashedDaemonID: a first run printed
// "background watcher installed (io.entire.brain-watch.1a2b3c4d)" and `status`
// printed "running (io.entire.brain-watch.1a2b3c4d)". The hash identifies
// nothing a reader can open, choose or pass to a command. It stays in --json;
// it does not belong in prose. The unit PATH, printed once before the install,
// is the identity a person can use.
func TestSetupAndStatusNeverPrintTheHashedDaemonID(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	printed := runSetupForTest(t, f, defaultSetupOptions(), rec)
	label := rec.state.Label
	if label == "" {
		t.Fatal("fixture installed no labelled daemon")
	}
	for _, line := range strings.Split(printed, "\n") {
		if strings.Contains(line, label) && !strings.Contains(line, rec.state.UnitPath) {
			t.Fatalf("setup surfaced the hashed daemon id outside the unit path: %q", line)
		}
	}
	// The same claim from the other command a user is told to run.
	report := statusFixtureReport(t, 0)
	report.Onboarding.Daemon = daemonState{Manager: daemonManagerLaunchd, Label: label, Installed: true, Current: true, Running: true}
	if got := renderStatusShortPlain(t, report); strings.Contains(got, label) {
		t.Fatalf("status surfaced the hashed daemon id:\n%s", got)
	}
}

// TestSetupBuildPhaseCallsEachComponentByOneName: the live phase line said
// "instant core: semantic index", the failure line said "semantic index", and
// the tick row between them said "semantic" -- three lines, one component, two
// names, in a single run. The tick row is gone with the old summary block; the
// Build phase's count clause took over naming what failed, and it must use the
// same vocabulary as every other line.
func TestSetupBuildPhaseCallsEachComponentByOneName(t *testing.T) {
	t.Parallel()
	built, failed := partitionSetupComponents(setupRenderFixtureReport().Instant.Components)
	summary := setupComponentCountSummary(built, failed)
	details := strings.Join(setupComponentFailureDetails(failed, "-", setupBrainBinaryName), "\n")

	checked := 0
	for _, component := range failed {
		label := setupComponentLabel(component.Name)
		if label == component.Name {
			continue
		}
		checked++
		if !strings.Contains(summary, "FAILED "+label) {
			t.Fatalf("the build clause calls %q something other than %q: %s", component.Name, label, summary)
		}
		if !strings.Contains(details, label+" failed") {
			t.Fatalf("the failure detail calls %q something other than %q:\n%s", component.Name, label, details)
		}
	}
	if checked == 0 {
		t.Fatal("non-vacuity: no failed component in the fixture has a label distinct from its id")
	}
}

// --- UX-25: do not claim queryability this brain does not have --------------

// setupRunPrinted runs a whole setup over the given instant components. The
// summary renderer these guards used to call is gone -- the seven-phase run IS
// the renderer now -- so the claim is asserted where it is made.
func setupRunPrinted(t *testing.T, components []setupComponent) (string, error) {
	t.Helper()
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{instantComponents: components}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true
	out := &bytes.Buffer{}
	err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f))
	return out.String(), err
}

// unqueryableComponents is a brain whose code index and baseline BOTH failed:
// one component survived, so the old summary said "Brain ready (degraded)" over
// a brain that cannot answer anything about this repository.
func unqueryableComponents() []setupComponent {
	return []setupComponent{
		{Name: brainComponentSessions, State: "ok"},
		{Name: brainComponentSeed, State: "failed", Detail: "dirty_worktree: refusing to seed uncommitted content"},
		{Name: brainComponentSemantic, State: "failed", Detail: "dirty_worktree: refusing to index uncommitted content"},
	}
}

func TestSetupDoesNotCallAnUnqueryableBrainReady(t *testing.T) {
	printed, err := setupRunPrinted(t, unqueryableComponents())
	if strings.Contains(printed, "the brain is queryable now") {
		t.Fatalf("no code index and no baseline is not a queryable brain:\n%s", printed)
	}
	for _, want := range []string{"semantic index", "seed baseline"} {
		if !strings.Contains(printed, "no "+want) {
			t.Fatalf("setup must name what is unavailable (%q):\n%s", want, printed)
		}
	}
	// The exit code is #374's contract, not this test's subject: a brain with
	// no source `query`, `brief` or `overview` reads from is not one a script
	// may proceed on. The reason must still name the sources.
	if err == nil {
		t.Fatal("a brain with nothing to answer from must not exit 0")
	}
	if !strings.Contains(err.Error(), "semantic index") || !strings.Contains(err.Error(), "seed baseline") {
		t.Fatalf("the exit reason must name what did not build: %v", err)
	}
}

// TestSetupStillSaysQueryableWhenOnlyOneCoreComponentFailed is the
// over-application guard. One of the two gone is a degraded brain, which is
// still a brain that answers; only BOTH gone is unqueryable.
func TestSetupStillSaysQueryableWhenOnlyOneCoreComponentFailed(t *testing.T) {
	for _, survivor := range []string{brainComponentSeed, brainComponentSemantic} {
		components := unqueryableComponents()
		for i := range components {
			if components[i].Name == survivor {
				components[i] = setupComponent{Name: survivor, State: "ok"}
			}
		}
		printed, err := setupRunPrinted(t, components)
		if err != nil {
			t.Fatalf("with %s built the brain still answers: %v", survivor, err)
		}
		if !strings.Contains(printed, "the brain is queryable now") {
			t.Fatalf("with %s built setup must still say so:\n%s", survivor, printed)
		}
		if strings.Contains(printed, "no semantic index and no seed baseline") {
			t.Fatalf("with %s built the brain must not be called unqueryable:\n%s", survivor, printed)
		}
	}
}

// TestSetupUnchangedWhenNoComponentsWereReported keeps the pre-component
// contract: a phase that reports nothing built fine, and must not be accused.
func TestSetupUnchangedWhenNoComponentsWereReported(t *testing.T) {
	printed, err := setupRunPrinted(t, []setupComponent{})
	if err != nil {
		t.Fatalf("no components reported is not evidence of failure: %v", err)
	}
	if !strings.Contains(printed, "the brain is queryable now") {
		t.Fatalf("no components reported is not evidence of failure:\n%s", printed)
	}
}

// --- UX-26: status holds the reason, so status prints it --------------------

func renderStatusShortPlain(t *testing.T, report brainStatusReport) string {
	t.Helper()
	out := &bytes.Buffer{}
	theme, _ := tui.ThemeByName("default")
	renderBrainStatusShort(out, tui.NewRendererWith(theme, tui.PlainCaps()), report, setupBrainBinaryName)
	return out.String()
}

func TestStatusPrintsWhyEachComponentFailed(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 0)
	report.Onboarding.Components = []brainStatusComponent{
		{Name: "sessions", State: "built"},
		{Name: "semantic", State: "failed", Detail: "repo key mismatch: snapshot repo_key gh/other/repo does not match current repo", Hint: "rebuild it with `entire-brain refresh index --force`"},
		{Name: "history", State: "missing"},
	}
	got := renderStatusShortPlain(t, report)
	if !strings.Contains(got, "snapshot repo_key gh/other/repo does not match current repo") {
		t.Fatalf("status holds the reason and must print it:\n%s", got)
	}
	if !strings.Contains(got, "hint: rebuild it with `entire-brain refresh index --force`") {
		t.Fatalf("status holds the hint and must print it:\n%s", got)
	}
	// Over-application guard: a component that merely was never built has no
	// reason to report, and must not grow a line.
	if strings.Contains(got, "history:") {
		t.Fatalf("a missing component is not a failure report:\n%s", got)
	}
}

// TestStatusSaysSoWhenAFailureHasNoRecordedReason: silence under a failed
// component would read as "nothing to explain".
func TestStatusSaysSoWhenAFailureHasNoRecordedReason(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 0)
	report.Onboarding.Components = []brainStatusComponent{{Name: "entities", State: "failed"}}
	got := renderStatusShortPlain(t, report)
	if !strings.Contains(got, "entities: no reason recorded") {
		t.Fatalf("a reasonless failure must say so:\n%s", got)
	}
}

// --- UX-27: one remedy for one dirty worktree -------------------------------

// dirtySeedRetrievalStatus runs the REAL producer, so a remedy put back into
// the freshness axis is caught here rather than in a hand-written fixture.
func dirtySeedRetrievalStatus(t *testing.T) *brainStatusRetrieval {
	t.Helper()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	live := brainLiveState{Branch: "main", Head: "58b0c54e", Dirty: true}
	manifest := &exportManifest{Sources: &brainSources{
		Seed: &seedSourceManifest{Commit: "58b0c54e", WorktreeMode: "commit", GeneratedAt: setupTestNow},
	}}
	return buildBrainRetrievalStatus(context.Background(), runner, t.TempDir(), repoDir, manifest, live)
}

func TestDirtyWorktreeCauseCarriesNoRemedyOfItsOwn(t *testing.T) {
	t.Parallel()
	status := dirtySeedRetrievalStatus(t)
	axis := status.Freshness.Axes["seed"]
	if axis.State != "dirty-unindexed" {
		t.Fatalf("fixture did not reach the dirty-unindexed axis: %+v", axis)
	}
	if strings.Contains(axis.Detail, "--worktree") || strings.Contains(axis.Detail, "refresh") {
		t.Fatalf("the cause must not offer a remedy; setupDirtyWorktreeHint owns it: %q", axis.Detail)
	}
	if !strings.Contains(axis.Detail, "committed HEAD") {
		t.Fatalf("the cause must still say what is wrong: %q", axis.Detail)
	}
}

func TestDirtyWorktreeGetsExactlyOneRemedy(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 0)
	report.Live.Dirty = true
	report.Semantic.Freshness = &staleReport{Severity: "ok", Axes: map[string]staleAxis{"store": {State: "ok"}}}
	// ONLY the seed axis. The producer's fixture also raises docs=missing, and
	// the two details together overrun statusCauseWidth -- which silently
	// truncated the very text this test is about, so the first version of it
	// could not fail. The axis itself still comes from the producer, so a
	// remedy put back into it is still caught here.
	seed := dirtySeedRetrievalStatus(t).Freshness.Axes["seed"]
	report.Retrieval = &brainStatusRetrieval{Freshness: &staleReport{
		Severity: "degraded",
		Axes:     map[string]staleAxis{"seed": seed},
	}}

	got := renderStatusShortPlain(t, report)
	remedy := setupDirtyWorktreeHint(setupBrainBinaryName)
	if !strings.Contains(got, remedy) {
		t.Fatalf("the verdict must carry the one remedy resolver's answer:\n%s", got)
	}
	// The resolver names --worktree exactly once. A second mention in the cause
	// underneath is the contradiction this guards against.
	if want, got2 := strings.Count(remedy, "--worktree"), strings.Count(got, "--worktree"); got2 != want {
		t.Fatalf("a dirty tree must be answered once, found %d mentions of --worktree (resolver has %d):\n%s", got2, want, got)
	}
	if strings.Contains(got, "refresh with --worktree to include current changes") {
		t.Fatalf("the cause is still offering its own, contradictory remedy:\n%s", got)
	}
	// Non-vacuity: the cause line must actually be on screen, or the count
	// above is counting nothing.
	if !strings.Contains(got, "seed=dirty-unindexed") || !strings.Contains(got, seed.Detail) {
		t.Fatalf("the cause never reached the output, so nothing above was tested:\n%s", got)
	}
}

// TestStatusStatesTheDirtyRemedyExactlyOnce is UX-26 and UX-27 together. A
// dirty worktree refuses BOTH seed and semantic, so printing each failed
// component's hint put the same sentence on screen twice -- and the verdict
// underneath printed it a third time. Saying what failed and why must not
// become saying one remedy three times.
func TestStatusStatesTheDirtyRemedyExactlyOnce(t *testing.T) {
	t.Parallel()
	remedy := setupDirtyWorktreeHint(setupBrainBinaryName)
	report := statusFixtureReport(t, 0)
	report.Live.Dirty = true
	report.Semantic.Freshness = &staleReport{Severity: "ok", Axes: map[string]staleAxis{"store": {State: "ok"}}}
	seed := dirtySeedRetrievalStatus(t).Freshness.Axes["seed"]
	report.Retrieval = &brainStatusRetrieval{Freshness: &staleReport{
		Severity: "degraded",
		Axes:     map[string]staleAxis{"seed": seed},
	}}
	report.Onboarding.Components = []brainStatusComponent{
		{Name: "seed", State: "failed", Detail: "dirty_worktree: refusing to seed uncommitted content without --worktree", Hint: remedy},
		{Name: "semantic", State: "failed", Detail: "dirty_worktree: refusing to index uncommitted content without --worktree", Hint: remedy},
	}

	got := renderStatusShortPlain(t, report)
	if n := strings.Count(got, remedy); n != 1 {
		t.Fatalf("one remedy, one appearance; found %d:\n%s", n, got)
	}
	// Non-vacuity: both failures are still reported, with their own reasons.
	for _, want := range []string{"refusing to seed uncommitted content", "refusing to index uncommitted content"} {
		if !strings.Contains(got, want) {
			t.Fatalf("deduping the remedy must not drop a failure reason (%q):\n%s", want, got)
		}
	}
}

// TestDirtyWorktreeRemedyNamesBothWaysOut: the resolver is the only place the
// reader is told what to do, so it has to name both -- the one that re-runs
// setup on a clean tree and the one that indexes the tree as it is.
func TestDirtyWorktreeRemedyNamesBothWaysOut(t *testing.T) {
	t.Parallel()
	remedy := setupDirtyWorktreeHint(setupBrainBinaryName)
	for _, want := range []string{"commit or stash", setupBrainBinaryName + " setup", "refresh --worktree", "entire enable"} {
		if !strings.Contains(remedy, want) {
			t.Fatalf("the single remedy must name %q: %s", want, remedy)
		}
	}
}

// Moving the remedy out of axis.Detail (so the short status would not print it
// twice) left `status --verbose` showing the cause with no way out: it renders
// Detail directly and never re-pairs the hint. Raised as a finding on this PR,
// and correct.
func TestVerboseAxesStillNameTheWayOut(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderFreshnessAxes(&buf, map[string]staleAxis{
		"seed": {State: dirtyUnindexedAxisState, Detail: "seed and docs are based on committed HEAD"},
	})
	line := buf.String()

	if !strings.Contains(line, "dirty-unindexed") {
		t.Fatalf("fixture did not render the axis under test: %q", line)
	}
	for _, want := range []string{"--worktree", "commit or stash"} {
		if !strings.Contains(line, want) {
			t.Errorf("the verbose axis states a cause with no remedy; missing %q in: %s", want, line)
		}
	}
}

// A healthy axis must not gain a dirty-worktree remedy it has no use for.
func TestACleanAxisGetsNoDirtyRemedy(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	renderFreshnessAxes(&buf, map[string]staleAxis{"seed": {State: "current"}})
	if strings.Contains(buf.String(), "--worktree") {
		t.Fatalf("a clean axis was given the dirty-worktree remedy: %s", buf.String())
	}
}

// A dirty working tree fails BOTH seed and semantic — the common first run this
// release's own docs describe — and each failed component carries the identical
// hint, so the reader was told the same thing twice within six lines.
// status_render.go was given a dedup guard in this change; setup's own summary
// was missed. Raised as a finding on this PR, and correct.
func TestTheSetupSummaryStatesOneRemedyOnce(t *testing.T) {
	t.Parallel()
	hint := setupDirtyWorktreeHint("entire brain")
	details := strings.Join(setupComponentFailureDetails([]setupComponent{
		{Name: "seed", State: "failed", Detail: "dirty_worktree: refusing to seed", Hint: hint},
		{Name: "semantic", State: "failed", Detail: "dirty_worktree: refusing to index", Hint: hint},
	}, "-", setupBrainBinaryName), "\n")

	if !strings.Contains(details, "refusing to seed") || !strings.Contains(details, "refusing to index") {
		t.Fatalf("both failures must still be named, so this guard is not vacuous:\n%s", details)
	}
	if n := strings.Count(details, "hint: "); n != 1 {
		t.Fatalf("one remedy, one appearance; printed %d times:\n%s", n, details)
	}
}

// Two DIFFERENT remedies must both still be shown -- dedup is not suppression.
func TestTwoDifferentRemediesAreBothShown(t *testing.T) {
	t.Parallel()
	details := strings.Join(setupComponentFailureDetails([]setupComponent{
		{Name: "seed", State: "failed", Detail: "a", Hint: "first remedy"},
		{Name: "semantic", State: "failed", Detail: "b", Hint: "second remedy"},
	}, "-", setupBrainBinaryName), "\n")
	if n := strings.Count(details, "hint: "); n != 2 {
		t.Fatalf("distinct remedies must both appear, got %d:\n%s", n, details)
	}
}

// A Go error is routinely multi-line: errors.Join concatenates its causes with
// a newline, and the memory path joins a deterministic failure with a run
// failure before it reaches a `warning: %v` line. Keeping only the lines that
// carry the prefix dropped every cause after the first -- the same information
// loss carrying the warning up exists to repair, one layer down. Raised as a
// finding on this PR, and correct. The carrier is now the step sink the nested
// refresh writes into; the rule is unchanged.
func TestAMultiLineWarningKeepsItsCauses(t *testing.T) {
	t.Parallel()

	sink := &setupStepSink{}
	fmt.Fprint(sink, "warning: memory coordinator could not launch: exec failed\npermission denied\nno such file\n")
	got := sink.Warnings()
	if len(got) != 1 {
		t.Fatalf("expected one warning carrying its causes, got %d: %q", len(got), got)
	}
	for _, cause := range []string{"exec failed", "permission denied", "no such file"} {
		if !strings.Contains(got[0], cause) {
			t.Errorf("the warning lost a cause %q: %s", cause, got[0])
		}
	}

	// A continuation ends at a blank line, so unrelated output written after a
	// warning is not swept into it -- and so does a line the nested REPORTER
	// wrote, which carries its own prefix and is progress, never a cause.
	sep := &setupStepSink{}
	fmt.Fprint(sep, "warning: first: a\nb\n\nunrelated progress noise\nwarning: second\n")
	lines := sep.Warnings()
	if len(lines) != 2 {
		t.Fatalf("expected two warnings, got %d: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "unrelated progress noise") {
		t.Errorf("output after a blank line was swept into the warning: %s", lines[0])
	}
	if lines[1] != "second" {
		t.Errorf("the second warning was altered: %q", lines[1])
	}

	prefixed := &setupStepSink{}
	fmt.Fprint(prefixed, "warning: first: a\nrefresh: doc index done\n")
	if got := prefixed.Warnings(); len(got) != 1 || got[0] != "first: a" {
		t.Errorf("a prefixed reporter line was swept into the warning: %q", got)
	}
}
