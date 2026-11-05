package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/setupui"
	"github.com/entireio/entire-brain/internal/tui"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestSetupShortOutputArrivesAsEachPhaseCompletes(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.yes = true
	opts.backfillBudget = 2
	var out bytes.Buffer
	steps := rec.steps(f)
	assertOutput := func(want []string, absent []string) {
		t.Helper()
		for _, text := range want {
			if strings.Count(out.String(), text) != 1 {
				t.Errorf("want %q exactly once at this stage:\n%s", text, out.String())
			}
		}
		for _, text := range absent {
			if strings.Contains(out.String(), text) {
				t.Errorf("%q appeared before its phase completed:\n%s", text, out.String())
			}
		}
	}
	intro := "Brain turns this repo's history"
	built := "Brain built — queryable now"
	distill := "Distilling 2 of 3 sessions in the background"
	watcher := "Watcher already running"
	detect := steps.detectAgent
	steps.detectAgent = func(ctx context.Context) string {
		if !strings.HasPrefix(out.String(), "\nBrain turns") {
			t.Errorf("introduction must start after a blank line: %q", out.String())
		}
		assertOutput([]string{intro}, []string{built, distill, watcher})
		return detect(ctx)
	}
	instant := steps.instant
	steps.instant = func(ctx context.Context) ([]setupComponent, error) {
		assertOutput([]string{intro}, []string{built, distill, watcher})
		return instant(ctx)
	}
	spawn := steps.spawnBackfill
	steps.spawnBackfill = func(ctx context.Context, plan setupBackfillPlan) (int, error) {
		assertOutput([]string{intro, built}, []string{distill, watcher})
		return spawn(ctx, plan)
	}
	install := steps.install
	steps.install = func(ctx context.Context, plan daemonPlan) error {
		assertOutput([]string{intro, built, distill}, []string{watcher})
		return install(ctx, plan)
	}
	if err := runSetup(context.Background(), setupTestCommand(t, &out, opts), f.opts, opts, f.repoDir, steps); err != nil {
		t.Fatal(err)
	}
	if rec.spawnCalls != 1 || rec.installCalls != 1 {
		t.Fatalf("expected backfill and watcher phases: %+v", rec)
	}
	assertOutput([]string{intro, built, distill, watcher}, nil)
	if !strings.HasSuffix(out.String(), "\n\n") {
		t.Errorf("setup must leave a blank line before the shell prompt: %q", out.String())
	}
}

func TestSetupShortTaskShowsWorkWithoutVerbosePhaseBar(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	progress := newTestProgress(t, &out, ttyCaps())
	sink := newSetupSink(progress, true)
	sink.StartTask("Building Brain…")
	defer sink.Suspend()
	// Nested refresh can report a substep while holding the terminal lock.
	updated := make(chan struct{})
	go func() {
		progress.live.emitAround(func() { sink.BuildStep("code index: parsing sources") })
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		t.Fatal("updating the short task deadlocked the terminal")
	}
	line := stripANSI(sink.task.liveLine())
	if !strings.HasPrefix(line, "  ") || !strings.Contains(line, "Building Brain — indexing code… (") {
		t.Fatalf("expected an indented active task with elapsed time: %q", line)
	}
	if strings.ContainsAny(line, "█░") || strings.Contains(line, "Build     ") {
		t.Fatalf("verbose phase bar leaked into short mode: %q", line)
	}
	sink.Suspend()
	if got := visibleLines(out.String()); len(got) != 0 {
		t.Fatalf("active task must disappear rather than become a permanent row: %q", got)
	}
}

func TestSetupShortBuildStepLabels(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"export sessions":                     "reading past AI sessions",
		"repository context: inventory":       "scanning repository files",
		"history index: 2/3 sessions scanned": "indexing session history",
		"doc index":                           "indexing documentation",
		"code index: parsing sources":         "indexing code",
		"reclassify facts":                    "organizing existing memories",
		"pattern layer":                       "finding patterns in past sessions",
		"entity index":                        "indexing code changes",
		"memory coordinator":                  "updating memory state",
		"code index: current skipped":         "",
		"doc index: 10 chunks done":           "",
		"warning: semantic index failed":      "",
		"refreshed brain: /tmp/brain":         "",
	} {
		if got := setupShortBuildLabel(input); got != want {
			t.Errorf("%q: got %q, want %q", input, got, want)
		}
	}
}

func TestSetupShortWarningSurvivesTransientTask(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	sink := newSetupSink(newTestProgress(t, &out, ttyCaps()), true)
	sink.StartTask("Saving setup…")
	sink.ShortWarning("Setup settings could not be saved: permission denied", "entire brain")
	sink.Suspend()
	lines := visibleLines(out.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "permission denied") || !strings.Contains(lines[0], "entire brain setup --verbose") {
		t.Fatalf("failed transient phase must leave its reason and remedy: %q", lines)
	}
}

func TestSetupShortOutputReportsBuildFailureWithoutLaterResults(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{instantErr: errors.New("build failed")}
	opts := defaultSetupOptions()
	var out bytes.Buffer
	err := runSetup(context.Background(), setupTestCommand(t, &out, opts), f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatal("expected build failure")
	}
	for _, want := range []string{"Brain turns this repo's history", "Brain could not be built", "doctor"} {
		if strings.Count(out.String(), want) != 1 {
			t.Errorf("want %q exactly once:\n%s", want, out.String())
		}
	}
	for _, absent := range []string{"queryable now", "Distilling", "Watcher"} {
		if strings.Contains(out.String(), absent) {
			t.Errorf("uncompleted phase reported %q:\n%s", absent, out.String())
		}
	}
}

// TestSetupDefaultIsTheShortForm pins the default. The phase view reports what
// the machine did and is still available behind --verbose; what a person gets
// by default is the four-line answer.
func TestSetupDefaultIsTheShortForm(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	out := runSetupTerseForTest(t, f, opts, rec)

	if !strings.Contains(out, "Brain turns this repo's history") {
		t.Fatalf("the short form opens by saying what Brain is for:\n%s", out)
	}
	if strings.Contains(out, "Ready. Try:") {
		t.Fatalf("the summary IS the output; no trailing hint after the status lines:\n%s", out)
	}
	// The phase view's vocabulary must be absent, or this is not the short form.
	for _, leaked := range []string{"Detect", "instant core", "FAILED", "Workspace"} {
		if strings.Contains(out, leaked) {
			t.Errorf("phase-view text %q leaked into the default output:\n%s", leaked, out)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n > 10 {
		t.Errorf("default output is %d lines; the point is that it is short:\n%s", n, out)
	}
}

func TestSetupTerseBackfillCountsOnlyPendingSessions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		budget    int
		distilled []string
		want      string
	}{
		{"capped", 1, []string{"s1"}, "Distilling 1 of 2 sessions in the background"},
		{"uncapped", 0, []string{"s1"}, "Distilling 2 sessions in the background"},
		{"last session", 1, []string{"s1", "s2"}, "Distilling 1 session in the background"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupTestFixture(t, "s1", "s2", "s3")
			f.markDistilled(t, tc.distilled...)
			rec := &recordedSetup{agent: "codex"}
			opts := defaultSetupOptions()
			opts.yes = true
			opts.noDaemon = true
			opts.backfillBudget = tc.budget

			out := runSetupTerseForTest(t, f, opts, rec)
			if rec.spawnCalls != 1 {
				t.Fatalf("expected one backfill, got %d", rec.spawnCalls)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("summary must exclude already distilled sessions; want %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestSetupVerboseStillReportsPhases keeps the escape hatch honest: --verbose
// has to bring back the thing the default hides, or hiding it is a loss.
func TestSetupVerboseStillReportsPhases(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	out := runSetupForTest(t, f, opts, rec) // forces verbose

	if !strings.Contains(out, "instant core") {
		t.Fatalf("--verbose must still report the phases:\n%s", out)
	}
}

// Explicit opt-in starts the backfill without adding a persistent cost line.
// Interactive disclosure stays in the consent prompt; verbose has diagnostics.
func TestSetupShortFormOmitsSpendAnnouncement(t *testing.T) {
	for _, flag := range []string{"backfill", "yes"} {
		t.Run(flag, func(t *testing.T) {
			f := newSetupTestFixture(t, "s1", "s2", "s3")
			rec := &recordedSetup{agent: "codex"}
			opts := defaultSetupOptions()
			opts.backfill = flag == "backfill"
			opts.yes = flag == "yes"
			opts.noDaemon = true
			opts.backfillBudget = 2
			opts.verbose = false
			out := &bytes.Buffer{}
			if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
				t.Fatalf("runSetup: %v", err)
			}
			if rec.spawnCalls != 1 {
				t.Fatalf("--%s should spawn one backfill, got %d", flag, rec.spawnCalls)
			}
			for _, unwanted := range []string{"fact backfill will", "SPEND TOKENS"} {
				if strings.Contains(out.String(), unwanted) {
					t.Fatalf("default output contains redundant announcement %q:\n%s", unwanted, out)
				}
			}
			if !strings.Contains(out.String(), "Distilling 2 of 3 sessions in the background") {
				t.Fatalf("default output must retain the budgeted result:\n%s", out)
			}
		})
	}
}

// TestDefaultModeStillHasATerminalForConsent is the guard for the worst bug in
// this change set.
//
// The first version suppressed the phase rows by handing newProgress
// io.Discard. io.Discard is not an *os.File, so caps collapsed to PlainCaps,
// the sink reported tty=false, and setupui.Consent took its `case !req.TTY`
// branch — returning "no terminal to ask" WITHOUT EVER PROMPTING. The consent
// gate, which is the entire reason setup asks before it spends, silently
// stopped existing in the default mode, and rebinding the prompt writer did
// nothing because no prompt was ever written.
//
// Capability detection must therefore come from the REAL stream even when the
// rows are thrown away. This asserts the sink's view of the terminal does not
// depend on whether rows are being kept.
func TestDefaultModeStillHasATerminalForConsent(t *testing.T) {
	t.Parallel()
	ttyCaps := tui.Caps{TTY: true, Color: true, Unicode: true, Width: 100}
	render := tui.NewRendererWith(mustTheme(t), ttyCaps)

	for _, quiet := range []bool{false, true} {
		progress := newProgressWithRenderer(&bytes.Buffer{}, "setup", render, newLiveTerminal())
		sink := newSetupSink(progress, quiet)
		if !sink.tty {
			t.Fatalf("quiet=%v: the sink lost the terminal; consent will refuse to prompt", quiet)
		}
		if got := sink.Renderer().Caps().Width; got != ttyCaps.Width {
			t.Errorf("quiet=%v: width came out %d, want the terminal's %d", quiet, got, ttyCaps.Width)
		}
		if !sink.Renderer().Caps().Unicode {
			t.Errorf("quiet=%v: unicode was lost, so the short form renders ASCII on a UTF-8 terminal", quiet)
		}
	}
}

func TestSetupPhaseProgressOnlyAppearsInVerboseMode(t *testing.T) {
	t.Parallel()
	for _, verbose := range []bool{false, true} {
		var out bytes.Buffer
		progress := newTestProgress(t, &out, ttyCaps())
		sink := newSetupSink(progress, !verbose)
		run := setupui.NewRun(sink, sink.Renderer())
		phase := run.Begin(setupui.PhaseBuild)
		phase.Step("6/9 pattern layer")
		phase.OK("built")

		if !verbose && out.Len() != 0 {
			t.Fatalf("default setup must hide live and settled phase output, got %q", out.String())
		}
		if verbose && !strings.Contains(out.String(), "Build") {
			t.Fatalf("verbose setup lost phase output: %q", out.String())
		}
	}
}

func mustTheme(t *testing.T) tui.Theme {
	t.Helper()
	theme, _ := tui.ThemeByName("default")
	return theme
}

// TestTerseDistillOutcomeReadsThePhaseNotTheCounts pins the second bug: the
// mapper branched on report.Facts.Running and .Agent, neither of which
// factsBackfillStatusForBrain ever sets. Every run with pending sessions
// therefore reported "no agent CLI to distill them" — immediately after the
// same command had printed "agent codex".
func TestTerseDistillOutcomeReadsThePhaseNotTheCounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		report setupReport
		want   setupui.DistillOutcome
	}{
		{
			name: "spawned",
			report: setupReport{
				Facts:    factsBackfillStatus{Sessions: 274},
				Backfill: setupPhase{State: "ok"},
			},
			want: setupui.DistillStarted,
		},
		{
			name: "attempted and broke",
			report: setupReport{
				Facts:    factsBackfillStatus{Sessions: 1},
				Backfill: setupPhase{State: "failed", Detail: "backfill state not recorded"},
			},
			want: setupui.DistillFailed,
		},
		{
			name: "human declined",
			report: setupReport{
				Facts:    factsBackfillStatus{Sessions: 274},
				Backfill: setupPhase{State: "skipped", consentWhy: setupui.ConsentReasonDeclined},
			},
			want: setupui.DistillDeclined,
		},
		{
			name: "genuinely no agent",
			report: setupReport{
				Facts:    factsBackfillStatus{Sessions: 274},
				Backfill: setupPhase{State: "skipped", consentWhy: setupui.ConsentReasonNoAgent},
			},
			want: setupui.DistillNoAgent,
		},
		{
			name: "no sessions",
			report: setupReport{
				Backfill: setupPhase{State: "skipped", consentWhy: setupui.ConsentReasonNoSessions},
			},
			want: setupui.DistillNoSessions,
		},
		{
			name: "--no-backfill",
			report: setupReport{
				Facts:    factsBackfillStatus{Sessions: 274},
				Backfill: setupPhase{State: "skipped", consentWhy: setupui.ConsentReasonNoBackfill},
			},
			want: setupui.DistillSkippedFlag,
		},
		{
			name: "skipped before consent, everything already distilled",
			report: setupReport{
				Facts:    factsBackfillStatus{Sessions: 274, Distilled: 274},
				Backfill: setupPhase{State: "skipped", Detail: "all sessions already distilled"},
			},
			want: setupui.DistillAlreadyDone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := terseDistillOutcome(tc.report); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The specific misdirection, named: a run that used an agent must never tell
// the reader to install one.
func TestSpawnedBackfillIsNeverReportedAsMissingAgent(t *testing.T) {
	t.Parallel()
	report := setupReport{
		Facts:    factsBackfillStatus{Sessions: 274},
		Backfill: setupPhase{State: "ok", Detail: "pid 123, 274 of 274 pending session(s) queued"},
	}
	if got := terseDistillOutcome(report); got == setupui.DistillNoAgent {
		t.Fatal("a backfill that SPAWNED reported as having no agent CLI")
	}
}

// TestProgressWriterIsOnlySilencedByJSON guards the decision that broke.
//
// The sink derives tty from the progress reporter's writer, and
// setupui.Consent refuses to prompt without a tty. So silencing the default
// short form by passing io.Discard here does not just hide rows — it tells
// the whole run there is no terminal, and the consent gate stops asking
// before it spends. Rows are suppressed at the sink; this writer is not the
// lever for that.
func TestProgressWriterIsOnlySilencedByJSON(t *testing.T) {
	t.Parallel()
	real := &bytes.Buffer{}

	if got := setupProgressWriter(real, false); got != io.Writer(real) {
		t.Fatal("default mode must keep the real writer, or terminal detection dies and consent never prompts")
	}
	if got := setupProgressWriter(real, true); got != io.Discard {
		t.Fatalf("--json must silence the reporter to keep stdout parseable, got %T", got)
	}
}

// TestConnectOfferSurvivesTheShortForm is the merge guard for COR-2112.
//
// Hosted sync is opt-in and `connect` is the only way in, so main put it in
// setup's closing Next block. The short form replaced that block as the
// DEFAULT output, which would have retired the only announcement the feature
// has without anyone deciding to. These three assertions are the whole
// contract: offered when nothing is bound, silent once something is, and
// silent again when the person has said no.
func TestConnectOfferSurvivesTheShortForm(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()

	if !setupOffersConnect(brainDir) {
		t.Fatal("a repo with no binding has never been told `connect` exists; it must be offered")
	}

	// The rendered line, not just the flag: a bool nobody prints is not a
	// feature.
	out := renderTerseFor(t, setupui.TerseReport{Queryable: true, BrainCmd: "entire brain", OfferConnect: true})
	if !strings.Contains(out, "entire brain connect") {
		t.Fatalf("the short form must name the command:\n%s", out)
	}
	if off := renderTerseFor(t, setupui.TerseReport{Queryable: true, BrainCmd: "entire brain"}); strings.Contains(off, "connect") {
		t.Fatalf("a bound repo must not be nagged:\n%s", off)
	}

	// And the wiring between the two. Asserting the predicate and the
	// renderer separately leaves the line that joins them untested, which is
	// exactly how this got lost in the merge in the first place.
	report := setupReport{BrainPath: brainDir}
	if !terseReportFor(report, nil, true, defaultSetupOptions(), "entire brain").OfferConnect {
		t.Fatal("terseReportFor must carry the offer, not just be able to compute it")
	}

	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: "https://cell.api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if setupOffersConnect(brainDir) {
		t.Fatal("a repo that is already connected must not be told to connect")
	}

	if _, err := brainstore.SetHostedOptOut(brainDir); err != nil {
		t.Fatal(err)
	}
	if setupOffersConnect(brainDir) {
		t.Fatal("an explicit opt-out is a decision, not a prompt to re-ask")
	}
}

// TestUpToDateRerunDoesNotRepeatTheConnectOffer keeps the discovery a
// discovery. The one-line re-run is the output a person sees most often, and
// a sales line on every single run is the noise this whole PR removes.
func TestUpToDateRerunDoesNotRepeatTheConnectOffer(t *testing.T) {
	t.Parallel()
	out := renderTerseFor(t, setupui.TerseReport{
		Queryable:    true,
		UpToDate:     true,
		BrainCmd:     "entire brain",
		OfferConnect: true,
	})
	if strings.Contains(out, "connect") {
		t.Fatalf("an up-to-date re-run must stay one line:\n%s", out)
	}
}

// renderTerseFor renders the short form into a string.
func renderTerseFor(t *testing.T, rep setupui.TerseReport) string {
	t.Helper()
	var buf bytes.Buffer
	theme, _ := tui.ThemeByName("default")
	setupui.RenderTerse(&buf, setupui.NewRendererWidth(tui.NewRendererWith(theme, tui.PlainCaps()), 200), rep)
	return buf.String()
}

// TestVerboseNextBlockStillNamesConnect holds the surface main actually
// changed. The short form is the default, but `--verbose` is what a person
// reaches for when setup did something they did not expect, and the Next
// block there is where `connect` was announced before this PR existed.
func TestVerboseNextBlockStillNamesConnect(t *testing.T) {
	t.Parallel()
	named := func(steps []setupui.NextStep) bool {
		for _, step := range steps {
			if strings.Contains(step.Command, "connect") {
				return true
			}
		}
		return false
	}
	if !named(setupNextSteps("entire brain", false, 0, true, true)) {
		t.Fatal("an unconnected repo must be offered `connect` in the Next block")
	}
	if named(setupNextSteps("entire brain", false, 0, false, true)) {
		t.Fatal("a connected or opted-out repo must not be offered it")
	}
}

// TestSetupStillRefreshesThePushHook is the second merge guard.
//
// main added the push-hook refresh as a flat block after the watcher; this PR
// replaced that whole region with phases, so the refresh had to move or be
// lost. Nothing on main asserted setup called it at all -- the block was only
// ever progress text -- which is precisely why it was one `--ours` away from
// disappearing. The AGENTS phase is where it lives now, and `--verbose` is
// where its line shows.
func TestSetupStillRefreshesThePushHook(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	out := runSetupForTest(t, f, opts, rec) // forces verbose

	if !strings.Contains(out, "push hook") {
		t.Fatalf("setup must still report what it did about the push hook:\n%s", out)
	}
	// And not in the default output: it is a detail, and details are what
	// --verbose is for.
	if terse := runSetupTerseForTest(t, newSetupTestFixture(t), opts, &recordedSetup{}); strings.Contains(terse, "push hook") {
		t.Fatalf("the short form must not carry hook plumbing:\n%s", terse)
	}
}

// --- the three review findings on this PR ---

// Finding 1. An empty repository is not a broken one.
//
// Setup returns early before the daemon is inspected or any source is built,
// so the normal short form rendered two statements nobody measured -- "Brain
// could not be built" and "Watcher unavailable" -- and sent the reader to
// `doctor` for a condition one commit fixes.
func TestAnEmptyRepositoryIsNotReportedAsBroken(t *testing.T) {
	t.Parallel()
	out := renderTerseFor(t, setupui.TerseReport{
		BrainCmd:     "entire brain",
		NoCommitsYet: true,
		// Exactly the half-filled shape the early return produces.
		Queryable: false,
		Watcher:   setupui.WatcherUnavailable,
	})

	if !strings.Contains(out, "no commits") {
		t.Errorf("the one true reason must be stated:\n%s", out)
	}
	if !strings.Contains(out, "Make a commit") {
		t.Errorf("the one action must be stated:\n%s", out)
	}
	for _, lie := range []string{"could not be built", "Watcher", "doctor"} {
		if strings.Contains(out, lie) {
			t.Errorf("nothing measured the %q claim on this path:\n%s", lie, out)
		}
	}
}

// Finding 2. A component with no reader-facing label is still a failure.
//
// UpToDate was computed from len(Unavailable), and an unlabelled component
// (`entities`, say) contributes nothing to it -- so a build that lost one
// collapsed to the single line "Brain up to date" and reported a degraded
// brain as a clean one.
func TestAnUnlabelledComponentFailureIsStillAFailure(t *testing.T) {
	t.Parallel()
	report := setupReport{
		Instant: setupPhase{Components: []setupComponent{
			{Name: "semantic", State: "ok"},
			{Name: "entities", State: "failed", Detail: "provider exited 1"},
		}},
	}
	terse := terseReportFor(report, nil, true, defaultSetupOptions(), "entire brain")

	if terse.UnnamedFailures != 1 {
		t.Fatalf("an unlabelled failure must be counted, got %d", terse.UnnamedFailures)
	}
	// Every OTHER up-to-date condition deliberately satisfied, so the only
	// thing that can keep this false is the unnamed failure. Without that,
	// the assertion passes through Sessions==0 and proves nothing.
	clean := setupReport{
		Instant: setupPhase{Components: []setupComponent{
			{Name: "semantic", State: "ok"},
			{Name: "entities", State: "failed", Detail: "provider exited 1"},
		}},
		Facts:  factsBackfillStatus{Sessions: 4, Distilled: 4},
		Daemon: daemonState{Installed: true, Running: true},
	}
	upToDate := terseReportFor(clean, nil, true, defaultSetupOptions(), "entire brain")
	if upToDate.Watcher != setupui.WatcherAlreadyRunning || clean.Facts.Pending() != 0 {
		t.Fatalf("fixture no longer satisfies the other conditions (watcher=%v pending=%d), so this proves nothing",
			upToDate.Watcher, clean.Facts.Pending())
	}
	if upToDate.UpToDate {
		t.Fatal("a build that lost a component is not up to date, however healthy the rest is")
	}
	if terse.UpToDate {
		t.Fatal("a build that lost a component is not up to date")
	}
	out := renderTerseFor(t, setupui.TerseReport{
		BrainCmd: "entire brain", Queryable: true, UnnamedFailures: 1,
	})
	if !strings.Contains(out, "did not build") {
		t.Errorf("the short form must say something did not build:\n%s", out)
	}
	if !strings.Contains(out, "doctor") {
		t.Errorf("and point at the command that names it:\n%s", out)
	}
	// Not by its internal id: that is the reason it has no label.
	if strings.Contains(out, "entities") {
		t.Errorf("an internal component id is not something a reader can act on:\n%s", out)
	}
}

// Finding 3. The skips that happen BEFORE consent are not declines.
//
// They all fell through one default and were reported as "Distillation
// skipped — run `setup` again anytime". For a running backfill that
// contradicts the machine; for a paused brain it is advice that cannot work,
// because the next setup skips for the same reason.
func TestPreConsentSkipsAreNotReportedAsDeclines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		why     setupui.ConsentReason
		outcome setupui.DistillOutcome
		mustSay []string
		mustNot []string
	}{
		{setupui.ConsentReasonPaused, setupui.DistillPaused,
			[]string{"paused", "entire brain start"}, []string{"skipped — run"}},
		{setupui.ConsentReasonAlreadyRunning, setupui.DistillInFlight,
			[]string{"already running"}, []string{"skipped"}},
		{setupui.ConsentReasonAlreadyDistilled, setupui.DistillAlreadyDone,
			[]string{"already distilled"}, []string{"skipped"}},
		{setupui.ConsentReasonNoExecutable, setupui.DistillNoExecutable,
			[]string{"would not resolve"}, []string{"skipped"}},
	} {
		report := setupReport{Backfill: setupPhase{State: "skipped", consentWhy: tc.why}}
		// Pending sessions present: this is the shape that used to take the
		// counts-based default straight to DistillDeclined.
		report.Facts = factsBackfillStatus{Sessions: 9, Distilled: 1}

		if got := terseDistillOutcome(report); got != tc.outcome {
			t.Errorf("reason %v: got outcome %v, want %v", tc.why, got, tc.outcome)
			continue
		}
		out := renderTerseFor(t, setupui.TerseReport{
			BrainCmd: "entire brain", Queryable: true, Distill: tc.outcome,
			Sessions: 9, Distilled: 1,
		})
		for _, want := range tc.mustSay {
			if !strings.Contains(out, want) {
				t.Errorf("reason %v: line must contain %q:\n%s", tc.why, want, out)
			}
		}
		for _, unwanted := range tc.mustNot {
			if strings.Contains(out, unwanted) {
				t.Errorf("reason %v: line must not contain %q:\n%s", tc.why, unwanted, out)
			}
		}
	}
}

// And the wiring: the product's own skip sites must set those reasons, or
// the three cases above are unreachable and the test suite above is theatre.
func TestEverySkipSiteRecordsWhy(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("setup.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	// An untyped skip helper is how all four lost their reason the first
	// time. There must not be one to call.
	if strings.Contains(text, "func setupBackfillSkipped(") {
		t.Error("the untyped skip helper is back; every caller took it and defaulted to Unset")
	}
	for _, reason := range []string{
		"ConsentReasonPaused", "ConsentReasonAlreadyRunning",
		"ConsentReasonAlreadyDistilled", "ConsentReasonNoExecutable",
	} {
		if !strings.Contains(text, reason) {
			t.Errorf("no skip site records %s, so its short-form line is unreachable", reason)
		}
	}
}

// And the wiring for the empty repo: a short form that renders the right
// thing only when someone remembers to set the flag is one refactor from
// regressing. This drives `setup` in a real repository with no commits.
func TestSetupItselfFlagsTheEmptyRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// NOT t.TempDir(). Its cleanup is a strict RemoveAll, and on Windows a
	// directory cannot be removed while any handle into it is open -- this
	// path runs real `git` subprocesses and opens the brain's stores, so the
	// cleanup failed the test on content it had already asserted correctly.
	// The directory still goes; failing to remove it does not fail the run.
	repoDir, err := os.MkdirTemp("", "brain-empty-repo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
	gitAt(t, repoDir, "init", "-q")
	home, err := os.MkdirTemp("", "brain-empty-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	isolateDaemonEnv(t, home)
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}

	out, err := execute(t, NewRootCommand(opts), "setup", "--no-daemon", "--no-backfill")
	if err == nil {
		t.Fatalf("setup in a repository with no commits must still fail:\n%s", out)
	}
	if !strings.Contains(out, "no commits") {
		t.Errorf("the short form must name the condition:\n%s", out)
	}
	if !strings.Contains(out, "Make a commit") {
		t.Errorf("the short form must name the action:\n%s", out)
	}
	for _, lie := range []string{"could not be built", "Watcher unavailable"} {
		if strings.Contains(out, lie) {
			t.Errorf("nothing on this path measured %q:\n%s", lie, out)
		}
	}
}

// TestNextStepsLeadWithAgentWiringWhenItDidNotLand pins the one ordering that
// matters in the closing block. Every other next step assumes an agent can
// find this brain; until the agent instructions land, none can, so a run whose
// wiring failed has to name the re-run FIRST rather than third.
func TestNextStepsLeadWithAgentWiringWhenItDidNotLand(t *testing.T) {
	t.Parallel()
	wired := setupNextSteps("entire brain", false, 0, false, true)
	for _, step := range wired {
		if strings.Contains(step.Why, "AGENTS.md") {
			t.Fatalf("wiring landed; the re-run step must not be offered: %+v", wired)
		}
	}
	unwired := setupNextSteps("entire brain", false, 0, false, false)
	if len(unwired) == 0 || !strings.Contains(unwired[0].Command, "setup") {
		t.Fatalf("wiring did not land; the setup re-run must come first, got: %+v", unwired)
	}
	if len(unwired) != len(wired)+1 {
		t.Fatalf("the re-run step must be added, not substituted: wired=%d unwired=%d", len(wired), len(unwired))
	}
	// And it must say WHAT it writes, or it reads as one more optional command.
	// The reported case was a user who ran setup, found no agent files, and
	// concluded they had run it from the wrong directory. They had not.
	if !strings.Contains(unwired[0].Why, "AGENTS.md") {
		t.Fatalf("the re-run row does not say what it writes: %q", unwired[0].Why)
	}
}
