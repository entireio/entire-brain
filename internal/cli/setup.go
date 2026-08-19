package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// setup.go is the one-command onboarding path. Before it there were three
// separate ways to stand a brain up — build it, build it with an agent, extract
// facts — and the only one that made the brain useful immediately was the one
// people skipped, while the one that took O(sessions) minutes was the one they
// ran in the foreground and then forgot to ever run again.
//
// `entire-brain setup` collapses that into three phases with honest cost labels:
//
//  1. INSTANT (blocking, seconds, $0): the deterministic core — session export,
//     semantic index, seed, docs, history. This is the SAME free path `watch`
//     runs every tick with seed agent "none". When it returns, the brain is
//     queryable.
//  2. BACKFILL (detached, spends tokens): distill over past sessions, NEWEST
//     FIRST so the most useful facts land first, on the cheap model/effort, with
//     a session budget and the persisted distill cache so a re-run never
//     re-spends. It is detached, so the prompt comes back immediately.
//  3. DAEMON (installed once, machine-wide): the repo joins a workspace and a
//     single `workspace watch` service keeps every member fresh. Deterministic
//     refresh is free; the agent step stays gated by --distill-every + --budget.
//
// Every phase is idempotent. Running setup twice must produce the same machine,
// not two daemons and two workspace entries.
const (
	setupDefaultWorkspace = "default"
	// setupBackfillDefaultEffort keeps the background token spend cheap by
	// default. Model is deliberately NOT defaulted to a hard-coded name — model
	// ids age badly — so the agent's own default applies unless --model is given.
	setupBackfillDefaultEffort = "low"
	setupBackfillStateFile     = "backfill.json"
	setupBackfillLogFile       = "backfill.log"
	setupBackfillStateVersion  = 1
	// setupRecordFile remembers which workspace and daemon identity this repo
	// was set up with, so `status` reports on the daemon that actually exists
	// rather than on the default one it would have created.
	setupRecordFile = "setup.json"
	// setupDaemonBudget caps gated agent runs per daemon process. One per
	// --distill-every window is the frugal default the docs recommend.
	setupDaemonBudget = 1
)

type setupCommandOptions struct {
	workspace       string
	daemonName      string
	noDaemon        bool
	uninstallDaemon bool
	noBackfill      bool
	agent           string
	model           string
	effort          string
	backfillBudget  int
	interval        time.Duration
	distillEvery    time.Duration
	json            bool
}

func defaultSetupOptions() setupCommandOptions {
	watch := defaultWatchOptions()
	return setupCommandOptions{
		workspace:    setupDefaultWorkspace,
		daemonName:   daemonDefaultName,
		agent:        "auto",
		effort:       setupBackfillDefaultEffort,
		interval:     watch.interval,
		distillEvery: watch.distillEvery,
	}
}

// setupSteps are the side effects of one setup run, injected so the
// orchestration (phase order, idempotence, skip reasons, reporting) is testable
// without running a real refresh, spawning a real process, or touching a real
// service manager.
type setupSteps struct {
	now           func() time.Time
	instant       func(context.Context) error
	detectAgent   func(context.Context) string
	spawnBackfill func(context.Context, setupBackfillPlan) (int, error)
	inspect       func(context.Context, daemonPlan) daemonState
	install       func(context.Context, daemonPlan) error
	uninstall     func(context.Context, daemonPlan) error
}

// setupBackfillPlan is the detached distill invocation.
type setupBackfillPlan struct {
	Binary  string
	Args    []string
	Dir     string
	LogPath string
	Env     []string
}

type setupPhase struct {
	State   string  `json:"state"` // ok | skipped | failed
	Detail  string  `json:"detail,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

type setupWorkspaceState struct {
	Name       string `json:"name"`
	Created    bool   `json:"created"`
	Registered bool   `json:"registered"`
	Already    bool   `json:"already_registered"`
	RepoKey    string `json:"repo_key,omitempty"`
}

type setupBackfillState struct {
	SchemaVersion int       `json:"schema_version"`
	StartedAt     time.Time `json:"started_at"`
	PID           int       `json:"pid"`
	Agent         string    `json:"agent,omitempty"`
	Model         string    `json:"model,omitempty"`
	Effort        string    `json:"effort,omitempty"`
	MaxSessions   int       `json:"max_sessions,omitempty"`
	LogPath       string    `json:"log_path,omitempty"`
	Sessions      int       `json:"sessions,omitempty"`
}

type setupReport struct {
	SchemaVersion int                 `json:"schema_version"`
	GeneratedAt   time.Time           `json:"generated_at"`
	Repo          string              `json:"repo"`
	RepoKey       string              `json:"repo_key"`
	BrainPath     string              `json:"brain_path"`
	Instant       setupPhase          `json:"instant"`
	Backfill      setupPhase          `json:"backfill"`
	Workspace     setupWorkspaceState `json:"workspace"`
	Daemon        daemonState         `json:"daemon"`
	Hook          setupHookState      `json:"session_end_hook"`
	Facts         factsBackfillStatus `json:"facts"`
	Warnings      []string            `json:"warnings,omitempty"`
}

func newSetupCommand(opts Options) *cobra.Command {
	setupOpts := defaultSetupOptions()
	cmd := &cobra.Command{
		Use:   "setup [path]",
		Short: "Set the brain up in one command: build it now, backfill facts in the background, keep it fresh",
		Long: `setup is the one command that makes a repository's brain useful and keeps it
that way. It runs three phases:

  1. instant   build the deterministic core (semantic index, sessions, seed,
               docs, history). Blocking, seconds, spends NO agent tokens. The
               brain is queryable the moment this returns.
  2. backfill  distill past sessions into durable facts, NEWEST FIRST, detached
               in the background on a cheap model. Spends tokens; bounded by
               --backfill-budget and the persisted distill cache, and skipped
               entirely when no agent CLI is available.
  3. daemon    register the repo into a workspace and install ONE machine-wide
               watcher service (launchd on macOS, a systemd user unit on Linux)
               so freshness never depends on remembering to run anything.

Re-running setup is safe: it detects the existing workspace membership and
daemon instead of duplicating them.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(cmd.Context(), cmd, opts, setupOpts, agentSurfaceTarget(opts, args), setupSteps{})
		},
	}
	cmd.Flags().StringVar(&setupOpts.workspace, "workspace", setupOpts.workspace, "Workspace the repo joins (created when absent)")
	cmd.Flags().BoolVar(&setupOpts.noDaemon, "no-daemon", false, "Run the instant phase and backfill but do not install the background watcher")
	cmd.Flags().BoolVar(&setupOpts.uninstallDaemon, "uninstall-daemon", false, "Stop and remove the background watcher, then exit")
	cmd.Flags().BoolVar(&setupOpts.noBackfill, "no-backfill", false, "Do not start the background fact backfill (no token spend at all)")
	cmd.Flags().StringVar(&setupOpts.agent, "agent", setupOpts.agent, "Agent for the background backfill: auto, none, codex, claude-code")
	cmd.Flags().StringVar(&setupOpts.model, "model", "", "Cheap/fast model for the background backfill (recommended)")
	cmd.Flags().StringVar(&setupOpts.effort, "effort", setupOpts.effort, "Reasoning effort for the background backfill")
	cmd.Flags().IntVar(&setupOpts.backfillBudget, "backfill-budget", 0, "Cap sessions the background backfill distills in one pass (0 = the whole corpus, newest first)")
	cmd.Flags().DurationVar(&setupOpts.interval, "interval", setupOpts.interval, "Daemon poll interval")
	cmd.Flags().DurationVar(&setupOpts.distillEvery, "distill-every", setupOpts.distillEvery, "Minimum interval between the daemon's gated agent runs")
	cmd.Flags().StringVar(&setupOpts.daemonName, "daemon-name", setupOpts.daemonName, "Service identity (launchd label / systemd unit stem); override to avoid clobbering an existing daemon")
	cmd.Flags().BoolVar(&setupOpts.json, "json", false, "Emit the setup report as JSON")
	_ = cmd.Flags().MarkHidden("daemon-name")
	return cmd
}

func runSetup(ctx context.Context, cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, target string, steps setupSteps) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("setup requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	// A repo already set up under a non-default daemon name must keep pointing
	// at THAT daemon, or `setup --uninstall-daemon` would look for one that was
	// never installed and leave the real one running.
	setupOpts = applySetupRecordDefaults(setupOpts, filepath.Dir(storage.HeadPath))
	steps = resolveSetupSteps(cmd, perRepo, setupOpts, repoDir, steps)

	// --json must keep stdout parseable, so the friendly per-step lines are
	// dropped rather than interleaved with the report.
	progressOut := cmd.OutOrStdout()
	if setupOpts.json {
		progressOut = io.Discard
	}
	progress := newProgress(progressOut, "setup")

	report := setupReport{
		SchemaVersion: 1,
		GeneratedAt:   steps.now().UTC(),
		Repo:          repoDir,
		RepoKey:       storage.Key,
		BrainPath:     storage.BrainDir,
	}

	plan, planErr := brainWatchDaemonPlan(perRepo, setupOpts, storage)
	if planErr != nil {
		report.Warnings = append(report.Warnings, "daemon plan unavailable: "+planErr.Error())
	}

	// --uninstall-daemon is a standalone maintenance verb: it must not build,
	// backfill, or register anything.
	if setupOpts.uninstallDaemon {
		return runSetupUninstall(ctx, cmd, setupOpts, steps, plan, planErr, report)
	}

	// Phase 1 — INSTANT. Everything after this point is optional; the brain is
	// usable as soon as this returns.
	instantTask := progress.Begin("instant core (deterministic, no agent tokens)")
	instantStarted := time.Now()
	if err := steps.instant(ctx); err != nil {
		instantTask.Finish(err)
		report.Instant = setupPhase{State: "failed", Detail: err.Error(), Seconds: time.Since(instantStarted).Seconds()}
		return fmt.Errorf("instant phase failed: %w", err)
	}
	report.Instant = setupPhase{State: "ok", Seconds: time.Since(instantStarted).Seconds()}
	instantTask.Update(fmt.Sprintf("instant core ready in %s — the brain is queryable now", roundedSeconds(report.Instant.Seconds)))
	instantTask.Finish(nil)

	report.Facts = factsBackfillStatusForBrain(storage.BrainDir)

	// Phase 2 — BACKFILL (detached, spends tokens).
	report.Backfill = runSetupBackfill(ctx, progress, perRepo, setupOpts, steps, storage, repoDir, &report)

	// Phase 3 — DAEMON (workspace registration + one machine-wide watcher).
	report.Workspace, err = registerRepoInWorkspace(ctx, cmd, perRepo, setupOpts.workspace, repoDir, storage.Key)
	if err != nil {
		report.Warnings = append(report.Warnings, "workspace registration failed: "+err.Error())
		progress.Skip("workspace registration: " + err.Error())
	} else if report.Workspace.Already {
		progress.Skip(fmt.Sprintf("workspace %q already has this repo", report.Workspace.Name))
	} else {
		task := progress.Begin("workspace registration")
		task.Update(fmt.Sprintf("workspace %q now tracks %s", report.Workspace.Name, report.Workspace.RepoKey))
		task.Finish(nil)
	}

	switch {
	case setupOpts.noDaemon:
		progress.Skip("background watcher (--no-daemon)")
		report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "skipped: --no-daemon"}
	case planErr != nil:
		progress.Skip("background watcher: " + planErr.Error())
		report.Daemon = daemonState{Manager: daemonManagerUnsupported, Detail: planErr.Error()}
	default:
		report.Daemon = runSetupDaemon(ctx, progress, steps, plan, &report)
	}

	report.Hook = inspectSessionEndHook(repoDir)
	reportSetupHook(progress, report.Hook)

	// Remember the identities this run used so `status` inspects the daemon
	// that exists rather than the default one it would have created.
	if err := writeSetupRecord(filepath.Dir(storage.HeadPath), setupRecord{
		SchemaVersion: setupBackfillStateVersion,
		UpdatedAt:     steps.now().UTC(),
		Workspace:     setupOpts.workspace,
		DaemonName:    setupOpts.daemonName,
	}); err != nil {
		report.Warnings = append(report.Warnings, "setup record not saved: "+err.Error())
	}

	if setupOpts.json {
		return writeJSON(cmd, report)
	}
	printSetupSummary(cmd.OutOrStdout(), report)
	return nil
}

func runSetupUninstall(ctx context.Context, cmd *cobra.Command, setupOpts setupCommandOptions, steps setupSteps, plan daemonPlan, planErr error, report setupReport) error {
	out := cmd.OutOrStdout()
	if planErr != nil {
		return planErr
	}
	before := steps.inspect(ctx, plan)
	if err := steps.uninstall(ctx, plan); err != nil {
		return err
	}
	report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "uninstalled"}
	if setupOpts.json {
		return writeJSON(cmd, report)
	}
	if !before.Installed {
		fmt.Fprintf(out, "setup: no watcher was installed (%s)\n", plan.Label)
		return nil
	}
	fmt.Fprintf(out, "setup: removed the watcher %s (%s)\n", plan.Label, plan.UnitPath)
	return nil
}

func runSetupDaemon(ctx context.Context, progress *refreshProgress, steps setupSteps, plan daemonPlan, report *setupReport) daemonState {
	if !plan.supported() {
		progress.Skip(fmt.Sprintf("background watcher: unsupported on %s (run `entire-brain workspace watch %s` yourself)", plan.OS, report.Workspace.Name))
		return daemonState{Manager: daemonManagerUnsupported, Detail: "unsupported OS: " + plan.OS}
	}
	before := steps.inspect(ctx, plan)
	// Idempotence: an installed, byte-identical, running unit is left strictly
	// alone. Reloading it would kill an in-flight refresh for no reason.
	if before.Installed && before.Current && before.Running {
		progress.Skip(fmt.Sprintf("background watcher already running (%s)", plan.Label))
		return before
	}
	task := progress.Begin("background watcher")
	if err := steps.install(ctx, plan); err != nil {
		task.Finish(err)
		report.Warnings = append(report.Warnings, "daemon install failed: "+err.Error())
		state := before
		state.Detail = err.Error()
		return state
	}
	after := steps.inspect(ctx, plan)
	verb := "installed"
	if before.Installed {
		verb = "updated"
	}
	task.Update(fmt.Sprintf("background watcher %s (%s)", verb, plan.Label))
	task.Finish(nil)
	return after
}

// runSetupBackfill starts the detached distill pass. Every skip reason is one
// clear line and never an error: a machine with no agent CLI, or a repo with no
// captured sessions yet, is a completely valid setup — it just has no facts to
// extract.
func runSetupBackfill(ctx context.Context, progress *refreshProgress, opts Options, setupOpts setupCommandOptions, steps setupSteps, storage repoStorage, repoDir string, report *setupReport) setupPhase {
	if setupOpts.noBackfill {
		progress.Skip("fact backfill (--no-backfill)")
		return setupPhase{State: "skipped", Detail: "--no-backfill"}
	}
	if report.Facts.Sessions == 0 {
		progress.Skip("fact backfill: no captured sessions yet — it will start once sessions land")
		return setupPhase{State: "skipped", Detail: "no captured sessions"}
	}
	if report.Facts.Pending() == 0 {
		progress.Skip(fmt.Sprintf("fact backfill: already complete (%d/%d sessions distilled)", report.Facts.Distilled, report.Facts.Sessions))
		return setupPhase{State: "skipped", Detail: "all sessions already distilled"}
	}
	agent := strings.TrimSpace(setupOpts.agent)
	if agent == "" || agent == "auto" {
		agent = steps.detectAgent(ctx)
	}
	if agent == "none" {
		progress.Skip("fact backfill: no agent CLI on PATH (install codex or claude, then re-run `entire-brain setup`)")
		return setupPhase{State: "skipped", Detail: "no agent CLI available"}
	}
	stateDir := filepath.Dir(storage.HeadPath)
	plan := setupBackfillPlanFor(opts, setupOpts, agent, repoDir, stateDir)
	if plan.Binary == "" {
		progress.Skip("fact backfill: could not resolve this executable's path")
		return setupPhase{State: "skipped", Detail: "executable path unavailable"}
	}
	pid, err := steps.spawnBackfill(ctx, plan)
	if err != nil {
		progress.Skip("fact backfill: " + err.Error())
		report.Warnings = append(report.Warnings, "backfill did not start: "+err.Error())
		return setupPhase{State: "failed", Detail: err.Error()}
	}
	state := setupBackfillState{
		SchemaVersion: setupBackfillStateVersion,
		StartedAt:     steps.now().UTC(),
		PID:           pid,
		Agent:         agent,
		Model:         strings.TrimSpace(setupOpts.model),
		Effort:        strings.TrimSpace(setupOpts.effort),
		MaxSessions:   setupOpts.backfillBudget,
		LogPath:       plan.LogPath,
		Sessions:      report.Facts.Pending(),
	}
	if err := writeSetupBackfillState(stateDir, state); err != nil {
		report.Warnings = append(report.Warnings, "backfill state not recorded: "+err.Error())
	}
	task := progress.Begin("fact backfill")
	task.Update(fmt.Sprintf("fact backfill started in the background: %d session(s), newest first, agent %s (pid %d)",
		state.Sessions, agent, pid))
	task.Finish(nil)
	return setupPhase{State: "ok", Detail: fmt.Sprintf("pid %d, %d session(s) queued", pid, state.Sessions)}
}

// setupBackfillPlanFor builds the detached distill argv. --newest-first is the
// whole point of the background pass: the run is O(sessions) and the user only
// ever sees its early output soon, so the most recent sessions must be first.
func setupBackfillPlanFor(opts Options, setupOpts setupCommandOptions, agent, repoDir, stateDir string) setupBackfillPlan {
	binary, err := os.Executable()
	if err != nil {
		return setupBackfillPlan{}
	}
	args := []string{"distill", repoDir, "--newest-first", "--agent", agent}
	if model := strings.TrimSpace(setupOpts.model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(setupOpts.effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	if setupOpts.backfillBudget > 0 {
		args = append(args, "--max-sessions", fmt.Sprint(setupOpts.backfillBudget))
	}
	return setupBackfillPlan{
		Binary:  binary,
		Args:    args,
		Dir:     repoDir,
		LogPath: filepath.Join(stateDir, setupBackfillLogFile),
		Env:     setupChildEnv(opts.Env, repoDir),
	}
}

// setupChildEnv carries the plugin dirs into the detached child. Without them a
// background process started from a test or a non-default install would resolve
// a DIFFERENT brain store than the CLI that spawned it.
func setupChildEnv(env EntireEnv, repoDir string) []string {
	out := os.Environ()
	for key, value := range setupPluginEnv(env, repoDir) {
		out = append(out, key+"="+value)
	}
	return out
}

func setupPluginEnv(env EntireEnv, repoDir string) map[string]string {
	values := map[string]string{envRepoRoot: repoDir}
	for key, value := range map[string]string{
		envPluginConfigDir: env.PluginConfigDir,
		envPluginDataDir:   env.PluginDataDir,
		envPluginStateDir:  env.PluginStateDir,
		envPluginCacheDir:  env.PluginCacheDir,
	} {
		if strings.TrimSpace(value) != "" {
			values[key] = value
		}
	}
	return values
}

// brainWatchDaemonPlan describes the ONE machine-wide watcher. It watches the
// workspace, not this repo, so a second `setup` in a second repo adds a member
// rather than a second daemon.
func brainWatchDaemonPlan(opts Options, setupOpts setupCommandOptions, storage repoStorage) (daemonPlan, error) {
	binary, err := os.Executable()
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve home directory: %w", err)
	}
	spec := daemonSpec{
		Name:       setupOpts.daemonName,
		Binary:     binary,
		Args:       brainWatchDaemonArgs(setupOpts),
		WorkingDir: filepath.Dir(binary),
		LogPath:    filepath.Join(filepath.Dir(storage.HeadPath), "watch.log"),
		Env:        daemonEnv(opts.Env),
	}
	return planBrainWatchDaemon(runtime.GOOS, home, os.Getenv(xdgConfigHome), spec)
}

// brainWatchDaemonArgs are the token-frugal watcher flags: the deterministic
// refresh runs free on every tick; distill is enabled but gated by
// --distill-every and hard-capped by --budget, on the cheap model/effort.
func brainWatchDaemonArgs(setupOpts setupCommandOptions) []string {
	args := []string{
		"workspace", "watch", setupOpts.workspace,
		"--interval", setupOpts.interval.String(),
		"--distill",
		"--distill-every", setupOpts.distillEvery.String(),
		"--budget", fmt.Sprint(setupDaemonBudget),
	}
	if model := strings.TrimSpace(setupOpts.model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.TrimSpace(setupOpts.effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	return args
}

func daemonEnv(env EntireEnv) map[string]string {
	values := map[string]string{}
	for key, value := range map[string]string{
		envPluginConfigDir: env.PluginConfigDir,
		envPluginDataDir:   env.PluginDataDir,
		envPluginStateDir:  env.PluginStateDir,
		envPluginCacheDir:  env.PluginCacheDir,
	} {
		if strings.TrimSpace(value) != "" {
			values[key] = value
		}
	}
	// launchd/systemd jobs get a minimal PATH; the deterministic refresh shells
	// out to `git` and `entire`, so an inherited PATH is not optional.
	if path := strings.TrimSpace(os.Getenv("PATH")); path != "" {
		values["PATH"] = path
	}
	return values
}

// registerRepoInWorkspace makes the repo a member of the workspace the daemon
// watches, creating the workspace on first use. Idempotent: an existing member
// is reported as already-registered and the manifest is not rewritten.
func registerRepoInWorkspace(ctx context.Context, cmd *cobra.Command, opts Options, name, repoDir, repoKey string) (setupWorkspaceState, error) {
	state := setupWorkspaceState{Name: name, RepoKey: repoKey}
	manifest, err := loadWorkspaceManifest(opts.Env, name)
	if err != nil {
		if !os.IsNotExist(err) {
			return state, err
		}
		manifest = workspaceManifest{SchemaVersion: workspaceSchemaVersion, Name: name}
		if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
			return state, err
		}
		state.Created = true
	}
	for _, repo := range manifest.Repos {
		if repo.RepoKey == repoKey {
			state.Already = true
			state.Registered = true
			return state, nil
		}
	}
	quiet := &cobra.Command{}
	quiet.SetContext(ctx)
	quiet.SetOut(io.Discard)
	quiet.SetErr(io.Discard)
	if err := runWorkspaceAdd(ctx, quiet, opts, workspaceAddOptions{}, name, repoDir); err != nil {
		return state, err
	}
	state.Registered = true
	return state, nil
}

// resolveSetupSteps fills any injection point the caller left nil with the real
// implementation.
func resolveSetupSteps(cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, repoDir string, steps setupSteps) setupSteps {
	if steps.now == nil {
		steps.now = opts.Now
		if steps.now == nil {
			steps.now = time.Now
		}
	}
	if steps.instant == nil {
		steps.instant = func(ctx context.Context) error {
			// Exactly the free path `watch` runs: sessions + semantic + seed +
			// docs + history reconciliation with seed agent "none".
			sub := &cobra.Command{}
			sub.SetContext(ctx)
			sub.SetOut(cmd.OutOrStdout())
			sub.SetErr(cmd.ErrOrStderr())
			if setupOpts.json {
				sub.SetOut(io.Discard)
				sub.SetErr(io.Discard)
			}
			return watchDeterministicRefresh(ctx, sub, opts, repoDir)
		}
	}
	if steps.detectAgent == nil {
		steps.detectAgent = func(ctx context.Context) string {
			return defaultRefreshAgent(ctx, opts.Runner, repoDir)
		}
	}
	if steps.spawnBackfill == nil {
		steps.spawnBackfill = func(ctx context.Context, plan setupBackfillPlan) (int, error) {
			return spawnDetached(plan)
		}
	}
	if steps.inspect == nil {
		steps.inspect = func(ctx context.Context, plan daemonPlan) daemonState {
			return inspectDaemon(ctx, opts.Runner, plan)
		}
	}
	if steps.install == nil {
		steps.install = func(ctx context.Context, plan daemonPlan) error {
			return installDaemon(ctx, opts.Runner, plan)
		}
	}
	if steps.uninstall == nil {
		steps.uninstall = func(ctx context.Context, plan daemonPlan) error {
			return uninstallDaemon(ctx, opts.Runner, plan)
		}
	}
	return steps
}

// setupRecord is what a previous `setup` chose for this repo. It exists so
// later reads (status) inspect the real daemon identity instead of assuming the
// defaults, which would report a custom-named watcher as "not installed".
type setupRecord struct {
	SchemaVersion int       `json:"schema_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	Workspace     string    `json:"workspace,omitempty"`
	DaemonName    string    `json:"daemon_name,omitempty"`
}

// applySetupRecordDefaults carries a previous run's identities forward for any
// option the caller left at its default, so a repo set up with a custom
// workspace or daemon name keeps addressing the same ones.
func applySetupRecordDefaults(setupOpts setupCommandOptions, stateDir string) setupCommandOptions {
	recorded := setupOptionsFromRecord(stateDir)
	if setupOpts.daemonName == daemonDefaultName {
		setupOpts.daemonName = recorded.daemonName
	}
	if setupOpts.workspace == setupDefaultWorkspace {
		setupOpts.workspace = recorded.workspace
	}
	return setupOpts
}

func writeSetupRecord(stateDir string, record setupRecord) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(stateDir, setupRecordFile), record)
}

// setupOptionsFromRecord returns the setup options a previous run used, falling
// back to the defaults when this repo has never been set up.
func setupOptionsFromRecord(stateDir string) setupCommandOptions {
	opts := defaultSetupOptions()
	data, err := os.ReadFile(filepath.Join(stateDir, setupRecordFile))
	if err != nil {
		return opts
	}
	var record setupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return opts
	}
	if strings.TrimSpace(record.Workspace) != "" {
		opts.workspace = record.Workspace
	}
	if strings.TrimSpace(record.DaemonName) != "" {
		opts.daemonName = record.DaemonName
	}
	return opts
}

func writeSetupBackfillState(stateDir string, state setupBackfillState) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(stateDir, setupBackfillStateFile), state)
}

func roundedSeconds(seconds float64) string {
	return time.Duration(seconds * float64(time.Second)).Round(100 * time.Millisecond).String()
}

func reportSetupHook(progress *refreshProgress, hook setupHookState) {
	if hook.Installed {
		progress.Skip(fmt.Sprintf("session-end hook already wired (%s)", hook.Source))
		return
	}
	progress.Skip("session-end hook not wired — run `entire enable` in this repo so each session distills as it ends")
}

func printSetupSummary(out io.Writer, report setupReport) {
	fmt.Fprintln(out, "\nBrain ready")
	fmt.Fprintf(out, "  repo:  %s (key %s)\n", report.Repo, report.RepoKey)
	fmt.Fprintf(out, "  brain: %s\n", report.BrainPath)
	fmt.Fprintf(out, "  facts: %d/%d sessions distilled\n", report.Facts.Distilled, report.Facts.Sessions)
	switch report.Backfill.State {
	case "ok":
		fmt.Fprintf(out, "  backfill: running in the background (%s)\n", report.Backfill.Detail)
	case "skipped":
		fmt.Fprintf(out, "  backfill: skipped (%s)\n", report.Backfill.Detail)
	case "failed":
		fmt.Fprintf(out, "  backfill: failed (%s)\n", report.Backfill.Detail)
	}
	fmt.Fprintf(out, "  daemon: %s\n", describeDaemonState(report.Daemon))
	fmt.Fprintf(out, "  workspace: %s\n", report.Workspace.Name)
	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", warning)
	}
	fmt.Fprintln(out, "\nNext")
	fmt.Fprintln(out, "  entire-brain overview        what this project is")
	fmt.Fprintln(out, "  entire-brain brief \"<task>\"   task-shaped context")
	fmt.Fprintln(out, "  entire-brain status          backfill progress and daemon health")
}

func describeDaemonState(state daemonState) string {
	switch {
	case state.Detail != "" && !state.Installed:
		return state.Detail
	case state.Running:
		return fmt.Sprintf("running (%s)", state.Label)
	case state.Installed:
		return fmt.Sprintf("installed but not running (%s)", state.Label)
	default:
		return "not installed"
	}
}
