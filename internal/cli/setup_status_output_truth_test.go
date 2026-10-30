package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/setupui"
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
func TestSetupInstantSubcommandNeverWritesToARealStream(t *testing.T) {
	t.Parallel()
	sink := &bytes.Buffer{}
	sub, captured := newSetupInstantCommand(context.Background(), sink)

	// stdout goes to the PHASE SINK, not io.Discard as it once did: the nested
	// reporter has no idea it is nested, and its progress belongs inside the
	// phase rather than thrown away. The contract that survived the change is
	// that it never reaches a real stream.
	if got := sub.OutOrStdout(); got != io.Writer(sink) {
		t.Fatalf("the nested refresh must write stdout to the phase sink, got %T", got)
	}
	if captured == nil {
		t.Fatal("the nested refresh's stderr must be captured so its warnings can be re-emitted")
	}

	// stderr is TEED, so identity cannot be asserted -- and a type check on
	// io.MultiWriter would pass on a tee to the wrong pair of writers. Assert
	// the behaviour instead: one write has to land in BOTH places, because
	// sending it to only one is how either the rendering or the warnings go
	// missing.
	const line = "warning: coordinator did not launch\n"
	if _, err := sub.ErrOrStderr().Write([]byte(line)); err != nil {
		t.Fatalf("write to the nested stderr: %v", err)
	}
	if !strings.Contains(sink.String(), line) {
		t.Fatalf("the nested refresh's stderr must reach the phase sink, got %q", sink.String())
	}
	if !strings.Contains(captured.String(), line) {
		t.Fatalf("the nested refresh's stderr must reach the capture, got %q", captured.String())
	}
	if sub.ErrOrStderr() == io.Writer(os.Stderr) {
		t.Fatal("the nested refresh must never write to the real stderr")
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
	// The tick row this used to inspect is gone: the terse block counts
	// components and defers the names to `doctor`, so there is no second
	// spelling in the rendered summary left to disagree with the first.
	//
	// The guarantee moved into setupComponentCountSummary, which is where it
	// is asserted now. Testing it through the rendered block would be a
	// substring search over prose -- "sessions" appears in the standing intro
	// sentence, so that search passes on output that names nothing at all.
	report := setupRenderFixtureReport()
	built, skipped, failed := partitionSetupComponents(report.Instant.Components)
	summary := setupComponentCountSummary(built, skipped, failed)

	named := false
	for _, component := range failed {
		label := setupComponentLabel(component.Name)
		if !strings.Contains(summary, label) {
			t.Fatalf("the count summary does not name %q by its label %q: %q", component.Name, label, summary)
		}
		named = true
		if label != component.Name && strings.Contains(summary, component.Name) {
			t.Fatalf("the count summary uses the raw id %q beside the label %q: %q", component.Name, label, summary)
		}
	}
	if !named {
		t.Fatalf("the fixture reported no failed component, so this guard checked nothing: %q", summary)
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
	// Readiness has its own row in the terse block; the first line is the
	// standing intro sentence, so asserting on it tested the wrong string.
	if strings.Contains(got, "queryable now") {
		t.Fatalf("no code index and no baseline is not a queryable brain:\n%s", got)
	}
	// The terse block names the lost CAPABILITY, not the component id -- that
	// is the vocabulary a reader of this block has. `--verbose` and `doctor`
	// carry the component names.
	if !strings.Contains(got, "code context unavailable") {
		t.Fatalf("the summary must name what is unavailable:\n%s", got)
	}
	if !strings.Contains(got, "could not be built") {
		t.Fatalf("the summary must say the brain did not build:\n%s", got)
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
		if !strings.Contains(got, "queryable now") {
			t.Fatalf("with %s built the brain still answers:\n%s", survivor, got)
		}
		if strings.Contains(got, "queries have nothing to read") {
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
	if strings.Contains(got, "queries have nothing to read") {
		t.Fatalf("no components reported is not evidence of failure:\n%s", got)
	}
}

// TestSetupSaysWhatIsUnavailableWhenNothingCanAnswer is the whole-run
// property. It used to assert exit 0 ("one component built, so the brain is
// worth keeping"), which main has since superseded: `setup --help` now states
// the contract outright -- setup exits 0 whenever the brain is QUERYABLE, and
// queryable means at least one of the semantic index, the repository context or the
// history index built. Session export is none of those.
//
// The half of this PR that stands is the other one, and it is what is asserted
// here: whatever the exit code, setup must not claim queryability it does not
// have, and must name what is missing.
func TestSetupSaysWhatIsUnavailableWhenNothingCanAnswer(t *testing.T) {
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
	err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatalf("no index and no baseline is not a queryable brain, so setup must not exit 0:\n%s", out.String())
	}
	// The failure has to NAME both missing sources. "setup failed" alone sends
	// the reader back to the output they already could not act on.
	for _, want := range []string{"repository context", "code index"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("setup must name what is unavailable (%q): %v", want, err)
		}
	}
	if strings.Contains(out.String(), "queryable now") {
		t.Fatalf("setup claimed queryability with no index and no baseline:\n%s", out.String())
	}
}

// --- UX-26: status holds the reason, so status prints it --------------------

func renderStatusShortPlain(t *testing.T, report brainStatusReport) string {
	t.Helper()
	out := &bytes.Buffer{}
	theme, _ := tui.ThemeByName("default")
	renderBrainStatusShort(out, tui.NewRendererWith(theme, tui.PlainCaps()), report, base.SetupBrainBinaryName)
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
	remedy := setupDirtyWorktreeHint(base.SetupBrainBinaryName)
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
	if !strings.Contains(got, "repository context=dirty-unindexed") || !strings.Contains(got, seed.Detail) {
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
	remedy := setupDirtyWorktreeHint(base.SetupBrainBinaryName)
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
	remedy := setupDirtyWorktreeHint(base.SetupBrainBinaryName)
	for _, want := range []string{"commit or stash", base.SetupBrainBinaryName + " setup", "refresh --worktree", "entire enable"} {
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
	// The remedy now travels ON the axis, so the fixture carries it the way
	// a producer does. Rendering it from the state alone is what left the
	// JSON report -- which is what an agent and every script read -- holding
	// a cause with no next step.
	renderFreshnessAxes(&buf, map[string]staleAxis{
		"seed": {
			State:  dirtyUnindexedAxisState,
			Detail: "seed and docs are based on committed HEAD",
			Remedy: setupDirtyWorktreeHint("entire brain"),
		},
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
	// Hints are PHASE DETAILS, not terse-block rows: the terse block counts
	// failures and sends the reader to doctor. setupComponentFailureDetails is
	// where the dedup lives, so it is asserted there rather than through a
	// renderer that never prints a hint at all.
	_, _, failed := partitionSetupComponents(report.Instant.Components)
	out := strings.Join(setupComponentFailureDetails(failed, "--", base.SetupBrainBinaryName), "\n")

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
	_, _, failed := partitionSetupComponents(report.Instant.Components)
	out := strings.Join(setupComponentFailureDetails(failed, "--", base.SetupBrainBinaryName), "\n")
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

// TestEveryDirtyUnindexedAxisShipsItsRemedy closes the gap the renderer-only
// fix left: an agent reading `status --json` got the diagnosis and no way
// out, because only the text surface re-attached the hint.
//
// It walks the PRODUCERS, so adding a third dirty-unindexed axis without a
// remedy fails here rather than silently reaching the JSON consumers.
func TestEveryDirtyUnindexedAxisShipsItsRemedy(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"semantic.go", "agent_surface.go"} {
		src := readGoSourceForTest(t, file)
		rest := src
		for {
			i := strings.Index(rest, "State:  dirtyUnindexedAxisState")
			if i < 0 {
				i = strings.Index(rest, "State:   dirtyUnindexedAxisState")
			}
			if i < 0 {
				break
			}
			// The literal ends at the first line that closes it.
			end := strings.Index(rest[i:], "\n\t\t\t}")
			if end < 0 {
				end = strings.Index(rest[i:], "\n\t\t}")
			}
			if end < 0 {
				end = len(rest) - i
			}
			literal := rest[i : i+end]
			if !strings.Contains(literal, "Remedy:") {
				t.Errorf("%s: a dirty-unindexed axis ships with no remedy, so `status --json` states a cause and no next step:\n%s", file, literal)
			}
			rest = rest[i+end:]
		}
	}
}

// And the JSON side specifically: the field must survive encoding, because
// that is the surface the finding was about.
func TestTheRemedyReachesTheJSONReport(t *testing.T) {
	t.Parallel()
	axis := staleAxis{
		State:  dirtyUnindexedAxisState,
		Detail: "semantic index is based on committed HEAD",
		Remedy: setupDirtyWorktreeHint("entire brain"),
	}
	encoded, err := json.Marshal(axis)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	remedy, _ := back["remedy"].(string)
	if !strings.Contains(remedy, "--worktree") {
		t.Fatalf("an agent reading the report gets no next step; remedy=%q in %s", remedy, encoded)
	}
	// Additive: a clean axis must not grow an empty key.
	clean, err := json.Marshal(staleAxis{State: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "remedy") {
		t.Fatalf("remedy must be omitempty so existing readers are unaffected: %s", clean)
	}
}

// setupRenderFixtureReport and renderSetupSummaryPlain used to live in
// setup_render_test.go, beside the long closing block this PR extended. That
// renderer is gone (#401 replaced it with the phase + terse output), so they
// are rebuilt here against the CURRENT renderer.
//
// The point of rebuilding rather than deleting: these tests assert substance,
// not layout -- that an unqueryable brain is never called ready, that a
// component is called by one name, that a remedy is stated once. Every one of
// those claims is still about what a reader sees, and the reader now sees the
// terse block. Retargeting keeps the claims; deleting would have dropped them
// with the renderer that happened to carry them.
func setupRenderFixtureReport() setupReport {
	return setupReport{
		SchemaVersion: 1,
		GeneratedAt:   setupTestNow,
		Repo:          "/repo",
		RepoKey:       "gh/demo-org/brain-demo",
		BrainPath:     "/brain",
		Instant: setupPhase{
			State:   "degraded",
			Seconds: 0.7,
			Components: []setupComponent{
				{Name: brainComponentSessions, State: "failed", Detail: "checkpoint_scope_incomplete", Hint: "add a git remote"},
				{Name: brainComponentSeed, State: "ok"},
				{Name: brainComponentDocs, State: "ok"},
				{Name: brainComponentSemantic, State: "ok"},
			},
		},
		Backfill:  setupPhase{State: "skipped", Detail: "--no-backfill"},
		Workspace: setupWorkspaceState{Name: "default", Registered: true},
		Daemon:    daemonState{Manager: daemonManagerUnsupported, Detail: "skipped: --no-daemon"},
		Facts:     factsBackfillStatus{Sessions: 618, Distilled: 300},
	}
}

// renderSetupSummaryPlain renders the closing block a reader actually gets, in
// plain mode. Queryability is derived from the report rather than passed, so a
// test cannot assert "not ready" while handing the renderer a ready flag.
func renderSetupSummaryPlain(t *testing.T, report setupReport, timings *setupTimings) string {
	t.Helper()
	out := &bytes.Buffer{}
	theme, _ := tui.ThemeByName("default")
	queryable := len(setupUnqueryableCore(report.Instant.Components)) == 0
	terse := terseReportFor(report, timings, queryable, defaultSetupOptions(), base.SetupBrainBinaryName)
	setupui.RenderTerse(out, setupui.NewRendererWidth(tui.NewRendererWith(theme, tui.PlainCaps()), 200), terse)
	return out.String()
}

// TestQueryabilityClauseNeverOverclaims covers the --verbose phase line, which
// no other test in this file reaches: renderSetupSummaryPlain derives
// queryability itself, and the whole-run test asserts on the error. Removing
// the guard left both green, so it is asserted directly here.
func TestQueryabilityClauseNeverOverclaims(t *testing.T) {
	t.Parallel()
	ok := []setupComponent{{Name: brainComponentSeed, State: "ok"}, {Name: brainComponentSemantic, State: "ok"}}
	if got := setupQueryabilityClause(ok); !strings.Contains(got, "queryable now") {
		t.Fatalf("a built core is queryable: %q", got)
	}
	none := []setupComponent{
		{Name: brainComponentSeed, State: "failed", Detail: "dirty_worktree"},
		{Name: brainComponentSemantic, State: "failed", Detail: "dirty_worktree"},
	}
	got := setupQueryabilityClause(none)
	if strings.Contains(got, "queryable now") {
		t.Fatalf("no index and no baseline is not queryable: %q", got)
	}
	for _, want := range []string{"code index", "repository context"} {
		if !strings.Contains(got, "no "+want) {
			t.Fatalf("the clause must name what is unavailable (%q): %q", want, got)
		}
	}
}
