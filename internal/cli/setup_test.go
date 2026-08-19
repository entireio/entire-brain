package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

func newSetupTestFixture(t *testing.T, sessionIDs ...string) *setupTestFixture {
	t.Helper()
	repoDir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
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
	instantErr     error
}

func (r *recordedSetup) steps() setupSteps {
	return setupSteps{
		now:     func() time.Time { return setupTestNow },
		instant: func(context.Context) error { r.instantCalls++; return r.instantErr },
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

func runSetupForTest(t *testing.T, f *setupTestFixture, opts setupCommandOptions, rec *recordedSetup) string {
	t.Helper()
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps()); err != nil {
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
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	opts := defaultSetupOptions()
	opts.noDaemon = true

	err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps())
	if err == nil || !strings.Contains(err.Error(), "instant phase failed") {
		t.Fatalf("a failed core build must fail setup, got %v", err)
	}
	// Nothing downstream may run on an unbuilt brain.
	if rec.spawnCalls != 0 || rec.installCalls != 0 {
		t.Fatalf("no backfill or daemon work may follow a failed instant phase: %+v", rec)
	}
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
// is both interval-gated and budget-capped.
func TestBrainWatchDaemonArgsStayTokenFrugal(t *testing.T) {
	t.Parallel()
	opts := defaultSetupOptions()
	opts.effort = "low"
	args := strings.Join(brainWatchDaemonArgs(opts), " ")
	for _, want := range []string{"workspace watch default", "--distill-every 24h0m0s", "--budget 1", "--effort low"} {
		if !strings.Contains(args, want) {
			t.Fatalf("daemon argv missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--seed-agent") {
		t.Fatalf("the daemon must never enable agent seed synthesis: %s", args)
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
