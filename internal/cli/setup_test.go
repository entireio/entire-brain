package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

var setupTestNow = time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

// setupTestFixture is an isolated repo + plugin store + fake HOME. Nothing here
// touches the developer's real config, brain store, or launchd/systemd state:
// HOME is redirected so even the daemon plan's unit path lands in a temp dir.
type setupTestFixture struct {
	repoDir string
	home    string
	env     EntireEnv
	runner  *fakeCommandRunner
	opts    Options
	storage repoStorage
}

// isolateDaemonEnv severs every path this package can take to a REAL service
// manager. Redirecting HOME is not enough on its own:
//
//   - os.UserHomeDir reads USERPROFILE on windows, not HOME, so a windows run
//     would plan against the developer's actual profile directory;
//   - ENTIRE_BRAIN_DAEMON_DIR overrides the unit directory outright, and a
//     developer who has one exported (a smoke run, a demo install) made these
//     tests read that live daemon — TestBuildBrainOnboardingStatusReportsBackfillAndDaemon
//     failed locally with "no daemon was installed in this fixture" while
//     passing in CI, because in CI nobody had one.
//
// Pointing the knob at a temp directory makes the fixture's answer to "is a
// daemon installed?" a property of the fixture, not of the machine.
func isolateDaemonEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(envDaemonUnitDir, filepath.Join(t.TempDir(), "daemon"))
}

func newSetupTestFixture(t *testing.T, sessionIDs ...string) *setupTestFixture {
	t.Helper()
	repoDir := t.TempDir()
	home := t.TempDir()
	isolateDaemonEnv(t, home)
	env := semanticTestEnv(t, repoDir)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return setupTestNow }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("repoStoragePaths: %v", err)
	}
	fixture := &setupTestFixture{repoDir: repoDir, home: home, env: env, runner: runner, opts: opts, storage: storage}
	if len(sessionIDs) > 0 {
		fixture.writeSessions(t, sessionIDs...)
	}
	return fixture
}

// writeSessions lays down a brain manifest with the named sessions so the
// backfill and the status counters have something real to count.
func (f *setupTestFixture) writeSessions(t *testing.T, ids ...string) {
	t.Helper()
	if err := os.MkdirAll(f.storage.BrainDir, 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	sessions := make([]exportSession, 0, len(ids))
	for i, id := range ids {
		path := filepath.Join("sessions", "main", id+".jsonl")
		full := filepath.Join(f.storage.BrainDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("marker "+id+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, exportSession{
			SessionID:        id,
			Branch:           "main",
			LatestCheckpoint: "cp-" + id,
			TranscriptPath:   filepath.ToSlash(path),
			CreatedAt:        setupTestNow.Add(time.Duration(i-len(ids)) * time.Hour),
		})
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   setupTestNow,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: setupTestNow, DefaultBranch: "main", Sessions: sessions},
		},
	}
	if err := writeBrainManifestAndReadme(f.storage.BrainDir, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// markDistilled writes distill cache entries so a session counts as done.
func (f *setupTestFixture) markDistilled(t *testing.T, ids ...string) {
	t.Helper()
	cache := loadDistillCache(f.storage.BrainDir)
	if cache.Sessions == nil {
		cache.Sessions = map[string]string{}
	}
	cache.Version = distillCacheVersion
	for _, id := range ids {
		cache.Sessions[distillSessionCacheKey("main", id)] = "fingerprint-" + id
	}
	if err := saveDistillCache(f.storage.BrainDir, cache); err != nil {
		t.Fatalf("save distill cache: %v", err)
	}
}

// recordedSetup tracks how many times each injected step ran, which is how the
// idempotence properties are asserted.
type recordedSetup struct {
	instantCalls   int
	spawnCalls     int
	installCalls   int
	uninstallCalls int
	spawned        setupBackfillPlan
	agent          string
	state          daemonState
	// instantErr is the PHASE-fatal error: the instant phase could not run at
	// all. A component that failed on its own belongs in instantComponents.
	instantErr        error
	instantComponents []setupComponent
	// targetOS is the OS the injected daemon plan is built for; empty means
	// darwin. The install/idempotence/retirement properties these tests assert
	// are OS-independent GIVEN a supported plan, so pinning the target keeps
	// them meaningful on every host — including the windows runner, where the
	// real plan is unsupported by design and used to turn all of them into
	// "installs = 0" failures.
	targetOS string
}

func (r *recordedSetup) plannedOS() string {
	if r.targetOS == "" {
		return "darwin"
	}
	return r.targetOS
}

func (r *recordedSetup) steps(f *setupTestFixture) setupSteps {
	return setupSteps{
		now: func() time.Time { return setupTestNow },
		plan: func(setupOpts setupCommandOptions) (daemonPlan, error) {
			return brainWatchDaemonPlanFor(r.plannedOS(), f.opts, setupOpts)
		},
		instant: func(context.Context) ([]setupComponent, error) {
			r.instantCalls++
			return r.instantComponents, r.instantErr
		},
		detectAgent: func(context.Context) string {
			if r.agent == "" {
				return "none"
			}
			return r.agent
		},
		spawnBackfill: func(_ context.Context, plan setupBackfillPlan) (int, error) {
			r.spawnCalls++
			r.spawned = plan
			return 4242, nil
		},
		inspect: func(context.Context, daemonPlan) daemonState { return r.state },
		install: func(_ context.Context, plan daemonPlan) error {
			r.installCalls++
			r.state = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Installed: true, Current: true, Running: true}
			return nil
		},
		uninstall: func(context.Context, daemonPlan) error {
			r.uninstallCalls++
			r.state = daemonState{}
			return nil
		},
	}
}

// setupTestCommand builds the cobra command runSetup asks "did the user type
// this flag?". Tests set option fields directly, so the helper marks exactly the
// fields that differ from the defaults as typed — the same thing a user running
// `setup --workspace custom-ws` would produce. (Whether Changed or a
// value-vs-default comparison is the right question is settled separately by
// TestApplySetupRecordDefaultsUsesFlagsChangedNotValues.)
func setupTestCommand(t *testing.T, out *bytes.Buffer, opts setupCommandOptions) *cobra.Command {
	t.Helper()
	cmd := newSetupCommand(Options{})
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	defaults := defaultSetupOptions()
	typed := map[string]string{}
	if opts.workspace != defaults.workspace {
		typed[setupFlagWorkspace] = opts.workspace
	}
	if opts.daemonName != defaults.daemonName {
		typed[setupFlagDaemonName] = opts.daemonName
	}
	if opts.interval != defaults.interval {
		typed[setupFlagInterval] = opts.interval.String()
	}
	if opts.distillEvery != defaults.distillEvery {
		typed[setupFlagDistillEvery] = opts.distillEvery.String()
	}
	if opts.model != defaults.model {
		typed[setupFlagModel] = opts.model
	}
	if opts.effort != defaults.effort {
		typed[setupFlagEffort] = opts.effort
	}
	for name, value := range typed {
		if err := cmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s=%q: %v", name, value, err)
		}
	}
	return cmd
}

func runSetupForTest(t *testing.T, f *setupTestFixture, opts setupCommandOptions, rec *recordedSetup) string {
	t.Helper()
	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, opts)
	if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	return out.String()
}

// TestSetupInstantPhaseRunsFirstAndReportsQueryable is the core promise: the
// blocking phase is the free deterministic one, and setup says so.
func TestSetupInstantPhaseRunsFirstAndReportsQueryable(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.instantCalls != 1 {
		t.Fatalf("the instant phase must run exactly once, ran %d", rec.instantCalls)
	}
	for _, want := range []string{"instant core", "the brain is queryable now", "Brain ready"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in setup output:\n%s", want, out)
		}
	}
}

func TestSetupFailsLoudlyWhenTheInstantPhaseFails(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{instantErr: context.DeadlineExceeded}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	cmd := setupTestCommand(t, &bytes.Buffer{}, opts)

	err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil || !strings.Contains(err.Error(), "instant phase failed") {
		t.Fatalf("a failed core build must fail setup, got %v", err)
	}
	// Nothing downstream may run on an unbuilt brain.
	if rec.spawnCalls != 0 || rec.installCalls != 0 {
		t.Fatalf("no backfill or daemon work may follow a failed instant phase: %+v", rec)
	}
}

// setupSemanticMismatchError is the reproduced defect verbatim: the installed
// Entire CLI and the brain derive different repo keys for a repo with no git
// remote, so the semantic snapshot the brain holds is rejected as another
// repo's.
const setupSemanticMismatchError = `semantic snapshot repo_key "local/repo" does not match current repo "local/repo-6f1c2a"`

// TestSetupContinuesWhenTheSemanticComponentFails drives the REAL instant phase
// over a fake runner whose semantic step fails with the repo-key skew. Before
// this, that one component aborted runSetup: no workspace, no daemon, no
// status, on a brain whose other four sources built perfectly. Setup must
// report it, keep going, and exit 0.
func TestSetupContinuesWhenTheSemanticComponentFails(t *testing.T) {
	f, runner := newSetupRefreshFixture(t)
	runner.responses[fakeCommandKey("entire", "graph", "snapshot", "--repo", f.repoDir, "--format", "ndjson", "--no-network")] = fakeCommandResponse{
		err: errors.New(setupSemanticMismatchError),
	}
	rec := &recordedSetup{}
	steps := rec.steps(f)
	steps.instant = nil // the real, best-effort instant phase
	opts := defaultSetupOptions()
	opts.noBackfill = true
	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, opts)

	if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, steps); err != nil {
		t.Fatalf("one failed component must not fail setup: %v\n%s", err, out)
	}

	for _, want := range []string{
		"semantic index failed",
		"repo key mismatch",
		// The dash is ASCII here on purpose: the test buffer is not a terminal,
		// so the renderer resolves to the plain, locale-independent glyph set.
		"-- continuing; run 'entire-brain doctor' for detail",
		setupRepoKeyMismatchHint,
		"the brain is queryable now",
		"FAILED semantic",
		"Brain ready",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("setup output missing %q:\n%s", want, out)
		}
	}
	// The phase summary must say what BUILT as well as what failed.
	if !strings.Contains(out.String(), "built ") {
		t.Fatalf("the phase summary must name the components that built:\n%s", out)
	}
	// Every later phase still ran.
	manifest, err := loadWorkspaceManifest(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatalf("a degraded component must not cost the repo its workspace: %v", err)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].RepoKey != f.storage.Key {
		t.Fatalf("workspace registration did not happen: %+v", manifest.Repos)
	}
	if rec.installCalls != 1 {
		t.Fatalf("the daemon must still be installed after a degraded component, installs = %d", rec.installCalls)
	}
	// The reason survives the terminal: status says failed, doctor has the why.
	stateDir := filepath.Dir(f.storage.HeadPath)
	failed := failedSetupComponents(stateDir)
	if len(failed) != 1 || failed[0].Name != brainComponentSemantic {
		t.Fatalf("the failed component must be recorded for doctor: %+v", failed)
	}
	brainManifest, _ := loadBrainManifest(f.storage.BrainDir)
	components := instantPhaseComponents(brainManifest, readSetupInstantRecord(stateDir))
	if state := statusComponentState(components, brainComponentSemantic); state != "failed" {
		t.Fatalf("status must show semantic=failed, got %q: %+v", state, components)
	}
	if state := statusComponentState(components, brainComponentSeed); state != "built" {
		t.Fatalf("the components that built must still read built, seed = %q", state)
	}
	// The entity index reports through the same reporter as every other
	// deterministic source, so its outcome is recorded for `doctor` instead of
	// vanishing into a stderr line a --json run discards. Like memory, branches
	// and patterns it is not one of the five manifest-backed sources `status`
	// lists, so the instant record is where it must show up.
	entities, found := setupComponent{}, false
	for _, component := range readSetupInstantRecord(stateDir).Components {
		if component.Name == brainComponentEntities {
			entities, found = component, true
		}
	}
	if !found || entities.State != "ok" {
		t.Fatalf("the entity index must report through the component reporter, got %+v (found=%v)", entities, found)
	}
}

// TestSetupFailsWhenEveryInstantComponentFails is the other half of the rule:
// best-effort is not "never fail". A phase that produced nothing usable exits
// non-zero, with every reason it collected.
func TestSetupFailsWhenEveryInstantComponentFails(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{instantComponents: []setupComponent{
		newSetupComponent(brainComponentSessions, errors.New("export unavailable")),
		newSetupComponent(brainComponentSemantic, errors.New(setupSemanticMismatchError)),
	}}
	opts := defaultSetupOptions()
	cmd := setupTestCommand(t, &bytes.Buffer{}, opts)

	err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatal("a phase in which every component failed must exit non-zero")
	}
	for _, want := range []string{"instant phase failed", "export unavailable", "repo key mismatch"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must collect every reason, missing %q: %v", want, err)
		}
	}
	if rec.spawnCalls != 0 || rec.installCalls != 0 {
		t.Fatalf("nothing may follow an instant phase that built nothing: %+v", rec)
	}
}

// TestSetupIsStillDegradedNotFailedWithASingleSurvivor pins the boundary: one
// built component is enough to keep going.
func TestSetupIsStillDegradedNotFailedWithASingleSurvivor(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{instantComponents: []setupComponent{
		newSetupComponent(brainComponentSeed, nil),
		newSetupComponent(brainComponentSemantic, errors.New(setupSemanticMismatchError)),
	}}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if !strings.Contains(out, "built seed; FAILED semantic") {
		t.Fatalf("the phase line must name both halves:\n%s", out)
	}
}

func statusComponentState(components []brainStatusComponent, name string) string {
	for _, component := range components {
		if component.Name == name {
			return component.State
		}
	}
	return ""
}

// newSetupRefreshFixture is a setup fixture wired to the real refresh path: a
// seeded repo, a fake command runner that answers every git/entire call the
// deterministic build makes, and a stubbed memory worker launch.
// setupFixtureEntityHead is the branch tip the entity index resolves to in the
// setup fixture. Nothing is indexed above it, which is the steady state on a
// repo whose entity index is already current.
const setupFixtureEntityHead = "e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9"

// addSetupEntityIndexFixture answers the entity index's deterministic git walk
// with "already current, nothing new to index". The entity index is a component
// of the SAME free refresh tick as sessions/seed/semantic, so a fixture that
// models that tick has to answer for it too — otherwise the component fails on
// an unmodelled command and the degradation test can no longer tell a real
// failure apart from a gap in the fixture.
func addSetupEntityIndexFixture(runner *fakeCommandRunner) {
	runner.responses[fakeCommandKey("git", "rev-parse", "--verify", "main^{commit}")] = fakeCommandResponse{
		stdout: setupFixtureEntityHead + "\n",
	}
	previous := runner.fallback
	runner.fallback = func(name string, args []string) (fakeCommandResponse, bool) {
		if name == "git" && len(args) > 0 {
			switch {
			// The builder picks its own range and bound, so answer the shape
			// rather than one pre-baked argv: no commits to count, none to walk.
			case args[0] == "rev-list" && len(args) > 2 && args[1] == "--count" && args[2] == "--first-parent":
				return fakeCommandResponse{stdout: "0\n"}, true
			case args[0] == "log" && len(args) > 1 && args[1] == "--first-parent":
				return fakeCommandResponse{}, true
			}
		}
		if previous != nil {
			return previous(name, args)
		}
		return fakeCommandResponse{}, false
	}
}

func newSetupRefreshFixture(t *testing.T) (*setupTestFixture, *fakeCommandRunner) {
	t.Helper()
	repoDir := seedFixtureRepo(t)
	home := t.TempDir()
	isolateDaemonEnv(t, home)
	env := semanticTestEnv(t, repoDir)
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	addSetupEntityIndexFixture(runner)
	runner.responses[fakeCommandKey("entire", "checkpoint", "explain", "--json", "--search-all")] = fakeCommandResponse{stdout: "[]\n"}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	previousLaunch := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { return nil }
	t.Cleanup(func() { memoryWorkerLaunch = previousLaunch })
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return setupTestNow }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("repoStoragePaths: %v", err)
	}
	return &setupTestFixture{repoDir: repoDir, home: home, env: env, runner: runner, opts: opts, storage: storage}, runner
}

// TestSetupBackfillIsDetachedNewestFirstAndCheap pins the backfill contract:
// detached, newest-first, on the cheap effort, with the budget honored.
func TestSetupBackfillIsDetachedNewestFirstAndCheap(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.backfillBudget = 2
	opts.model = "cheap-model"

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 1 {
		t.Fatalf("expected one detached backfill, got %d", rec.spawnCalls)
	}
	args := strings.Join(rec.spawned.Args, " ")
	for _, want := range []string{"distill", "--newest-first", "--agent codex", "--model cheap-model", "--effort low", "--max-sessions 2"} {
		if !strings.Contains(args, want) {
			t.Fatalf("backfill argv missing %q: %s", want, args)
		}
	}
	if rec.spawned.Dir != f.repoDir {
		t.Fatalf("backfill must run in the repo, got %s", rec.spawned.Dir)
	}
	if !strings.Contains(out, "fact backfill started in the background") {
		t.Fatalf("setup must say the backfill is detached:\n%s", out)
	}

	// The marker the status surface reads must exist, with the live pid.
	state, ok := readSetupBackfillState(filepath.Dir(f.storage.HeadPath))
	if !ok || state.PID != 4242 || state.Agent != "codex" || state.Sessions != 3 {
		t.Fatalf("backfill state not recorded correctly: %+v (ok=%v)", state, ok)
	}
}

func TestSetupSkipsBackfillWithoutAnAgent(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{} // detectAgent -> "none"
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 0 {
		t.Fatalf("no agent CLI means no backfill, spawned %d", rec.spawnCalls)
	}
	if !strings.Contains(out, "no agent CLI on PATH") {
		t.Fatalf("the skip must be one clear line:\n%s", out)
	}
}

func TestSetupSkipsBackfillWithoutSessions(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 0 {
		t.Fatalf("a repo with no captured sessions has nothing to distill, spawned %d", rec.spawnCalls)
	}
	if !strings.Contains(out, "no captured sessions yet") {
		t.Fatalf("the skip must be one clear line:\n%s", out)
	}
}

func TestSetupSkipsBackfillWhenEverythingIsAlreadyDistilled(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	f.markDistilled(t, "s1", "s2")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 0 {
		t.Fatalf("a fully distilled corpus must not re-spend, spawned %d", rec.spawnCalls)
	}
	if !strings.Contains(out, "already complete (2/2 sessions distilled)") {
		t.Fatalf("expected the already-complete line:\n%s", out)
	}
}

func TestSetupNoBackfillFlagSpendsNothing(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 0 {
		t.Fatalf("--no-backfill must spend nothing, spawned %d", rec.spawnCalls)
	}
	if !strings.Contains(out, "--no-backfill") {
		t.Fatalf("expected the opt-out to be reported:\n%s", out)
	}
}

// TestSetupRegistersRepoInWorkspaceIdempotently is the workspace half of
// idempotence: a second setup must find the existing membership, not append a
// duplicate.
func TestSetupRegistersRepoInWorkspaceIdempotently(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	first := runSetupForTest(t, f, opts, rec)
	if !strings.Contains(first, "workspace \"default\" now tracks") {
		t.Fatalf("first run should register the repo:\n%s", first)
	}
	second := runSetupForTest(t, f, opts, rec)
	if !strings.Contains(second, "already has this repo") {
		t.Fatalf("second run should detect existing membership:\n%s", second)
	}

	manifest, err := loadWorkspaceManifest(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(manifest.Repos) != 1 {
		t.Fatalf("re-running setup must not duplicate workspace members, got %d: %+v", len(manifest.Repos), manifest.Repos)
	}
	if manifest.Repos[0].RepoKey != f.storage.Key {
		t.Fatalf("workspace member key mismatch: %s vs %s", manifest.Repos[0].RepoKey, f.storage.Key)
	}
}

func TestSetupCreatesTheWorkspaceOnFirstUse(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.workspace = "brand-new"

	runSetupForTest(t, f, opts, rec)

	if _, err := loadWorkspaceManifest(f.env, "brand-new"); err != nil {
		t.Fatalf("setup must create the workspace it registers into: %v", err)
	}
}

// TestSetupInstallsExactlyOneDaemonAcrossRuns is THE idempotence guard: two
// setups in a row must leave one daemon, installed once.
func TestSetupInstallsExactlyOneDaemonAcrossRuns(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()

	first := runSetupForTest(t, f, opts, rec)
	if rec.installCalls != 1 {
		t.Fatalf("first setup must install the watcher once, got %d", rec.installCalls)
	}
	if !strings.Contains(first, "background watcher installed") {
		t.Fatalf("first run should report the install:\n%s", first)
	}

	second := runSetupForTest(t, f, opts, rec)
	if rec.installCalls != 1 {
		t.Fatalf("re-running setup must NOT reinstall a healthy watcher, installs = %d", rec.installCalls)
	}
	if !strings.Contains(second, "background watcher already running") {
		t.Fatalf("second run should detect the running watcher:\n%s", second)
	}
}

// TestSetupReinstallsADriftedDaemon is the flip side: idempotence must not mean
// "never repair". A unit that no longer matches this build is rewritten.
func TestSetupReinstallsADriftedDaemon(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{state: daemonState{Installed: true, Current: false, Running: true, Label: "io.entire.brain-watch"}}
	opts := defaultSetupOptions()

	out := runSetupForTest(t, f, opts, rec)

	if rec.installCalls != 1 {
		t.Fatalf("a drifted unit must be rewritten, installs = %d", rec.installCalls)
	}
	if !strings.Contains(out, "background watcher updated") {
		t.Fatalf("expected the update wording:\n%s", out)
	}
}

// TestSetupSkipsTheDaemonOnAnOSWithNoServiceManager pins the windows contract.
// There is no launchd and no systemd there, so setup must SAY so in one line and
// carry on — not fail, and not pretend to have installed something. The plan is
// injected for "windows" rather than read from the host, so this holds on every
// runner (it is the only interesting case a mac or linux box cannot reach).
func TestSetupSkipsTheDaemonOnAnOSWithNoServiceManager(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{targetOS: "windows"}
	opts := defaultSetupOptions()
	opts.json = false

	out := runSetupForTest(t, f, opts, rec)

	if rec.installCalls != 0 || rec.uninstallCalls != 0 {
		t.Fatalf("an OS with no service manager must not install or uninstall anything: %+v", rec)
	}
	if !strings.Contains(out, "background watcher: not supported on windows yet") {
		t.Fatalf("the skip must name the platform in one plain line:\n%s", out)
	}
	if !strings.Contains(out, "skipped") {
		t.Fatalf("the line must read as a skip, not as a step that ran:\n%s", out)
	}
	if !strings.Contains(out, "entire-brain workspace watch") {
		t.Fatalf("the skip must say how to keep the brain fresh by hand:\n%s", out)
	}
	// One line, not two: the phase must not also announce a failure or a warning.
	if strings.Count(out, "background watcher") != 1 {
		t.Fatalf("the unsupported daemon must cost exactly one line:\n%s", out)
	}
	// Everything the supported phases do still happens.
	if rec.instantCalls != 1 {
		t.Fatalf("the instant phase must still run, got %d", rec.instantCalls)
	}
	manifest, err := loadWorkspaceManifest(f.env, setupDefaultWorkspace)
	if err != nil {
		t.Fatalf("workspace registration must still happen: %v", err)
	}
	if len(manifest.Repos) != 1 {
		t.Fatalf("the repo must still join the workspace: %+v", manifest.Repos)
	}
}

// TestSetupReportsTheUnsupportedDaemonInJSON: whatever the terminal says, the
// machine-readable report must not claim an install either.
func TestSetupReportsTheUnsupportedDaemonInJSON(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{targetOS: "windows"}
	opts := defaultSetupOptions()
	opts.json = true

	out := runSetupForTest(t, f, opts, rec)

	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("--json must emit only the report: %v\n%s", err, out)
	}
	if report.Daemon.Manager != daemonManagerUnsupported || report.Daemon.Installed {
		t.Fatalf("the report must say unsupported and not installed: %+v", report.Daemon)
	}
	if !strings.Contains(report.Daemon.Detail, "windows") {
		t.Fatalf("the detail must name the OS: %+v", report.Daemon)
	}
}

func TestSetupNoDaemonSkipsTheInstall(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.installCalls != 0 {
		t.Fatalf("--no-daemon must not install anything, installs = %d", rec.installCalls)
	}
	if !strings.Contains(out, "--no-daemon") {
		t.Fatalf("expected the opt-out to be reported:\n%s", out)
	}
	// The instant phase and workspace registration still happen.
	if rec.instantCalls != 1 {
		t.Fatalf("--no-daemon must not skip the instant phase")
	}
}

func TestSetupUninstallDaemonIsStandalone(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{state: daemonState{Installed: true, Running: true, Label: "io.entire.brain-watch"}}
	opts := defaultSetupOptions()
	opts.uninstallDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.uninstallCalls != 1 {
		t.Fatalf("expected one uninstall, got %d", rec.uninstallCalls)
	}
	if rec.instantCalls != 0 || rec.spawnCalls != 0 || rec.installCalls != 0 {
		t.Fatalf("--uninstall-daemon must do nothing else: %+v", rec)
	}
	if !strings.Contains(out, "removed the watcher") {
		t.Fatalf("expected the removal line:\n%s", out)
	}
}

func TestSetupJSONReportIsMachineReadable(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	f.markDistilled(t, "s1")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.json = true

	out := runSetupForTest(t, f, opts, rec)

	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("--json must emit only the report: %v\n%s", err, out)
	}
	if report.Instant.State != "ok" || report.Backfill.State != "ok" {
		t.Fatalf("unexpected phase states: %+v", report)
	}
	if report.Facts.Sessions != 2 || report.Facts.Distilled != 1 {
		t.Fatalf("expected 1/2 distilled in the report, got %d/%d", report.Facts.Distilled, report.Facts.Sessions)
	}
	if report.RepoKey != f.storage.Key {
		t.Fatalf("report repo key mismatch: %s", report.RepoKey)
	}
}

// TestBrainWatchDaemonArgsStayTokenFrugal pins the daemon's cost contract: the
// deterministic refresh runs free every tick, and the only token-spending step
// is interval-gated. It is a shape check only — what the flags MEAN under a
// long-lived daemon is proved by TestDaemonBudgetSemanticsAllowOneRunPerWindow.
func TestBrainWatchDaemonArgsStayTokenFrugal(t *testing.T) {
	t.Parallel()
	opts := defaultSetupOptions()
	opts.effort = "low"
	args := strings.Join(brainWatchDaemonArgs(opts), " ")
	for _, want := range []string{"workspace watch default", "--distill-every 24h0m0s", "--effort low"} {
		if !strings.Contains(args, want) {
			t.Fatalf("daemon argv missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--seed-agent") {
		t.Fatalf("the daemon must never enable agent seed synthesis: %s", args)
	}
}

// daemonWatchOptions parses the argv setup actually installs, so the semantics
// test is driven by the real flags rather than by a hand-built option struct
// that could drift away from them.
func daemonWatchOptions(t *testing.T, setupOpts setupCommandOptions) watchCommandOptions {
	t.Helper()
	w := defaultWatchOptions()
	cmd := &cobra.Command{Use: "watch", RunE: func(*cobra.Command, []string) error { return nil }}
	bindWatchFlags(cmd, &w)
	args := brainWatchDaemonArgs(setupOpts)
	// Drop the "workspace watch <name>" verb prefix; only the flags are parsed.
	if err := cmd.ParseFlags(args[3:]); err != nil {
		t.Fatalf("parse daemon argv %v: %v", args, err)
	}
	return w
}

// TestDaemonBudgetSemanticsAllowOneRunPerWindow is the regression test for the
// one-shot daemon. The installed watcher is a KeepAlive service that never
// exits, and --budget counts gated agent runs for the life of the PROCESS with
// no reset — so `--budget 1` did not mean "one run per --distill-every window",
// it meant one run EVER, machine-wide, across every repo. A machine that
// distilled once and then never again looked healthy in every status output.
//
// This simulates the real daemon: one long-lived process, one shared agentCalls
// counter (workspaceWatchLoop's), two member repos with their own cursors,
// across three --distill-every windows. The contract is one gated run per repo
// per window: 6, not 1.
func TestDaemonBudgetSemanticsAllowOneRunPerWindow(t *testing.T) {
	t.Parallel()
	w := daemonWatchOptions(t, defaultSetupOptions())
	if !w.distill {
		t.Fatal("the installed daemon must enable distill")
	}

	type repo struct {
		cursorPath  string
		fingerprint string
	}
	repos := []*repo{
		{cursorPath: filepath.Join(t.TempDir(), "watch.json"), fingerprint: "a0"},
		{cursorPath: filepath.Join(t.TempDir(), "watch.json"), fingerprint: "b0"},
	}
	// One process, one shared counter — exactly what workspaceWatchLoop keeps.
	agentCalls := 0
	distilled := 0
	now := setupTestNow
	const windows = 3
	for window := 0; window < windows; window++ {
		for i, r := range repos {
			// New work landed in each repo since the last window.
			r.fingerprint = fmt.Sprintf("repo%d-window%d", i, window)
			steps := watchSteps{
				now:         func() time.Time { return now },
				fingerprint: func(context.Context) string { return r.fingerprint },
				refresh:     func(context.Context) error { return nil },
				seed:        func(context.Context) error { return nil },
				distill:     func(context.Context) error { distilled++; return nil },
			}
			watchTick(context.Background(), io.Discard, w, r.cursorPath, steps, &agentCalls)
		}
		now = now.Add(w.distillEvery)
	}

	if want := windows * len(repos); distilled != want {
		t.Fatalf("a long-lived daemon must distill once per repo per --distill-every window: got %d, want %d "+
			"(a process-lifetime --budget makes this 1 and the daemon never spends again)", distilled, want)
	}
}

// TestDaemonBudgetGateStillHoldsInsideAWindow is the other half: dropping
// --budget must not make the daemon spend every tick.
func TestDaemonBudgetGateStillHoldsInsideAWindow(t *testing.T) {
	t.Parallel()
	w := daemonWatchOptions(t, defaultSetupOptions())
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	agentCalls := 0
	distilled := 0
	now := setupTestNow
	for tick := 0; tick < 5; tick++ {
		fingerprint := fmt.Sprintf("changed-every-tick-%d", tick)
		steps := watchSteps{
			now:         func() time.Time { return now },
			fingerprint: func(context.Context) string { return fingerprint },
			refresh:     func(context.Context) error { return nil },
			seed:        func(context.Context) error { return nil },
			distill:     func(context.Context) error { distilled++; return nil },
		}
		watchTick(context.Background(), io.Discard, w, cursorPath, steps, &agentCalls)
		now = now.Add(w.interval)
	}
	if distilled != 1 {
		t.Fatalf("inside one --distill-every window the cursor must allow exactly one spend, got %d", distilled)
	}
}

func TestSetupChildEnvCarriesPluginDirs(t *testing.T) {
	t.Parallel()
	env := EntireEnv{PluginDataDir: "/data", PluginStateDir: "/state"}
	values := setupPluginEnv(env, "/repo")
	if values[envPluginDataDir] != "/data" || values[envPluginStateDir] != "/state" || values[envRepoRoot] != "/repo" {
		t.Fatalf("detached child must inherit the plugin store it was launched against: %+v", values)
	}
	if _, ok := values[envPluginConfigDir]; ok {
		t.Fatalf("unset dirs must not be forwarded as empty: %+v", values)
	}
}

// TestSetupChildEnvMergesTheProcessEnvironment covers the half setupPluginEnv
// cannot: the detached child needs PATH (it shells out to git and the agent
// CLI) and the rest of the ambient environment, with the plugin dirs OVERRIDING
// whatever the parent inherited rather than being appended alongside a stale
// value. Go's exec resolves duplicate keys last-wins, which is what makes the
// append order load-bearing.
func TestSetupChildEnvMergesTheProcessEnvironment(t *testing.T) {
	t.Setenv("ENTIRE_SETUP_CHILD_ENV_MARKER", "inherited")
	t.Setenv(envPluginStateDir, "/stale-from-parent")
	t.Setenv(envRepoRoot, "/stale-repo")

	got := setupChildEnv(EntireEnv{PluginStateDir: "/state"}, "/repo")

	// last-wins resolution, the same rule os/exec applies.
	resolved := map[string]string{}
	for _, entry := range got {
		if key, value, ok := strings.Cut(entry, "="); ok {
			resolved[key] = value
		}
	}
	if resolved["ENTIRE_SETUP_CHILD_ENV_MARKER"] != "inherited" {
		t.Fatalf("the child must inherit the process environment (PATH, HOME, ...): %+v", resolved["ENTIRE_SETUP_CHILD_ENV_MARKER"])
	}
	if resolved[envPluginStateDir] != "/state" {
		t.Fatalf("the resolved plugin dir must override the inherited one, got %q", resolved[envPluginStateDir])
	}
	if resolved[envRepoRoot] != "/repo" {
		t.Fatalf("the child must target the repo setup ran in, got %q", resolved[envRepoRoot])
	}
	if len(got) <= len(setupPluginEnv(EntireEnv{PluginStateDir: "/state"}, "/repo")) {
		t.Fatalf("setupChildEnv must MERGE os.Environ, not replace it (%d entries)", len(got))
	}
}

// TestSetupRemembersItsIdentitiesForLaterReads guards a gap a live run exposed:
// with a custom --daemon-name, `status` and `setup --uninstall-daemon` were
// looking for the DEFAULT daemon and reporting the real one as not installed.
func TestSetupRemembersItsIdentitiesForLaterReads(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.workspace = "custom-ws"
	opts.daemonName = "entire-brain-watch-custom"

	runSetupForTest(t, f, opts, rec)

	stateDir := filepath.Dir(f.storage.HeadPath)
	recorded, existed, recordErr := setupOptionsFromRecord(stateDir)
	if recordErr != nil {
		t.Fatalf("read setup record: %v", recordErr)
	}
	if !existed {
		t.Fatal("setup must write a record for this repo")
	}
	if recorded.workspace != "custom-ws" || recorded.daemonName != "entire-brain-watch-custom" {
		t.Fatalf("setup must record the identities it used, got %+v", recorded)
	}

	// A later invocation that names neither must inherit both.
	inherited := applySetupRecordDefaults(defaultSetupOptions(), recorded, nothingChanged)
	if inherited.workspace != "custom-ws" || inherited.daemonName != "entire-brain-watch-custom" {
		t.Fatalf("a later run must address the same daemon and workspace, got %+v", inherited)
	}

	// An explicit flag still wins over the record.
	explicit := defaultSetupOptions()
	explicit.daemonName = "entire-brain-watch-other"
	if got := applySetupRecordDefaults(explicit, recorded, changedFlags(setupFlagDaemonName)).daemonName; got != "entire-brain-watch-other" {
		t.Fatalf("an explicit --daemon-name must win, got %s", got)
	}
}

func nothingChanged(string) bool { return false }

func changedFlags(names ...string) func(string) bool {
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	return func(name string) bool { return set[name] }
}

// TestSetupRecordRemembersTheDaemonTuning is the second half of the identity
// record: a repo set up with a custom interval/model/effort must keep them on a
// bare re-run. Reverting them silently would re-render the machine-wide unit
// with stock values and restart the daemon.
func TestSetupRecordRemembersTheDaemonTuning(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.interval = 90 * time.Second
	opts.distillEvery = 6 * time.Hour
	opts.model = "cheap-model"
	opts.effort = "minimal"

	runSetupForTest(t, f, opts, rec)

	recorded, _, _ := setupOptionsFromRecord(filepath.Dir(f.storage.HeadPath))
	inherited := applySetupRecordDefaults(defaultSetupOptions(), recorded, nothingChanged)
	if inherited.interval != 90*time.Second || inherited.distillEvery != 6*time.Hour {
		t.Fatalf("a bare re-run must keep the recorded cadence, got interval=%s distill-every=%s", inherited.interval, inherited.distillEvery)
	}
	if inherited.model != "cheap-model" || inherited.effort != "minimal" {
		t.Fatalf("a bare re-run must keep the recorded model/effort, got %+v", inherited)
	}
	// And the daemon it would install is byte-identical, so nothing restarts.
	first := strings.Join(brainWatchDaemonArgs(opts), " ")
	if second := strings.Join(brainWatchDaemonArgs(inherited), " "); first != second {
		t.Fatalf("a bare re-run must re-render the same unit:\n%s\n%s", first, second)
	}
}

// TestApplySetupRecordDefaultsUsesFlagsChangedNotValues is the bug the review
// named: deciding "did the user pass this?" by comparing against the default
// makes an EXPLICIT default indistinguishable from silence. A user moving a
// repo back onto the default daemon by typing --daemon-name entire-brain-watch
// had the recorded custom name restored instead.
func TestApplySetupRecordDefaultsUsesFlagsChangedNotValues(t *testing.T) {
	t.Parallel()
	recorded := defaultSetupOptions()
	recorded.daemonName = "entire-brain-watch-custom"
	recorded.workspace = "custom-ws"
	recorded.interval = time.Hour

	// Explicitly typing the DEFAULT value must win over the record.
	explicit := defaultSetupOptions()
	got := applySetupRecordDefaults(explicit, recorded, changedFlags(setupFlagDaemonName, setupFlagWorkspace, setupFlagInterval))
	if got.daemonName != daemonDefaultName {
		t.Fatalf("an explicit --daemon-name %s must not be overridden by the record, got %s", daemonDefaultName, got.daemonName)
	}
	if got.workspace != setupDefaultWorkspace {
		t.Fatalf("an explicit --workspace %s must win, got %s", setupDefaultWorkspace, got.workspace)
	}
	if got.interval != defaultSetupOptions().interval {
		t.Fatalf("an explicit --interval must win, got %s", got.interval)
	}

	// Silence still inherits.
	silent := applySetupRecordDefaults(defaultSetupOptions(), recorded, nothingChanged)
	if silent.daemonName != "entire-brain-watch-custom" || silent.workspace != "custom-ws" || silent.interval != time.Hour {
		t.Fatalf("an unmentioned flag must inherit the record, got %+v", silent)
	}
}

// TestSetupFlagChangedReadsCobra binds the real command so the flag names the
// record keys off cannot drift away from the flags actually registered.
func TestSetupFlagChangedReadsCobra(t *testing.T) {
	t.Parallel()
	cmd := newSetupCommand(Options{})
	for _, name := range []string{setupFlagWorkspace, setupFlagDaemonName, setupFlagInterval, setupFlagDistillEvery, setupFlagModel, setupFlagEffort} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("setup must register the remembered flag %q", name)
		}
	}
	if err := cmd.Flags().Parse([]string{"--effort", "low"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	changed := setupFlagChanged(cmd)
	if !changed(setupFlagEffort) {
		t.Fatal("a typed flag must read as changed")
	}
	if changed(setupFlagModel) {
		t.Fatal("an untyped flag must read as unchanged")
	}
	if changed("no-such-flag") {
		t.Fatal("an unknown flag must not panic or read as changed")
	}
}

// TestSetupRenamingTheDaemonRetiresTheOldUnit: writing the new unit without
// removing the old one leaves a second watcher running under a label no later
// command can name.
func TestSetupRenamingTheDaemonRetiresTheOldUnit(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	first := defaultSetupOptions()
	first.daemonName = "entire-brain-watch-old"
	runSetupForTest(t, f, first, rec)
	if rec.uninstallCalls != 0 {
		t.Fatalf("the first setup has nothing to retire, got %d", rec.uninstallCalls)
	}

	// rec.state is whatever install left behind, so the old unit inspects as
	// installed — which is the condition that must trigger the retirement.
	second := defaultSetupOptions()
	second.daemonName = "entire-brain-watch-new"
	out := runSetupForTest(t, f, second, rec)

	if rec.uninstallCalls != 1 {
		t.Fatalf("renaming the daemon must unload the old unit exactly once, got %d", rec.uninstallCalls)
	}
	if !strings.Contains(out, "previous watcher") || !strings.Contains(out, "removed") {
		t.Fatalf("the retirement must be reported:\n%s", out)
	}
	if recorded, _, _ := setupOptionsFromRecord(filepath.Dir(f.storage.HeadPath)); recorded.daemonName != "entire-brain-watch-new" {
		t.Fatalf("the record must move to the new daemon, got %s", recorded.daemonName)
	}
}

// TestSetupFirstRunNeverRetiresAnotherReposDaemon is the guard on the guard: a
// repo that has never been set up has no daemon of its own, so a first-ever
// `setup --daemon-name mine` must not read the absent record as the default
// name and uninstall the machine-wide watcher every other repo depends on.
func TestSetupFirstRunNeverRetiresAnotherReposDaemon(t *testing.T) {
	f := newSetupTestFixture(t)
	// A default-named watcher is already installed and healthy on this machine.
	rec := &recordedSetup{state: daemonState{Manager: daemonManagerLaunchd, Label: launchdLabel(daemonDefaultName), Installed: true, Current: true, Running: true}}
	opts := defaultSetupOptions()
	opts.daemonName = "entire-brain-watch-mine"

	runSetupForTest(t, f, opts, rec)

	if rec.uninstallCalls != 0 {
		t.Fatalf("a first-ever setup must not uninstall anything, got %d", rec.uninstallCalls)
	}
}

func TestSetupOptionsFromRecordFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	got, existed, recordErr := setupOptionsFromRecord(t.TempDir())
	if recordErr != nil {
		t.Fatalf("a never-set-up repo must not be an error: %v", recordErr)
	}
	if existed {
		t.Fatal("a never-set-up repo must report that it has no record")
	}
	if got.workspace != setupDefaultWorkspace || got.daemonName != daemonDefaultName {
		t.Fatalf("a never-set-up repo must read as the defaults, got %+v", got)
	}
}

// TestSetupSkipsBackfillWhileOneIsStillRunning is the re-entrancy guard: setup
// is what an impatient user re-runs, and the backfill is the phase that takes
// hours. Spawning a second detached pass doubles the token spend on exactly the
// sessions the first pass is working through.
func TestSetupSkipsBackfillWhileOneIsStillRunning(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	runSetupForTest(t, f, opts, rec)
	if rec.spawnCalls != 1 {
		t.Fatalf("the first setup must start the backfill, got %d", rec.spawnCalls)
	}

	// The recorded pid must look alive for the guard to fire; use our own.
	stateDir := filepath.Dir(f.storage.HeadPath)
	state, ok := readSetupBackfillState(stateDir)
	if !ok {
		t.Fatal("the first run must record backfill state")
	}
	state.PID = os.Getpid()
	if err := writeSetupBackfillState(stateDir, state); err != nil {
		t.Fatalf("write backfill state: %v", err)
	}

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 1 {
		t.Fatalf("a live backfill must not be duplicated, spawned %d times", rec.spawnCalls)
	}
	if !strings.Contains(out, "already running") {
		t.Fatalf("the skip must report the running pass and its progress:\n%s", out)
	}
	if !strings.Contains(out, "0/3 sessions distilled so far") {
		t.Fatalf("the skip must report progress, not just 'busy':\n%s", out)
	}
}

// TestSetupRestartsTheBackfillWhenTheRecordedProcessIsDead is the flip side:
// skip-if-running must not become skip-forever after a crash or a reboot.
func TestSetupRestartsTheBackfillWhenTheRecordedProcessIsDead(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	stateDir := filepath.Dir(f.storage.HeadPath)
	if err := writeSetupBackfillState(stateDir, setupBackfillState{PID: 0x7FFFFFFF, StartedAt: setupTestNow}); err != nil {
		t.Fatalf("write backfill state: %v", err)
	}

	runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 1 {
		t.Fatalf("a dead recorded pid must not block a new backfill, spawned %d", rec.spawnCalls)
	}
}

// TestSetupCapsTheDefaultBackfillSpend: a bare `setup` used to distill the whole
// corpus — potentially years of sessions — with no cost line anywhere.
func TestSetupCapsTheDefaultBackfillSpend(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2", "s3")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions() // no --backfill-budget given
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	args := strings.Join(rec.spawned.Args, " ")
	if !strings.Contains(args, fmt.Sprintf("--max-sessions %d", setupDefaultBackfillBudget)) {
		t.Fatalf("a bare setup must cap the background spend: %s", args)
	}
	// Nothing was capped here (3 < 25), so no cap line should appear.
	if strings.Contains(out, "capped at") {
		t.Fatalf("a corpus under the cap must not claim it was capped:\n%s", out)
	}
}

func TestSetupReportsWhatTheCapDidAndHowToLiftIt(t *testing.T) {
	ids := make([]string, 0, setupDefaultBackfillBudget+3)
	for i := 0; i < setupDefaultBackfillBudget+3; i++ {
		ids = append(ids, fmt.Sprintf("s%02d", i))
	}
	f := newSetupTestFixture(t, ids...)
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if !strings.Contains(out, fmt.Sprintf("capped at %d of %d pending session(s)", setupDefaultBackfillBudget, len(ids))) {
		t.Fatalf("the cap must say what it did:\n%s", out)
	}
	if !strings.Contains(out, "--backfill-budget 0") {
		t.Fatalf("the cap must say how to lift it:\n%s", out)
	}
}

// TestSetupBackfillBudgetZeroIsExplicitlyUnlimited keeps the escape hatch: 0 now
// means "I really do want the whole corpus", not "no opinion".
func TestSetupBackfillBudgetZeroIsExplicitlyUnlimited(t *testing.T) {
	f := newSetupTestFixture(t, "s1", "s2")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.backfillBudget = 0

	runSetupForTest(t, f, opts, rec)

	if args := strings.Join(rec.spawned.Args, " "); strings.Contains(args, "--max-sessions") {
		t.Fatalf("--backfill-budget 0 must not cap the pass: %s", args)
	}
}

// TestBrainWatchDaemonPlanIsRepoIndependent is the daemon-churn guard: the unit
// is machine-wide, so nothing in it may vary with which repo ran setup. A
// per-repo log path made every new repo's setup see Contents != Current and
// unload/reload the running watcher.
func TestBrainWatchDaemonPlanIsRepoIndependent(t *testing.T) {
	first := newSetupTestFixture(t)
	// Planned for a named OS, not the host's: on windows the real plan is
	// unsupported and carries no contents at all, so a host-keyed comparison
	// would compare two empty plans and pass without testing anything.
	firstPlan, err := brainWatchDaemonPlanFor("darwin", first.opts, defaultSetupOptions())
	if err != nil {
		t.Fatalf("first plan: %v", err)
	}
	// A second repo in the SAME plugin store — the ordinary case of setting up
	// another project on one machine.
	secondRepo := t.TempDir()
	secondOpts := first.opts
	secondOpts.Env.RepoRoot = secondRepo
	secondPlan, err := brainWatchDaemonPlanFor("darwin", secondOpts, defaultSetupOptions())
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}

	if firstPlan.Contents != secondPlan.Contents {
		t.Fatalf("the machine-wide unit must not depend on the repo that ran setup:\n--- first ---\n%s\n--- second ---\n%s",
			firstPlan.Contents, secondPlan.Contents)
	}
	if firstPlan.UnitPath != secondPlan.UnitPath || firstPlan.Label != secondPlan.Label {
		t.Fatalf("both repos must address one daemon: %s/%s vs %s/%s",
			firstPlan.Label, firstPlan.UnitPath, secondPlan.Label, secondPlan.UnitPath)
	}
	if strings.Contains(firstPlan.Contents, first.repoDir) {
		t.Fatalf("the unit must not name the repo that installed it:\n%s", firstPlan.Contents)
	}
}

// TestSetupSecondRepoDoesNotRestartTheDaemon is the same property one level up,
// through runSetup itself: setting up a second repo must leave the running
// watcher strictly alone.
func TestSetupSecondRepoDoesNotRestartTheDaemon(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	opts := defaultSetupOptions()

	runSetupForTest(t, f, opts, rec)
	if rec.installCalls != 1 {
		t.Fatalf("the first repo installs the watcher, got %d", rec.installCalls)
	}

	// A second repo, same machine, same plugin store, same daemon.
	secondRepo := t.TempDir()
	secondFixture := *f
	secondFixture.repoDir = secondRepo
	// inspect must answer from the REAL plan comparison, so re-derive the state
	// the same way inspectDaemon would: installed + current + running.
	plan, err := brainWatchDaemonPlanFor(rec.plannedOS(), f.opts, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rec.state = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Installed: true, Current: true, Running: true}

	out := runSetupForTest(t, &secondFixture, opts, rec)

	if rec.installCalls != 1 {
		t.Fatalf("a second repo must not reinstall the machine-wide watcher, installs = %d", rec.installCalls)
	}
	if !strings.Contains(out, "background watcher already running") {
		t.Fatalf("the second repo should find the running watcher:\n%s", out)
	}
}

// TestDaemonUnitPathHonorsTheEnvOverride: a sandboxed run with a REAL $HOME must
// still be unable to write into the developer's LaunchAgents / systemd user dir.
func TestDaemonUnitPathHonorsTheEnvOverride(t *testing.T) {
	sandbox := t.TempDir()
	for _, tc := range []struct{ goos, home string }{
		{"darwin", "/Users/real"},
		{"linux", "/home/real"},
	} {
		plan, err := planBrainWatchDaemon(tc.goos, tc.home, "/real/.config", sandbox, fixedDaemonSpec())
		if err != nil {
			t.Fatalf("%s plan: %v", tc.goos, err)
		}
		if filepath.Dir(plan.UnitPath) != sandbox {
			t.Fatalf("%s: %s must be redirected into %s, got %s", tc.goos, envDaemonUnitDir, sandbox, plan.UnitPath)
		}
		if strings.Contains(plan.UnitPath, tc.home) {
			t.Fatalf("%s: a real home must never appear in a redirected unit path: %s", tc.goos, plan.UnitPath)
		}
	}
}

// TestBrainWatchDaemonPlanReadsTheEnvOverride proves the knob is actually wired
// into the path setup takes, not just into planBrainWatchDaemon's signature.
func TestBrainWatchDaemonPlanReadsTheEnvOverride(t *testing.T) {
	f := newSetupTestFixture(t)
	sandbox := t.TempDir()
	t.Setenv(envDaemonUnitDir, sandbox)

	// Planned for a named OS so the guard is exercised on every host: skipping
	// it where the host has no service manager would leave the sandbox knob —
	// the thing standing between a test run and a real LaunchAgents write —
	// untested on exactly the platform whose CI is most likely to be ignored.
	plan, err := brainWatchDaemonPlanFor("darwin", f.opts, defaultSetupOptions())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if filepath.Dir(plan.UnitPath) != sandbox {
		t.Fatalf("%s must redirect the unit into %s, got %s", envDaemonUnitDir, sandbox, plan.UnitPath)
	}
}
