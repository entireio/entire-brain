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
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ashtom/entire-brain/internal/tui"
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

// setupRepoKeyMismatchHint covers what is LEFT of the repo-key mismatch once
// the two tools' naming conventions no longer collide.
//
// It used to say "this repo has no git remote; add one, or upgrade the entire
// CLI" — advice that was wrong in both directions. It fired on repositories
// that DID have a remote (any origin that is not github.com: the provider names
// those `local/<basename>` while the brain names them `gl/…`, `bb/…`), it
// blamed the reader's repository for a disagreement between two of our own
// tools, and no upgrade of either tool would have made the two spellings meet.
// That whole class is now reconciled at ingest (semantic_repokey.go), so a
// mismatch that still reaches here means the snapshot genuinely describes some
// other repository — in practice a provider snapshot cached against an earlier
// remote — and rebuilding it is the thing to do.
func setupRepoKeyMismatchHint(brainCmd string) string {
	return "the snapshot names a different repository than this one, usually a provider snapshot cached before this repo's origin changed; rebuild it with `" +
		setupBrainCommand(brainCmd) + " refresh index --force`"
}

// setupDirtyWorktreeHint is the second thing a first run hits, and it is caused
// by the step the docs put immediately before setup. `entire enable` writes
// .entire/settings.json and the agent's hook settings (.claude/settings.json)
// and does NOT commit them, so the very next `entire-brain setup` finds a dirty
// worktree and refuses to seed or index it. The error text names --worktree,
// which `setup` does not accept — leaving the reader with a flag they cannot
// pass to the command they ran. Both halves of the remedy belong here: what to
// do, and why the tree is dirty when they never edited anything.
func setupDirtyWorktreeHint(brainCmd string) string {
	return "commit or stash the working tree, then re-run `" + setupBrainCommand(brainCmd) +
		" setup`; `entire enable` writes .entire/ and .claude/ without committing them, which is what a first run usually trips over"
}

// setupNoSessionsHint covers the state EVERY repo is in between `entire enable`
// and its first finished agent session: Entire has no checkpoints to hand over,
// and when the checkpoint remote is also unreachable (offline, or not yet
// authenticated) the brain cannot prove the inventory is empty rather than
// merely unreadable, so it reports a failure. "no readable checkpoint IDs from
// an incomplete persistent-store inventory" is true and useless; this says what
// it means and what the reader does about it.
func setupNoSessionsHint(brainCmd string) string {
	return "no captured sessions are readable yet — if no agent session has finished in this repo since `entire enable`, that is expected: work in a session, then re-run `" +
		setupBrainCommand(brainCmd) + " setup`; `entire checkpoint list` shows what Entire itself can see"
}

// setupBuildsHistoryProjection and watchTickBuildsHistoryProjection are the one
// place the two callers of the free deterministic refresh disagree.
//
// A watch TICK leaves the durable history projection to the memory coordinator:
// it runs every few minutes, the per-tick delta already makes new conversations
// recallable, and re-projecting on every tick would bypass the work ledger.
//
// `setup` is not a tick. It runs once, and it ends by telling the user to run
// `entire-brain brief` — which, with no projection, drops its transcript half
// and answers "history index missing; run `entire brain refresh`". Setup's own
// recommended next command reporting a gap in setup's own output is the first
// impression this whole path exists to get right.
const (
	setupBuildsHistoryProjection     = true
	watchTickBuildsHistoryProjection = false
)

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
func newSetupComponent(name string, err error, brainCmd string) setupComponent {
	if err == nil {
		return setupComponent{Name: name, State: "ok"}
	}
	component := setupComponent{Name: name, State: "failed", Detail: strings.TrimSpace(err.Error())}
	switch {
	case setupIsRepoKeyMismatch(component.Detail):
		component.Hint = setupRepoKeyMismatchHint(brainCmd)
		component.Detail = "repo key mismatch: " + component.Detail
	case setupIsDirtyWorktree(component.Detail):
		component.Hint = setupDirtyWorktreeHint(brainCmd)
	case setupIsNoReadableSessions(component.Detail):
		component.Hint = setupNoSessionsHint(brainCmd)
	}
	return component
}

func setupIsRepoKeyMismatch(detail string) bool {
	return strings.Contains(detail, "repo_key") && strings.Contains(detail, "does not match current repo")
}

// setupIsDirtyWorktree matches the machine-readable prefix both refusals carry
// (seed.go and semantic.go), not their prose, so rewording either message
// cannot silently drop the hint.
func setupIsDirtyWorktree(detail string) bool {
	return strings.Contains(detail, dirtyWorktreeErrorCode+":")
}

// setupIsNoReadableSessions matches the export failure raised when nothing is
// enumerable AND the inventory could not be proven complete. The narrower
// "routed discovery failed" variants keep their own detail: those name a real
// fault to fix, while this one is usually just a repo with no sessions yet.
func setupIsNoReadableSessions(detail string) bool {
	return strings.Contains(detail, checkpointScopeIncompleteCode+":") &&
		strings.Contains(detail, "no readable checkpoint IDs")
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
// dash is passed in rather than written inline because an em dash is a
// non-ASCII glyph and this line is printed to a terminal whose locale may not
// be able to draw one.
func setupComponentFailureLine(component setupComponent, dash, brainCmd string) string {
	detail := strings.TrimSpace(component.Detail)
	if detail == "" {
		detail = "no detail reported"
	}
	return fmt.Sprintf("%s failed (%s) %s continuing; run '%s doctor' for detail",
		setupComponentLabel(component.Name), detail, dash, setupBrainCommand(brainCmd))
}

// reportSetupComponentFailures prints one line per degraded component (plus its
// hint) and returns. It must never abort: losing the workspace, the daemon and
// the status output because one source could not be built is the failure this
// whole path exists to prevent.
func reportSetupComponentFailures(progress *refreshProgress, failed []setupComponent, brainCmd string) {
	for _, component := range failed {
		progress.NotePhase(setupComponentFailureLine(component, progress.dash(), brainCmd), tui.PhaseFailed)
		if hint := strings.TrimSpace(component.Hint); hint != "" {
			progress.NotePhase("hint: "+hint, tui.PhaseSkipped)
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
	instant func(context.Context) ([]setupComponent, error)
	// observeComponent is notified as each instant-phase component lands, so the
	// progress line can tick components off live instead of revealing all of
	// them at once when the phase returns.
	observeComponent *setupComponentObserver
	detectAgent      func(context.Context) string
	spawnBackfill    func(context.Context, setupBackfillPlan) (int, error)
	stopBackfill     func(int) error
	// plan resolves the daemon artifact for a set of options. It is a step, not
	// a direct call, for the same reason planBrainWatchDaemon takes goos as an
	// argument: the install/idempotence/retirement logic above it is
	// OS-independent given a supported plan, so injecting the plan lets those
	// properties be tested from ANY host — including a windows runner, where the
	// real plan is deliberately unsupported and would otherwise make every one of
	// those tests vacuous (they failed instead: installs == 0).
	plan      func(setupCommandOptions) (daemonPlan, error)
	inspect   func(context.Context, daemonPlan) daemonState
	install   func(context.Context, daemonPlan) error
	uninstall func(context.Context, daemonPlan) error
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
		Long: fmt.Sprintf(`setup is the one command that makes a repository's brain useful and keeps it
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
               ONE means one: the installed unit is identical for every repo and
               every workspace, and each workspace's interval, distill cadence,
               agent, model, effort and session cap live in a machine-level watch
               plan the running watcher re-reads. Setting up a second repo adds a
               row to that plan; it does not rewrite, restart or steal the
               watcher the first repo is relying on.

TOKEN SPEND: phases 2 and 3 call an agent. This is the first release in which
the watcher's --distill step and the session-end hook's distill actually run
(both were dead before), so setup is a real, recurring cost, bounded by four
gates: --backfill-budget sessions per background pass, one gated agent run per
--distill-every per repo, the persisted distill cache (a session is never
distilled twice), and the cheap --model/--effort. Passing --no-backfill with
--no-daemon spends nothing at all, and "%[1]s status" reports what the
backfill has done so far and whether the watcher is alive.

EXIT CODE: setup exits 0 whenever the brain is queryable — that is, whenever at
least one instant-phase component built. A component that fails (a semantic
index whose snapshot carries a different repo key, say) is reported as one line,
skipped, and the remaining components, the backfill, the workspace registration
and the daemon install all still run; "%[1]s status" then shows that
component as failed and "%[1]s doctor" prints the reason. setup exits
non-zero only when nothing usable exists: the brain directory cannot be built,
or every component failed.

Re-running setup is safe: it detects the existing workspace membership and
daemon instead of duplicating them, resumes rather than restarts a backfill
that is still running, and keeps whatever --interval/--distill-every/--model/
--effort the previous run was given unless you pass the flag again.`, setupCommandPrefix(os.LookupEnv)),
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
	cmd.Flags().StringVar(&setupOpts.daemonName, "daemon-name", setupOpts.daemonName, "RENAME the machine's single watcher (launchd label / systemd unit stem). It moves the watcher, it does not add a second one: the previous unit is retired first")
	cmd.Flags().BoolVar(&setupOpts.json, "json", false, "Emit the setup report as JSON")
	_ = cmd.Flags().MarkHidden("daemon-name")
	return cmd
}

func runSetup(ctx context.Context, cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, target string, steps setupSteps) error {
	// How this run was reached, resolved ONCE so every command this run prints
	// is spelled the way the reader typed it -- the pre-flight lines, every
	// skip and failure line, the hints, and the summary's Next block all move
	// together and cannot drift apart. See setupCommandPrefix.
	brainCmd := setupCommandPrefix(os.LookupEnv)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("setup requires a local repository path: %s", target)
	}
	// Refuse a non-repository UP FRONT. Everything below is a side effect --
	// a workspace registration, a machine watch-plan row, a per-repo state
	// directory, an installed KeepAlive service -- and every deterministic
	// source setup builds reads git. Without this check a plain directory got
	// all of those side effects, four components failing with a raw
	// `fatal: not a git repository`, and exit 0.
	//
	// --uninstall-daemon is exempt: it is machine-level maintenance that must
	// keep working from anywhere, including the directory whose failed setup is
	// the reason someone is uninstalling.
	if !setupOpts.uninstallDaemon {
		if _, ok := gitWorkTreeRoot(ctx, opts.Runner, repoDir); !ok {
			return notARepositoryError(ctx, opts.Runner, repoDir,
				setupBrainCommand(brainCmd)+" setup builds every source it has -- sessions, seed, docs, semantic index, entities -- from git history, so it needs git and a repository.",
				setupBrainCommand(brainCmd)+" setup",
				setupBrainCommand(brainCmd)+" setup <path>")
		}
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
	previous, setUpBefore, recordErr := setupOptionsFromRecord(stateDir)
	if recordErr != nil {
		// Refusing here is the whole point: every path below acts on a PREVIOUS
		// daemon identity, and without the record we would act on the default
		// one — retiring or uninstalling a watcher that belongs to another repo.
		return fmt.Errorf("%w\nthis repo's setup record is unreadable; delete it and re-run `%s setup` to re-adopt the watcher", recordErr, brainCmd)
	}
	changed := setupFlagChanged(cmd)
	setupOpts = applySetupRecordDefaults(setupOpts, previous, changed)
	if steps.observeComponent == nil {
		steps.observeComponent = &setupComponentObserver{}
	}
	steps = resolveSetupSteps(cmd, perRepo, setupOpts, repoDir, brainCmd, steps)

	// --json must keep stdout parseable, so the friendly per-step lines are
	// dropped rather than interleaved with the report.
	progressOut := cmd.OutOrStdout()
	if setupOpts.json {
		progressOut = io.Discard
	}
	progress := newProgress(progressOut, "setup")
	timings := &setupTimings{}

	report := setupReport{
		SchemaVersion: 1,
		GeneratedAt:   steps.now().UTC(),
		Repo:          repoDir,
		RepoKey:       storage.Key,
		BrainPath:     storage.BrainDir,
	}

	// The workspace name is the daemon's ONLY argument, and it is validated deep
	// inside registerRepoInWorkspace — a phase whose failure setup deliberately
	// downgrades to a warning so one bad source never costs the whole run. That
	// combination installed a KeepAlive / Restart=always unit running
	// `workspace watch <name the CLI rejects>`: it exits immediately, the
	// service manager restarts it every 60s forever, and setup exits 0. Reject
	// the name here, before anything is planned or installed, where the only
	// thing it can cost is this command.
	if err := validateWorkspaceName(setupOpts.workspace); err != nil {
		return fmt.Errorf("--workspace %q: %w", setupOpts.workspace, err)
	}

	configuredAgent := setupOpts.agent

	// Resolve the agent ONCE, here, before the daemon plan is built. The plan
	// embeds the argv the watcher runs forever, so an agent resolved later (in
	// the backfill phase) reached the detached child but never the daemon, which
	// then fell back to watch's "codex" default no matter what setup detected,
	// reported, and paid the backfill with. Skipped for --uninstall-daemon,
	// which needs only the label and must not probe for CLIs it will not use.
	if !setupOpts.uninstallDaemon {
		if agent := strings.TrimSpace(setupOpts.agent); agent == "" || agent == "auto" {
			setupOpts.agent = steps.detectAgent(ctx)
		}
	}

	// The machine-level watch plan is the authority on which watcher is
	// installed. The per-repo setup record only knows what THIS repo installed.
	machinePlan, machinePlanErr := loadSetupWatchPlan(perRepo.Env)
	if machinePlanErr != nil {
		report.Warnings = append(report.Warnings, "machine watch plan unreadable: "+machinePlanErr.Error())
	}
	if !changed(setupFlagDaemonName) {
		if installedName, known := installedDaemonName(machinePlan, machinePlanErr, previous, setUpBefore); known {
			setupOpts.daemonName = installedName
		}
	}

	plan, planErr := steps.plan(setupOpts)
	if planErr != nil {
		report.Warnings = append(report.Warnings, "daemon plan unavailable: "+planErr.Error())
	}

	// --uninstall-daemon is a standalone maintenance verb: it must not build,
	// backfill, or register anything.
	if setupOpts.uninstallDaemon {
		return runSetupUninstall(ctx, cmd, opts, setupOpts, steps, plan, planErr, report)
	}

	// Renaming the daemon must MOVE it, not fork it: without this the old
	// launchd job / systemd unit keeps running under its old label forever,
	// invisible to every later `status` and `--uninstall-daemon`.
	previousName, previousKnown := installedDaemonName(machinePlan, machinePlanErr, previous, setUpBefore)
	// A repo that has never completed setup does not own the machine-wide
	// watcher merely because another repo recorded it in the shared plan.
	if !setUpBefore {
		previousKnown = false
	}

	// The one side effect of `setup` that OUTLIVES the command gets announced
	// before any of it happens. Everything else setup does lives under the
	// brain directory and dies with an `rm -rf`; the watcher is a service
	// registered with the machine's service manager that starts again at every
	// login. Reporting that in the middle of the closing summary — after it is
	// already installed and running — tells the reader what was done TO their
	// machine, not what is ABOUT to be. The daemon phase re-inspects for
	// itself rather than trusting this observation: it runs after the instant
	// and backfill phases, and its idempotence decision must be made on the
	// state as it is THEN, not as it was before any of that work.
	//
	// This does not change the default. Whether the watcher should install
	// unasked is a product decision; making it impossible to miss, and naming
	// both the flag that skips it and the command that removes it, is not.
	if !setupOpts.noDaemon && planErr == nil && plan.supported() {
		if line := setupDaemonPreflightLine(plan, steps.inspect(ctx, plan), brainCmd); line != "" {
			progress.NotePhase(line, tui.PhaseDaemon)
		}
	}

	// Phase 1 — INSTANT. Per-component BEST-EFFORT: a component that fails is
	// reported as one line and the phase carries on, because first-run
	// onboarding must not die on one broken source. Losing the workspace, the
	// daemon and the status output because a stale semantic snapshot carried
	// another CLI's repo key is a worse outcome, by far, than a brain with four
	// sources instead of five.
	instantTask := progress.BeginPhase("instant core (deterministic, no agent tokens)", tui.PhaseInstant)
	// Tick each component off as it lands. The marks accumulate on the live
	// line, so a twenty-second phase shows five components already built rather
	// than a spinner that reveals everything at the end.
	var liveMarks []tui.Mark
	var liveMarksMu sync.Mutex
	steps.observeComponent.bind(func(component setupComponent) {
		liveMarksMu.Lock()
		liveMarks = append(liveMarks, setupComponentMark(component.State))
		marks := append([]tui.Mark(nil), liveMarks...)
		liveMarksMu.Unlock()
		instantTask.SetMarks(marks)
		instantTask.Update("instant core: " + setupComponentLabel(component.Name))
	})
	instantStarted := time.Now()
	components, instantErr := steps.instant(ctx)
	elapsed := time.Since(instantStarted)
	timings.record("instant", elapsed)
	steps.observeComponent.bind(nil)
	seconds := elapsed.Seconds()
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
	instantTask.Update(fmt.Sprintf("instant core ready in %s %s %sthe brain is queryable now",
		roundedSeconds(seconds), progress.dash(), summary))
	instantTask.Finish(nil)
	reportSetupComponentFailures(progress, failed, brainCmd)

	report.Facts = factsBackfillStatusForBrain(storage.BrainDir)

	// Phase 2 — BACKFILL (detached, spends tokens).
	backfillStarted := time.Now()
	report.Backfill = runSetupBackfill(ctx, progress.withPhase(tui.PhaseBackfill), perRepo, setupOpts, steps, storage, repoDir, &report, brainCmd)
	timings.record("backfill", time.Since(backfillStarted))

	// Phase 3 — DAEMON (workspace registration + one machine-wide watcher).
	daemonProgress := progress.withPhase(tui.PhaseDaemon)
	workspaceStarted := time.Now()
	report.Workspace, err = registerRepoInWorkspace(ctx, cmd, perRepo, setupOpts.workspace, repoDir, storage.Key)
	if err != nil {
		report.Warnings = append(report.Warnings, "workspace registration failed: "+err.Error())
		daemonProgress.Skip("workspace registration: " + err.Error())
	} else if report.Workspace.Already {
		daemonProgress.Skip(fmt.Sprintf("workspace %q already has this repo", report.Workspace.Name))
	} else {
		task := daemonProgress.BeginPhase("workspace registration", tui.PhaseDaemon)
		task.Update(fmt.Sprintf("workspace %q now tracks %s", report.Workspace.Name, report.Workspace.RepoKey))
		task.Finish(nil)
	}
	timings.record("workspace", time.Since(workspaceStarted))

	// Record this workspace's tuning in the machine-level plan BEFORE the daemon
	// is installed, so the watcher's very first tick reads a plan that already
	// contains this repo's workspace instead of reporting "nothing to watch".
	// A failure here is a warning, not a fatal: everything the deterministic
	// phases built stays usable, and the next `setup` re-attempts it.
	var watchPlanErr error
	if !setupOpts.noDaemon && planErr == nil && report.Workspace.Registered {
		if _, watchPlanErr = recordSetupWatchPlan(perRepo.Env, setupOpts.daemonName, setupWatchPlanEntryFor(setupOpts, steps.now())); watchPlanErr != nil {
			report.Warnings = append(report.Warnings, "machine watch plan not saved: "+watchPlanErr.Error())
			daemonProgress.Skip("machine watch plan not saved: " + watchPlanErr.Error())
		}
	}

	daemonStarted := time.Now()
	switch {
	case setupOpts.noDaemon:
		daemonProgress.Skip("background watcher (--no-daemon)")
		report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "skipped: --no-daemon"}
	case !report.Workspace.Registered:
		daemonProgress.Skip("background watcher: workspace registration failed")
		report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "skipped: workspace registration failed"}
	case watchPlanErr != nil:
		report.Daemon = daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath, Detail: "skipped: machine watch plan not saved"}
	case planErr != nil:
		daemonProgress.Skip("background watcher: " + planErr.Error())
		report.Daemon = daemonState{Manager: daemonManagerUnsupported, Detail: planErr.Error()}
	default:
		report.Daemon = runSetupDaemon(ctx, daemonProgress, opts.Env, steps, plan, &report, brainCmd)
		// Switch watcher names only after the replacement is confirmed on disk.
		// This preserves the old service if the instant phase or new install
		// fails, while still preventing two names from running indefinitely.
		if report.Daemon.Installed && report.Daemon.Label == plan.Label {
			if detail, retireErr := retireRenamedDaemon(ctx, setupOpts, previousName, previousKnown, steps, planErr); retireErr != nil {
				report.Warnings = append(report.Warnings, retireErr.Error())
				daemonProgress.Skip(retireErr.Error())
			} else if detail != "" {
				daemonProgress.Skip(detail)
			}
		}
	}
	timings.record("daemon", time.Since(daemonStarted))

	report.Hook = inspectSessionEndHook(repoDir)
	reportSetupHook(progress, report.Hook)

	// Remember the identities AND the tuning this run used, so `status` inspects
	// the daemon that exists rather than the default one it would have created,
	// and so a later bare `setup` re-installs the same daemon rather than one
	// reverted to stock intervals and models.
	// A failed plan must not become the identity used by the next setup.
	recordedDaemonName := setupOpts.daemonName
	if planErr != nil {
		recordedDaemonName, _ = installedDaemonName(machinePlan, machinePlanErr, previous, setUpBefore)
	}
	if err := writeSetupRecord(stateDir, setupRecord{
		SchemaVersion:  setupBackfillStateVersion,
		UpdatedAt:      steps.now().UTC(),
		Workspace:      setupOpts.workspace,
		DaemonName:     recordedDaemonName,
		Agent:          configuredAgent,
		BackfillBudget: &setupOpts.backfillBudget,
		Interval:       setupOpts.interval.String(),
		DistillEvery:   setupOpts.distillEvery.String(),
		Model:          strings.TrimSpace(setupOpts.model),
		Effort:         strings.TrimSpace(setupOpts.effort),
	}); err != nil {
		report.Warnings = append(report.Warnings, "setup record not saved: "+err.Error())
	}

	if setupOpts.json {
		return writeJSON(cmd, report)
	}
	// Re-read rather than reuse the value recordSetupWatchPlan returned: another
	// repo's `setup` may have joined the plan while this one was building, and
	// the coverage line is only worth printing if it is the truth right now.
	summaryPlan, _ := loadSetupWatchPlan(perRepo.Env)
	renderSetupSummary(cmd.OutOrStdout(), tui.NewRenderer(cmd.OutOrStdout()), report, timings, summaryPlan, brainCmd)
	return nil
}

func runSetupUninstall(ctx context.Context, cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, steps setupSteps, plan daemonPlan, planErr error, report setupReport) error {
	out := cmd.OutOrStdout()
	if planErr != nil {
		return planErr
	}
	var before daemonState
	if err := withDaemonRegistrationLock(opts.Env, func() error {
		before = steps.inspect(ctx, plan)
		return steps.uninstall(ctx, plan)
	}); err != nil {
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

// setupDaemonPreflightLine says, in one line and BEFORE the work starts, what
// setup is about to register with the machine's service manager, that it
// survives logout, which flag declines it, and which command removes it. It
// returns "" on a platform with no watcher to install, where there is nothing
// to warn about.
//
// The wording distinguishes the install from a re-run that finds the service
// already there: "will install" is a lie on the second `setup`, and a reader
// who is told a service will be installed every time stops reading the line.
func setupDaemonPreflightLine(plan daemonPlan, before daemonState, brainCmd string) string {
	if !plan.supported() {
		return ""
	}
	what := strings.TrimSpace(plan.Label)
	if what == "" {
		what = "background watcher"
	}
	if unit := strings.TrimSpace(plan.UnitPath); unit != "" {
		what += " at " + unit
	}
	if before.Installed {
		return fmt.Sprintf(
			"background watcher: this run keeps %s installed — a persistent %s service that starts again at every login; remove it with `%s setup --uninstall-daemon`",
			what, plan.Manager, setupBrainCommand(brainCmd))
	}
	return fmt.Sprintf(
		"background watcher: this run will install %s — a persistent %s service that starts again at every login; pass --no-daemon to skip it, or remove it later with `%s setup --uninstall-daemon`",
		what, plan.Manager, setupBrainCommand(brainCmd))
}

// runSetupDaemon installs the one machine-wide watcher. On an OS with no
// service manager this file plans for — windows today — it is a DOCUMENTED
// no-op: setup says so in one line, keeps its exit code, and everything the
// deterministic phases built stays usable. Pretending to install a launchd
// agent on windows, or failing setup because the platform has no launchd, would
// both be worse than saying plainly that freshness there is manual for now.
func runSetupDaemon(ctx context.Context, progress *refreshProgress, env EntireEnv, steps setupSteps, plan daemonPlan, report *setupReport, brainCmd string) daemonState {
	if !plan.supported() {
		// progress.Skip appends "skipped", so the label must not say it twice.
		progress.Skip(fmt.Sprintf("background watcher: not supported on %s yet (run `%s workspace watch %s` yourself to keep the brain fresh)", plan.OS, setupBrainCommand(brainCmd), report.Workspace.Name))
		return daemonState{Manager: daemonManagerUnsupported, Detail: "not supported on " + plan.OS + " yet"}
	}
	// Observe, decide and act as one step across processes: see
	// withDaemonRegistrationLock.
	var state daemonState
	_ = withDaemonRegistrationLock(env, func() error {
		state = runSetupDaemonLocked(ctx, progress, steps, plan, report)
		return nil
	})
	return state
}

func runSetupDaemonLocked(ctx context.Context, progress *refreshProgress, steps setupSteps, plan daemonPlan, report *setupReport) daemonState {
	before := steps.inspect(ctx, plan)
	// Idempotence: an installed, byte-identical, running unit is left strictly
	// alone. Reloading it would kill an in-flight refresh for no reason.
	if before.Installed && before.Current && before.Running {
		// Left strictly alone at the service manager, but the file is brought up
		// to date so the inherited PATH is refreshed at the next natural restart
		// instead of being frozen at whatever the first install saw.
		_ = refreshDaemonUnitBytes(plan)
		progress.Skip(fmt.Sprintf("background watcher already running (%s)", plan.Label))
		return before
	}
	// A no-register run can never observe "running", so byte-identical is the
	// whole of idempotence there; without this a second setup would report
	// "updated" forever and a harness could not demonstrate re-run safety.
	if plan.NoRegister && before.Installed && before.Current {
		progress.Skip(fmt.Sprintf("background watcher unit current, not registered (%s, %s)", plan.Label, envDaemonNoRegister))
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
func runSetupBackfill(ctx context.Context, progress *refreshProgress, opts Options, setupOpts setupCommandOptions, steps setupSteps, storage repoStorage, repoDir string, report *setupReport, brainCmd string) setupPhase {
	if setupOpts.noBackfill {
		progress.Skip("fact backfill (--no-backfill)")
		return setupPhase{State: "skipped", Detail: "--no-backfill"}
	}
	if report.Facts.Sessions == 0 {
		progress.Skip("fact backfill: no captured sessions yet " + progress.dash() + " it will start once sessions land")
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
	if previous, ok := readSetupBackfillState(stateDir); ok && backfillRunning(storage.BrainDir, previous.PID) {
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
		progress.Skip("fact backfill: no agent CLI on PATH (install codex or claude, then re-run `" + setupBrainCommand(brainCmd) + " setup`)")
		return setupPhase{State: "skipped", Detail: "no agent CLI available"}
	}
	budget := setupResolvedBackfillBudget(setupOpts)
	plan := setupBackfillPlanFor(opts, setupOpts, agent, repoDir, stateDir)
	if plan.Binary == "" {
		progress.Skip("fact backfill: could not resolve this executable's path")
		return setupPhase{State: "skipped", Detail: "executable path unavailable"}
	}
	// Say what this is about to cost BEFORE spending it. Everything the phase
	// used to print — how many sessions, which agent, which model — arrived
	// AFTER the detached child was already running, so the first thing the user
	// could do with the number was read it about a spend already underway. The
	// order is the whole point of a cost line: a user who did not want this has
	// to be able to see it and Ctrl-C, and to be told the one flag that turns it
	// off.
	queued := report.Facts.Pending()
	if budget > 0 && queued > budget {
		queued = budget
	}
	progress.Skip(fmt.Sprintf("fact backfill will SPEND TOKENS now: %s over %d session(s), newest first%s (--no-backfill skips it)",
		setupBackfillSpendLabel(agent, setupOpts), queued, setupBackfillOfLabel(queued, report.Facts.Pending())))

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
		MaxSessions:   budget,
		LogPath:       plan.LogPath,
		Sessions:      report.Facts.Pending(),
	}
	if err := writeSetupBackfillState(stateDir, state); err != nil {
		detail := "backfill state not recorded: " + err.Error()
		if stopErr := steps.stopBackfill(pid); stopErr != nil {
			detail += "; stopping untracked backfill: " + stopErr.Error()
		}
		progress.Skip("fact backfill: " + detail)
		report.Warnings = append(report.Warnings, detail)
		return setupPhase{State: "failed", Detail: detail}
	}
	task := progress.Begin("fact backfill")
	task.Update(fmt.Sprintf("fact backfill started in the background: %d session(s), newest first, agent %s (pid %d)",
		queued, agent, pid))
	task.Finish(nil)
	// Say what the cap did and how to lift it. A silent cap is as surprising as
	// a silent uncapped spend: the user is left believing the whole corpus was
	// distilled when only the newest slice was.
	if budget > 0 && state.Sessions > budget {
		progress.Skip(fmt.Sprintf(
			"fact backfill capped at %d of %d pending session(s) this pass (--backfill-budget); re-run `%s setup` for the next batch, or `--backfill-budget 0` to distill the whole corpus in one spend",
			budget, state.Sessions, setupBrainCommand(brainCmd)))
	}
	return setupPhase{State: "ok", Detail: fmt.Sprintf("pid %d, %d of %d pending session(s) queued", pid, queued, state.Sessions)}
}

// setupBackfillSpendLabel names WHAT will be spent, in the terms the user chose:
// the agent, and the model and effort if they were set. "agent claude on
// haiku/low" is a cost a reader can price; "the backfill" is not.
func setupBackfillSpendLabel(agent string, setupOpts setupCommandOptions) string {
	label := "agent " + agent
	model := strings.TrimSpace(setupOpts.model)
	effort := strings.TrimSpace(setupOpts.effort)
	switch {
	case model != "" && effort != "":
		label += " on " + model + "/" + effort
	case model != "":
		label += " on " + model
	case effort != "":
		label += " at " + effort + " effort"
	}
	return label
}

// setupBackfillOfLabel spells out the cap when one is biting, so "25 sessions"
// is never mistaken for "all of them".
func setupBackfillOfLabel(queued, pending int) string {
	if queued >= pending {
		return ""
	}
	return fmt.Sprintf(" (capped at %d of %d pending; --backfill-budget 0 for all)", queued, pending)
}

// setupResolvedBackfillBudget normalizes --backfill-budget into the number the
// distill passes actually receive, so the detached backfill and the daemon it
// installs are capped by the SAME value. Only an EXPLICIT 0 means unlimited; a
// negative value is a typo, not a request to distill the whole corpus, and
// silently reading it as one is the most expensive way to misread a flag.
func setupResolvedBackfillBudget(setupOpts setupCommandOptions) int {
	if setupOpts.backfillBudget < 0 {
		return setupDefaultBackfillBudget
	}
	return setupOpts.backfillBudget
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
	if budget := setupResolvedBackfillBudget(setupOpts); budget > 0 {
		args = append(args, "--max-sessions", fmt.Sprint(budget))
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
	return brainWatchDaemonPlanFor(runtime.GOOS, opts, setupOpts)
}

// brainWatchDaemonPlanFor is brainWatchDaemonPlan with the target OS as an
// argument. runtime.GOOS is read in exactly one place (above) so that the whole
// plan — spec, label, unit path, rendered contents — can be built for darwin or
// linux from any host, which is what makes the daemon behaviour testable on a
// windows runner instead of silently untested there.
func brainWatchDaemonPlanFor(goos string, opts Options, setupOpts setupCommandOptions) (daemonPlan, error) {
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
	plan, err := planBrainWatchDaemon(goos, home, os.Getenv(xdgConfigHome), os.Getenv(envDaemonUnitDir), spec)
	if err != nil {
		return daemonPlan{}, err
	}
	// Read the no-register decision ONCE, here, so install, uninstall and the
	// running-probe all act on the same answer for the life of this command.
	plan.NoRegister = daemonRegistrationDisabled()
	return plan, nil
}

// brainWatchDaemonArgs are the argv of the ONE machine-wide watcher, and they
// are deliberately CONSTANT: `workspace watch --distill`, with no positional
// workspace and no tuning flags. Nothing about which repo ran `setup` reaches
// the unit file.
//
// That is the whole point. The launchd label and systemd unit name derive from
// the daemon NAME alone, so they are machine-wide — but the args used to be
// per-repo (`workspace watch <ws> --interval ... --model ...`). Two repos that
// asked for two different workspaces resolved to the SAME unit path and rendered
// DIFFERENT bytes: the second `setup` overwrote the first repo's watcher, the
// first repo was never watched again, and its `status` still said "running"
// because the drift was computed and then dropped. Even two repos on the SAME
// workspace clobbered each other's interval, model and effort.
//
// With a constant unit, everything that varies per workspace — interval,
// distill cadence, agent, model, effort, session cap — lives in the machine-level
// watch plan (setup_watchplan.go) that the running daemon re-reads on every
// outer pass. A second repo's `setup` adds a row to that plan; the unit's bytes
// do not change, so inspectDaemon sees no drift and the service is never
// unloaded and reloaded under an in-flight refresh.
//
// --distill stays here rather than in the plan because it is not tuning: it is
// the statement that this service is allowed to spend at all, and `setup` always
// installs a watcher that is. How MUCH and how OFTEN it may spend are both in
// the plan (--max-sessions, --distill-every) and both bind, per workspace.
//
// --budget is deliberately NOT passed. It counts gated agent runs for the life
// of the PROCESS and never resets, and this daemon is a KeepAlive service that
// is meant to run for months — so `--budget 1` did not mean "one run per window",
// it meant one run EVER, machine-wide, across every repo in the workspace. The
// durable, self-resetting guard is the pair watch.go documents: --distill-every
// plus each repo's persisted cursor, which survives restarts and bounds spend
// per repo per window without ever latching off.
func brainWatchDaemonArgs(setupCommandOptions) []string {
	return []string{"workspace", "watch", "--distill"}
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
func resolveSetupSteps(cmd *cobra.Command, opts Options, setupOpts setupCommandOptions, repoDir, brainCmd string, steps setupSteps) setupSteps {
	if steps.now == nil {
		steps.now = opts.Now
		if steps.now == nil {
			steps.now = time.Now
		}
	}
	if steps.instant == nil {
		steps.instant = func(ctx context.Context) ([]setupComponent, error) {
			// The free path `watch` runs — sessions + semantic + seed + docs +
			// entities + memory reconciliation with seed agent "none" — in
			// best-effort mode, so each component reports its own outcome and one
			// failure does not abort the build. History is the one difference from
			// a tick: onboarding builds the projection now rather than leaving the
			// first `brief` to answer without it.
			sub := &cobra.Command{}
			sub.SetContext(ctx)
			sub.SetOut(cmd.OutOrStdout())
			sub.SetErr(cmd.ErrOrStderr())
			if setupOpts.json {
				sub.SetOut(io.Discard)
				sub.SetErr(io.Discard)
			}
			var components []setupComponent
			err := watchDeterministicRefreshComponents(ctx, sub, opts, repoDir, setupBuildsHistoryProjection, func(name string, err error) {
				component := newSetupComponent(name, err, brainCmd)
				components = append(components, component)
				steps.observeComponent.notify(component)
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
	if steps.stopBackfill == nil {
		steps.stopBackfill = stopDetachedProcess
	}
	if steps.plan == nil {
		steps.plan = func(so setupCommandOptions) (daemonPlan, error) {
			return brainWatchDaemonPlan(opts, so)
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
	Interval       string `json:"interval,omitempty"`
	DistillEvery   string `json:"distill_every,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	Agent          string `json:"agent,omitempty"`
	BackfillBudget *int   `json:"backfill_budget,omitempty"`
}

// setupFlagNames maps each remembered option to its flag, so "was this passed?"
// is asked of cobra rather than inferred.
const (
	setupFlagAgent          = "agent"
	setupFlagBackfillBudget = "backfill-budget"
	setupFlagWorkspace      = "workspace"
	setupFlagDaemonName     = "daemon-name"
	setupFlagInterval       = "interval"
	setupFlagDistillEvery   = "distill-every"
	setupFlagModel          = "model"
	setupFlagEffort         = "effort"
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
	if !changed(setupFlagAgent) && recorded.agent != "" {
		setupOpts.agent = recorded.agent
	}
	if !changed(setupFlagBackfillBudget) {
		setupOpts.backfillBudget = recorded.backfillBudget
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
func retireRenamedDaemon(ctx context.Context, setupOpts setupCommandOptions, old string, known bool, steps setupSteps, planErr error) (string, error) {
	old = strings.TrimSpace(old)
	// Only a machine that actually installed a watcher before can have one to
	// retire. Without that check, a first-ever `setup --daemon-name mine` would
	// read the absent record as the default name and uninstall the machine-wide
	// default watcher that every other repo depends on.
	if !known || planErr != nil || old == "" || old == setupOpts.daemonName || setupOpts.noDaemon {
		return "", nil
	}
	retired := setupOpts
	retired.daemonName = old
	oldPlan, err := steps.plan(retired)
	if err != nil {
		return "", fmt.Errorf("previous watcher %q could not be planned for removal: %w", old, err)
	}
	// An OS with no service manager never installed the old daemon either, so
	// there is nothing to retire and nothing worth saying about it.
	if !oldPlan.supported() {
		return "", nil
	}
	// Uninstall is idempotent, so do not re-inspect here: the replacement has
	// already been installed and the old unit may no longer be representable by
	// a single-state test adapter. A known previous name is enough authority.
	if err := steps.uninstall(ctx, oldPlan); err != nil {
		return "", fmt.Errorf("previous watcher %s not removed: %w", oldPlan.Label, err)
	}
	return fmt.Sprintf("previous watcher %s removed (renamed to %s)", oldPlan.Label, setupOpts.daemonName), nil
}

// installedDaemonName answers "which watcher does this MACHINE have installed",
// preferring the machine-level watch plan over this repo's own setup record.
//
// The per-repo record is the fallback for a machine set up by an older build
// that never wrote a plan; it is correct for the single-repo case and wrong for
// exactly the case the plan exists to fix, so the plan wins whenever it has an
// answer. An unreadable plan falls back rather than guessing: retiring the
// wrong daemon is worse than retiring none.
func installedDaemonName(plan setupWatchPlan, planErr error, previous setupCommandOptions, setUpBefore bool) (string, bool) {
	if planErr == nil {
		if name := strings.TrimSpace(plan.DaemonName); name != "" {
			return name, true
		}
	}
	return strings.TrimSpace(previous.daemonName), setUpBefore
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
//
// The error is the third state, and it is NOT the same as "never set up": a
// setup.json that exists but cannot be parsed means the recorded daemon name is
// unknown, and answering "never set up, here are the defaults" made
// `--uninstall-daemon` uninstall the DEFAULT watcher — which on a shared
// machine is the one every OTHER repo depends on — instead of this repo's. A
// caller that is about to act on a previous daemon must refuse; a caller that
// only wants defaults may ignore it.
func setupOptionsFromRecord(stateDir string) (setupCommandOptions, bool, error) {
	opts := defaultSetupOptions()
	data, err := os.ReadFile(filepath.Join(stateDir, setupRecordFile))
	if err != nil {
		if os.IsNotExist(err) {
			return opts, false, nil
		}
		return opts, false, fmt.Errorf("read %s: %w", filepath.Join(stateDir, setupRecordFile), err)
	}
	var record setupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return opts, false, fmt.Errorf("parse %s: %w", filepath.Join(stateDir, setupRecordFile), err)
	}
	if record.Agent != "" {
		opts.agent = record.Agent
	}
	if record.BackfillBudget != nil {
		opts.backfillBudget = *record.BackfillBudget
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
	return opts, true, nil
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
	dash := progress.dash()
	switch {
	case hook.Installed && !hook.EntireCLI:
		progress.Skip(fmt.Sprintf("session-end hook wired (%s) but `%s` is not on PATH %s sessions will NOT distill until the Entire CLI is installed", hook.Source, entireBinaryName, dash))
	case hook.Installed:
		progress.Skip(fmt.Sprintf("session-end hook already wired (%s)", hook.Source))
	case !hook.EntireCLI:
		progress.Skip(fmt.Sprintf("session-end hook not wired and `%s` is not on PATH %s install the Entire CLI, then run `entire enable` in this repo", entireBinaryName, dash))
	default:
		progress.Skip("session-end hook not wired " + dash + " run `entire enable` in this repo so each session distills as it ends (SPENDS TOKENS per session)")
	}
}

func describeDaemonState(state daemonState, brainCmd string) string {
	if state.Detail != "" && state.Installed {
		detail := state.Detail
		state.Detail = ""
		return describeDaemonState(state, brainCmd) + "; " + detail
	}
	switch {
	case state.Detail != "" && !state.Installed:
		return state.Detail
	case state.Running && !state.Current:
		// A running watcher whose unit on disk is not what this repo would write
		// is the one state a reader most needs told. inspectDaemon computed
		// Current and this function dropped it, so a watcher installed by a
		// different build or a different name reported as plain "running" and
		// nothing in `status` said otherwise.
		return fmt.Sprintf("running, but the installed unit is stale and not current (%s); re-run `%s setup`", state.Label, setupBrainCommand(brainCmd))
	case state.Running:
		return fmt.Sprintf("running (%s)", state.Label)
	case state.Installed && !state.Current:
		return fmt.Sprintf("installed but not running, and not current (%s)", state.Label)
	case state.Installed:
		return fmt.Sprintf("installed but not running (%s)", state.Label)
	default:
		return "not installed"
	}
}
