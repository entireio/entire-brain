package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setup_watchplan_test.go pins the one-watcher invariant setup.go states in its
// header: "Running setup twice must produce the same machine, not two daemons
// and two workspace entries."
//
// Each test here replaces an audit scratch test that DEMONSTRATED the defect by
// asserting the broken behaviour. The defect is unchanged; only the direction of
// the assertion is. Every one of them fails on the code before the fix.

// realDaemonSteps wires the REAL inspect/install/uninstall into the recorded
// setup steps, so a test can read what actually landed in the unit directory.
// The recorded fakes are not enough here: the whole question is which FILES
// exist on disk after two setups, and a fake uninstall that only bumps a counter
// cannot answer it.
func realDaemonSteps(f *setupTestFixture, rec *recordedSetup, runner CommandRunner) setupSteps {
	steps := rec.steps(f)
	steps.inspect = func(ctx context.Context, plan daemonPlan) daemonState {
		return inspectDaemon(ctx, runner, plan)
	}
	steps.install = func(ctx context.Context, plan daemonPlan) error {
		rec.installCalls++
		return installDaemon(ctx, runner, plan)
	}
	steps.uninstall = func(ctx context.Context, plan daemonPlan) error {
		rec.uninstallCalls++
		return uninstallDaemon(ctx, runner, plan)
	}
	return steps
}

// secondRepoFixture builds a SECOND repo that shares the first repo's machine:
// one plugin store, one daemon unit directory, one home. That sharing is the
// whole point — the defects below are all cross-repo, and a fixture that gave
// each repo its own store could not see any of them.
func secondRepoFixture(t *testing.T, fa *setupTestFixture) *setupTestFixture {
	t.Helper()
	repoB := t.TempDir()
	envB := fa.env
	envB.RepoRoot = repoB
	optsB := fa.opts
	optsB.Env = envB
	storageB, err := repoStoragePaths(context.Background(), fa.runner, envB, repoB)
	if err != nil {
		t.Fatalf("repoStoragePaths for the second repo: %v", err)
	}
	return &setupTestFixture{repoDir: repoB, home: fa.home, env: envB, runner: fa.runner, opts: optsB, storage: storageB}
}

// The daemon's identity is machine-wide (the launchd label and systemd unit name
// derive from the daemon NAME alone) but its arguments used to be per-repo. Two
// repos asking for two different workspaces therefore resolved to the SAME unit
// path and rendered DIFFERENT bytes: the second `setup` overwrote the first
// repo's watcher and the first repo was never watched again.
//
// The unit must now be workspace-independent, so two workspaces produce
// byte-identical units and there is nothing to overwrite.
func TestDaemonUnitIsIdenticalForEveryWorkspace(t *testing.T) {
	f := newSetupTestFixture(t)

	alpha := defaultSetupOptions()
	alpha.workspace = "alpha"
	alpha.interval = 3 * time.Minute
	alpha.model = "model-a"
	beta := defaultSetupOptions()
	beta.workspace = "beta"
	beta.interval = 90 * time.Minute
	beta.model = "model-b"

	for _, goos := range []string{"darwin", "linux"} {
		planA, err := brainWatchDaemonPlanFor(goos, f.opts, alpha)
		if err != nil {
			t.Fatalf("%s plan alpha: %v", goos, err)
		}
		planB, err := brainWatchDaemonPlanFor(goos, f.opts, beta)
		if err != nil {
			t.Fatalf("%s plan beta: %v", goos, err)
		}
		if planA.UnitPath != planB.UnitPath {
			t.Fatalf("%s: the watcher is machine-wide, so both workspaces must plan the same unit path; got %q and %q",
				goos, planA.UnitPath, planB.UnitPath)
		}
		if planA.Contents != planB.Contents {
			t.Fatalf("%s: two workspaces render DIFFERENT bytes into ONE machine-wide unit, so the second setup silently steals the first repo's watcher\nA args: %v\nB args: %v",
				goos, planA.Spec.Args, planB.Spec.Args)
		}
		for _, leaked := range []string{"alpha", "beta", "model-a", "model-b", "3m0s", "1h30m0s"} {
			if strings.Contains(planA.Contents, leaked) || strings.Contains(planB.Contents, leaked) {
				t.Fatalf("%s: per-repo value %q reached the machine-wide unit; per-workspace tuning belongs in the watch plan\n%s",
					goos, leaked, planA.Contents)
			}
		}
	}
}

// The tuning the unit no longer carries has to live somewhere the daemon reads,
// or the fix above would simply lose it. Two repos on two workspaces must both
// end up in the machine-level watch plan, each with its OWN interval, model and
// cap — the settings that used to be clobbered.
func TestSecondRepoJoinsTheWatchPlanInsteadOfStealingTheWatcher(t *testing.T) {
	fa := newSetupTestFixture(t)
	shared := t.TempDir()
	t.Setenv(envDaemonUnitDir, shared)
	fb := secondRepoFixture(t, fa)
	runner := &recordingDaemonRunner{}

	optsA := defaultSetupOptions()
	optsA.workspace = "alpha"
	optsA.interval = 3 * time.Minute
	optsA.model = "model-a"
	if err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, optsA), fa.opts, optsA, fa.repoDir, realDaemonSteps(fa, &recordedSetup{}, runner)); err != nil {
		t.Fatalf("setup A: %v", err)
	}

	optsB := defaultSetupOptions()
	optsB.workspace = "beta"
	optsB.interval = 90 * time.Minute
	optsB.model = "model-b"
	if err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, optsB), fb.opts, optsB, fb.repoDir, realDaemonSteps(fb, &recordedSetup{}, runner)); err != nil {
		t.Fatalf("setup B: %v", err)
	}

	entries, err := os.ReadDir(shared)
	if err != nil {
		t.Fatalf("read daemon dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly one machine-wide unit, got %d: %v", len(entries), names)
	}

	plan, err := loadSetupWatchPlan(fa.env)
	if err != nil {
		t.Fatalf("load watch plan: %v", err)
	}
	entryA, okA := plan.entry("alpha")
	if !okA {
		t.Fatalf("repo A's workspace is no longer watched: the machine watch plan carries only %+v", plan.Workspaces)
	}
	entryB, okB := plan.entry("beta")
	if !okB {
		t.Fatalf("repo B's workspace is not watched: the machine watch plan carries only %+v", plan.Workspaces)
	}
	if entryA.Interval != "3m0s" || entryA.Model != "model-a" {
		t.Fatalf("repo B's setup clobbered repo A's tuning: %+v", entryA)
	}
	if entryB.Interval != "1h30m0s" || entryB.Model != "model-b" {
		t.Fatalf("repo B's own tuning was not recorded: %+v", entryB)
	}
}

// `--daemon-name` was a fork, not a rename: the retirement of the previous
// watcher consulted only THIS repo's setup record, so a second repo naming a
// different daemon installed a SECOND unit that no `status` and no
// `--uninstall-daemon` could see. Both would tick and both would distill.
//
// The machine-level plan records which name is installed, so the second setup
// retires the first.
func TestRenamingTheDaemonMovesItInsteadOfForkingIt(t *testing.T) {
	fa := newSetupTestFixture(t)
	shared := t.TempDir()
	t.Setenv(envDaemonUnitDir, shared)
	fb := secondRepoFixture(t, fa)
	runner := &recordingDaemonRunner{}

	optsA := defaultSetupOptions()
	if err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, optsA), fa.opts, optsA, fa.repoDir, realDaemonSteps(fa, &recordedSetup{}, runner)); err != nil {
		t.Fatalf("setup A: %v", err)
	}
	optsB := defaultSetupOptions()
	optsB.daemonName = "entire-brain-watch-two"
	if err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, optsB), fb.opts, optsB, fb.repoDir, realDaemonSteps(fb, &recordedSetup{}, runner)); err != nil {
		t.Fatalf("setup B: %v", err)
	}

	entries, err := os.ReadDir(shared)
	if err != nil {
		t.Fatalf("read daemon dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(entries) != 1 {
		t.Fatalf("two machine-wide watchers installed, both will tick and both will distill: %v", names)
	}
	if !strings.Contains(names[0], "brain-watch-two") {
		t.Fatalf("the rename did not take effect; the surviving unit is %v", names)
	}
	plan, err := loadSetupWatchPlan(fa.env)
	if err != nil {
		t.Fatalf("load watch plan: %v", err)
	}
	if plan.DaemonName != "entire-brain-watch-two" {
		t.Fatalf("the machine plan still names %q, so the NEXT rename will fork again", plan.DaemonName)
	}
}

// A first-ever `setup --daemon-name mine` on a machine that has never installed
// anything must not "retire" the default watcher it never had. This is the guard
// that keeps the rename above from becoming a way to uninstall other repos'
// watchers by accident.
func TestFirstSetupWithACustomNameRetiresNothing(t *testing.T) {
	f := newSetupTestFixture(t)
	shared := t.TempDir()
	t.Setenv(envDaemonUnitDir, shared)
	runner := &recordingDaemonRunner{}
	rec := &recordedSetup{}

	opts := defaultSetupOptions()
	opts.daemonName = "entire-brain-watch-mine"
	if err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, opts), f.opts, opts, f.repoDir, realDaemonSteps(f, rec, runner)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if rec.uninstallCalls != 0 {
		t.Fatalf("a first-ever setup retired a watcher that never existed (%d uninstall calls)", rec.uninstallCalls)
	}
}

// inspectDaemon computes Current — whether the unit ON DISK is the unit this
// repo would write — and describeDaemonState dropped it, so a drifted or
// foreign watcher printed as plain "running" and the reader had no way to learn
// it was not theirs.
func TestDescribeDaemonStateSurfacesADriftedUnit(t *testing.T) {
	base := daemonState{
		Manager:   daemonManagerLaunchd,
		Label:     "io.entire.brain-watch.a2d3fd66",
		UnitPath:  "/Users/demo/Library/LaunchAgents/io.entire.brain-watch.a2d3fd66.plist",
		Installed: true,
	}
	drifted := base
	drifted.Running = true
	got := describeDaemonState(drifted, setupBrainBinaryName)
	if !strings.Contains(got, "not current") {
		t.Fatalf("a running-but-not-current daemon must not be described as plain %q", got)
	}
	current := drifted
	current.Current = true
	if got := describeDaemonState(current, setupBrainBinaryName); strings.Contains(got, "not current") {
		t.Fatalf("a current, running daemon must not be reported as stale: %q", got)
	}
	stopped := base
	if got := describeDaemonState(stopped, setupBrainBinaryName); !strings.Contains(got, "not current") {
		t.Fatalf("an installed, stopped, drifted daemon must say so: %q", got)
	}
}

func TestWatchPlanEntryCarriesTheBudgetSetupReported(t *testing.T) {
	opts := defaultSetupOptions()
	opts.workspace = "ws"
	opts.agent = "claude"
	opts.model = "haiku"
	opts.effort = "low"
	opts.backfillBudget = 7
	entry := setupWatchPlanEntryFor(opts, time.Unix(0, 0))
	if entry.MaxSessions != 7 {
		t.Fatalf("the cap setup reported must bind on the daemon that keeps spending: %+v", entry)
	}
	if entry.Agent != "claude" {
		t.Fatalf("the daemon must use the agent setup resolved and paid the backfill with: %+v", entry)
	}
	// Unresolved automatic selection keeps the daemon default. Explicit
	// "none" is retained and disables distillation when the plan is applied.
	for _, sentinel := range []string{"auto", ""} {
		opts.agent = sentinel
		if got := setupWatchPlanEntryFor(opts, time.Unix(0, 0)).Agent; got != "" {
			t.Fatalf("agent %q must not reach the plan, got %q", sentinel, got)
		}
	}
}

func TestApplyWatchPlanEntryOverlaysOnlyWhatThePlanCarries(t *testing.T) {
	base := defaultWatchOptions()
	base.distillAgent = "codex"
	got := applyWatchPlanEntry(base, setupWatchPlanEntry{Workspace: "ws", Interval: "7m", MaxSessions: 4})
	if got.interval != 7*time.Minute || got.distillMaxSessions != 4 {
		t.Fatalf("the plan's own values must bind: %+v", got)
	}
	if got.distillAgent != "codex" || got.distillEvery != base.distillEvery {
		t.Fatalf("a field the plan does not carry must keep the daemon default, so an older plan still runs: %+v", got)
	}
}

// The supervised loop's wait is the SHORTEST interval any workspace asked for.
// Taking the longest, or the first, would silently downgrade a workspace that
// asked to be checked often as soon as a slower one joined.
func TestWatchPlanSleepTakesTheShortestInterval(t *testing.T) {
	plan := setupWatchPlan{Workspaces: []setupWatchPlanEntry{
		{Workspace: "slow", Interval: "1h"},
		{Workspace: "fast", Interval: "2m"},
		{Workspace: "broken", Interval: "not-a-duration"},
	}}
	if got := watchPlanSleep(plan, time.Minute); got != 2*time.Minute {
		t.Fatalf("want the shortest interval 2m, got %s", got)
	}
	if got := watchPlanSleep(setupWatchPlan{}, 9*time.Minute); got != 9*time.Minute {
		t.Fatalf("an empty plan must fall back to the daemon's own interval, got %s", got)
	}
	if got := watchPlanSleep(setupWatchPlan{}, 0); got != 5*time.Minute {
		t.Fatalf("a zero fallback must not become a busy loop, got %s", got)
	}
}

// Two setups racing in two repos is the ordinary case on a developer machine.
// An unlocked read-modify-write drops one of the two workspaces — the same
// last-writer-wins class of bug the unit itself had.
func TestWatchPlanUpsertKeepsEveryWorkspace(t *testing.T) {
	f := newSetupTestFixture(t)
	for _, name := range []string{"beta", "alpha", "gamma"} {
		if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: name, Interval: "5m"}); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	// Re-recording an existing workspace REPLACES its row rather than adding one.
	if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: "alpha", Interval: "1m"}); err != nil {
		t.Fatalf("re-record alpha: %v", err)
	}
	plan, err := loadSetupWatchPlan(f.env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(plan.Workspaces) != 3 {
		t.Fatalf("want 3 workspaces, got %+v", plan.Workspaces)
	}
	// Sorted, so the file is byte-stable across runs and a diff of it is readable.
	if plan.Workspaces[0].Workspace != "alpha" || plan.Workspaces[2].Workspace != "gamma" {
		t.Fatalf("watch plan is not in a stable order: %+v", plan.Workspaces)
	}
	if plan.Workspaces[0].Interval != "1m" {
		t.Fatalf("re-recording a workspace must replace its tuning: %+v", plan.Workspaces[0])
	}
}

// A machine that has never run `setup` has no plan file. The daemon is a
// KeepAlive/Restart=always service, so reading that as an error would be a
// restart loop rather than the honest "nothing to watch yet".
func TestLoadWatchPlanTreatsAMissingFileAsEmpty(t *testing.T) {
	f := newSetupTestFixture(t)
	plan, err := loadSetupWatchPlan(f.env)
	if err != nil {
		t.Fatalf("a machine with no plan is not an error: %v", err)
	}
	if len(plan.Workspaces) != 0 {
		t.Fatalf("want an empty plan, got %+v", plan.Workspaces)
	}

	path, err := setupWatchPlanPath(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSetupWatchPlan(f.env); err == nil {
		t.Fatal("a corrupt plan must be reported, not silently read as empty: a silently-empty plan stops watching every workspace")
	}
}

func TestLoadWatchPlanRejectsAnUnversionedDocument(t *testing.T) {
	f := newSetupTestFixture(t)
	path, err := setupWatchPlanPath(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"workspaces":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSetupWatchPlan(f.env); err == nil || !strings.Contains(err.Error(), "schema_version 0") {
		t.Fatalf("unversioned watch plan was accepted: %v", err)
	}
}

func TestSetupDoesNotInstallDaemonWithoutItsWatchPlan(t *testing.T) {
	f := newSetupTestFixture(t)
	path, err := setupWatchPlanPath(f.env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	out := &bytes.Buffer{}
	if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("the instant brain should remain usable: %v", err)
	}
	if rec.installCalls != 0 {
		t.Fatalf("daemon installed without persisted tuning: %d calls", rec.installCalls)
	}
	if !strings.Contains(out.String(), "machine watch plan not saved") {
		t.Fatalf("the skipped daemon must name its blocker: %q", out.String())
	}
}

// The supervised loop is what the installed unit actually runs. Its whole job is
// to visit EVERY workspace the machine registered, each with its OWN recorded
// tuning — the tuning that used to be clobbered when a second repo overwrote the
// unit.
func TestSupervisedWatchVisitsEveryWorkspaceWithItsOwnTuning(t *testing.T) {
	f := newSetupTestFixture(t)
	for _, entry := range []setupWatchPlanEntry{
		{Workspace: "alpha", Interval: "3m", Model: "model-a", MaxSessions: 5},
		{Workspace: "beta", Interval: "90m", Model: "model-b", MaxSessions: 9},
	} {
		if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, entry); err != nil {
			t.Fatalf("record %s: %v", entry.Workspace, err)
		}
	}

	type visit struct {
		workspace   string
		interval    time.Duration
		model       string
		maxSessions int
		once        bool
	}
	var visits []visit
	base := defaultWatchOptions()
	base.once = true // one outer pass, then return
	err := supervisedWatchLoop(context.Background(), &bytes.Buffer{}, f.env, base,
		func(workspace string, pass watchCommandOptions, _ *int) error {
			visits = append(visits, visit{workspace, pass.interval, pass.model, pass.distillMaxSessions, pass.once})
			return nil
		}, nil)
	if err != nil {
		t.Fatalf("supervised loop: %v", err)
	}
	want := []visit{
		{"alpha", 3 * time.Minute, "model-a", 5, true},
		{"beta", 90 * time.Minute, "model-b", 9, true},
	}
	if len(visits) != len(want) {
		t.Fatalf("the supervised watcher must visit every registered workspace; got %+v", visits)
	}
	for i := range want {
		if visits[i] != want[i] {
			t.Fatalf("workspace %d ran with the wrong tuning\n got %+v\nwant %+v", i, visits[i], want[i])
		}
	}
}

// A machine mid-onboarding has an empty plan. The unit is KeepAlive /
// Restart=always, so returning an error there is a restart loop that repairs
// nothing; it must say what is missing and keep waiting.
func TestSupervisedWatchSurvivesAnEmptyPlan(t *testing.T) {
	f := newSetupTestFixture(t)
	out := &bytes.Buffer{}
	base := defaultWatchOptions()
	base.once = true
	called := 0
	if err := supervisedWatchLoop(context.Background(), out, f.env, base,
		func(string, watchCommandOptions, *int) error { called++; return nil }, nil); err != nil {
		t.Fatalf("an empty plan must not take the service down: %v", err)
	}
	if called != 0 {
		t.Fatalf("nothing to watch, but %d workspace passes ran", called)
	}
	if !strings.Contains(out.String(), "no workspaces registered yet") {
		t.Fatalf("the reader must be told what is missing and what to do: %q", out.String())
	}
}

// One broken workspace — a manifest deleted by hand, a member repo unmounted —
// must not stop the machine's other workspaces from being watched.
func TestSupervisedWatchKeepsGoingAfterOneWorkspaceFails(t *testing.T) {
	f := newSetupTestFixture(t)
	for _, name := range []string{"broken", "fine"} {
		if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: name, Interval: "5m"}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	base := defaultWatchOptions()
	base.once = true
	out := &bytes.Buffer{}
	if err := supervisedWatchLoop(context.Background(), out, f.env, base,
		func(workspace string, _ watchCommandOptions, _ *int) error {
			seen = append(seen, workspace)
			if workspace == "broken" {
				return errors.New("manifest gone")
			}
			return nil
		}, nil); err != nil {
		t.Fatalf("one failing workspace must not fail the loop: %v", err)
	}
	if len(seen) != 2 || seen[1] != "fine" {
		t.Fatalf("the healthy workspace was skipped after the broken one: %v", seen)
	}
	if !strings.Contains(out.String(), "manifest gone") {
		t.Fatalf("the failure must be reported, not swallowed: %q", out.String())
	}
}

func TestSupervisedWatchBudgetSurvivesOuterPasses(t *testing.T) {
	f := newSetupTestFixture(t)
	if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: "alpha", Interval: "1ms"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen []int
	err := supervisedWatchLoop(ctx, &bytes.Buffer{}, f.env, defaultWatchOptions(),
		func(_ string, _ watchCommandOptions, agentCalls *int) error {
			seen = append(seen, *agentCalls)
			*agentCalls++
			if len(seen) == 2 {
				cancel()
			}
			return nil
		}, nil)
	if err != nil {
		t.Fatalf("supervised loop: %v", err)
	}
	if len(seen) != 2 || seen[0] != 0 || seen[1] != 1 {
		t.Fatalf("--budget counter reset between outer passes: %v", seen)
	}
}

// The backfill is the phase that spends money. Everything the user needs to
// decide about it — what it will run, over how many sessions, and the flag that
// turns it off — used to be printed AFTER the detached child was already
// running, so the first thing the number was good for was reading it about a
// spend already underway.
func TestSetupSaysWhatTheBackfillCostsBeforeItSpends(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3")
	rec := &recordedSetup{agent: "claude"}
	steps := rec.steps(f)

	out := &bytes.Buffer{}
	var atSpawn string
	inner := steps.spawnBackfill
	steps.spawnBackfill = func(ctx context.Context, plan setupBackfillPlan) (int, error) {
		// Snapshot what the user had been told at the instant of the spend.
		atSpawn = out.String()
		return inner(ctx, plan)
	}

	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.model = "haiku"
	opts.effort = "low"
	if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, steps); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	if rec.spawnCalls != 1 {
		t.Fatalf("the backfill must have been spawned once, got %d", rec.spawnCalls)
	}
	for _, want := range []string{"SPEND TOKENS", "agent claude on haiku/low", "--no-backfill"} {
		if !strings.Contains(atSpawn, want) {
			t.Fatalf("the user was not told %q before the spend started; all they had was:\n%s", want, atSpawn)
		}
	}
}

// A cap that bites must say so in the same breath as the cost, or "3 sessions"
// reads as "all of them" and the user believes the corpus is done.
func TestTheBackfillCostLineNamesTheCapWhenOneBites(t *testing.T) {
	t.Parallel()
	if got := setupBackfillOfLabel(25, 400); !strings.Contains(got, "capped at 25 of 400") {
		t.Fatalf("a biting cap must be named: %q", got)
	}
	if got := setupBackfillOfLabel(9, 9); got != "" {
		t.Fatalf("an uncapped pass must not invent a cap: %q", got)
	}
	if got := setupBackfillSpendLabel("codex", setupCommandOptions{}); got != "agent codex" {
		t.Fatalf("with no model or effort the label is just the agent: %q", got)
	}
	if got := setupBackfillSpendLabel("codex", setupCommandOptions{effort: "low"}); got != "agent codex at low effort" {
		t.Fatalf("effort alone must still be priceable: %q", got)
	}
}

// The daemon line used to be a hashed launchd label and nothing else, which
// answers none of what a reader came to ask — least of all right after a second
// repo ran `setup`, the exact moment the old code silently stopped watching the
// first one.
func TestWatchPlanCoverageNamesWhatTheWatcherCovers(t *testing.T) {
	t.Parallel()
	got := describeWatchPlanCoverage(setupWatchPlan{Workspaces: []setupWatchPlanEntry{
		{Workspace: "alpha", Interval: "3m"},
		{Workspace: "beta", Interval: "45m"},
	}})
	for _, want := range []string{"2 workspaces", "alpha, beta", "3m0s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("coverage line must contain %q, got %q", want, got)
		}
	}
	if got := describeWatchPlanCoverage(setupWatchPlan{Workspaces: []setupWatchPlanEntry{{Workspace: "solo", Interval: "5m"}}}); !strings.Contains(got, "1 workspace (solo)") {
		t.Fatalf("a single workspace must not be pluralised: %q", got)
	}
	// A long list is elided rather than wrapped: the question is "am I in
	// there", and the count still answers it when the names do not fit.
	many := setupWatchPlan{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		many.Workspaces = append(many.Workspaces, setupWatchPlanEntry{Workspace: name, Interval: "5m"})
	}
	if got := describeWatchPlanCoverage(many); !strings.Contains(got, "+2 more") || !strings.Contains(got, "6 workspaces") {
		t.Fatalf("a long list must elide and still count: %q", got)
	}
	if got := describeWatchPlanCoverage(setupWatchPlan{}); got != "" {
		t.Fatalf("an empty plan has no coverage to describe: %q", got)
	}
}

// Status is where a user of the FIRST repo goes to ask "is my repo still being
// watched" after a second repo was set up. The answer has to be on that screen.
func TestStatusNamesTheWorkspacesTheWatcherCovers(t *testing.T) {
	t.Parallel()
	report := statusFixtureReport(t, 44)
	report.Onboarding.Daemon = daemonState{Installed: true, Running: true, Current: true, Label: "io.entire.brain-watch.a2d3fd66"}
	report.Onboarding.WatchPlan = setupWatchPlan{Workspaces: []setupWatchPlanEntry{
		{Workspace: "alpha", Interval: "3m"},
		{Workspace: "beta", Interval: "45m"},
	}}
	got := renderStatus(t, report, false)
	if !strings.Contains(got, "alpha, beta") {
		t.Fatalf("status must name what the one machine-wide watcher covers:\n%s", got)
	}
}
