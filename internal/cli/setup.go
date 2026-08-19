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
//     FIRST so the most useful facts land first, on the cheap model/effort,
//     capped at --backfill-budget sessions per pass, with the persisted distill
//     cache so a re-run never re-spends. It is detached, so the prompt comes
//     back immediately.
//  3. DAEMON (installed once, machine-wide): the repo joins a workspace and a
//     single `workspace watch` service keeps every member fresh. Deterministic
//     refresh is free; the agent step stays gated by --distill-every and the
//     per-repo watch cursor.
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
	// setupDefaultBackfillBudget caps how many sessions a bare `entire-brain
	// setup` distills in its FIRST background pass. An uncapped default meant
	// one unadorned command could spend the whole corpus — years of sessions —
	// before anyone saw a cost line. 25 newest sessions is enough to make the
	// facts layer useful immediately; the rest arrive on later runs (or all at
	// once with the explicit --backfill-budget 0).
	setupDefaultBackfillBudget = 25
	// setupDaemonLogFile is machine-level ON PURPOSE. A per-repo path inside a
	// machine-wide unit made the rendered plist repo-dependent, so every new
	// repo's setup saw Contents != Current and unloaded/reloaded the running
	// daemon — restart ping-pong plus false "not current" status.
	setupDaemonLogFile = "watch.log"
	// setupInstantRecordFile remembers the last instant phase's per-component
	// outcome so `status` can say semantic=failed instead of semantic=missing
	// and `doctor` can print WHY without rebuilding anything.
	setupInstantRecordFile = "instant.json"
)

// brainComponent* are the instant phase's component ids. They are the same
// vocabulary `status` prints on its instant line, so a component reported as
// failed by setup is the component the reader then finds as failed there.
const (
	brainComponentSessions       = "sessions"
	brainComponentSeed           = "seed"
	brainComponentDocs           = "docs"
	brainComponentSemantic       = "semantic"
	brainComponentHistory        = "history"
	brainComponentHistoryVectors = "history-vectors"
	brainComponentBranches       = "branches"
	brainComponentFacts          = "facts"
	brainComponentPatterns       = "patterns"
	brainComponentMemory         = "memory"
	brainComponentEntities       = "entities"
)

// setupComponentLabels name each component the way the setup lines say it.
var setupComponentLabels = map[string]string{
	brainComponentSessions:       "session export",
	brainComponentSeed:           "seed baseline",
	brainComponentDocs:           "doc index",
	brainComponentSemantic:       "semantic index",
	brainComponentHistory:        "history index",
	brainComponentHistoryVectors: "history vectors",
	brainComponentBranches:       "branch overlays",
	brainComponentFacts:          "fact reclassification",
	brainComponentPatterns:       "pattern layer",
	brainComponentMemory:         "memory coordinator",
	brainComponentEntities:       "entity index",
}

// setupRepoKeyMismatchHint is the actionable half of the one known
// key-derivation skew: the installed Entire CLI and the brain can derive
// DIFFERENT repo keys for a repo with no git remote, so a snapshot written
// under one key is rejected under the other. The error text alone
// ("semantic snapshot repo_key %q does not match current repo %q") tells the
// reader nothing they can act on, and this is the first thing a brand-new local
// repo hits.
const setupRepoKeyMismatchHint = "this repo has no git remote; add one (git remote add origin ...) or upgrade the entire CLI so both derive the same key"

// setupComponent is one instant-phase component and how it ended.
type setupComponent struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok | failed
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (c setupComponent) failed() bool { return c.State == "failed" }

func setupComponentLabel(name string) string {
	if label, ok := setupComponentLabels[name]; ok {
		return label
	}
	return name
}

// newSetupComponent turns one stage outcome into a reportable component,
// attaching the hint for the failure modes whose error text is not actionable
// on its own.
func newSetupComponent(name string, err error) setupComponent {
	if err == nil {
		return setupComponent{Name: name, State: "ok"}
	}
	component := setupComponent{Name: name, State: "failed", Detail: strings.TrimSpace(err.Error())}
	if setupIsRepoKeyMismatch(component.Detail) {
		component.Hint = setupRepoKeyMismatchHint
		component.Detail = "repo key mismatch: " + component.Detail
	}
	return component
}

func setupIsRepoKeyMismatch(detail string) bool {
	return strings.Contains(detail, "repo_key") && strings.Contains(detail, "does not match current repo")
}

// partitionSetupComponents splits the phase's outcome into what built and what
// failed, in report order.
func partitionSetupComponents(components []setupComponent) (built, failed []setupComponent) {
	for _, component := range components {
		if component.failed() {
			failed = append(failed, component)
			continue
		}
		built = append(built, component)
	}
	return built, failed
}

func setupComponentNames(components []setupComponent) []string {
	names := make([]string, 0, len(components))
	for _, component := range components {
		names = append(names, component.Name)
	}
	return names
}

// setupComponentSummary is the phase line's "what built, what failed" clause.
// A phase that reports nothing (an injected instant step, an older build) gets
// an empty clause rather than a misleading "built nothing".
func setupComponentSummary(built, failed []setupComponent) string {
	parts := make([]string, 0, 2)
	if len(built) > 0 {
		parts = append(parts, "built "+strings.Join(setupComponentNames(built), ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "FAILED "+strings.Join(setupComponentNames(failed), ", "))
	}
	return strings.Join(parts, "; ")
}

// setupComponentFailureLine is the one line a degraded component gets: what
// failed, why, that setup is continuing anyway, and where the detail lives.
func setupComponentFailureLine(component setupComponent) string {
	detail := strings.TrimSpace(component.Detail)
	if detail == "" {
		detail = "no detail reported"
	}
	return fmt.Sprintf("%s failed (%s) — continuing; run 'entire-brain doctor' for detail",
		setupComponentLabel(component.Name), detail)
}

// reportSetupComponentFailures prints one line per degraded component (plus its
// hint) and returns. It must never abort: losing the workspace, the daemon and
// the status output because one source could not be built is the failure this
// whole path exists to prevent.
func reportSetupComponentFailures(out io.Writer, failed []setupComponent) {
	for _, component := range failed {
		fmt.Fprintf(out, "setup: %s\n", setupComponentFailureLine(component))
		if hint := strings.TrimSpace(component.Hint); hint != "" {
			fmt.Fprintf(out, "setup: hint: %s\n", hint)
		}
	}
}

// setupInstantFailureReasons collects every reason the phase produced nothing
// usable, so the non-zero exit says WHAT failed rather than only that setup did.
func setupInstantFailureReasons(fatal error, failed []setupComponent) string {
	reasons := make([]string, 0, len(failed)+1)
	if fatal != nil {
		reasons = append(reasons, strings.TrimSpace(fatal.Error()))
	}
	for _, component := range failed {
		reason := setupComponentLabel(component.Name) + ": " + strings.TrimSpace(component.Detail)
		if hint := strings.TrimSpace(component.Hint); hint != "" {
			reason += " (" + hint + ")"
		}
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return "no component reported an outcome"
	}
	return strings.Join(reasons, "; ")
}

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
		workspace:      setupDefaultWorkspace,
		daemonName:     daemonDefaultName,
		agent:          "auto",
		effort:         setupBackfillDefaultEffort,
		backfillBudget: setupDefaultBackfillBudget,
		interval:       watch.interval,
		distillEvery:   watch.distillEvery,
	}
}

// setupSteps are the side effects of one setup run, injected so the
// orchestration (phase order, idempotence, skip reasons, reporting) is testable
// without running a real refresh, spawning a real process, or touching a real
// service manager.
type setupSteps struct {
	now func() time.Time
	// instant returns one entry per component it attempted. The error is
	// reserved for a phase that could not run AT ALL (no brain directory, no
	// resolvable output path); a component that failed on its own comes back as
	// a failed component with the rest still built.
	instant       func(context.Context) ([]setupComponent, error)
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
	State   string  `json:"state"` // ok | degraded | skipped | failed
	Detail  string  `json:"detail,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	// Components is the instant phase's per-component outcome. "degraded" means
	// the brain is queryable but at least one of these failed.
	Components []setupComponent `json:"components,omitempty"`
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

TOKEN SPEND: phases 2 and 3 call an agent. This is the first release in which
the watcher's --distill step and the session-end hook's distill actually run
(both were dead before), so setup is a real, recurring cost, bounded by four
gates: --backfill-budget sessions per background pass, one gated agent run per
--distill-every per repo, the persisted distill cache (a session is never
distilled twice), and the cheap --model/--effort. Passing --no-backfill with
--no-daemon spends nothing at all, and "entire-brain status" reports what the
backfill has done so far and whether the watcher is alive.

EXIT CODE: setup exits 0 whenever the brain is queryable — that is, whenever at
least one instant-phase component built. A component that fails (a semantic
index whose snapshot carries a different repo key, say) is reported as one line,
skipped, and the remaining components, the backfill, the workspace registration
and the daemon install all still run; "entire-brain status" then shows that
component as failed and "entire-brain doctor" prints the reason. setup exits
non-zero only when nothing usable exists: the brain directory cannot be built,
or every component failed.

Re-running setup is safe: it detects the existing workspace membership and
daemon instead of duplicating them, resumes rather than restarts a backfill
that is still running, and keeps whatever --interval/--distill-every/--model/
--effort the previous run was given unless you pass the flag again.`,
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
	cmd.Flags().IntVar(&setupOpts.backfillBudget, "backfill-budget", setupOpts.backfillBudget, "Cap sessions the background backfill distills in one pass, newest first (0 = explicitly unlimited: the WHOLE corpus in one spend)")
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
	stateDir := filepath.Dir(storage.HeadPath)
	// A repo already set up under a non-default daemon name must keep pointing
	// at THAT daemon, or `setup --uninstall-daemon` would look for one that was
	// never installed and leave the real one running. Same for the tuning flags:
	// a re-run that does not name them must not silently revert them.
	previous, setUpBefore := setupOptionsFromRecord(stateDir)
	setupOpts = applySetupRecordDefaults(setupOpts, previous, setupFlagChanged(cmd))
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

	plan, planErr := brainWatchDaemonPlan(perRepo, setupOpts)
	if planErr != nil {
		report.Warnings = append(report.Warnings, "daemon plan unavailable: "+planErr.Error())
	}

	// --uninstall-daemon is a standalone maintenance verb: it must not build,
	// backfill, or register anything.
	if setupOpts.uninstallDaemon {
		return runSetupUninstall(ctx, cmd, setupOpts, steps, plan, planErr, report)
	}

	// Renaming the daemon must MOVE it, not fork it: without this the old
	// launchd job / systemd unit keeps running under its old label forever,
	// invisible to every later `status` and `--uninstall-daemon`.
	if detail := retireRenamedDaemon(ctx, perRepo, setupOpts, previous, setUpBefore, steps, planErr); detail != "" {
		progress.Skip(detail)
	}

	// Phase 1 — INSTANT. Per-component BEST-EFFORT: a component that fails is
	// reported as one line and the phase carries on, because first-run
	// onboarding must not die on one broken source. Losing the workspace, the
	// daemon and the status output because a stale semantic snapshot carried
	// another CLI's repo key is a worse outcome, by far, than a brain with four
	// sources instead of five.
	instantTask := progress.Begin("instant core (deterministic, no agent tokens)")
	instantStarted := time.Now()
	components, instantErr := steps.instant(ctx)
	seconds := time.Since(instantStarted).Seconds()
	built, failed := partitionSetupComponents(components)
	// Persist the outcome BEFORE deciding whether to continue: `status` and
	// `doctor` must be able to name the failed component either way, and the
	// hard-fail path is exactly where the reason is needed most.
	if err := writeSetupInstantRecord(stateDir, steps.now().UTC(), components); err != nil {
		report.Warnings = append(report.Warnings, "instant component record not saved: "+err.Error())
	}
	report.Instant = setupPhase{Seconds: seconds, Components: components}
	// Hard-fail ONLY when nothing usable came out: the phase could not run at
	// all (unwritable brain directory, unresolvable repo) or every component it
	// attempted failed. Anything less is a degraded brain, which is still a
	// brain, and setup keeps going. A phase that reports no components at all
	// and no error built fine — that is the pre-component contract, and reading
	// it as "everything failed" would fail every such caller.
	if instantErr != nil || (len(failed) > 0 && len(built) == 0) {
		reasons := setupInstantFailureReasons(instantErr, failed)
		instantTask.Finish(fmt.Errorf("%s", reasons))
		report.Instant.State = "failed"
		report.Instant.Detail = reasons
		if instantErr != nil {
			return fmt.Errorf("instant phase failed, the brain is not usable: %w", instantErr)
		}
		return fmt.Errorf("instant phase failed, every component failed: %s", reasons)
	}
	report.Instant.State = "ok"
	if len(failed) > 0 {
		report.Instant.State = "degraded"
		report.Instant.Detail = setupInstantFailureReasons(nil, failed)
	}
	summary := setupComponentSummary(built, failed)
	if summary != "" {
		summary += "; "
	}
	instantTask.Update(fmt.Sprintf("instant core ready in %s — %sthe brain is queryable now",
		roundedSeconds(seconds), summary))
	instantTask.Finish(nil)
	reportSetupComponentFailures(progressOut, failed)

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

	// Remember the identities AND the tuning this run used, so `status` inspects
	// the daemon that exists rather than the default one it would have created,
	// and so a later bare `setup` re-installs the same daemon rather than one
	// reverted to stock intervals and models.
	if err := writeSetupRecord(stateDir, setupRecord{
		SchemaVersion: setupBackfillStateVersion,
		UpdatedAt:     steps.now().UTC(),
		Workspace:     setupOpts.workspace,
		DaemonName:    setupOpts.daemonName,
		Interval:      setupOpts.interval.String(),
		DistillEvery:  setupOpts.distillEvery.String(),
		Model:         strings.TrimSpace(setupOpts.model),
		Effort:        strings.TrimSpace(setupOpts.effort),
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
	// Re-entrancy: `setup` is the command people re-run when they are impatient,
	// and the backfill is the phase that takes hours. Without this check a second
	// setup spawns a SECOND detached distill over the same pending sessions —
	// double token spend on the very corpus the first pass is working through.
	stateDir := filepath.Dir(storage.HeadPath)
	if previous, ok := readSetupBackfillState(stateDir); ok && processAlive(previous.PID) {
		detail := fmt.Sprintf("already running (pid %d, started %s, %d/%d sessions distilled so far)",
			previous.PID, previous.StartedAt.Format(time.RFC3339), report.Facts.Distilled, report.Facts.Sessions)
		progress.Skip("fact backfill: " + detail)
		if previous.LogPath != "" {
			progress.Skip("fact backfill log: " + previous.LogPath)
		}
		return setupPhase{State: "skipped", Detail: detail}
	}
	agent := strings.TrimSpace(setupOpts.agent)
	if agent == "" || agent == "auto" {
		agent = steps.detectAgent(ctx)
	}
	if agent == "none" {
		progress.Skip("fact backfill: no agent CLI on PATH (install codex or claude, then re-run `entire-brain setup`)")
		return setupPhase{State: "skipped", Detail: "no agent CLI available"}
	}
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
	queued := state.Sessions
	if setupOpts.backfillBudget > 0 && queued > setupOpts.backfillBudget {
		queued = setupOpts.backfillBudget
	}
	task.Update(fmt.Sprintf("fact backfill started in the background: %d session(s), newest first, agent %s (pid %d)",
		queued, agent, pid))
	task.Finish(nil)
	// Say what the cap did and how to lift it. A silent cap is as surprising as
	// a silent uncapped spend: the user is left believing the whole corpus was
	// distilled when only the newest slice was.
	if setupOpts.backfillBudget > 0 && state.Sessions > setupOpts.backfillBudget {
		progress.Skip(fmt.Sprintf(
			"fact backfill capped at %d of %d pending session(s) this pass (--backfill-budget); re-run `entire-brain setup` for the next batch, or `--backfill-budget 0` to distill the whole corpus in one spend",
			setupOpts.backfillBudget, state.Sessions))
	}
	return setupPhase{State: "ok", Detail: fmt.Sprintf("pid %d, %d of %d pending session(s) queued", pid, queued, state.Sessions)}
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
//
// Nothing in the rendered unit may depend on which repo ran setup. The log path
// used to: it pointed inside the calling repo's state directory, so setting up a
// second repo produced different bytes for the same machine-wide job, which
// inspectDaemon correctly read as drift and "repaired" by unloading and
// reloading the running daemon — every single time.
func brainWatchDaemonPlan(opts Options, setupOpts setupCommandOptions) (daemonPlan, error) {
	binary, err := os.Executable()
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve home directory: %w", err)
	}
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		return daemonPlan{}, fmt.Errorf("resolve plugin state dir: %w", err)
	}
	spec := daemonSpec{
		Name:       setupOpts.daemonName,
		Binary:     binary,
		Args:       brainWatchDaemonArgs(setupOpts),
		WorkingDir: filepath.Dir(binary),
		LogPath:    filepath.Join(dirs.State, "logs", setupDaemonLogFile),
		Env:        daemonEnv(opts.Env),
	}
	return planBrainWatchDaemon(runtime.GOOS, home, os.Getenv(xdgConfigHome), os.Getenv(envDaemonUnitDir), spec)
}

// brainWatchDaemonArgs are the token-frugal watcher flags: the deterministic
// refresh runs free on every tick; distill is enabled but gated by
// --distill-every and the per-repo watch cursor, on the cheap model/effort.
//
// --budget is deliberately NOT passed. It counts gated agent runs for the life
// of the PROCESS and never resets, and this daemon is a KeepAlive service that
// is meant to run for months — so `--budget 1` did not mean "one run per window",
// it meant one run EVER, machine-wide, across every repo in the workspace. The
// durable, self-resetting guard is the pair watch.go documents: --distill-every
// plus each repo's persisted cursor, which survives restarts and bounds spend
// per repo per window without ever latching off.
func brainWatchDaemonArgs(setupOpts setupCommandOptions) []string {
	args := []string{
		"workspace", "watch", setupOpts.workspace,
		"--interval", setupOpts.interval.String(),
		"--distill",
		"--distill-every", setupOpts.distillEvery.String(),
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
// The whole read-modify-write runs under the workspace manifest lock, because
// two `setup` runs in two repos at the same time is the ordinary case on a
// developer machine, and an unlocked last-writer-wins rewrite silently drops one
// of the two registrations.
func registerRepoInWorkspace(_ context.Context, _ *cobra.Command, opts Options, name, repoDir, repoKey string) (setupWorkspaceState, error) {
	state := setupWorkspaceState{Name: name, RepoKey: repoKey}
	result, err := addWorkspaceRepoLocked(opts.Env, name, workspaceRepo{RepoKey: repoKey, LocalPathHint: repoDir})
	if err != nil {
		return state, err
	}
	state.Created = result.Created
	state.Already = result.Already
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
		steps.instant = func(ctx context.Context) ([]setupComponent, error) {
			// Exactly the free path `watch` runs: sessions + semantic + seed +
			// docs + history reconciliation with seed agent "none" — but in
			// best-effort mode, so each component reports its own outcome and one
			// failure does not abort the build.
			sub := &cobra.Command{}
			sub.SetContext(ctx)
			sub.SetOut(cmd.OutOrStdout())
			sub.SetErr(cmd.ErrOrStderr())
			if setupOpts.json {
				sub.SetOut(io.Discard)
				sub.SetErr(io.Discard)
			}
			var components []setupComponent
			err := watchDeterministicRefreshComponents(ctx, sub, opts, repoDir, func(name string, err error) {
				components = append(components, newSetupComponent(name, err))
			})
			return components, err
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
	// The daemon's tuning is part of its identity too: a re-run that does not
	// mention these must re-render the SAME unit, not one reverted to stock.
	Interval     string `json:"interval,omitempty"`
	DistillEvery string `json:"distill_every,omitempty"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
}

// setupFlagNames maps each remembered option to its flag, so "was this passed?"
// is asked of cobra rather than inferred.
const (
	setupFlagWorkspace    = "workspace"
	setupFlagDaemonName   = "daemon-name"
	setupFlagInterval     = "interval"
	setupFlagDistillEvery = "distill-every"
	setupFlagModel        = "model"
	setupFlagEffort       = "effort"
)

// setupFlagChanged reports which flags the user actually typed. Comparing a
// value against its default cannot answer that: `--daemon-name entire-brain-watch`
// and `--interval 5m` are indistinguishable from not passing them, so a repo set
// up with a custom daemon could never be moved back to the default one, and
// explicitly re-affirming a default silently restored the recorded value
// instead. cmd may be nil in tests that drive runSetup directly; then nothing
// was typed.
func setupFlagChanged(cmd *cobra.Command) func(string) bool {
	if cmd == nil {
		return func(string) bool { return false }
	}
	flags := cmd.Flags()
	return func(name string) bool {
		if flags.Lookup(name) == nil {
			return false
		}
		return flags.Changed(name)
	}
}

// applySetupRecordDefaults carries a previous run's identities and tuning
// forward for every option the caller did NOT pass, so a repo set up with a
// custom workspace, daemon name, interval, model or effort keeps them on a bare
// re-run instead of silently reverting to stock and rewriting the unit.
func applySetupRecordDefaults(setupOpts setupCommandOptions, recorded setupCommandOptions, changed func(string) bool) setupCommandOptions {
	if changed == nil {
		changed = func(string) bool { return false }
	}
	if !changed(setupFlagDaemonName) {
		setupOpts.daemonName = recorded.daemonName
	}
	if !changed(setupFlagWorkspace) {
		setupOpts.workspace = recorded.workspace
	}
	if !changed(setupFlagInterval) && recorded.interval > 0 {
		setupOpts.interval = recorded.interval
	}
	if !changed(setupFlagDistillEvery) && recorded.distillEvery > 0 {
		setupOpts.distillEvery = recorded.distillEvery
	}
	if !changed(setupFlagModel) && strings.TrimSpace(recorded.model) != "" {
		setupOpts.model = recorded.model
	}
	if !changed(setupFlagEffort) && strings.TrimSpace(recorded.effort) != "" {
		setupOpts.effort = recorded.effort
	}
	return setupOpts
}

// retireRenamedDaemon unloads the daemon a previous run installed when THIS run
// names a different one. Writing the new unit without removing the old one
// leaves two watchers running: the old label is not in any later plan, so
// `status` cannot see it and `--uninstall-daemon` cannot remove it. It would
// keep ticking (and spending) until the machine was rebuilt.
func retireRenamedDaemon(ctx context.Context, opts Options, setupOpts, previous setupCommandOptions, setUpBefore bool, steps setupSteps, planErr error) string {
	old := strings.TrimSpace(previous.daemonName)
	// Only a repo THIS machine actually set up before can have a daemon to
	// retire. Without that check, a first-ever `setup --daemon-name mine` in one
	// repo would read the absent record as the default name and uninstall the
	// machine-wide default watcher that every other repo depends on.
	if !setUpBefore || planErr != nil || old == "" || old == setupOpts.daemonName || setupOpts.noDaemon {
		return ""
	}
	retired := setupOpts
	retired.daemonName = old
	oldPlan, err := brainWatchDaemonPlan(opts, retired)
	if err != nil {
		return fmt.Sprintf("previous watcher %q could not be planned for removal: %v", old, err)
	}
	if state := steps.inspect(ctx, oldPlan); !state.Installed {
		return ""
	}
	if err := steps.uninstall(ctx, oldPlan); err != nil {
		return fmt.Sprintf("previous watcher %s not removed: %v", oldPlan.Label, err)
	}
	return fmt.Sprintf("previous watcher %s removed (renamed to %s)", oldPlan.Label, setupOpts.daemonName)
}

func writeSetupRecord(stateDir string, record setupRecord) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(stateDir, setupRecordFile), record)
}

// setupOptionsFromRecord returns the setup options a previous run used, falling
// back to the defaults when this repo has never been set up. The bool says
// which of the two it is — "never set up" and "set up with the defaults" are
// indistinguishable in the values but mean opposite things to anything that
// acts on a previous daemon.
func setupOptionsFromRecord(stateDir string) (setupCommandOptions, bool) {
	opts := defaultSetupOptions()
	data, err := os.ReadFile(filepath.Join(stateDir, setupRecordFile))
	if err != nil {
		return opts, false
	}
	var record setupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return opts, false
	}
	if strings.TrimSpace(record.Workspace) != "" {
		opts.workspace = record.Workspace
	}
	if strings.TrimSpace(record.DaemonName) != "" {
		opts.daemonName = record.DaemonName
	}
	if d, err := time.ParseDuration(strings.TrimSpace(record.Interval)); err == nil && d > 0 {
		opts.interval = d
	}
	if d, err := time.ParseDuration(strings.TrimSpace(record.DistillEvery)); err == nil && d > 0 {
		opts.distillEvery = d
	}
	if strings.TrimSpace(record.Model) != "" {
		opts.model = record.Model
	}
	if strings.TrimSpace(record.Effort) != "" {
		opts.effort = record.Effort
	}
	return opts, true
}

// setupInstantRecord is the last instant phase's per-component outcome, kept
// beside the other per-repo setup state. Without it a failed component is
// indistinguishable from one that was never built: `status` would print
// semantic=missing for a semantic index that failed loudly ten seconds ago, and
// the reason would exist nowhere at all once the terminal scrolled.
type setupInstantRecord struct {
	SchemaVersion int              `json:"schema_version"`
	UpdatedAt     time.Time        `json:"updated_at"`
	Components    []setupComponent `json:"components,omitempty"`
}

// writeSetupInstantRecord persists the phase outcome. A phase that reported
// nothing (an injected step, an older build) leaves the previous record alone
// rather than erasing a real failure with an empty one.
func writeSetupInstantRecord(stateDir string, now time.Time, components []setupComponent) error {
	if len(components) == 0 {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(stateDir, setupInstantRecordFile), setupInstantRecord{
		SchemaVersion: setupBackfillStateVersion,
		UpdatedAt:     now,
		Components:    components,
	})
}

func readSetupInstantRecord(stateDir string) setupInstantRecord {
	data, err := os.ReadFile(filepath.Join(stateDir, setupInstantRecordFile))
	if err != nil {
		return setupInstantRecord{}
	}
	var record setupInstantRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return setupInstantRecord{}
	}
	return record
}

// failedSetupComponents is what `doctor` prints: the components the last setup
// could not build, with the reason and the hint.
func failedSetupComponents(stateDir string) []setupComponent {
	_, failed := partitionSetupComponents(readSetupInstantRecord(stateDir).Components)
	return failed
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
	switch {
	case hook.Installed && !hook.EntireCLI:
		progress.Skip(fmt.Sprintf("session-end hook wired (%s) but `%s` is not on PATH — sessions will NOT distill until the Entire CLI is installed", hook.Source, entireBinaryName))
	case hook.Installed:
		progress.Skip(fmt.Sprintf("session-end hook already wired (%s)", hook.Source))
	case !hook.EntireCLI:
		progress.Skip(fmt.Sprintf("session-end hook not wired and `%s` is not on PATH — install the Entire CLI, then run `entire enable` in this repo", entireBinaryName))
	default:
		progress.Skip("session-end hook not wired — run `entire enable` in this repo so each session distills as it ends (SPENDS TOKENS per session)")
	}
}

func printSetupSummary(out io.Writer, report setupReport) {
	fmt.Fprintln(out, "\nBrain ready")
	fmt.Fprintf(out, "  repo:  %s (key %s)\n", report.Repo, report.RepoKey)
	fmt.Fprintf(out, "  brain: %s\n", report.BrainPath)
	if built, failed := partitionSetupComponents(report.Instant.Components); len(report.Instant.Components) > 0 {
		fmt.Fprintf(out, "  instant: %s\n", setupComponentSummary(built, failed))
		for _, component := range failed {
			fmt.Fprintf(out, "    %s: %s\n", setupComponentLabel(component.Name), component.Detail)
			if hint := strings.TrimSpace(component.Hint); hint != "" {
				fmt.Fprintf(out, "      hint: %s\n", hint)
			}
		}
	}
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
