package cli

import (
	"bytes"
	"context"
	"io"
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

// TestSetupInstantSubcommandAlwaysDiscardsItsOutput: only --json used to
// discard the nested deterministic refresh, so a human first run printed
// setup's renderer AND the inner refresh's on top of it -- its own consent
// text, its own component names, its own daemon identity.
//
// Pointing either writer back at a real stream still compiles, and fails here.
func TestSetupInstantSubcommandAlwaysDiscardsItsOutput(t *testing.T) {
	t.Parallel()
	sub, captured := newSetupInstantCommand(context.Background())
	// Writer IDENTITY for stdout: a buffer this test owns is not wired to
	// anything the sub-command could reach, so it would pass whatever the
	// writer was. io.Discard is the contract, and pointing it at a real stream
	// still compiles.
	if got := sub.OutOrStdout(); got != io.Discard {
		t.Fatalf("the nested refresh must write stdout to io.Discard, got %T", got)
	}
	// stderr is CAPTURED, not discarded: the same stream carries real warnings
	// (a coordinator that could not launch, a skipped entity index), and
	// discarding it lost them silently. It must reach the buffer the caller
	// reads, and must NOT reach a real stream.
	if captured == nil {
		t.Fatal("the nested refresh's stderr must be captured so its warnings can be re-emitted")
	}
	if got := sub.ErrOrStderr(); got != io.Writer(captured) {
		t.Fatalf("the nested refresh's stderr must be the captured buffer, got %T", got)
	}
	sub.ErrOrStderr().Write([]byte("warning: memory coordinator: worker launch failed\n"))
	if got := setupWarningsFrom(captured); len(got) != 1 || got[0] != "memory coordinator: worker launch failed" {
		t.Fatalf("a warning on the nested stderr must be recoverable, got %q", got)
	}
	if got := setupWarningsFrom(captured); len(got) == 0 {
		t.Fatal("non-vacuity: the extractor matched nothing")
	}
	if sub.Context() == nil {
		t.Fatal("the sub-command must carry the setup context")
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

// TestSetupSummaryCallsEachComponentByOneName: the live phase line said
// "instant core: semantic index", the failure line said "semantic index", and
// the tick row between them said "semantic" -- three lines, one component, two
// names, in a single run.
func TestSetupSummaryCallsEachComponentByOneName(t *testing.T) {
	t.Parallel()
	report := setupRenderFixtureReport()
	got := renderSetupSummaryPlain(t, report, nil)

	var row string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "instant ") {
			row = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "instant"))
			break
		}
	}
	if row == "" {
		t.Fatalf("no instant tick row in the summary:\n%s", got)
	}
	entries := strings.Split(row, "  ")
	if len(entries) != len(report.Instant.Components) {
		t.Fatalf("tick row has %d entries for %d components: %q", len(entries), len(report.Instant.Components), row)
	}
	for i, component := range report.Instant.Components {
		name := strings.TrimSpace(entries[i][1:])
		want := setupComponentLabel(component.Name)
		if name != want {
			// The failure lines under this row, and the live phase line above
			// it, both use setupComponentLabel. Anything else here is the same
			// component answering to two names in one run.
			t.Fatalf("tick row calls %q %q; every other line calls it %q", component.Name, name, want)
		}
	}
}

// --- UX-25: do not claim queryability this brain does not have --------------

// unqueryableReport is a brain whose code index and baseline BOTH failed: one
// component survived, so setup exits 0 and the old summary said "Brain ready
// (degraded)" over a brain that cannot answer anything about this repository.
func unqueryableReport() setupReport {
	report := setupRenderFixtureReport()
	report.Instant.Components = []setupComponent{
		{Name: brainComponentSessions, State: "ok"},
		{Name: brainComponentSeed, State: "failed", Detail: "dirty_worktree: refusing to seed uncommitted content"},
		{Name: brainComponentSemantic, State: "failed", Detail: "dirty_worktree: refusing to index uncommitted content"},
	}
	return report
}

func TestSetupSummaryDoesNotCallAnUnqueryableBrainReady(t *testing.T) {
	t.Parallel()
	got := renderSetupSummaryPlain(t, unqueryableReport(), nil)
	headline := strings.SplitN(strings.TrimLeft(got, "\n"), "\n", 2)[0]
	if strings.Contains(headline, "Brain ready") {
		t.Fatalf("no code index and no baseline is not a ready brain: %q", headline)
	}
	if !strings.Contains(headline, "not queryable") {
		t.Fatalf("the headline must say the brain cannot be queried: %q", headline)
	}
	for _, want := range []string{"semantic index", "seed baseline"} {
		if !strings.Contains(got, "no "+want) {
			t.Fatalf("the summary must name what is unavailable (%q):\n%s", want, got)
		}
	}
}

// TestSetupSummaryStillSaysReadyWhenOnlyOneCoreComponentFailed is the
// over-application guard. One of the two gone is a degraded brain, which is
// still a brain that answers; only BOTH gone is unqueryable.
func TestSetupSummaryStillSaysReadyWhenOnlyOneCoreComponentFailed(t *testing.T) {
	t.Parallel()
	for _, survivor := range []string{brainComponentSeed, brainComponentSemantic} {
		report := unqueryableReport()
		for i := range report.Instant.Components {
			if report.Instant.Components[i].Name == survivor {
				report.Instant.Components[i] = setupComponent{Name: survivor, State: "ok"}
			}
		}
		got := renderSetupSummaryPlain(t, report, nil)
		headline := strings.SplitN(strings.TrimLeft(got, "\n"), "\n", 2)[0]
		if !strings.Contains(headline, "Brain ready (degraded)") {
			t.Fatalf("with %s built the brain still answers: %q", survivor, headline)
		}
		if strings.Contains(got, "not queryable") {
			t.Fatalf("with %s built the brain must not be called unqueryable:\n%s", survivor, got)
		}
	}
}

// TestSetupSummaryUnchangedWhenNoComponentsWereReported keeps the pre-component
// contract: a phase that reports nothing built fine, and must not be accused.
func TestSetupSummaryUnchangedWhenNoComponentsWereReported(t *testing.T) {
	t.Parallel()
	report := setupRenderFixtureReport()
	report.Instant = setupPhase{State: "ok"}
	got := renderSetupSummaryPlain(t, report, nil)
	if strings.Contains(got, "not queryable") {
		t.Fatalf("no components reported is not evidence of failure:\n%s", got)
	}
}

// TestSetupExitsZeroButSaysWhatIsUnavailable is the whole-run property: the
// exit code is deliberately unchanged (one component built, so the brain is
// worth keeping) and the words are not.
func TestSetupExitsZeroButSaysWhatIsUnavailable(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{instantComponents: []setupComponent{
		{Name: brainComponentSessions, State: "ok"},
		{Name: brainComponentSeed, State: "failed", Detail: "dirty_worktree: refusing to seed uncommitted content"},
		{Name: brainComponentSemantic, State: "failed", Detail: "dirty_worktree: refusing to index uncommitted content"},
	}}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	out := &bytes.Buffer{}
	if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("one surviving component still exits 0: %v", err)
	}
	printed := out.String()
	if strings.Contains(printed, "the brain is queryable now") {
		t.Fatalf("setup claimed queryability with no index and no baseline:\n%s", printed)
	}
	if !strings.Contains(printed, "no semantic index and no seed baseline") {
		t.Fatalf("setup must name what is unavailable:\n%s", printed)
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
	// audit=false is the lightweight status path, which is the surface this
	// test is about; the audit path is doctor's.
	return buildBrainRetrievalStatus(context.Background(), runner, t.TempDir(), repoDir, manifest, live, false)
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
	report := setupReport{
		Instant: setupPhase{Components: []setupComponent{
			{Name: "seed", State: "failed", Detail: "dirty_worktree: refusing to seed", Hint: hint},
			{Name: "semantic", State: "failed", Detail: "dirty_worktree: refusing to index", Hint: hint},
		}},
	}
	out := renderSetupSummaryPlain(t, report, &setupTimings{})

	if !strings.Contains(out, "refusing to seed") || !strings.Contains(out, "refusing to index") {
		t.Fatalf("both failures must still be named, so this guard is not vacuous:\n%s", out)
	}
	if n := strings.Count(out, "hint: "); n != 1 {
		t.Fatalf("one remedy, one appearance; printed %d times:\n%s", n, out)
	}
}

// Two DIFFERENT remedies must both still be shown — dedup is not suppression.
func TestTwoDifferentRemediesAreBothShown(t *testing.T) {
	t.Parallel()
	report := setupReport{
		Instant: setupPhase{Components: []setupComponent{
			{Name: "seed", State: "failed", Detail: "a", Hint: "first remedy"},
			{Name: "semantic", State: "failed", Detail: "b", Hint: "second remedy"},
		}},
	}
	out := renderSetupSummaryPlain(t, report, &setupTimings{})
	if n := strings.Count(out, "hint: "); n != 2 {
		t.Fatalf("distinct remedies must both appear, got %d:\n%s", n, out)
	}
}

// A Go error is routinely multi-line: errors.Join concatenates its causes with
// a newline, and the memory path joins a deterministic failure with a run
// failure before it reaches a `warning: %v` line. Keeping only the lines that
// carry the prefix dropped every cause after the first — the same information
// loss this re-emission exists to repair, one layer down. Raised as a finding
// on this PR, and correct.
func TestAMultiLineWarningKeepsItsCauses(t *testing.T) {
	t.Parallel()

	got := setupWarningsFrom(bytes.NewBufferString(
		"warning: memory coordinator could not launch: exec failed\npermission denied\nno such file\n"))
	if len(got) != 1 {
		t.Fatalf("expected one warning carrying its causes, got %d: %q", len(got), got)
	}
	for _, cause := range []string{"exec failed", "permission denied", "no such file"} {
		if !strings.Contains(got[0], cause) {
			t.Errorf("the re-emitted warning lost a cause %q: %s", cause, got[0])
		}
	}

	// A continuation ends at a blank line, so unrelated stderr written after a
	// warning is not swept into it.
	sep := setupWarningsFrom(bytes.NewBufferString(
		"warning: first: a\nb\n\nunrelated progress noise\nwarning: second\n"))
	if len(sep) != 2 {
		t.Fatalf("expected two warnings, got %d: %q", len(sep), sep)
	}
	if strings.Contains(sep[0], "unrelated progress noise") {
		t.Errorf("stderr after a blank line was swept into the warning: %s", sep[0])
	}
	if sep[1] != "second" {
		t.Errorf("the second warning was altered: %q", sep[1])
	}
}
