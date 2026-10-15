package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/setupui"
	"github.com/entireio/entire-brain/internal/tui"
)

// setup_ui_test.go covers the seven-phase wiring: the properties that only
// exist once setupui is driven by the real command, as opposed to the model
// properties internal/setupui asserts about itself.

// failOnReadStdin fails the test if anything reads it. "Does not block without
// a terminal" is a far weaker claim than "does not read stdin at all", and the
// weaker one is satisfied by a prompt that happens to get EOF in CI.
type failOnReadStdin struct{ t *testing.T }

func (r failOnReadStdin) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Fatal("setup read stdin on a run with no terminal; a prompt in CI is a wedged pipeline")
	return 0, io.EOF
}

// TestSetupSpendsNothingWithoutATerminalOrYes is the consent gate's whole
// point. Distilling was opt-OUT, so a first run in CI distilled unless the
// operator knew about --no-backfill.
func TestSetupSpendsNothingWithoutATerminalOrYes(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, opts)
	cmd.SetIn(failOnReadStdin{t})
	if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if rec.spawnCalls != 0 {
		t.Fatalf("silence is not consent: a run with no terminal must spend nothing, spawned %d", rec.spawnCalls)
	}
	for _, want := range []string{"no terminal to ask", "--yes"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the skip must say why it skipped and how to spend on purpose, missing %q:\n%s", want, out)
		}
	}
	if _, ok := readSetupBackfillState(filepath.Dir(f.storage.HeadPath)); ok {
		t.Fatal("a backfill that never ran must leave no state behind")
	}
}

// TestSetupSpendsWhenToldYes is the other half: --yes is the ONE way to spend
// without a human, and the cost is still on screen before the child starts.
func TestSetupSpendsWhenToldYes(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	rec := &recordedSetup{agent: "codex"}
	steps := rec.steps(f)
	out := &bytes.Buffer{}
	var atSpawn string
	inner := steps.spawnBackfill
	steps.spawnBackfill = func(ctx context.Context, plan setupBackfillPlan) (int, error) {
		atSpawn = out.String()
		return inner(ctx, plan)
	}

	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.yes = true
	cmd := setupTestCommand(t, out, opts)
	cmd.SetIn(failOnReadStdin{t})
	if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, steps); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	if rec.spawnCalls != 1 {
		t.Fatalf("--yes must spend, spawned %d", rec.spawnCalls)
	}
	if !strings.Contains(atSpawn, "SPEND TOKENS") {
		t.Fatalf("the cost must be on screen BEFORE the detached child starts; all the user had was:\n%s", atSpawn)
	}
}

// TestSetupNeverPromptsWhenThereIsNothingToSpendOn: a prompt a reader cannot
// answer usefully is worse than no prompt. Zero sessions and no agent are both
// "spending is impossible", and both must report that instead of asking.
func TestSetupNeverPromptsWhenThereIsNothingToSpendOn(t *testing.T) {
	for name, build := range map[string]func(*testing.T) (*setupTestFixture, *recordedSetup){
		"no sessions": func(t *testing.T) (*setupTestFixture, *recordedSetup) {
			return newSetupTestFixture(t), &recordedSetup{agent: "codex"}
		},
		"no agent": func(t *testing.T) (*setupTestFixture, *recordedSetup) {
			return newSetupTestFixture(t, "s1"), &recordedSetup{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, rec := build(t)
			opts := defaultSetupOptions()
			opts.noDaemon = true
			out := &bytes.Buffer{}
			cmd := setupTestCommand(t, out, opts)
			cmd.SetIn(failOnReadStdin{t})
			if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
				t.Fatalf("runSetup: %v", err)
			}
			if strings.Contains(out.String(), "Distill now?") {
				t.Fatalf("nothing to spend on, so nothing to ask:\n%s", out)
			}
			if rec.spawnCalls != 0 {
				t.Fatalf("spawned %d", rec.spawnCalls)
			}
		})
	}
}

// TestSetupBuildPhaseGivesNoSubStepItsOwnLine is the anti-pytest invariant at
// the command level. Nine components produced nine marks beside the spinner,
// nine names in the completion sentence and nine more in the summary; now the
// phase is one row and the names appear at most once, inside it.
func TestSetupBuildPhaseGivesNoSubStepItsOwnLine(t *testing.T) {
	f := newSetupTestFixture(t)
	components := []setupComponent{}
	for _, name := range []string{
		brainComponentSessions, brainComponentSeed, brainComponentHistory, brainComponentDocs,
		brainComponentSemantic, brainComponentFacts, brainComponentPatterns,
		brainComponentEntities, brainComponentMemory,
	} {
		components = append(components, newSetupComponent(name, nil, setupBrainBinaryName))
	}
	rec := &recordedSetup{instantComponents: components}
	steps := rec.steps(f)
	// The fixture's injected instant returns a slice and reports NOTHING as it
	// goes, so asserting on sub-steps against it would assert on an empty set:
	// the leak this test exists to catch could not happen. Drive the two
	// channels the real phase uses instead — the component observer and the
	// nested reporter's byte stream.
	observer := &setupComponentObserver{}
	chatter := &setupStepSink{}
	steps.observeComponent = observer
	steps.stepOutput = chatter
	steps.instant = func(context.Context) ([]setupComponent, error) {
		rec.instantCalls++
		fmt.Fprintln(chatter, "refresh: locate brain")
		for _, component := range components {
			fmt.Fprintf(chatter, "refresh: %s: building\n", setupComponentLabel(component.Name))
			observer.notify(component)
		}
		fmt.Fprintln(chatter, "refreshed brain: /somewhere/else")
		return components, nil
	}

	opts := defaultSetupOptions()
	opts.noDaemon = true
	out := &bytes.Buffer{}
	if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, steps); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	printed := out.String()

	// Counted as ROWS whose phase column is Build, not as occurrences of the
	// word: the fixture's own temp directory is named after this test, so a
	// substring count would pass for the wrong reason.
	if got := setupPhaseRows(printed, "Build"); got != 1 {
		t.Fatalf("the Build phase must cost exactly one row, got %d:\n%s", got, printed)
	}
	// Non-vacuity: the sub-steps really were reported.
	if len(observer.seen()) == 0 {
		t.Fatal("the fixture reported no sub-steps, so this test proves nothing")
	}
	for _, leaked := range []string{"doc index", "pattern layer", "memory coordinator", "locate brain", "refreshed brain"} {
		if strings.Contains(printed, leaked) {
			t.Fatalf("sub-step %q reached a permanent line; it belongs to the phase's bar:\n%s", leaked, printed)
		}
	}
	if !strings.Contains(printed, "9 components") {
		t.Fatalf("the phase line must say how much built:\n%s", printed)
	}
}

// TestSetupExitsNonZeroWhenTheBrainCannotAnswer is the exit-code contract.
// `setup --help` promised exit 0 "whenever the brain is queryable -- that is,
// whenever at least one instant-phase component built". On a dirty worktree
// the seed, semantic and history components all refuse while docs and
// entities build, so the proxy said 0 for a brain with nothing to answer from
// and `setup && query` proceeded on it.
func TestSetupExitsNonZeroWhenTheBrainCannotAnswer(t *testing.T) {
	f := newSetupTestFixture(t)
	dirty := errors.New(dirtyWorktreeErrorCode + ": refusing to index uncommitted content")
	rec := &recordedSetup{instantComponents: []setupComponent{
		newSetupComponent(brainComponentDocs, nil, setupBrainBinaryName),
		newSetupComponent(brainComponentEntities, nil, setupBrainBinaryName),
		newSetupComponent(brainComponentMemory, nil, setupBrainBinaryName),
		newSetupComponent(brainComponentSeed, dirty, setupBrainBinaryName),
		newSetupComponent(brainComponentSemantic, dirty, setupBrainBinaryName),
		newSetupComponent(brainComponentHistory, dirty, setupBrainBinaryName),
	}}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	out := &bytes.Buffer{}

	err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatalf("a brain that cannot answer must not report success:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not queryable") || !strings.Contains(err.Error(), "seed baseline") {
		t.Fatalf("the exit must name what could not be built: %v", err)
	}
	if !strings.Contains(out.String(), "the brain cannot answer yet") {
		t.Fatalf("the verdict must agree with the exit code:\n%s", out)
	}
}

// TestSetupExitsZeroWhenOnlyASupportingComponentFails pins the other side of
// the boundary: a doc or entity index that failed beside a semantic index
// that built is a degraded brain, and a degraded brain still answers.
func TestSetupExitsZeroWhenOnlyASupportingComponentFails(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{instantComponents: []setupComponent{
		newSetupComponent(brainComponentSemantic, nil, setupBrainBinaryName),
		newSetupComponent(brainComponentDocs, errors.New("doc index unavailable"), setupBrainBinaryName),
		newSetupComponent(brainComponentEntities, errors.New("entity index unavailable"), setupBrainBinaryName),
	}}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	out := &bytes.Buffer{}

	if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("a degraded brain still answers and must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "the brain is queryable") {
		t.Fatalf("the verdict must say the brain answers:\n%s", out)
	}
}

// TestSetupBrainQueryableIgnoresSupportingComponents is the rule itself, per
// component, so a future component added to the content set has to be added
// here on purpose.
func TestSetupBrainQueryableIgnoresSupportingComponents(t *testing.T) {
	t.Parallel()
	ok := func(name string) setupComponent { return setupComponent{Name: name, State: "ok"} }
	bad := func(name string) setupComponent { return setupComponent{Name: name, State: "failed"} }
	cases := []struct {
		name       string
		components []setupComponent
		want       bool
	}{
		{"no components is the pre-component contract", nil, true},
		{"semantic alone answers", []setupComponent{ok(brainComponentSemantic), bad(brainComponentDocs)}, true},
		{"seed alone answers", []setupComponent{ok(brainComponentSeed)}, true},
		{"history alone answers", []setupComponent{ok(brainComponentHistory)}, true},
		{"docs and entities alone do not", []setupComponent{
			ok(brainComponentDocs), ok(brainComponentEntities), ok(brainComponentMemory),
			bad(brainComponentSeed), bad(brainComponentSemantic), bad(brainComponentHistory),
		}, false},
		{"a failed content source does not count", []setupComponent{bad(brainComponentSemantic)}, false},
	}
	for _, tc := range cases {
		if got := setupBrainQueryable(tc.components); got != tc.want {
			t.Errorf("%s: setupBrainQueryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// unbornHeadRunner scripts the three probes that settle "this repository has
// no commits yet" the way git really answers them: rev-list FAILS on an
// unborn HEAD, so the count alone can never detect the condition.
func unbornHeadRunner(repoDir string) *fakeCommandRunner {
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                        {stdout: repoDir + "\n"},
		fakeCommandKey("git", "rev-list", "--count", "HEAD"):                         {err: errors.New("fatal: ambiguous argument 'HEAD'")},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "HEAD"):                     {stdout: "refs/heads/main\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", "refs/heads/main"): {err: errors.New("exit status 1")},
	}}
}

// TestSetupRepoFactsNeedsPositiveEvidenceOfAnUnbornHead: "no commits" skips
// every later phase and exits non-zero, so inferring it from a git call that
// merely failed would turn "git is not on PATH" into a confident wrong
// diagnosis that silently declines to set anything up.
func TestSetupRepoFactsNeedsPositiveEvidenceOfAnUnbornHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	facts, known := setupRepoFacts(ctx, unbornHeadRunner("/repo"), "/repo")
	if !known || facts.Commits != 0 {
		t.Fatalf("a readable repo whose branch does not resolve has no commits: %+v known=%v", facts, known)
	}

	// git answered nothing at all: unknown, NOT zero.
	silent := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	if _, known := setupRepoFacts(ctx, silent, "/repo"); known {
		t.Fatal("a repository git could not answer about must not be reported as empty")
	}

	// A detached HEAD has no symbolic ref and is not this condition.
	detached := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-list", "--count", "HEAD"):     {stdout: "13\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "HEAD"): {err: errors.New("exit status 1")},
	}}
	facts, known = setupRepoFacts(ctx, detached, "/repo")
	if !known || facts.Commits != 13 || !facts.Detached {
		t.Fatalf("a detached HEAD with commits must read as such: %+v known=%v", facts, known)
	}
}

// TestSetupEmptyRepositoryReportsAPreconditionNotFailures: a repo with no
// commits produced 47 lines and three red component failures to say "you have
// not committed anything yet", while a directory that is not a repository at
// all handled the same class of problem in two lines and handled it well.
func TestSetupEmptyRepositoryReportsAPreconditionNotFailures(t *testing.T) {
	f := newSetupTestFixture(t)
	f.opts.Runner = unbornHeadRunner(f.repoDir)
	rec := &recordedSetup{instantComponents: []setupComponent{
		newSetupComponent(brainComponentSeed, errors.New("no commits"), setupBrainBinaryName),
		newSetupComponent(brainComponentDocs, nil, setupBrainBinaryName),
	}}
	opts := defaultSetupOptions()
	out := &bytes.Buffer{}

	err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatalf("there is nothing to index, so the brain cannot answer:\n%s", out)
	}
	printed := out.String()
	if !strings.Contains(printed, "no commits yet, so there is nothing to index") {
		t.Fatalf("the condition must be stated once, plainly:\n%s", printed)
	}
	if !strings.Contains(printed, "make one commit, then:") {
		t.Fatalf("the one runnable action must be named:\n%s", printed)
	}
	if strings.Contains(printed, "failed") || strings.Contains(printed, "FAILED") {
		t.Fatalf("nothing is broken in an empty repository; it must not render as a failure:\n%s", printed)
	}
	// No side effects: an empty repository must not acquire a persistent
	// service or a workspace row it has nothing to put in.
	if rec.installCalls != 0 || rec.spawnCalls != 0 {
		t.Fatalf("an empty repository must not be given a daemon or a backfill: %+v", rec)
	}
	if lines := len(strings.Split(strings.TrimRight(printed, "\n"), "\n")); lines > 10 {
		t.Fatalf("the empty-repository report must stay short, got %d lines:\n%s", lines, printed)
	}
}

// TestSetupComponentFailureDetailsAreOneLineEach: a component's Detail is
// another process's stderr. The row budget governs WIDTH, so a newline inside
// one escapes it entirely and a three-line git refusal lands under the phase.
func TestSetupComponentFailureDetailsAreOneLineEach(t *testing.T) {
	t.Parallel()
	failed := []setupComponent{{
		Name:   brainComponentSeed,
		State:  "failed",
		Detail: "this repository has no commits yet: /repo\nentire-brain pins every source to a commit.\nmake at least one commit.",
		Hint:   "first line\nsecond line",
	}}
	details := setupComponentFailureDetails(failed, "--", setupBrainBinaryName)
	if len(details) != 2 {
		t.Fatalf("a failed component is allowed its reason and its hint, got %d: %q", len(details), details)
	}
	for _, detail := range details {
		if strings.ContainsAny(detail, "\n\r") {
			t.Fatalf("a failure detail must be one line: %q", detail)
		}
	}
	// Bounding the COMPOSED line instead of the detail clips the actionable
	// tail off exactly the long failures that need it.
	if !strings.HasSuffix(details[0], "continuing; run 'entire-brain doctor' for detail") {
		t.Fatalf("the reason must keep what the reader is supposed to do about it: %q", details[0])
	}
}

// TestSetupWarningsFromTheNestedRefreshSurvive: the nested reporter's output
// becomes a repaint, and a warning that only ever existed as a repaint has
// not been reported.
func TestSetupWarningsFromTheNestedRefreshSurvive(t *testing.T) {
	t.Parallel()
	sink := &setupStepSink{}
	fmt.Fprintf(sink, "refresh: semantic index: parsing sources\n")
	fmt.Fprintf(sink, "warning: memory coordinator: queue is stale\n")
	fmt.Fprintf(sink, "refresh: doc index done\n")
	got := sink.Warnings()
	if len(got) != 1 || got[0] != "memory coordinator: queue is stale" {
		t.Fatalf("the warning must survive, the chatter must not: %q", got)
	}
}

// TestSetupStepSinkDoesNotDeadlockInsideTheLiveTerminal is the regression for
// a deadlock that only a PTY reproduced: the nested refresh writes some of its
// output through printAroundLiveLine, which HOLDS the live terminal's lock
// while it writes, and the step sink routed that write into the phase's live
// line, which took the same non-reentrant lock again. Every real terminal run
// of `setup` hung there with the whole report still to print.
func TestSetupStepSinkDoesNotDeadlockInsideTheLiveTerminal(t *testing.T) {
	theme, _ := tui.ThemeByName("default")
	render := tui.NewRendererWith(theme, tui.Caps{TTY: true, Color: false, Unicode: false, Width: 80})
	live := newLiveTerminal()
	progress := newProgressWithRenderer(io.Discard, "setup", render, live)
	ui := newSetupSink(progress)
	if !ui.tty {
		t.Fatal("the fixture must model a terminal; there is no deadlock to reproduce without one")
	}
	run := setupui.NewRun(ui, ui.Renderer())
	phase := run.Begin(setupui.PhaseBuild)
	sink := &setupStepSink{}
	sink.bind(phase.Step)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Exactly the production stack: a writer invoked from inside the live
		// terminal's own emit.
		live.emitAround(func() { fmt.Fprintf(sink, "refresh: semantic index: parsing sources\n") })
		phase.OK("built")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a sub-step written from inside the live terminal deadlocked the run")
	}
}

// TestSetupRenderWidthDoesNotClipAPipe: setupui clips every row so a terminal
// line can never wrap. A pipe has no row to wrap, and the two rows the closing
// block exists to carry are paths longer than an 80-cell budget leaves room
// for, so clipping there deletes the answer rather than protecting it.
func TestSetupRenderWidthDoesNotClipAPipe(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	if got := setupRenderWidth(tui.Caps{}, none); got != setupPipeWidth {
		t.Fatalf("a pipe with no COLUMNS must not be clipped to a terminal width, got %d", got)
	}
	if got := setupRenderWidth(tui.Caps{TTY: true, Width: 40}, none); got != 40 {
		t.Fatalf("a terminal's own width wins, got %d", got)
	}
	columns := func(name string) (string, bool) {
		if name == "COLUMNS" {
			return "60", true
		}
		return "", false
	}
	if got := setupRenderWidth(tui.Caps{}, columns); got != 60 {
		t.Fatalf("an explicit COLUMNS must still bind a pipe, got %d", got)
	}
}

// TestSetupConsentOverrideMapsTheFlags pins which flag can spend. TWO flags
// can answer yes -- --yes, and --backfill, whose help promises it starts the
// spend "even if a previous run declined it"; after a remembered decline it is
// the only way back. --no-backfill wins every contradiction: the safe answer
// must win.
//
// --backfill matters more than it looks. The prompt defaults to No and never
// blocks without a TTY, so a --backfill that did NOT consent would silently do
// nothing in exactly the unattended runs the flag exists for.
func TestSetupConsentOverrideMapsTheFlags(t *testing.T) {
	t.Parallel()
	base := defaultSetupOptions()
	if got := setupConsentOverride(base); got != setupui.OverrideNone {
		t.Fatalf("a bare setup leaves the decision to the prompt, got %v", got)
	}
	yes := base
	yes.yes = true
	if got := setupConsentOverride(yes); got != setupui.OverrideYes {
		t.Fatalf("--yes must consent, got %v", got)
	}
	no := base
	no.noBackfill = true
	if got := setupConsentOverride(no); got != setupui.OverrideNo {
		t.Fatalf("--no-backfill must decline, got %v", got)
	}
	backfill := base
	backfill.backfill = true
	if got := setupConsentOverride(backfill); got != setupui.OverrideYes {
		t.Fatalf("--backfill is a typed demand to spend and must consent, got %v", got)
	}
	both := base
	both.yes, both.noBackfill = true, true
	if got := setupConsentOverride(both); got != setupui.OverrideNo {
		t.Fatalf("a contradiction must resolve to the answer that spends nothing, got %v", got)
	}
	bothBackfill := base
	bothBackfill.backfill, bothBackfill.noBackfill = true, true
	if got := setupConsentOverride(bothBackfill); got != setupui.OverrideNo {
		t.Fatalf("--backfill against --no-backfill must still resolve to no spend, got %v", got)
	}
}

// TestSetupBuildTotalNeverReadsPastFull: the bar's denominator is an estimate
// on a first run, and a gauge that reads 11/9 is worse than no gauge.
func TestSetupBuildTotalNeverReadsPastFull(t *testing.T) {
	t.Parallel()
	if got := setupBuildTotal(0, 3); got != setupEstimatedInstantComponents {
		t.Fatalf("a first run uses the estimate, got %d", got)
	}
	if got := setupBuildTotal(8, 4); got != 8 {
		t.Fatalf("a previous run's count sizes the bar exactly, got %d", got)
	}
	if got := setupBuildTotal(8, 11); got != 11 {
		t.Fatalf("the gauge may never read past full, got 11/%d", got)
	}
}

// setupPhaseRows counts the rows whose phase column holds title. A row is
// "<glyph> <title padded to the column width> <text>", so the title is the
// second whitespace-separated field of a row that has a glyph.
func setupPhaseRows(printed, title string) int {
	count := 0
	for _, line := range strings.Split(printed, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && len(fields[0]) == 1 && fields[1] == title {
			count++
		}
	}
	return count
}

// seen reports every component the observer forwarded, so a test can prove it
// actually had sub-steps to leak before asserting that none leaked.
func (o *setupComponentObserver) seen() []setupComponent {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]setupComponent(nil), o.observed...)
}

// TestSetupFactsLineKeepsTheDistilledCounter: the closing block that was
// deleted carried "300/618 sessions distilled" on its own row. That number is
// the one thing in it no phase line said, so the collapse had to keep it
// rather than hit a line target by dropping it.
func TestSetupFactsLineKeepsTheDistilledCounter(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3")
	f.markDistilled(t, "s1")
	rec := &recordedSetup{} // no agent CLI: skipped, and still counted
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if !strings.Contains(out, "1/3 sessions distilled") {
		t.Fatalf("the Facts line must still carry how much is done:\n%s", out)
	}
	if got := setupPhaseRows(out, "Facts"); got != 1 {
		t.Fatalf("and it must carry it as a clause, not as a second row, got %d:\n%s", got, out)
	}
}

// TestSetupFactsCounterSaysNothingAboutNothing: a repository with no captured
// sessions has no ratio, and "0/0 distilled" is noise on a line whose job is
// to state a reason.
func TestSetupFactsCounterSaysNothingAboutNothing(t *testing.T) {
	t.Parallel()
	theme, _ := tui.ThemeByName("default")
	rend := setupui.NewRendererWidth(tui.NewRendererWith(theme, tui.PlainCaps()), 200)
	if got := setupFactsCounter(rend, factsBackfillStatus{}); got != "" {
		t.Fatalf("no sessions means no counter, got %q", got)
	}
	if got := setupFactsCounter(rend, factsBackfillStatus{Sessions: 12, Distilled: 3}); !strings.Contains(got, "3/12 sessions distilled") {
		t.Fatalf("the counter must read as a ratio, got %q", got)
	}
}
