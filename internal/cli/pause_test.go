package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/pause"
	"github.com/entireio/entire-brain/internal/cli/command/watch"
	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"github.com/spf13/cobra"
)

func TestStopPausesAndStartResumesBackgroundWork(t *testing.T) {
	f := newSetupTestFixture(t)

	if out, err := execute(t, pause.NewStop(f.opts)); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if !clibase.BackgroundPaused(f.env) {
		t.Fatal("stop did not pause background work")
	}
	if out, err := execute(t, pause.NewStart(f.opts)); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	if clibase.BackgroundPaused(f.env) {
		t.Fatal("start did not resume background work")
	}
}

func TestStartWhenNotPausedIsANoOp(t *testing.T) {
	f := newSetupTestFixture(t)
	if out, err := execute(t, pause.NewStart(f.opts)); err != nil {
		t.Fatalf("start on a running machine must succeed: %v\n%s", err, out)
	}
}

// A forgotten `stop` otherwise looks like a brain that quietly went stale.
func TestStatusReportsThePause(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	report, err := buildBrainStatusReport(context.Background(), f.opts, f.repoDir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if report.Onboarding.Paused {
		t.Fatal("status reports a pause nobody asked for")
	}

	if err := clibase.SetBackgroundPaused(f.env, true); err != nil {
		t.Fatal(err)
	}
	report, err = buildBrainStatusReport(context.Background(), f.opts, f.repoDir)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !report.Onboarding.Paused {
		t.Fatal("status does not record the pause")
	}
	if got := renderStatus(t, report, false); !strings.Contains(got, "background work: paused") {
		t.Fatalf("status text does not show the pause:\n%s", got)
	}
	for _, warning := range report.Warnings {
		if strings.Contains(warning, "paused") {
			return
		}
	}
	t.Fatalf("status does not warn about the pause: %v", report.Warnings)
}

// The installed watcher is KeepAlive: killing it only restarts it. Pausing has
// to make the running watcher idle instead.
func TestSupervisedWatchIdlesWhilePaused(t *testing.T) {
	f := newSetupTestFixture(t)
	if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: "alpha", Interval: "5m"}); err != nil {
		t.Fatal(err)
	}
	if err := clibase.SetBackgroundPaused(f.env, true); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	base := watch.DefaultFlags()
	base.Once = true
	passes := 0
	if err := supervisedWatchLoop(context.Background(), out, f.env, base,
		func(string, watch.Flags, *int) error { passes++; return nil }, nil, nil); err != nil {
		t.Fatalf("a paused watcher must keep running, not exit: %v", err)
	}
	if passes != 0 {
		t.Fatalf("paused, but %d workspace passes ran", passes)
	}
	if !strings.Contains(out.String(), "paused") {
		t.Fatalf("the log must say why nothing ran: %q", out.String())
	}
}

// `stop` is most likely run while a pass is busy; the workspaces after the
// current one must not start their gated distill.
func TestSupervisedWatchStopsVisitingWorkspacesWhenPausedMidPass(t *testing.T) {
	f := newSetupTestFixture(t)
	for _, name := range []string{"alpha", "beta"} {
		if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: name, Interval: "5m"}); err != nil {
			t.Fatal(err)
		}
	}
	base := watch.DefaultFlags()
	base.Once = true
	var seen []string
	if err := supervisedWatchLoop(context.Background(), &bytes.Buffer{}, f.env, base,
		func(workspace string, _ watch.Flags, _ *int) error {
			seen = append(seen, workspace)
			return clibase.SetBackgroundPaused(f.env, true)
		}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("paused during alpha, but the pass went on to visit %v", seen)
	}
}

// `watch [path]` and `workspace watch <name>` bypass the supervised loop, and
// setup tells users on platforms without a service manager to run the latter
// themselves, so each tick has to honour the pause on its own.
func TestWatchTickDoesNothingWhilePaused(t *testing.T) {
	f := newSetupTestFixture(t)
	if err := clibase.SetBackgroundPaused(f.env, true); err != nil {
		t.Fatal(err)
	}
	steps := watchStepsForRepo(&cobra.Command{}, f.opts, watch.DefaultFlags(), f.repoDir, time.Now)
	var ran []string
	steps.Fingerprint = func(context.Context) string { ran = append(ran, "fingerprint"); return "changed" }
	steps.Delta = func(context.Context) (historystore.ShortTermStats, error) {
		ran = append(ran, "delta")
		return historystore.ShortTermStats{}, nil
	}
	steps.Reconcile = func(context.Context) error { ran = append(ran, "reconcile"); return nil }
	steps.Refresh = func(context.Context) error { ran = append(ran, "refresh"); return nil }
	steps.Seed = func(context.Context) error { ran = append(ran, "seed"); return nil }
	steps.Distill = func(context.Context) error { ran = append(ran, "distill"); return nil }
	out := &bytes.Buffer{}
	agentCalls := 0
	watch.Tick(context.Background(), out, watch.DefaultFlags(), filepath.Join(t.TempDir(), "watch.json"), steps, &agentCalls)
	if len(ran) != 0 {
		t.Fatalf("paused, but the tick ran %v", ran)
	}
	if !strings.Contains(out.String(), "paused") {
		t.Fatalf("the log must say why nothing ran: %q", out.String())
	}
}

func TestSetupSkipsBackfillWhilePaused(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	if err := clibase.SetBackgroundPaused(f.env, true); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 0 {
		t.Fatalf("paused, but setup spawned %d backfill(s)", rec.spawnCalls)
	}
	if !strings.Contains(out, "paused") || !strings.Contains(out, "re-run setup") {
		t.Fatalf("the skip must say why and how to get the backfill back:\n%s", out)
	}
}

// session-end distills the ended session itself, outside the worker, so it
// spends tokens on every session unless it honours the pause too.
func TestHookSessionEndDoesNothingWhilePaused(t *testing.T) {
	opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	t.Setenv(memoryWorkerOriginEnv, "")
	old := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { t.Error("paused hook launched a worker"); return nil }
	t.Cleanup(func() { memoryWorkerLaunch = old })
	if err := clibase.SetBackgroundPaused(opts.Env, true); err != nil {
		t.Fatal(err)
	}
	before := privacyTreeDigest(t, brainDir)
	stdout, stderr, err := executeSplit(t, NewRootCommand(opts), "hook", "session-end", "--session", "ended-session")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("paused hook: stdout=%s stderr=%s err=%v", stdout, stderr, err)
	}
	if privacyTreeDigest(t, brainDir) != before {
		t.Fatal("paused hook modified the brain")
	}
}

// Hooks, watch ticks, MCP startup and self-relaunch all start a worker, so the
// worker itself honours the pause rather than each of those launchers.
func TestMemoryWorkerDoesNothingWhilePaused(t *testing.T) {
	oldPrepass, oldLaunchAfter := memoryWorkerPrepass, memoryWorkerLaunchAfter
	defer func() { memoryWorkerPrepass, memoryWorkerLaunchAfter = oldPrepass, oldLaunchAfter }()
	prepassCalls, relaunches := 0, 0
	memoryWorkerPrepass = func(context.Context, Options, string) error { prepassCalls++; return nil }
	memoryWorkerLaunchAfter = func(string, time.Duration, int) error { relaunches++; return nil }

	opts, _ := newPrepassBackoffBrain(t, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err := clibase.SetBackgroundPaused(opts.Env, true); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, newMemoryWorkerCommand(opts), "--once")
	if err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}
	if prepassCalls != 0 || relaunches != 0 {
		t.Fatalf("paused worker did work: prepass=%d relaunches=%d", prepassCalls, relaunches)
	}
	if !strings.Contains(out, `"paused": true`) {
		t.Fatalf("the worker must report that it was paused: %s", out)
	}
}

// The tail of a delayed worker's wait holds the coordinator lease; a `stop`
// that lands there must still win.
func TestMemoryWorkerHonoursAPauseDuringItsLeasedSleep(t *testing.T) {
	oldPrepass, oldLaunchAfter := memoryWorkerPrepass, memoryWorkerLaunchAfter
	defer func() { memoryWorkerPrepass, memoryWorkerLaunchAfter = oldPrepass, oldLaunchAfter }()
	prepassCalls := 0
	memoryWorkerPrepass = func(context.Context, Options, string) error { prepassCalls++; return nil }
	memoryWorkerLaunchAfter = func(string, time.Duration, int) error { return nil }

	opts, _ := newPrepassBackoffBrain(t, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	// The whole 2s delay is under memoryWorkerMaxLeaseSleep, so it is all
	// leased; the pause lands 50ms into it.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = clibase.SetBackgroundPaused(opts.Env, true)
	}()
	out, err := execute(t, newMemoryWorkerCommand(opts), "--once", "--delay", "2s")
	if err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}
	if prepassCalls != 0 || !strings.Contains(out, `"paused": true`) {
		t.Fatalf("a pause during the leased sleep was ignored: prepass=%d out=%s", prepassCalls, out)
	}
}
