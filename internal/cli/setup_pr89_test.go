package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// --- the cap the daemon was never given ---------------------------------

// TestDaemonDistillCarriesTheSameCapSetupReported is the money regression.
// `setup` printed "fact backfill capped at 25 of N ... re-run setup for the next
// batch" and then installed a KeepAlive watcher whose distill pass carried no
// --max-sessions at all, so its first gated tick distilled the remaining N-25 —
// for every repo in the workspace. --distill-every and the cursor bound how
// OFTEN the daemon spends; nothing bounded how MUCH.
//
// The assertion runs the argv through the REAL watch flags, so it fails if the
// flag is dropped, renamed, or stops reaching the distill options.
func TestDaemonDistillCarriesTheSameCapSetupReported(t *testing.T) {
	t.Parallel()
	setupOpts := defaultSetupOptions()

	w := daemonWatchOptions(t, setupOpts)
	if w.distillMaxSessions != setupDefaultBackfillBudget {
		t.Fatalf("the installed daemon distills an UNCAPPED corpus every window: --max-sessions parsed as %d, want %d (argv %v)",
			w.distillMaxSessions, setupDefaultBackfillBudget, brainWatchDaemonArgs(setupOpts))
	}
	distillOpts := watchDistillOptions(w)
	if distillOpts.maxSessions != setupDefaultBackfillBudget {
		t.Fatalf("the flag never reaches the distill pass: maxSessions=%d, want %d", distillOpts.maxSessions, setupDefaultBackfillBudget)
	}
	if !distillOpts.newestFirst {
		t.Fatal("a capped pass must be newest-first, matching what setup told the user it did")
	}
}

// TestDaemonDistillHonorsAnExplicitlyUnlimitedBudget keeps the escape hatch:
// --backfill-budget 0 means "I really do want the whole corpus", and the daemon
// must inherit that decision rather than a silently reimposed default.
func TestDaemonDistillHonorsAnExplicitlyUnlimitedBudget(t *testing.T) {
	t.Parallel()
	setupOpts := defaultSetupOptions()
	setupOpts.backfillBudget = 0
	if args := strings.Join(brainWatchDaemonArgs(setupOpts), " "); strings.Contains(args, "--max-sessions") {
		t.Fatalf("--backfill-budget 0 must not cap the daemon either: %s", args)
	}
	if w := daemonWatchOptions(t, setupOpts); w.distillMaxSessions != 0 {
		t.Fatalf("explicit 0 must stay 0, got %d", w.distillMaxSessions)
	}
}

// TestNegativeBackfillBudgetIsNotUnlimited: -1 is a typo, and reading it as the
// documented 0 ("unlimited") spends the whole corpus on a mistyped flag — in the
// detached backfill AND in the daemon.
func TestNegativeBackfillBudgetIsNotUnlimited(t *testing.T) {
	t.Parallel()
	setupOpts := defaultSetupOptions()
	setupOpts.backfillBudget = -1

	plan := setupBackfillPlanFor(Options{}, setupOpts, "codex", t.TempDir(), t.TempDir())
	if !strings.Contains(strings.Join(plan.Args, " "), "--max-sessions") {
		t.Fatalf("--backfill-budget -1 produced an UNCAPPED distill argv: %v", plan.Args)
	}
	if w := daemonWatchOptions(t, setupOpts); w.distillMaxSessions != setupDefaultBackfillBudget {
		t.Fatalf("a negative budget must fall back to the default cap, got %d", w.distillMaxSessions)
	}
	// A negative value that reaches watch directly must not mean unlimited either.
	w := defaultWatchOptions()
	w.distillMaxSessions = -1
	if got := watchDistillOptions(w).maxSessions; got != 0 {
		t.Fatalf("a negative --max-sessions must not be passed through, got %d", got)
	}
}

// --- the agent the daemon was never given -------------------------------

// TestDaemonDistillUsesTheAgentSetupResolved: setup detects an agent, tells the
// user which one, and pays the backfill with it — then installed a daemon that
// used watch's "codex" default regardless. On a claude-only machine that daemon
// can never succeed, and because the agent name is part of the distill cache
// salt, a codex that appears later re-distills the whole corpus already paid for.
func TestDaemonDistillUsesTheAgentSetupResolved(t *testing.T) {
	t.Parallel()
	setupOpts := defaultSetupOptions()
	setupOpts.agent = "claude-code"

	w := daemonWatchOptions(t, setupOpts)
	if w.distillAgent != "claude-code" {
		t.Fatalf("the daemon ignores setup's resolved agent and falls back to %q: %v",
			defaultWatchOptions().distillAgent, brainWatchDaemonArgs(setupOpts))
	}
	if got := watchDistillOptions(w).agent; got != "claude-code" {
		t.Fatalf("the agent never reaches the distill pass, got %q", got)
	}
}

// TestSetupResolvesTheAgentBeforePlanningTheDaemon is the end-to-end half: the
// plan is built BEFORE the backfill phase, so an agent resolved inside that
// phase reached the detached child and never the unit installed for months.
func TestSetupResolvesTheAgentBeforePlanningTheDaemon(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{agent: "claude-code"}
	steps := rec.steps(f)
	inner := steps.plan
	var planned []string
	steps.plan = func(so setupCommandOptions) (daemonPlan, error) {
		planned = brainWatchDaemonArgs(so)
		return inner(so)
	}

	opts := defaultSetupOptions() // agent "auto", exactly as a bare `setup` runs
	out := &bytes.Buffer{}
	if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, steps); err != nil {
		t.Fatalf("runSetup: %v", err)
	}

	// The resolved agent no longer rides in the unit's argv — the unit is
	// machine-wide and carries nothing per-repo — so the assertion is on the
	// machine watch plan the daemon reads instead. What matters is unchanged:
	// the agent must be resolved BEFORE the daemon is planned, or the daemon
	// gets watch's "codex" default no matter what setup detected and paid with.
	if got := strings.Join(planned, " "); got != "workspace watch --distill" {
		t.Fatalf("the installed argv must carry nothing per-repo, got %q", got)
	}
	plan, err := loadSetupWatchPlan(f.env)
	if err != nil {
		t.Fatalf("load watch plan: %v", err)
	}
	entry, ok := plan.entry(opts.workspace)
	if !ok {
		t.Fatalf("setup registered no workspace for the daemon to watch: %+v", plan.Workspaces)
	}
	if entry.Agent != "claude-code" {
		t.Fatalf("the daemon does not use the agent setup resolved and paid with: %+v", entry)
	}
	if got := strings.Join(rec.spawned.Args, " "); !strings.Contains(got, "--agent claude-code") {
		t.Fatalf("the detached backfill must still use the resolved agent: %s", got)
	}
}

// TestSetupOmitsTheAgentWhenNoneWasResolved: a machine with no agent CLI must
// not pin the daemon to the literal "none" — one may be installed tomorrow.
func TestSetupOmitsTheAgentWhenNoneWasResolved(t *testing.T) {
	t.Parallel()
	setupOpts := defaultSetupOptions()
	setupOpts.agent = "none"
	if args := strings.Join(brainWatchDaemonArgs(setupOpts), " "); strings.Contains(args, "--agent") {
		t.Fatalf("an unresolved agent must not be pinned into the unit: %s", args)
	}
}

// --- the workspace name that crash-loops a KeepAlive unit ----------------

// TestInvalidWorkspaceIsRejectedBeforeAnythingIsInstalled: --workspace was
// validated only inside registerRepoInWorkspace, whose failure setup downgrades
// to a warning so one bad source never costs the whole run — and the daemon was
// installed afterwards regardless. The result was a machine-wide
// KeepAlive/Restart=always unit running `workspace watch <name the CLI
// rejects>`, exiting immediately, restarted every 60s forever, while setup
// exited 0.
func TestInvalidWorkspaceIsRejectedBeforeAnythingIsInstalled(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.workspace = "my ws"

	err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, opts), f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatal("setup exited 0 for a workspace name the CLI rejects")
	}
	if !strings.Contains(err.Error(), "--workspace") {
		t.Fatalf("the error must name the flag the user has to fix, got %v", err)
	}
	if rec.installCalls != 0 {
		t.Fatalf("a crash-looping watcher was installed anyway (%d install calls)", rec.installCalls)
	}
	if rec.instantCalls != 0 {
		t.Fatal("a name the CLI rejects must cost nothing: rejection belongs before the first phase")
	}
}

// --- the never-abort hole in the instant phase ---------------------------

// TestBestEffortRefreshReportsTheSeedProbeFailure: seedRefreshNeeded's error was
// returned RAW, never through stage(), so a best-effort refresh still aborted.
// It fires on the RE-RUN path — a repo that already has a seed and whose `git
// rev-parse HEAD` fails (unborn branch, interrupted git state, git missing from
// a daemon's PATH). setup's instant phase then reported ZERO components, so
// instant.json was never written, status and doctor could not name the reason,
// and the workspace, the daemon and the backfill were all skipped.
func TestBestEffortRefreshReportsTheSeedProbeFailure(t *testing.T) {
	f := newSetupTestFixture(t)

	if err := os.MkdirAll(f.storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   setupTestNow,
		DefaultBranch: "main",
		Sources: &brainSources{
			Seed: &seedSourceManifest{GeneratedAt: setupTestNow, Commit: "deadbeef", WorktreeMode: "commit", SummaryPath: "seed/SEED.md"},
		},
	}
	if err := writeBrainManifestAndReadme(f.storage.BrainDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var reported []string
	sub := &cobra.Command{}
	sub.SetContext(context.Background())
	sub.SetOut(io.Discard)
	sub.SetErr(io.Discard)

	// The fake runner errors on any command it was not given, which is exactly
	// what an unresolvable HEAD produces for real.
	err := runRefresh(context.Background(), sub, f.opts, refreshCommandOptions{
		outputDir:       defaultExportDir,
		checkpointLimit: defaultCheckpointLimit,
		entireBinary:    "entire",
		graphBinary:     "entire",
		scope:           exportScopeAll,
		semantic:        true,
		bestEffort:      true,
		component:       func(name string, _ error) { reported = append(reported, name) },
	})
	if err != nil && len(reported) == 0 {
		t.Fatalf("best-effort refresh aborted with ZERO components reported: %v\nsetup would exit non-zero, write no instant.json, and skip the workspace, the daemon and the backfill", err)
	}
	if !pr89Contains(reported, brainComponentSeed) {
		t.Fatalf("a freshness probe that could not run is a failed SEED source and must be reported as one, got %v", reported)
	}
}

func pr89Contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// --- corrupt setup.json must not retire another repo's daemon ------------

// TestSetupRefusesAnUnreadableSetupRecord: an unparseable setup.json read as
// "never set up, here are the defaults", so every path that acts on a PREVIOUS
// daemon identity acted on the DEFAULT one — and `--uninstall-daemon` removed
// the machine-wide watcher every other repo depends on.
func TestSetupRefusesAnUnreadableSetupRecord(t *testing.T) {
	f := newSetupTestFixture(t)
	stateDir := filepath.Dir(f.storage.HeadPath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, setupRecordFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.uninstallDaemon = true
	err := runSetup(context.Background(), setupTestCommand(t, &bytes.Buffer{}, opts), f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatal("an unreadable setup record must not be read as 'never set up'")
	}
	if rec.uninstallCalls != 0 {
		t.Fatalf("the DEFAULT watcher (another repo's) was uninstalled on a corrupt record: %d calls", rec.uninstallCalls)
	}
	if _, _, recordErr := setupOptionsFromRecord(stateDir); recordErr == nil {
		t.Fatal("setupOptionsFromRecord must distinguish corrupt from absent")
	}
	if _, existed, recordErr := setupOptionsFromRecord(t.TempDir()); existed || recordErr != nil {
		t.Fatalf("an absent record is not an error: existed=%v err=%v", existed, recordErr)
	}
}

// --- coverage the mutation survey proved was missing ---------------------

// TestBrainWatchDaemonPlanTargetsTheHostOS closes a hole big enough to disable
// the whole feature silently: every runSetup test injects steps.plan, so
// brainWatchDaemonPlan's ONE read of runtime.GOOS was executed by nothing.
// Pinning it to "windows" — which makes setup install nothing, forever, on every
// machine — left all 635 package tests passing.
func TestBrainWatchDaemonPlanTargetsTheHostOS(t *testing.T) {
	f := newSetupTestFixture(t)
	setupOpts := defaultSetupOptions()

	got, gotErr := brainWatchDaemonPlan(f.opts, setupOpts)
	want, wantErr := brainWatchDaemonPlanFor(runtime.GOOS, f.opts, setupOpts)

	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("brainWatchDaemonPlan must plan for the HOST os (%s): err=%v, host err=%v", runtime.GOOS, gotErr, wantErr)
	}
	if gotErr != nil {
		return
	}
	if got.Manager != want.Manager || got.Label != want.Label || got.UnitPath != want.UnitPath || got.Contents != want.Contents {
		t.Fatalf("brainWatchDaemonPlan planned for a different OS than %s:\n got %+v\nwant %+v", runtime.GOOS, got, want)
	}
	if got.Manager == daemonManagerUnsupported && (runtime.GOOS == "darwin" || runtime.GOOS == "linux") {
		t.Fatalf("%s must be a supported daemon host, got %q", runtime.GOOS, got.Manager)
	}
}

// --- a window burned on a pass that never ran ---------------------------

// TestWatchDoesNotSpendTheWindowOnAContendedDistill: `setup` spawns an
// hours-long detached backfill AND installs a watcher that ticks immediately,
// so the watcher's first gated distill reliably finds the pass lock held.
// withDistillPassLock returns nil in that case (correct for `distill`, which
// exits 0 because someone else is doing the work), and the watcher read that as
// a spend: it had already stamped LastAgentSpendAt, so it burned a whole
// --distill-every window — 24h by default — on a no-op, logged "spent tokens",
// and did it again for as long as the backfill held the lock.
func TestWatchDoesNotSpendTheWindowOnAContendedDistill(t *testing.T) {
	t.Parallel()
	w := daemonWatchOptions(t, defaultSetupOptions())
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	agentCalls := 0
	attempts := 0
	now := setupTestNow
	fingerprint := "fp-0"
	steps := func(distillErr error) watchSteps {
		return watchSteps{
			now:         func() time.Time { return now },
			fingerprint: func(context.Context) string { return fingerprint },
			refresh:     func(context.Context) error { return nil },
			seed:        func(context.Context) error { return nil },
			distill:     func(context.Context) error { attempts++; return distillErr },
		}
	}

	// Tick 1: the detached backfill holds the brain.
	watchTick(context.Background(), io.Discard, w, cursorPath, steps(errDistillPassBusy), &agentCalls)
	if attempts != 1 {
		t.Fatalf("the gated distill must have been attempted once, got %d", attempts)
	}
	if agentCalls != 0 {
		t.Fatalf("a pass that did nothing must not count against --budget, got %d", agentCalls)
	}
	if cursor := loadWatchCursor(cursorPath); !cursor.LastAgentSpendAt.IsZero() {
		t.Fatalf("a %s spend window was burned on a distill that never ran (cursor %s)", w.distillEvery, cursor.LastAgentSpendAt)
	}

	// Tick 2, well inside the same --distill-every window: the watcher must be
	// free to try again, because it never actually spent.
	now = now.Add(w.consolidateEvery + time.Minute)
	fingerprint = "fp-1"
	watchTick(context.Background(), io.Discard, w, cursorPath, steps(nil), &agentCalls)
	if attempts != 2 {
		t.Fatalf("the next tick must retry a distill that never ran, attempts=%d", attempts)
	}
	if cursor := loadWatchCursor(cursorPath); cursor.LastAgentSpendAt.IsZero() {
		t.Fatal("a distill that DID run must charge the window")
	}
	if agentCalls != 1 {
		t.Fatalf("a real spend must count against --budget, got %d", agentCalls)
	}
}

func TestWatchKeepsSeedSpendWhenDistillIsContended(t *testing.T) {
	t.Parallel()
	w := daemonWatchOptions(t, defaultSetupOptions())
	w.seedAgent = "codex"
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	agentCalls := 0
	seedCalls := 0
	watchTick(context.Background(), io.Discard, w, cursorPath, watchSteps{
		now:         func() time.Time { return setupTestNow },
		fingerprint: func(context.Context) string { return "fp" },
		refresh:     func(context.Context) error { return nil },
		seed:        func(context.Context) error { seedCalls++; return nil },
		distill:     func(context.Context) error { return errDistillPassBusy },
	}, &agentCalls)
	if seedCalls != 1 || agentCalls != 1 {
		t.Fatalf("seed spend was incorrectly refunded: seed=%d budget=%d", seedCalls, agentCalls)
	}
	if cursor := loadWatchCursor(cursorPath); cursor.LastAgentSpendAt.IsZero() {
		t.Fatal("seed synthesis spent tokens, so the spend window must remain reserved")
	}
}

// TestWatchDistillReportsContentionToItsCaller is the wiring half: the sentinel
// only reaches watchTick if runDistill's busy path tells the caller. The brain
// here has ZERO sessions, so this test cannot make an agent call under any
// outcome.
func TestWatchDistillReportsContentionToItsCaller(t *testing.T) {
	f := newSetupTestFixture(t)
	if err := os.MkdirAll(f.storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(f.storage.BrainDir, exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   setupTestNow,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: setupTestNow, DefaultBranch: "main"},
		},
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	w := defaultWatchOptions()
	held := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withDistillPassLock(f.storage.BrainDir, func() { t.Error("the test must be the one holding the lock") }, func() error {
			close(held)
			<-done // released below, after watchDistill has tried
			return nil
		})
	}()
	<-held

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := watchDistill(context.Background(), cmd, f.opts, w, f.repoDir)
	done <- nil
	<-done

	if !errors.Is(err, errDistillPassBusy) {
		t.Fatalf("a distill that skipped on contention must say so to its caller, got %v", err)
	}
}

// --- workspace membership ------------------------------------------------

// TestWorkspaceRegistrationKeepsAMemberName: `setup` re-registers this repo on
// EVERY run and carries no display name, so the whole member struct was
// overwritten and a name set with `workspace add --name` vanished on the next
// setup.
func TestWorkspaceRegistrationKeepsAMemberName(t *testing.T) {
	f := newSetupTestFixture(t)
	const key = "aaaaaaaaaaaaaaaa"
	if _, err := addWorkspaceRepoLocked(f.env, setupDefaultWorkspace, workspaceRepo{RepoKey: key, Name: "the-api", LocalPathHint: f.repoDir}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := registerRepoInWorkspace(context.Background(), nil, f.opts, setupDefaultWorkspace, f.repoDir, key); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	manifest, err := loadWorkspaceManifest(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].Name != "the-api" {
		t.Fatalf("re-running setup erased the member's name: %+v", manifest.Repos)
	}
}

// TestCorruptWorkspaceManifestNamesTheFileToFix: an unreadable manifest blocks
// registration for every repo in the workspace, permanently, and the raw JSON
// error named neither the file nor a way out.
func TestCorruptWorkspaceManifestNamesTheFileToFix(t *testing.T) {
	f := newSetupTestFixture(t)
	dir, err := workspaceDir(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, workspaceManifestName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, addErr := addWorkspaceRepoLocked(f.env, setupDefaultWorkspace, workspaceRepo{RepoKey: "bbbbbbbbbbbbbbbb", LocalPathHint: f.repoDir})
	if addErr == nil {
		t.Fatal("a corrupt manifest must not be silently replaced: the other members would be dropped")
	}
	if !strings.Contains(addErr.Error(), workspaceManifestName) {
		t.Fatalf("the error must name the file that wedged the workspace, got %v", addErr)
	}
}
