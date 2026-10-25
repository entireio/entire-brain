package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/workspace"
	"github.com/spf13/cobra"
)

const workspaceGraphName = "graph.json"

type workspaceAddOptions struct {
	name string
}

type workspaceContextOptions struct {
	limit int
	json  bool
}

type workspaceRetrieveOptions struct {
	limit  int
	branch string
	json   bool
	// Retrieval source selector and conversation filters, mirroring the
	// top-level verbs (Phase 5: the conversation source fans out across member
	// brains with the same explicit opt-in and caveat contract).
	source          string
	after           string
	before          string
	session         string
	agent           string
	concepts        []string
	includeAbstract bool
}

type workspaceImpactOptions struct {
	limit int
	depth int
	json  bool
}

type workspaceGraphOptions struct {
	limit int
	json  bool
}

type workspaceContextResult struct {
	RepoKey   string                  `json:"repo_key"`
	Name      string                  `json:"name,omitempty"`
	Freshness workspace.RepoFreshness `json:"freshness"`
	Symbols   []semanticRecord        `json:"symbols"`
	Error     string                  `json:"error,omitempty"`
}

type workspaceRetrieveResult struct {
	RepoKey                   string                   `json:"repo_key"`
	Name                      string                   `json:"name,omitempty"`
	Branch                    string                   `json:"branch,omitempty"`
	Freshness                 workspace.RepoFreshness  `json:"freshness"`
	Results                   []unifiedResult          `json:"results"`
	AbstractPreviews          []sessionAbstractPreview `json:"abstract_previews,omitempty"`
	AbstractPreviewsTruncated bool                     `json:"abstract_previews_truncated,omitempty"`
	Error                     string                   `json:"error,omitempty"`
}

type workspaceGetResult struct {
	RepoKey string          `json:"repo_key"`
	Branch  string          `json:"branch,omitempty"`
	Results []unifiedResult `json:"results"`
	Missing []string        `json:"missing"`
	Error   string          `json:"error,omitempty"`
}

type workspaceImpactResult struct {
	RepoKey   string                  `json:"repo_key"`
	Name      string                  `json:"name,omitempty"`
	Freshness workspace.RepoFreshness `json:"freshness"`
	Roots     []semanticRecord        `json:"roots"`
	Symbols   []semanticRecord        `json:"symbols"`
	Relations []semanticRecord        `json:"relations"`
	Error     string                  `json:"error,omitempty"`
}

type workspaceGraphResult struct {
	RepoKey   string                  `json:"repo_key"`
	Name      string                  `json:"name,omitempty"`
	Freshness workspace.RepoFreshness `json:"freshness"`
	Counts    map[string]int          `json:"counts,omitempty"`
	Languages []string                `json:"languages,omitempty"`
	// Retrieval-trust diagnostics carried from each repo's semantic source
	// manifest so an agent can tell a trusted index from a degraded one per repo.
	CompletenessLevel string            `json:"completeness_level,omitempty"`
	Trust             string            `json:"trust,omitempty"`
	LanguageTiers     map[string]string `json:"language_tiers,omitempty"`
	RelationTypes     []string          `json:"relation_types,omitempty"`
	Metrics           graphMetrics      `json:"metrics,omitempty"`
	Error             string            `json:"error,omitempty"`
}

type workspaceGraphPayload struct {
	Generation  string                 `json:"generation,omitempty"`
	Workspace   string                 `json:"workspace"`
	GeneratedAt time.Time              `json:"generated_at"`
	Results     []workspaceGraphResult `json:"results"`
	Contracts   []workspace.Contract   `json:"contracts"`
	CrossEdges  []workspace.CrossEdge  `json:"cross_edges"`
}

type workspaceRegressionResult struct {
	RepoKey   string                  `json:"repo_key"`
	Name      string                  `json:"name,omitempty"`
	Freshness workspace.RepoFreshness `json:"freshness"`
	RepoPath  string                  `json:"repo_path,omitempty"`
	Anomalies []regressionAnomaly     `json:"anomalies"`
	// Checked and FilesScanned are the machine-readable answer to "did this
	// repo actually get compared?", so a consumer never has to parse prose (or
	// infer from an empty anomaly list) to find out. Checked is false whenever
	// FilesScanned is zero: the detector read nothing, and an empty result is
	// therefore the absence of evidence, not evidence of absence.
	Checked      bool `json:"checked"`
	FilesScanned int  `json:"files_scanned"`
	// HistoryTruncated is true when the history scan stopped at a cap before
	// the end of the brain. Then an empty Anomalies list is an incomplete
	// answer, not a clean one -- the same reason Checked exists, one layer in.
	HistoryTruncated bool     `json:"history_truncated,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type workspaceReviewResult struct {
	RepoKey   string                  `json:"repo_key"`
	Name      string                  `json:"name,omitempty"`
	Freshness workspace.RepoFreshness `json:"freshness"`
	RepoPath  string                  `json:"repo_path,omitempty"`
	Summary   string                  `json:"summary"`
	Findings  []reviewFinding         `json:"findings"`
	// See workspaceRegressionResult: Checked/FilesScanned keep the "was this
	// repo compared at all?" answer out of the prose.
	Checked      bool `json:"checked"`
	FilesScanned int  `json:"files_scanned"`
	// See workspaceRegressionResult.HistoryTruncated.
	HistoryTruncated bool     `json:"history_truncated,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	Error            string   `json:"error,omitempty"`
}

func newWorkspaceCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage local multi-repo brain workspaces",
	}
	cmd.AddCommand(newWorkspaceCreateCommand(opts))
	cmd.AddCommand(newWorkspaceListCommand(opts))
	cmd.AddCommand(newWorkspaceAddCommand(opts))
	cmd.AddCommand(newWorkspaceRemoveCommand(opts))
	cmd.AddCommand(newWorkspaceRefreshCommand(opts))
	cmd.AddCommand(newWorkspaceWatchCommand(opts))
	cmd.AddCommand(newWorkspaceInspectCommand(opts))
	cmd.AddCommand(newWorkspaceSearchCommand(opts))
	cmd.AddCommand(newWorkspaceVsearchCommand(opts))
	cmd.AddCommand(newWorkspaceQueryCommand(opts))
	cmd.AddCommand(newWorkspaceGetCommand(opts))
	cmd.AddCommand(newWorkspaceReviewCommand(opts))
	cmd.AddCommand(newWorkspacePatternsCommand(opts))
	return cmd
}

// newWorkspaceInspectCommand mirrors the single-repo `inspect` group: the
// specialist symbol-graph and regression verbs live one level down, keeping the
// workspace surface a faithful multi-repo projection of the top-level one.
func newWorkspaceInspectCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Run specialist inspection commands across workspace repos",
	}
	cmd.AddCommand(newWorkspaceContextCommand(opts))
	cmd.AddCommand(newWorkspaceImpactCommand(opts))
	cmd.AddCommand(newWorkspaceGraphCommand(opts))
	cmd.AddCommand(newWorkspaceRegressionsCommand(opts))
	return cmd
}

func newWorkspaceCreateCommand(opts Options) *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create a local workspace brain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := workspaceStore(opts.Env).Dir(args[0])
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(dir, workspace.ManifestName)); err == nil {
				return fmt.Errorf("workspace already exists: %s", args[0])
			} else if err != nil && !os.IsNotExist(err) {
				return err
			}
			manifest := workspace.Manifest{SchemaVersion: workspace.SchemaVersion, Name: args[0]}
			if err := workspaceStore(opts.Env).WriteManifest(manifest); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), dir)
			return nil
		},
	}
}

func newWorkspaceAddCommand(opts Options) *cobra.Command {
	addOpts := workspaceAddOptions{}
	cmd := &cobra.Command{
		Use:   "add <workspace> <repo-path>",
		Short: "Add a local repository to a workspace by repo key",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceAdd(cmd.Context(), cmd, opts, addOpts, args[0], args[1])
		},
	}
	cmd.Flags().StringVar(&addOpts.name, "name", "", "Workspace-local repository name")
	return cmd
}

func newWorkspaceRefreshCommand(opts Options) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "refresh <workspace>",
		Short: "Refresh local workspace membership freshness (--full also refreshes each member brain)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRefresh(cmd.Context(), cmd, opts, args[0], full)
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "Run the deterministic single-repo refresh on every member first (no agent tokens)")
	return cmd
}

func newWorkspaceWatchCommand(opts Options) *cobra.Command {
	w := defaultWatchOptions()
	cmd := &cobra.Command{
		Use:   "watch [workspace]",
		Short: "Keep every repo in a workspace fresh automatically (deterministic refresh is free; agent steps gated)",
		Long: "Keep every repo in a workspace fresh automatically.\n\n" +
			"With a workspace name, watches that one workspace with the flags given here.\n\n" +
			"With NO workspace name this is SUPERVISED mode: it watches every workspace `setup`\n" +
			"has recorded in the machine-level watch plan, each with its own interval, distill\n" +
			"cadence, agent, model, effort and session cap. That is the form the installed\n" +
			"background service runs, and it is why the installed unit is byte-identical on\n" +
			"every machine: nothing about which repo ran `setup` reaches the unit file.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return runSupervisedWatch(cmd.Context(), cmd, opts, w)
			}
			return runWorkspaceWatch(cmd.Context(), cmd, opts, w, args[0])
		},
	}
	bindWatchFlags(cmd, &w)
	return cmd
}

// runSupervisedWatch is the ONE machine-wide watcher. It re-reads the watch plan
// on every outer pass, so a repo that runs `setup` while the daemon is already
// running joins the rotation WITHOUT the unit file changing and therefore
// without the service being unloaded and reloaded under an in-flight refresh.
//
// An empty plan is not an error. A KeepAlive/Restart=always unit that exits
// non-zero because nothing is registered yet is a restart loop, and the honest
// state of a machine mid-onboarding is "nothing to watch yet", so it says that
// once per pass and keeps waiting.
func runSupervisedWatch(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions) error {
	if w.distillJobs <= 0 {
		return fmt.Errorf("--jobs must be greater than 0")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ctx, stop := watchSignalContext(ctx)
	defer stop()
	onePass := func(workspaceName string, pass watchCommandOptions, agentCalls *int) error {
		return runWorkspaceWatchPass(ctx, cmd, opts, pass, workspaceName, now, agentCalls)
	}
	maintain := daemonLogMaintainer(opts.Env)
	var stale func() bool
	if maintain != nil {
		// Only the installed service watches its own binary: a foreground run
		// is attended, and exiting it over an upgrade would surprise the person
		// who started it. The maintainer is already the "am I the installed
		// service" test (stdout is the unit's log file).
		stale = watchBinaryChanged()
	}
	err := supervisedWatchLoop(ctx, cmd.OutOrStdout(), opts.Env, w, onePass, maintain, stale)
	if errors.Is(err, errWatchBinaryReplaced) {
		relaunchInstalledDaemon(cmd.OutOrStdout(), opts.Env)
		// Exit cleanly: KeepAlive/Restart=always makes the service manager
		// restart on the current binary; a non-zero exit adds nothing but noise.
		return nil
	}
	return err
}

// supervisedWatchLoop is the supervised loop with the per-workspace pass
// injected, so the part that matters — which workspaces get visited, with whose
// tuning, and what an empty or unreadable plan does — is testable without a real
// refresh, a real manifest, or a real agent.
// maintain is the daemon's per-pass self-upkeep (log rotation). It runs at the
// TOP of every outer pass, before the plan is even read, because the pass that
// most needs bounding is the one that finds nothing to do and prints a line
// anyway — an empty or unreadable plan logs once per interval forever. It is
// nil for any watcher that is not the installed service.
// stale reports that the executable this daemon started from was replaced on
// disk (nil = never check); the loop then returns errWatchBinaryReplaced so the
// caller can exit and let the service manager restart on the current binary,
// instead of running an outdated build forever (COR-2174).
func supervisedWatchLoop(ctx context.Context, out io.Writer, env EntireEnv, w watchCommandOptions, onePass func(workspace string, pass watchCommandOptions, agentCalls *int) error, maintain func(), stale func() bool) error {
	// This counter belongs to the supervised process, not to an individual
	// workspace pass. --budget is documented as a lifetime cap, so it must
	// survive both workspace fan-out and every outer-plan tick.
	agentCalls := 0
	for {
		if ctx.Err() != nil {
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		}
		if maintain != nil {
			maintain()
		}
		if stale != nil && stale() {
			fmt.Fprintln(out, "[watch] binary replaced on disk; exiting so the service manager restarts on the current build")
			return errWatchBinaryReplaced
		}
		plan, err := loadSetupWatchPlan(env)
		if err != nil {
			// An unreadable plan must not take the service down: report it and
			// retry on the next pass, because the file is repaired by the next
			// `setup` and a crash-looping daemon repairs nothing.
			fmt.Fprintf(out, "[watch] watch plan unusable: %v\n", err)
		}
		if len(plan.Workspaces) == 0 {
			fmt.Fprintln(out, "[watch] no workspaces registered yet — run `entire-brain setup` in a repo")
		}
		for _, entry := range plan.Workspaces {
			// Checked per workspace so a `stop` mid-pass spares the rest. Pausing
			// skips work but keeps the loop and its cadence: the unit is
			// KeepAlive, so exiting would only be restarted straight back here.
			if backgroundPaused(env) {
				fmt.Fprintln(out, "[watch] paused — `entire-brain start` resumes")
				break
			}
			pass := applyWatchPlanEntry(w, entry)
			// once: this loop owns the waiting, so the inner workspace loop runs
			// exactly one pass over its members and returns.
			pass.once = true
			if err := onePass(entry.Workspace, pass, &agentCalls); err != nil {
				// One broken workspace must not stop the others. A manifest
				// deleted by hand is a normal state, not a fatal one.
				fmt.Fprintf(out, "[watch] workspace %s: skipped (%v)\n", entry.Workspace, err)
			}
			if ctx.Err() != nil {
				fmt.Fprintln(out, "[watch] stopping")
				return nil
			}
		}
		if w.once {
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		case <-time.After(watchPlanSleep(plan, w.interval)):
		}
	}
}

// runWorkspaceWatch fans the WS3 watch loop over every member repo, reusing the same per-repo
// deterministic-refresh + gated-distill + per-repo cursor. The --budget cap is shared across all members
// so the --budget distill-run cap (and thus token spend) can't be multiplied across repos.
func runWorkspaceWatch(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions, workspaceName string) error {
	if w.distillJobs <= 0 {
		return fmt.Errorf("--jobs must be greater than 0")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	// Stop cleanly between ticks on SIGINT/SIGTERM (same as single-repo watch).
	ctx, stop := watchSignalContext(ctx)
	defer stop()
	agentCalls := 0
	return runWorkspaceWatchPass(ctx, cmd, opts, w, workspaceName, now, &agentCalls)
}

// runWorkspaceWatchPass is runWorkspaceWatch without the signal plumbing, so the
// supervised loop can drive one workspace at a time under a context it already
// owns instead of installing a second signal handler per workspace per pass.
func runWorkspaceWatchPass(ctx context.Context, cmd *cobra.Command, opts Options, w watchCommandOptions, workspaceName string, now func() time.Time, agentCalls *int) error {
	out := cmd.OutOrStdout()
	// repoOut is the member's already-attributed writer, handed down by the
	// loop (see workspaceWatchLoop): the fan-out is the case that needs the
	// prefix most, because without it a tick's lines name no repo at all --
	// only a header line several lines above them.
	repoTick := func(repoOut io.Writer, repoDir string, agentCalls *int) {
		storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
		if err != nil {
			fmt.Fprintf(repoOut, "  skipped: %v\n", err)
			return
		}
		cursorPath := filepath.Join(filepath.Dir(storage.HeadPath), "watch.json")
		watchTick(ctx, repoOut, w, cursorPath, watchStepsForRepo(watchStepCommand(cmd, repoOut), opts, w, repoDir, now), agentCalls)
	}
	return workspaceWatchLoop(ctx, out, opts, w, workspaceName, agentCalls, now, repoTick)
}

// workspaceWatchLoop iterates the workspace members, resolving each to a local repo and handing it to
// repoTick with a single shared agentCalls counter (so --budget caps total token spend across the whole
// workspace, not per-repo). repoTick is injected so the fan-out is testable without a real refresh.
func workspaceWatchLoop(ctx context.Context, out io.Writer, opts Options, w watchCommandOptions, workspaceName string, agentCalls *int, now func() time.Time, repoTick func(out io.Writer, repoDir string, agentCalls *int)) error {
	if w.interval <= 0 {
		w.interval = 5 * time.Minute
	}
	// The loop, not the tick, decides a line's attribution, so a tick cannot
	// forget it: workspace-level lines carry the clock alone, and every member
	// gets a writer already stamped with its repo key. Wrapped exactly once
	// per sink -- nesting two of these would stamp every line twice.
	raw := out
	// pass counts the fan-out sweeps so a member's lines say which sweep they
	// belong to, the same way a single-repo daemon's lines say which tick.
	pass := 0
	out = newWatchLogPrefixWriter(raw, watchLogFields{build: opts.Version, tick: &pass}, now)
	for {
		if ctx.Err() != nil {
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		}
		pass++
		manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "[watch] workspace %s — %d repos, distill=%v (every %s, max-sessions=%s, run-budget=%s)\n",
			manifest.Name, len(manifest.Repos), w.distill, w.distillEvery,
			watchCapLabel(w.distillMaxSessions), watchCapLabel(w.budget))
		dirs, err := resolvePluginDirs(opts.Env)
		if err != nil {
			return err
		}
		for _, repo := range manifest.Repos {
			repoDir, err := resolveWorkspaceMemberRepoDir(ctx, opts, dirs.Config, repo)
			if err != nil {
				fmt.Fprintf(out, "[watch] %s: skipped (%v)\n", repo.RepoKey, err)
				continue
			}
			fmt.Fprintf(out, "[watch] %s:\n", repo.RepoKey)
			repoTick(newWatchLogPrefixWriter(raw, watchLogFields{build: opts.Version, tick: &pass, repo: repo.RepoKey}, now), repoDir, agentCalls)
		}
		if w.once {
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		case <-time.After(w.interval):
		}
	}
}

func newWorkspaceContextCommand(opts Options) *cobra.Command {
	contextOpts := workspaceContextOptions{limit: 10}
	cmd := &cobra.Command{
		Use:   "context <workspace> <symbol-or-text>",
		Short: "Build code context for a symbol or query across workspace repos",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceContext(cmd, opts, contextOpts, args[0], args[1])
		},
	}
	cmd.Flags().IntVar(&contextOpts.limit, "limit", 10, "Maximum symbols per repo")
	cmd.Flags().BoolVar(&contextOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newWorkspaceImpactCommand(opts Options) *cobra.Command {
	impactOpts := workspaceImpactOptions{limit: 20, depth: 1}
	cmd := &cobra.Command{
		Use:   "impact <workspace> <symbol-or-text>",
		Short: "Run code-context impact across workspace repos",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceImpact(cmd, opts, impactOpts, args[0], args[1])
		},
	}
	cmd.Flags().IntVar(&impactOpts.limit, "limit", 20, "Maximum symbols per repo")
	cmd.Flags().IntVar(&impactOpts.depth, "depth", 1, "Relation traversal depth")
	cmd.Flags().BoolVar(&impactOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newWorkspaceGraphCommand(opts Options) *cobra.Command {
	graphOpts := workspaceGraphOptions{limit: 50}
	cmd := &cobra.Command{
		Use:   "graph <workspace>",
		Short: "Summarize code graph schemas and cross-repo contracts in a workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceGraph(cmd, opts, graphOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&graphOpts.limit, "limit", 50, "Maximum cross-repo contracts to return")
	cmd.Flags().BoolVar(&graphOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newWorkspaceSearchCommand(opts Options) *cobra.Command {
	return newWorkspaceRetrieveCommand(opts, "search", modeLexical, "Lexical keyword search across every workspace repo's brain (facts, history, docs)")
}

func newWorkspaceVsearchCommand(opts Options) *cobra.Command {
	return newWorkspaceRetrieveCommand(opts, "vsearch", modeHybrid, "Compatibility alias for workspace query: semantic retrieval was removed (trail #278); runs the same BM25 lexical ranking")
}

func newWorkspaceQueryCommand(opts Options) *cobra.Command {
	return newWorkspaceRetrieveCommand(opts, "query", modeHybrid, "Search every workspace repo's brain (BM25 lexical)")
}

// newWorkspaceRetrieveCommand mirrors the top-level search/vsearch/query verbs
// (retrieve_cmd.go) as a per-member fan-out: each repo's brain is ranked
// independently and results stay grouped by repo — scores from different brains'
// indexes are not comparable, so there is no cross-repo fusion.
func newWorkspaceRetrieveCommand(opts Options, use string, mode retrievalMode, short string) *cobra.Command {
	var selection querySelection
	retrieveOpts := workspaceRetrieveOptions{limit: 10}
	cmd := &cobra.Command{
		Use:   use + " <workspace> <query>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			selectedMode, err := selection.mode(mode)
			if err != nil {
				return err
			}
			warnRemovedSemanticAlias(cmd, use, selection.semantic)
			return runWorkspaceRetrieve(cmd, opts, retrieveOpts, selectedMode, args[0], args[1])
		},
	}
	configureQueryCommand(cmd, use, &selection, 1)
	cmd.Flags().IntVar(&retrieveOpts.limit, "limit", 10, "Maximum results per repo")
	cmd.Flags().StringVar(&retrieveOpts.branch, "branch", "", "Branch for facts in every repo (default: each repo's distill default)")
	cmd.Flags().BoolVar(&retrieveOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&retrieveOpts.source, "source", "", "Restrict retrieval to one source per repo: all, fact, history, conversation, or doc (conversation is experimental opt-in)")
	cmd.Flags().StringVar(&retrieveOpts.after, "after", "", "Conversation source only: sessions at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&retrieveOpts.before, "before", "", "Conversation source only: sessions before this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&retrieveOpts.session, "session", "", "Conversation source only: exchanges from this session id")
	cmd.Flags().StringVar(&retrieveOpts.agent, "agent", "", "Conversation source only: exchanges captured by this agent/harness")
	cmd.Flags().StringArrayVar(&retrieveOpts.concepts, "concept", nil, "Conversation source only: additional concept (repeatable, up to 4); each member repo's sessions must match the query AND every concept")
	cmd.Flags().BoolVar(&retrieveOpts.includeAbstract, "include-abstract", false, "Conversation source only: include bounded evidence-linked session previews per member repo")
	return cmd
}

func newWorkspaceGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var branch string
	var contextBefore, contextAfter, afterTurn, outlineLimit int
	cmd := &cobra.Command{
		Use:   "get <workspace> <repo-key/id>...",
		Short: "Fetch items in full by repo-qualified id (e.g. gh/owner/repo/fact:… or gh/owner/repo/conversation:…, as printed by workspace search)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			gopts := getOptions{
				ContextBefore: contextBefore, ContextAfter: contextAfter,
				AfterTurn: afterTurn, OutlineLimit: outlineLimit,
				ContextSet: cmd.Flags().Changed("context-before") || cmd.Flags().Changed("context-after"),
				OutlineSet: cmd.Flags().Changed("after-turn") || cmd.Flags().Changed("limit"),
			}
			return runWorkspaceGet(cmd, opts, args[0], args[1:], branch, jsonOut, gopts)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts in every repo (default: each repo's distill default)")
	cmd.Flags().IntVar(&contextBefore, "context-before", 0, "Adjacent earlier exchanges to include (conversation: ids only, 0-3)")
	cmd.Flags().IntVar(&contextAfter, "context-after", 0, "Adjacent later exchanges to include (conversation: ids only, 0-3)")
	cmd.Flags().IntVar(&afterTurn, "after-turn", 0, "Outline cursor: entries after this turn ordinal (conversation-session: ids only)")
	cmd.Flags().IntVar(&outlineLimit, "limit", 0, "Outline entries per page, max 50 (conversation-session: ids only)")
	return cmd
}

func runWorkspaceAdd(ctx context.Context, cmd *cobra.Command, opts Options, addOpts workspaceAddOptions, workspaceName, repoPath string) error {
	if err := workspace.ValidateRepoName(addOpts.name); err != nil {
		return err
	}
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, repoPath)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("workspace repo must be an existing local path: %s", repoPath)
	}
	// Existence is not repo-ness. resolveLocalTargetRepoDir deliberately
	// tolerates a non-repository and rewrites a FILE to its parent directory,
	// so `workspace add ws /etc/passwd` resolved to /etc, hashed it into a
	// local/ key and reported "added" -- for a path that can never grow a
	// brain. The rewrite stays (naming a file inside a repo is a reasonable
	// way to name the repo, and git resolves it to the work-tree root the same
	// way a subdirectory is resolved); what it resolves to now has to be a
	// repository, which is the same gate `setup` and `path` already apply.
	if _, ok := gitWorkTreeRoot(ctx, opts.Runner, repoDir); !ok {
		brainCmd := setupCommandPrefix(os.LookupEnv)
		return notARepositoryError(ctx, opts.Runner, repoDir,
			brainCmd+" workspace add registers a repository whose brain -- like every source it is built from -- comes from git.",
			brainCmd+" workspace add "+workspaceName+" "+repoPath,
			brainCmd+" workspace add "+workspaceName+" <path>")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	key := storage.Key
	result, err := workspaceStore(opts.Env).AddRepoLocked(workspaceName, workspace.Repo{RepoKey: key, Name: addOpts.name, LocalPathHint: repoDir})
	if err != nil {
		return err
	}
	// "added" was printed unconditionally, including when the key was ALREADY a
	// member and this call merely rebound it to a different checkout. Two paths
	// reach the same key routinely -- a git worktree of a member (same origin,
	// same key), the same checkout named through a symlink, a second clone --
	// and each of those `workspace add` calls reported a success that implied a
	// new member. The member count did not move, the earlier --name was
	// replaced, and the local path silently moved to the newly named tree, which
	// is the tree every later scan then runs against.
	//
	// addWorkspaceRepoLocked has always returned this; `setup` has always used
	// it. Only the human-facing command threw it away.
	out := cmd.OutOrStdout()
	switch {
	case !result.Already:
		fmt.Fprintf(out, "added %s\n", key)
	case result.PreviousPathHint != "" && result.PreviousPathHint != repoDir:
		fmt.Fprintf(out, "%s is already a member of %s; its path now points at %s (was %s)\n",
			key, workspaceName, repoDir, result.PreviousPathHint)
	default:
		fmt.Fprintf(out, "%s is already a member of %s\n", key, workspaceName)
	}
	return nil
}

func runWorkspaceRefresh(ctx context.Context, cmd *cobra.Command, opts Options, workspaceName string, full bool) error {
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	if full {
		// User-invoked (`entire brain workspace refresh <name> --full`): unlike
		// the watch daemon's tick, this keeps the transcript cache sweep.
		refreshRepo := func(repoDir string) error { return watchDeterministicRefresh(ctx, cmd, opts, repoDir, false) }
		if err := workspaceFullRefresh(ctx, cmd.OutOrStdout(), opts, manifest, refreshRepo); err != nil {
			return err
		}
		// Keep the cross-repo V2 corpus current as part of the full refresh, so
		// users don't need a separate `workspace patterns refresh` build path.
		// Deterministic and token-free (aggregates already-built member corpora).
		if counts, werr := buildWorkspacePatternCorpus(opts.Env, manifest, opts.Now().UTC()); werr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: workspace patterns: %v\n", werr)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "workspace patterns: %d cross-repo pattern(s) from %d/%d member corpora\n",
				counts.Patterns, counts.WithCorpus, counts.Members)
		}
	}
	freshness, err := workspaceFreshness(ctx, opts, manifest)
	if err != nil {
		return err
	}
	if err := workspaceStore(opts.Env).MergeRefreshResult(workspaceName, freshness, opts.Now().UTC()); err != nil {
		return err
	}
	if full {
		if artifact, err := writeWorkspaceGraphSnapshot(ctx, opts, manifest, 50); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: workspace graph: %v\n", err)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "workspace graph: %s\n", artifact)
		}
	}
	for _, repo := range freshness {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", repo.RepoKey, repo.State)
	}
	return nil
}

func runWorkspaceContext(cmd *cobra.Command, opts Options, contextOpts workspaceContextOptions, workspaceName, query string) error {
	if contextOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	var results []workspaceContextResult
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceContextResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
		if err != nil {
			return err
		}
		unlock, err := acquireSemanticIndexLock(brainDir)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		source, err := workspaceSemanticSource(cmd.Context(), opts, repo, brainDir)
		if err != nil {
			unlock()
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		symbols, _, _, err := semanticContextFacts(brainDir, source, query, contextOpts.limit, 0)
		unlock()
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Symbols = nonNil(symbols)
		}
		results = append(results, result)
	}
	if contextOpts.json {
		return writeJSON(cmd, struct {
			Workspace string                   `json:"workspace"`
			Results   []workspaceContextResult `json:"results"`
		}{Workspace: manifest.Name, Results: results})
	}
	for _, result := range results {
		for _, symbol := range result.Symbols {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s:%d-%d\n", result.RepoKey, displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
		}
		if result.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%s error %s\n", result.RepoKey, result.Error)
		}
	}
	return nil
}

func runWorkspaceImpact(cmd *cobra.Command, opts Options, impactOpts workspaceImpactOptions, workspaceName, query string) error {
	if impactOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	if impactOpts.depth <= 0 {
		return errors.New("--depth must be greater than zero")
	}
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	var results []workspaceImpactResult
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceImpactResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
		if err != nil {
			return err
		}
		unlock, err := acquireSemanticIndexLock(brainDir)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		source, err := workspaceSemanticSource(cmd.Context(), opts, repo, brainDir)
		if err != nil {
			unlock()
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		roots, symbols, relations, err := semanticImpactFacts(brainDir, source, query, impactOpts.depth, impactOpts.limit)
		unlock()
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Roots = roots
			result.Symbols = symbols
			result.Relations = relations
		}
		results = append(results, result)
	}
	if impactOpts.json {
		return writeJSON(cmd, struct {
			Workspace string                  `json:"workspace"`
			Results   []workspaceImpactResult `json:"results"`
		}{Workspace: manifest.Name, Results: results})
	}
	for _, result := range results {
		for _, symbol := range result.Symbols {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s:%d-%d\n", result.RepoKey, displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
		}
		if result.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%s error %s\n", result.RepoKey, result.Error)
		}
	}
	return nil
}

func runWorkspaceGraph(cmd *cobra.Command, opts Options, graphOpts workspaceGraphOptions, workspaceName string) error {
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	return runWorkspaceGraphManifest(cmd, opts, graphOpts, manifest, nil)
}

// workspaceSkippedResultError renders a member the caller dropped before
// execution. The wording matches the per-member skips the runners already emit
// for an unsafe pairing, so a reader cannot tell a skip apart by shape and then
// treat one of them as a silent success.
func workspaceSkippedResultError(f workspace.RepoFreshness) string {
	detail := strings.TrimSpace(f.Detail)
	if detail == "" {
		detail = "member could not be verified against a checkout in scope"
	}
	return "skipped (unverified): " + detail
}

func runWorkspaceGraphManifest(cmd *cobra.Command, opts Options, graphOpts workspaceGraphOptions, manifest workspace.Manifest, skipped []workspace.RepoFreshness) error {
	if graphOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	payload, err := buildWorkspaceGraphPayload(cmd.Context(), opts, manifest, graphOpts.limit)
	if err != nil {
		return err
	}
	// A skipped member is still a member: report it, so "six repos in the
	// workspace, five in the results" is never something the reader has to
	// notice for themselves.
	for _, f := range skipped {
		payload.Results = append(payload.Results, workspaceGraphResult{
			RepoKey: f.RepoKey, Name: f.Name, Freshness: f, Error: workspaceSkippedResultError(f),
		})
	}
	sort.Slice(payload.Results, func(i, j int) bool { return payload.Results[i].RepoKey < payload.Results[j].RepoKey })
	if _, err := writeWorkspaceGraphPayload(opts.Env, payload); err != nil {
		return err
	}
	if graphOpts.json {
		return writeJSON(cmd, payload)
	}
	for _, result := range payload.Results {
		if result.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%s error %s\n", result.RepoKey, result.Error)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s files:%d symbols:%d relations:%d\n", result.RepoKey, result.Counts["files"], result.Counts["symbols"], result.Counts["relations"])
	}
	for _, contract := range payload.Contracts {
		fmt.Fprintf(cmd.OutOrStdout(), "contract %s %s repos:%s count:%d\n", contract.Type, contract.Endpoint, strings.Join(contract.Repos, ","), contract.Count)
	}
	return nil
}

func buildWorkspaceGraphPayload(ctx context.Context, opts Options, manifest workspace.Manifest, limit int) (workspaceGraphPayload, error) {
	generation, err := workspaceGraphGeneration(opts.Env, manifest)
	if err != nil {
		return workspaceGraphPayload{}, err
	}
	var results []workspaceGraphResult
	contractIndex := map[string]*workspace.ExternalContractAggregate{}
	var repoIndexes []workspace.RepoGraphIndex
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(ctx, opts, repo)
		result := workspaceGraphResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		repoIndex := workspace.RepoGraphIndex{RepoKey: repo.RepoKey}
		brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
		if err != nil {
			return workspaceGraphPayload{}, err
		}
		unlock, err := acquireSemanticIndexLock(brainDir)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		source, err := workspaceSemanticSource(ctx, opts, repo, brainDir)
		if err != nil {
			unlock()
			result.Error = err.Error()
			results = append(results, result)
			continue
		}

		// Source-derived diagnostics (counts, languages, completeness/trust,
		// language tiers) come from the manifest, not the store — set them now so
		// a repo whose store fails to validate/open below still surfaces them
		// alongside its error, which is exactly the degraded/broken-index case
		// where trust info matters most.
		result.Counts = map[string]int{"files": source.Files, "symbols": source.Symbols, "relations": source.Relations, "externals": source.Externals}
		result.Languages = nonNil(source.Languages)
		result.CompletenessLevel = source.CompletenessLevel
		result.Trust = source.Trust
		result.LanguageTiers = source.LanguageTiers
		storePath, err := validateSemanticDeclaredStore(brainDir, source)
		if err != nil {
			unlock()
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		db, err := openSemanticReadDB(storePath)
		if err != nil {
			unlock()
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result.RelationTypes, err = graphDistinctStrings(db, `SELECT type FROM relations WHERE trim(type) <> '' GROUP BY type ORDER BY type`)
		if err == nil {
			result.Metrics, err = semanticGraphMetrics(db)
		}
		if err == nil {
			err = workspace.CollectExternalContracts(db, repo.RepoKey, contractIndex)
		}
		if err == nil {
			repoIndex.Imports, err = workspace.CollectImportRefs(db, repo.RepoKey)
		}
		if err == nil {
			repoIndex.ExternalSymbols, err = workspace.CollectExternalSymbolRefs(db, repo.RepoKey)
		}
		if err == nil {
			repoIndex.Candidates, err = workspace.CollectTargetCandidates(db, repo.RepoKey)
		}
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
		unlock()
		if err != nil {
			result.Error = err.Error()
		} else {
			repoIndexes = append(repoIndexes, repoIndex)
		}
		results = append(results, result)
	}
	crossEdges := workspace.GraphCrossEdges(contractIndex, limit)
	crossEdges = append(crossEdges, workspace.GraphRouteCallCrossEdges(contractIndex, limit)...)
	crossEdges = append(crossEdges, workspace.GraphGraphQLCrossEdges(contractIndex, limit)...)
	crossEdges = append(crossEdges, workspace.GraphChannelCrossEdges(contractIndex, limit)...)
	crossEdges = append(crossEdges, workspace.GraphResourceCrossEdges(contractIndex, repoIndexes, limit)...)
	crossEdges = append(crossEdges, workspace.GraphImportCrossEdges(repoIndexes, limit)...)
	crossEdges = append(crossEdges, workspace.GraphExternalSymbolCrossEdges(repoIndexes, limit)...)
	workspace.SortGraphCrossEdges(crossEdges)
	if len(crossEdges) > limit {
		crossEdges = crossEdges[:limit]
	}
	if crossEdges == nil {
		crossEdges = []workspace.CrossEdge{}
	}
	return workspaceGraphPayload{
		Workspace:   manifest.Name,
		Generation:  generation,
		GeneratedAt: opts.Now().UTC(),
		Results:     results,
		Contracts:   workspace.GraphContracts(contractIndex, limit),
		CrossEdges:  crossEdges,
	}, nil
}

func writeWorkspaceGraphSnapshot(ctx context.Context, opts Options, manifest workspace.Manifest, limit int) (string, error) {
	payload, err := buildWorkspaceGraphPayload(ctx, opts, manifest, limit)
	if err != nil {
		return "", err
	}
	return writeWorkspaceGraphPayload(opts.Env, payload)
}

func writeWorkspaceGraphPayload(env EntireEnv, payload workspaceGraphPayload) (string, error) {
	dir, err := workspaceStore(env).Dir(payload.Workspace)
	if err != nil {
		return "", err
	}
	rel := filepath.ToSlash(filepath.Join(workspace.DirName, payload.Workspace, workspaceGraphName))
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(dir, workspaceGraphName), data, 0o600); err != nil {
		return "", err
	}
	return rel, nil
}

func runWorkspaceRetrieve(cmd *cobra.Command, opts Options, retrieveOpts workspaceRetrieveOptions, mode retrievalMode, workspaceName, query string) error {
	if retrieveOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	if len(retrieveOpts.concepts) > 0 {
		if err := validateRetrievalLimit(retrieveOpts.limit); err != nil {
			return fmt.Errorf("--%s", err.Error())
		}
	}
	// Validate the source/filter contract once, before the fan-out, so an
	// invalid selector is one structured error rather than N per-repo copies.
	// The complete retrieval-options contract, including the branch filter,
	// reaches every member: an explicit --branch bounds conversation results
	// to that captured branch in each member brain, exactly like the
	// top-level verbs. Members without that branch simply return no
	// conversation hits.
	ropts, err := buildRetrievalOptions(retrieveOpts.source, retrieveOpts.after, retrieveOpts.before, retrieveOpts.session, retrieveOpts.agent, retrieveOpts.branch, retrieveOpts.concepts)
	if err != nil {
		return fmt.Errorf("--%s", err.Error())
	}
	ropts.IncludeAbstract = retrieveOpts.includeAbstract
	if ropts.IncludeAbstract && ropts.Source != retrievalSourceConversation {
		return fmt.Errorf(`--include-abstract requires --source conversation (got %q)`, ropts.Source)
	}
	if mode == modeVector && ropts.Source == retrievalSourceConversation {
		return errConversationVectorUnsupported
	}
	if ropts.hasConversationOnlyFilters() && ropts.Source != retrievalSourceConversation {
		return fmt.Errorf(`after/before/session/agent filters require --source conversation (got %q)`, ropts.Source)
	}
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	privacyPolicies, policyByRepo, err := captureWorkspaceRetrievalPrivacyPolicies(opts.Env, manifest)
	if err != nil {
		return err
	}
	dirs, dirsErr := resolvePluginDirs(opts.Env)
	var results []workspaceRetrieveResult
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceRetrieveResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness, Results: []unifiedResult{}}
		brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
		if err != nil {
			return err
		}
		branch, err := workspaceMemberBranch(brainDir, retrieveOpts.branch)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result.Branch = branch
		var repoDir string
		if dirsErr == nil {
			repoDir, _ = resolveWorkspaceMemberRepoDir(cmd.Context(), opts, dirs.Config, repo)
		}
		found, err := retrieveUnifiedWithOptions(repoDir, brainDir, branch, query, retrieveOpts.limit, mode, ropts)
		if err != nil {
			result.Error = err.Error()
		} else {
			if found != nil {
				if repoDir == "" {
					found = annotateCurrentCodeUnavailable(found)
				}
				result.Results = found
			}
			if ropts.IncludeAbstract {
				previews, truncated, previewErr := abstractPreviewsForResults(brainDir, found)
				if previewErr != nil {
					result.Error = previewErr.Error()
				} else {
					result.AbstractPreviews = previews
					result.AbstractPreviewsTruncated = truncated
				}
			}
		}
		results = append(results, result)
	}
	// The workspace-graph group joins only the default source set. An explicit
	// single-source selection (conversation, fact, history, doc) is a source
	// isolation contract: no fan-out or append stage may add records from any
	// other source.
	if mode != modeVector && ropts.Source == retrievalSourceAll {
		graphResults, err := retrieveWorkspaceGraphCrossEdges(opts.Env, manifest.Name, query, retrieveOpts.limit)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			results = append(results, workspaceRetrieveResult{
				RepoKey: manifest.Name,
				Name:    "workspace graph",
				Error:   err.Error(),
				Results: []unifiedResult{},
			})
		} else if len(graphResults) > 0 {
			results = append(results, workspaceRetrieveResult{
				RepoKey: manifest.Name,
				Name:    "workspace graph",
				Results: graphResults,
			})
		}
	}
	if len(ropts.Concepts) > 0 {
		// Recheck every member after the complete fan-out, immediately before
		// assembling the workspace response. No member's bytes are emitted until
		// all guards succeed.
		for i := range results {
			if results[i].RepoKey == manifest.Name {
				continue
			}
			brainDir, brainErr := brainDirForKey(opts.Env, results[i].RepoKey)
			if brainErr != nil {
				return brainErr
			}
			filtered, policyAfter, privacyErr := revalidateRetrievalResponsePrivacy(brainDir, results[i].Results)
			if privacyErr != nil {
				return privacyErr
			}
			results[i].Results = filtered
			if policyAfter != policyByRepo[results[i].RepoKey] {
				results[i].AbstractPreviews = nil
				results[i].AbstractPreviewsTruncated = true
			}
		}
	}
	if retrieveOpts.json {
		if len(ropts.Concepts) > 0 {
			payload, budgetErr := boundedWorkspaceRetrievalJSONPayload(manifest.Name, query, results)
			if budgetErr != nil {
				return budgetErr
			}
			serialized, marshalErr := jsonOutputBytes(payload)
			if marshalErr != nil {
				return marshalErr
			}
			return writeRetrievalResponseBytes(cmd.OutOrStdout(), serialized, privacyPolicies...)
		}
		qualified := qualifyWorkspaceRetrieveGroups(manifest.Name, results)
		serialized, marshalErr := jsonOutputBytes(struct {
			Workspace string                    `json:"workspace"`
			Query     string                    `json:"query"`
			Results   []workspaceRetrieveResult `json:"results"`
		}{Workspace: manifest.Name, Query: query, Results: qualified})
		if marshalErr != nil {
			return marshalErr
		}
		return writeRetrievalResponseBytes(cmd.OutOrStdout(), serialized, privacyPolicies...)
	}
	if len(ropts.Concepts) > 0 {
		text, budgetErr := boundedWorkspaceRetrievalText(manifest.Name, results)
		if budgetErr != nil {
			return budgetErr
		}
		return writeRetrievalResponseBytes(cmd.OutOrStdout(), text, privacyPolicies...)
	}
	var rendered bytes.Buffer
	out := io.Writer(&rendered)
	for _, result := range results {
		for _, r := range result.Results {
			loc := r.Path
			if r.Line > 0 {
				loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
			}
			ex := base.TruncateString(strings.Join(strings.Fields(r.Text), " "), 200)
			// Member ids are repo-qualified; workspace-level graph ids are already pasteable.
			printedID := result.RepoKey + "/" + r.ID
			if result.RepoKey == manifest.Name && strings.HasPrefix(r.ID, "graph:") {
				printedID = r.ID
			}
			label := r.Source
			if r.VerificationRequired {
				label += " verify"
			}
			fmt.Fprintf(out, "[%s] %s  %s\n    %s\n", label, printedID, loc, ex)
			printRetrievalCaveats(out, r)
		}
		if result.Error != "" {
			fmt.Fprintf(out, "%s error %s\n", result.RepoKey, result.Error)
		}
	}
	return writeRetrievalResponseBytes(cmd.OutOrStdout(), rendered.Bytes(), privacyPolicies...)
}

func captureWorkspaceRetrievalPrivacyPolicies(env EntireEnv, manifest workspace.Manifest) ([]retrievalPrivacyPolicy, map[string]string, error) {
	policies := make([]retrievalPrivacyPolicy, 0, len(manifest.Repos))
	byRepo := make(map[string]string, len(manifest.Repos))
	for _, repo := range manifest.Repos {
		if _, exists := byRepo[repo.RepoKey]; exists {
			continue
		}
		brainDir, err := brainDirForKey(env, repo.RepoKey)
		if err != nil {
			return nil, nil, err
		}
		policy, err := captureRetrievalPrivacyPolicy(brainDir)
		if err != nil {
			return nil, nil, err
		}
		policies = append(policies, policy)
		byRepo[repo.RepoKey] = policy.Identity
	}
	return policies, byRepo, nil
}

// captureWorkspaceDerivedReadPolicies additionally proves every member's
// transcript-derived projections are clean before a workspace aggregate is
// read. Policy capture alone only proves the tombstone file did not change; it
// cannot make a corpus built before an already-present tombstone safe.
func captureWorkspaceDerivedReadPolicies(env EntireEnv, manifest workspace.Manifest) ([]retrievalPrivacyPolicy, error) {
	policies, _, err := captureWorkspaceRetrievalPrivacyPolicies(env, manifest)
	if err != nil {
		return nil, err
	}
	for i := range policies {
		policy := &policies[i]
		policy.RequireDerivedClean = true
		allowed, gateErr := privacyDerivedReadGate(policy.BrainDir)
		if gateErr != nil {
			return nil, gateErr
		}
		if !allowed {
			return nil, fmt.Errorf("%s: workspace member has unverified transcript-derived projections: %s", memoryErrPrivacyDirty, policy.BrainDir)
		}
	}
	// Workspace pattern/theme/family rows live in their own aggregate corpus.
	// Give that Brain the same final-emission proof as every member so a corpus
	// alias or replacement after assembly cannot be collapsed into empty output.
	wsDir, err := workspaceStore(env).Dir(manifest.Name)
	if err != nil {
		return nil, err
	}
	wsPolicy, err := captureRetrievalPrivacyPolicy(wsDir)
	if err != nil {
		return nil, err
	}
	wsPolicy.RequireDerivedClean = true
	if err := requirePrivacyDerivedRead(wsDir); err != nil {
		return nil, err
	}
	policies = append(policies, wsPolicy)
	return policies, nil
}

func runWorkspaceGet(cmd *cobra.Command, opts Options, workspaceName string, qualifiedIDs []string, branchOverride string, jsonOut bool, gopts getOptions) error {
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	derivedAggregate := false
	for _, id := range qualifiedIDs {
		if strings.HasPrefix(id, "pattern:") || strings.HasPrefix(id, "theme:") ||
			strings.Contains(id, "/pattern:") || strings.Contains(id, "/theme:") {
			derivedAggregate = true
			break
		}
	}
	var privacyPolicies []retrievalPrivacyPolicy
	if derivedAggregate {
		privacyPolicies, err = captureWorkspaceDerivedReadPolicies(opts.Env, manifest)
	} else {
		privacyPolicies, _, err = captureWorkspaceRetrievalPrivacyPolicies(opts.Env, manifest)
	}
	if err != nil {
		return err
	}
	if derivedAggregate {
		wsDir, dirErr := workspaceStore(opts.Env).Dir(workspaceName)
		if dirErr != nil {
			return dirErr
		}
		if safetyErr := validatePatternCorpusReadSafety(wsDir); safetyErr != nil {
			return safetyErr
		}
	}
	members := make(map[string]workspace.Repo, len(manifest.Repos))
	for _, repo := range manifest.Repos {
		members[repo.RepoKey] = repo
	}
	// Bare pattern:/theme: ids address the workspace-level aggregate corpus
	// directly (the ids `workspace patterns` prints); repo-qualified ids drill
	// into a member repo. Resolve the workspace-level ones first.
	var wsResult *workspaceGetResult
	var memberIDs []string
	for _, qualified := range qualifiedIDs {
		if strings.HasPrefix(qualified, "pattern:") || strings.HasPrefix(qualified, "theme:") || strings.HasPrefix(qualified, "graph:") {
			if wsResult == nil {
				wsResult = &workspaceGetResult{RepoKey: manifest.Name, Results: []unifiedResult{}, Missing: []string{}}
			}
			if r, ok, err := getWorkspaceLevelRecord(opts.Env, manifest.Name, qualified); err != nil {
				return err
			} else if ok {
				wsResult.Results = append(wsResult.Results, r)
			} else {
				wsResult.Missing = append(wsResult.Missing, qualified)
			}
			continue
		}
		memberIDs = append(memberIDs, qualified)
	}
	// Group member ids by repo key, preserving first-appearance order of repos and
	// the input order of ids within each repo.
	var repoOrder []string
	idsByRepo := make(map[string][]string)
	for _, qualified := range memberIDs {
		repoKey, id, err := splitWorkspaceID(qualified)
		if err != nil {
			return err
		}
		if _, ok := members[repoKey]; !ok {
			return fmt.Errorf("repo %s is not a member of workspace %s", repoKey, manifest.Name)
		}
		if _, seen := idsByRepo[repoKey]; !seen {
			repoOrder = append(repoOrder, repoKey)
		}
		idsByRepo[repoKey] = append(idsByRepo[repoKey], id)
	}
	var results []workspaceGetResult
	if wsResult != nil {
		results = append(results, *wsResult)
	}
	dirs, dirsErr := resolvePluginDirs(opts.Env)
	for _, repoKey := range repoOrder {
		result := workspaceGetResult{RepoKey: repoKey, Results: []unifiedResult{}, Missing: []string{}}
		brainDir, err := brainDirForKey(opts.Env, repoKey)
		if err != nil {
			return err
		}
		branch, err := workspaceMemberBranch(brainDir, branchOverride)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		result.Branch = branch
		var repoDir string
		if dirsErr == nil {
			repoDir, _ = resolveWorkspaceMemberRepoDir(cmd.Context(), opts, dirs.Config, members[repoKey])
		}
		found, missing, err := getUnifiedBatchOptions(repoDir, brainDir, branch, idsByRepo[repoKey], gopts)
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		if found != nil {
			if repoDir == "" {
				found = annotateCurrentCodeUnavailable(found)
			}
			result.Results = found
		}
		// Re-qualify missing ids so the output names the brain they were missing from.
		for _, id := range missing {
			result.Missing = append(result.Missing, repoKey+"/"+id)
		}
		results = append(results, result)
	}
	if jsonOut {
		qualified := make([]workspaceGetResult, len(results))
		for i, result := range results {
			qualified[i] = result
			qualified[i].Results = make([]unifiedResult, len(result.Results))
			for j, item := range result.Results {
				qualified[i].Results[j] = qualifyWorkspaceUnifiedResult(result.RepoKey, manifest.Name, item)
			}
		}
		serialized, marshalErr := jsonOutputBytes(struct {
			Workspace string               `json:"workspace"`
			Results   []workspaceGetResult `json:"results"`
		}{Workspace: manifest.Name, Results: qualified})
		if marshalErr != nil {
			return marshalErr
		}
		return writeRetrievalResponseBytes(cmd.OutOrStdout(), serialized, privacyPolicies...)
	}
	var rendered bytes.Buffer
	out := io.Writer(&rendered)
	for _, result := range results {
		for _, r := range result.Results {
			loc := r.Path
			if r.Line > 0 {
				loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
			}
			label := r.Source
			if r.VerificationRequired {
				label += " verify"
			}
			fmt.Fprintf(out, "[%s] %s/%s  %s\n%s\n", label, result.RepoKey, r.ID, loc, r.Text)
			printRetrievalCaveats(out, r)
			fmt.Fprintln(out)
		}
		for _, id := range result.Missing {
			fmt.Fprintf(out, "not found: %s\n", id)
		}
		if result.Error != "" {
			fmt.Fprintf(out, "%s error %s\n", result.RepoKey, result.Error)
		}
	}
	return writeRetrievalResponseBytes(cmd.OutOrStdout(), rendered.Bytes(), privacyPolicies...)
}

func retrieveWorkspaceGraphCrossEdges(env EntireEnv, workspaceName, query string, limit int) ([]unifiedResult, error) {
	if limit <= 0 {
		limit = 10
	}
	payload, err := loadWorkspaceGraphPayload(env, workspaceName)
	if err != nil {
		return nil, err
	}
	type scored struct {
		result unifiedResult
		score  float64
	}
	scoredEdges := make([]scored, 0, len(payload.CrossEdges))
	for _, edge := range payload.CrossEdges {
		result := workspaceGraphCrossEdgeUnified(edge)
		score := workspace.GraphSearchScore(result.Text, query)
		if score <= 0 {
			continue
		}
		result.Score = score
		scoredEdges = append(scoredEdges, scored{result: result, score: score})
	}
	sort.Slice(scoredEdges, func(i, j int) bool {
		if scoredEdges[i].score != scoredEdges[j].score {
			return scoredEdges[i].score > scoredEdges[j].score
		}
		return scoredEdges[i].result.ID < scoredEdges[j].result.ID
	})
	if len(scoredEdges) > limit {
		scoredEdges = scoredEdges[:limit]
	}
	results := make([]unifiedResult, 0, len(scoredEdges))
	for _, scored := range scoredEdges {
		results = append(results, scored.result)
	}
	return results, nil
}

func getWorkspaceLevelRecord(env EntireEnv, workspaceName, id string) (unifiedResult, bool, error) {
	if strings.HasPrefix(id, "graph:") {
		payload, err := loadWorkspaceGraphPayload(env, workspaceName)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return unifiedResult{}, false, nil
			}
			return unifiedResult{}, false, err
		}
		for _, edge := range payload.CrossEdges {
			result := workspaceGraphCrossEdgeUnified(edge)
			if result.ID == id {
				return result, true, nil
			}
		}
		return unifiedResult{}, false, nil
	}
	return getWorkspaceCorpusRecordChecked(env, workspaceName, id)
}

func loadWorkspaceGraphPayload(env EntireEnv, workspaceName string) (workspaceGraphPayload, error) {
	dir, err := workspaceStore(env).Dir(workspaceName)
	if err != nil {
		return workspaceGraphPayload{}, err
	}
	data, err := base.SafeReadFile(filepath.Join(dir, workspaceGraphName), base.DefaultMaxReadBytes)
	if err != nil {
		return workspaceGraphPayload{}, err
	}
	var payload workspaceGraphPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return workspaceGraphPayload{}, err
	}
	manifest, err := workspaceStore(env).LoadManifest(workspaceName)
	if err != nil {
		return workspaceGraphPayload{}, err
	}
	generation, err := workspaceGraphGeneration(env, manifest)
	if err != nil {
		return workspaceGraphPayload{}, err
	}
	if payload.Generation == "" || payload.Generation != generation {
		return workspaceGraphPayload{Workspace: workspaceName, CrossEdges: []workspace.CrossEdge{}}, nil
	}
	return payload, nil
}

func workspaceGraphCrossEdgeUnified(edge workspace.CrossEdge) unifiedResult {
	fromName := workspace.GraphSymbolDisplay(edge.FromSymbol)
	toName := workspace.GraphSymbolDisplay(edge.ToSymbol)
	text := strings.Join(strings.Fields(fmt.Sprintf(
		"%s %s connects %s/%s to %s/%s through endpoint %s type %s shared_count %d from_path %s to_path %s",
		edge.RelationKind,
		edge.Type,
		edge.FromRepo,
		fromName,
		edge.ToRepo,
		toName,
		edge.Endpoint,
		edge.Type,
		edge.SharedCount,
		edge.FromSymbol.FilePath,
		edge.ToSymbol.FilePath,
	)), " ")
	return unifiedResult{
		Source:  "workspace_graph",
		ID:      workspace.GraphCrossEdgeID(edge),
		Path:    workspace.GraphCrossEdgePath(edge),
		Heading: edge.RelationKind,
		Text:    text,
	}
}

// workspaceMemberBranch picks the facts branch for a member brain. Workspace
// retrieval reads brains, not checkouts, so the branch comes from the member's
// export manifest (its distill default) rather than a live git branch.
func workspaceMemberBranch(brainDir, override string) (string, error) {
	if b := strings.TrimSpace(override); b != "" {
		return b, nil
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return "", err
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil && manifest.Sources.Sessions.DefaultBranch != "" {
		return manifest.Sources.Sessions.DefaultBranch, nil
	}
	return distillDefaultBranch, nil
}

// splitWorkspaceID splits a repo-qualified unified id ("gh/owner/repo/fact:abc")
// into the repo key and the brain-local id. Repo keys never contain ':', so the
// first path segment starting a known source prefix is the boundary.
func splitWorkspaceID(qualified string) (repoKey, id string, err error) {
	// conversation-session: must precede conversation: (prefix containment).
	for _, prefix := range []string{"fact:", "review:", "history:", conversationSessionIDPrefix, "conversation:", "doc:", "pattern:", "theme:"} {
		if strings.HasPrefix(qualified, prefix) {
			return "", "", fmt.Errorf("id %q is missing its repo key (expected <repo-key>/%s…)", qualified, prefix)
		}
		if i := strings.Index(qualified, "/"+prefix); i > 0 {
			return qualified[:i], qualified[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("unrecognized id %q (expected <repo-key>/fact:…, <repo-key>/review:…, <repo-key>/history:…, <repo-key>/conversation:…, <repo-key>/doc:…, <repo-key>/pattern:…, or <repo-key>/theme:…)", qualified)
}

// workspaceFullRefresh fans the free deterministic single-repo refresh over
// every member, behind the same identity gate as the watch loop. An explicit
// refresh always runs (no watch cursor), spends no agent tokens, and one
// member's failure never aborts the others. refreshRepo is injected so the
// fan-out is testable without a real refresh (matching workspaceWatchLoop).
func workspaceFullRefresh(ctx context.Context, out io.Writer, opts Options, manifest workspace.Manifest, refreshRepo func(repoDir string) error) error {
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		return err
	}
	for _, repo := range manifest.Repos {
		repoDir, err := resolveWorkspaceMemberRepoDir(ctx, opts, dirs.Config, repo)
		if err != nil {
			fmt.Fprintf(out, "%s skipped (%v)\n", repo.RepoKey, err)
			continue
		}
		if err := refreshRepo(repoDir); err != nil {
			fmt.Fprintf(out, "%s refresh failed: %v\n", repo.RepoKey, err)
			continue
		}
		fmt.Fprintf(out, "%s refreshed\n", repo.RepoKey)
	}
	return nil
}

// resolveWorkspaceMemberRepoDir resolves a member's local checkout from its
// local_path_hint and verifies the brain<->tree identity: the hint's derived
// repo key must still match the member's. This is the gate every workspace
// fan-out that touches a member's working tree (watch, refresh --full) applies
// before acting on a repo.
func resolveWorkspaceMemberRepoDir(ctx context.Context, opts Options, _ string, repo workspace.Repo) (string, error) {
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, repo.LocalPathHint)
	if err != nil || !local {
		return "", errors.New("no resolvable local path")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return "", fmt.Errorf("unsafe: %w", err)
	}
	hintKey := storage.Key
	if hintKey != repo.RepoKey {
		return "", fmt.Errorf("unsafe: local_path_hint repo_key mismatch: %s", hintKey)
	}
	return repoDir, nil
}

func workspaceFreshness(ctx context.Context, opts Options, manifest workspace.Manifest) ([]workspace.RepoFreshness, error) {
	var result []workspace.RepoFreshness
	for _, repo := range manifest.Repos {
		result = append(result, workspaceRepoFreshnessForRepo(ctx, opts, repo))
	}
	return result, nil
}

func workspaceRepoFreshnessForRepo(ctx context.Context, opts Options, repo workspace.Repo) workspace.RepoFreshness {
	status := workspace.RepoFreshness{RepoKey: repo.RepoKey, Name: repo.Name}
	brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
	if err != nil {
		status.State = "unsafe"
		status.Detail = err.Error()
		return status
	}
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	_, manifestErr := os.Stat(manifestPath)
	manifestMissing := os.IsNotExist(manifestErr)
	if manifestErr != nil && !manifestMissing {
		status.State = "unsafe"
		status.Detail = manifestErr.Error()
		return status
	}
	if repo.LocalPathHint == "" {
		if manifestMissing {
			status.State = "missing-brain"
			return status
		}
		status.State = "unknown"
		status.Detail = "no local_path_hint"
		return status
	}
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, repo.LocalPathHint)
	if err != nil {
		status.State = "unsafe"
		status.Detail = err.Error()
		return status
	}
	if !local {
		status.State = "unknown"
		status.Detail = "local_path_hint unavailable"
		return status
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		// Can't derive the tree's repo_key → can't verify the pairing → unsafe to scan.
		status.State = "unsafe"
		status.PairingUnsafe = true
		status.Detail = err.Error()
		return status
	}
	hintKey := storage.Key
	if workspace.RepoKeyMismatch(repo.RepoKey, hintKey) {
		// The tree the hint points at is a DIFFERENT repo than the registered brain → scanning would
		// manufacture bogus regressions. This is the genuine block case.
		status.State = "unsafe"
		status.PairingUnsafe = true
		status.Detail = "local_path_hint repo_key mismatch: " + hintKey
		return status
	}
	if manifestMissing {
		status.State = "missing-brain"
		return status
	}
	brainManifest, err := loadBrainManifest(brainDir)
	switch {
	case err != nil:
		status.State = "unsafe"
		status.Detail = err.Error()
		return status
	case brainManifest.Sources == nil || brainManifest.Sources.Semantic == nil:
		status.State = "missing-semantic"
		status.ContractState = "missing"
		status.ContractDetail = "semantic contract facts unavailable"
		return status
	}
	report, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		status.State = "unsafe"
		status.Detail = err.Error()
		status.ContractState = "unsafe"
		status.ContractDetail = "semantic freshness unavailable"
		return status
	}
	// State reflects overall freshness for display (refresh/context/impact). The brain<->tree IDENTITY
	// was already verified above, so semantic staleness here does NOT set PairingUnsafe — the scan
	// (which reads raw sessions) is still safe to run; only the index is stale.
	status.State = report.Severity
	if axis, ok := report.Axes["graph"]; ok {
		status.Detail = axis.Detail
	}
	status.ContractState = report.Severity
	switch report.Severity {
	case "ok":
		status.ContractDetail = "semantic contract facts current"
	case "degraded":
		status.ContractDetail = "semantic contract facts degraded"
	default:
		status.ContractDetail = "semantic contract freshness unsafe"
	}
	return status
}

func workspaceSemanticSource(ctx context.Context, opts Options, repo workspace.Repo, brainDir string) (*semanticSourceManifest, error) {
	if repo.LocalPathHint == "" {
		return nil, errors.New("Graph inspection requires a local repository checkout")
	}
	manifest, err := loadGraphManifest(ctx, opts, repo.LocalPathHint, brainDir)
	if err != nil {
		return nil, err
	}
	source := manifest.Sources.Semantic
	if !semanticRepoKeyEqual(source.graphStore.Header.RepoKey, repo.RepoKey) {
		storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repo.LocalPathHint)
		if err != nil {
			return nil, err
		}
		if !semanticRepoKeyEqual(storage.Key, repo.RepoKey) {
			return nil, errors.New("Graph repository identity does not match workspace member")
		}
		if err := validateSemanticProviderRepoKey(ctx, opts.Runner, repo.LocalPathHint, repo.RepoKey, source.graphStore.Header.RepoKey, "", ""); err != nil {
			return nil, err
		}
	}
	return source, nil
}

func writeJSON(cmd *cobra.Command, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	n, err := fmt.Fprintln(cmd.OutOrStdout(), string(data))
	if err == nil && n != len(data)+1 {
		return io.ErrShortWrite
	}
	return err
}

func writeText(cmd *cobra.Command, render func(io.Writer)) error {
	// Stream directly to stdout instead of buffering the whole rendered body:
	// large multi-get/fact output must not be held in memory in full. The
	// sticky writer short-circuits after the first failure and preserves the
	// error, so callers still skip receipt recording when output fails.
	return writeTextToWriter(cmd.OutOrStdout(), render)
}

func writeTextToWriter(writer io.Writer, render func(io.Writer)) error {
	out := &stickyErrorWriter{writer: writer}
	render(out)
	return out.err
}

type stickyErrorWriter struct {
	writer io.Writer
	err    error
}

func (w *stickyErrorWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

// ---- workspace list / remove (ergonomics) ----

func newWorkspaceListCommand(opts Options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List local workspaces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceList(cmd, opts, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runWorkspaceList(cmd *cobra.Command, opts Options, asJSON bool) error {
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		return err
	}
	// Enumerate the new home plus any not-yet-migrated legacy entries
	// (loadWorkspaceManifest migrates each on touch, so listing is also the
	// bulk-migration path for old installs).
	names := map[string]struct{}{}
	for _, root := range []string{
		filepath.Join(dirs.Data, workspace.DirName),
		filepath.Join(dirs.Data, repoStoreDirName, workspace.DirName),
	} {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				names[e.Name()] = struct{}{}
			}
		}
	}
	var summaries []workspace.Summary
	for name := range names {
		manifest, err := workspaceStore(opts.Env).LoadManifest(name)
		if err != nil {
			// Skip directories that are not valid workspaces (unreadable, bad name, schema drift).
			continue
		}
		summaries = append(summaries, workspace.Summary{Name: manifest.Name, Repos: len(manifest.Repos)})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	if asJSON {
		return writeJSON(cmd, struct {
			Workspaces []workspace.Summary `json:"workspaces"`
		}{Workspaces: summaries})
	}
	if len(summaries) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No workspaces.")
		return nil
	}
	for _, s := range summaries {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%d repo(s)\n", s.Name, s.Repos)
	}
	return nil
}

func newWorkspaceRemoveCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <workspace> [repo-key]",
		Short: "Remove a repo from a workspace, or delete the whole workspace",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoKey := ""
			if len(args) == 2 {
				repoKey = args[1]
			}
			return runWorkspaceRemove(cmd, opts, args[0], repoKey)
		},
	}
	return cmd
}

func runWorkspaceRemove(cmd *cobra.Command, opts Options, workspaceName, repoKey string) error {
	lock, err := workspaceStore(opts.Env).AcquireManifestLock(workspaceName)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	if repoKey == "" {
		// workspaceDir validates the name and rejects symlinked paths, so RemoveAll stays inside the store.
		dir, err := workspaceStore(opts.Env).Dir(workspaceName)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, workspace.ManifestName)); err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("workspace not found: %s", workspaceName)
			}
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed workspace %s\n", workspaceName)
		return nil
	}
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	kept := make([]workspace.Repo, 0, len(manifest.Repos))
	removed := false
	for _, r := range manifest.Repos {
		if r.RepoKey == repoKey {
			removed = true
			continue
		}
		kept = append(kept, r)
	}
	if !removed {
		return fmt.Errorf("repo not in workspace %s: %s", workspaceName, repoKey)
	}
	manifest.Repos = kept
	manifest.Freshness = nil
	if err := workspaceStore(opts.Env).WriteManifest(manifest); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s\n", repoKey, workspaceName)
	return nil
}

// ---- workspace regressions / review (cross-repo diff-less review) ----
//
// Unlike context/impact, these tolerate a sessions-only brain: detectRegressionAnomalies reads RAW
// sessions and uses the semantic index only when present, so a missing export manifest / semantic
// index is not fatal — the detector degrades to a raw-session scan. Each repo is scanned
// independently (one repo's missing/locked brain never aborts the others), then aggregated by
// repo_key, mirroring runWorkspaceContext.

func newWorkspaceRegressionsCommand(opts Options) *cobra.Command {
	ro := regressionDetectorOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "regressions <workspace> <query>",
		Short: "Flag suspected regressions across workspace repos (brain memory vs each current tree)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRegressions(cmd, opts, ro, args[0], args[1])
		},
	}
	cmd.Flags().IntVar(&ro.limit, "limit", 20, "Maximum suspected regressions per repo")
	cmd.Flags().BoolVar(&ro.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&ro.includeDeletions, "include-deletions", false, "Also flag deleted assignments (higher recall, noisier)")
	cmd.Flags().BoolVar(&ro.locationOnly, "location-only", false, "Emit only the suspected file:line, not the expected/current values")
	return cmd
}

func newWorkspaceReviewCommand(opts Options) *cobra.Command {
	ro := regressionDetectorOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "review <workspace> <query>",
		Short: "Diff-less review across workspace repos: suspected regressions as severity-ranked findings",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceReview(cmd, opts, ro, args[0], args[1])
		},
	}
	cmd.Flags().IntVar(&ro.limit, "limit", 20, "Maximum findings per repo")
	cmd.Flags().BoolVar(&ro.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&ro.includeDeletions, "include-deletions", false, "Also flag deleted assignments (lower confidence, noisier)")
	cmd.Flags().BoolVar(&ro.locationOnly, "location-only", false, "Emit only the suspected file:line, not the expected/current values")
	return cmd
}

// workspaceFreshnessWarning returns a loud, human-facing annotation for a repo that is scannable but
// not "ok" (so the result is qualified rather than silently trusted), or "" for ok. Surfaced in both
// JSON (prepended to result.Warnings) and text output. "unsafe" never reaches here — it blocks the
// scan in workspaceFreshnessBlocksScan and is reported as a skip instead.
func workspaceFreshnessWarning(f workspace.RepoFreshness) string {
	switch f.State {
	case "unsafe":
		// Reached only for a SCANNED repo (PairingUnsafe blocks before this), so "unsafe" here means
		// a stale/broken semantic index over a still-valid pairing — the scan ran on raw sessions.
		return "code index unsafe/stale — scanned raw sessions only; verify findings"
	case "degraded":
		return "freshness degraded — brain/index may be stale; verify findings"
	case "missing-brain":
		// No export manifest at the brain root. The detector still scans raw sessions (the real
		// mirrored-sessions brains are exactly this shape), so this qualifies rather than blocks.
		return "no export manifest — scanned raw sessions only (run `" + brainInvocation(os.LookupEnv, "refresh") + "` for a full brain)"
	case "missing-semantic":
		return "code index missing — scanned raw sessions only"
	case "unknown":
		detail := f.Detail
		if detail == "" {
			detail = "could not resolve repo state"
		}
		return "freshness unknown — " + detail
	default:
		return ""
	}
}

// workspaceRepoBlockNeedsHeader decides whether a repo's block in the text
// regression listing gets its "<repo_key> [<state>]" header.
//
// EVERY reason a repo emits a line has to appear here, and the warnings term
// was the one that did not. A repo that was scanned cleanly but carried a
// caveat -- "no code identifiers found in query", "semantic index missing --
// scanned raw sessions only" -- printed that caveat with no header above it. In
// a multi-repo listing an unattributed line is not merely untidy: it reads as
// belonging to the next repo printed, so a caveat about repo A appeared under
// repo B's heading, qualifying the wrong repo's findings.
//
// runWorkspaceReviewManifest's equivalent condition has always included the
// warnings term; this is the sibling renderer catching up.
func workspaceRepoBlockNeedsHeader(state string, anomalies, warnings int, errText string) bool {
	return anomalies > 0 || warnings > 0 || errText != "" || state != "ok"
}

// scanWorkspaceRepoRegressions runs the regression detector for a single workspace repo and returns
// the resolved working-tree path, anomalies, THE NUMBER OF FILES ACTUALLY COMPARED, warnings, and a
// per-repo error string (empty on success).
//
// The file count is not decoration. detectRegressionAnomalies returns zero for every path on which
// it read nothing -- no code identifiers in the query, no precise assertions in history, no current
// files to verify against -- and this function used to discard it with `_`. A caller with no count
// cannot tell "compared the tree and found nothing wrong" from "never opened a file", and the
// review summary went on to assert the first. See workspaceReviewSummary.
func scanWorkspaceRepoRegressions(ctx context.Context, opts Options, repo workspace.Repo, ro regressionDetectorOptions, query string) (string, []regressionAnomaly, int, []string, bool, string) {
	brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
	if err != nil {
		return "", nil, 0, nil, false, err.Error()
	}
	if repo.LocalPathHint == "" {
		return "", nil, 0, nil, false, "no local_path_hint (re-add the repo to record its working tree)"
	}
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, repo.LocalPathHint)
	if err != nil {
		return "", nil, 0, nil, false, err.Error()
	}
	if !local {
		return "", nil, 0, nil, false, "local_path_hint unavailable"
	}
	// Semantic source is optional: a sessions-only brain (no export manifest) still scans.
	var semSource *semanticSourceManifest
	if source, srcErr := workspaceSemanticSource(ctx, opts, repo, brainDir); srcErr == nil {
		semSource = source
	}
	// Capped, so a caller writing a summary sentence can qualify it. The
	// uncapped wrapper discarded this flag, which let the workspace surfaces
	// print an unqualified clean bill of health over a scan that stopped at a
	// cap -- the exact bug class the caps were added to eliminate.
	anomalies, scanned, warnings, caps := detectRegressionAnomaliesCapped(brainDir, repoDir, semSource, query, ro.limit, ro.includeDeletions)
	if ro.locationOnly {
		for i := range anomalies {
			anomalies[i].Expected = ""
			anomalies[i].Current = ""
			anomalies[i].Reason = "suspected regression site (location only)"
			if anomalies[i].Symbol != "" || len(anomalies[i].RelatedLocations) > 0 {
				anomalies[i].Reason = "suspected regression site (location only); inspect the enclosing symbol and related same-file locations"
			}
		}
	}
	return repoDir, anomalies, scanned, warnings, caps.Truncated(), ""
}

func runWorkspaceRegressions(cmd *cobra.Command, opts Options, ro regressionDetectorOptions, workspaceName, query string) error {
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	return runWorkspaceRegressionsManifest(cmd, opts, ro, manifest, query, nil)
}

func runWorkspaceRegressionsManifest(cmd *cobra.Command, opts Options, ro regressionDetectorOptions, manifest workspace.Manifest, query string, skipped []workspace.RepoFreshness) error {
	if ro.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	privacyPolicies, _, err := captureWorkspaceRetrievalPrivacyPolicies(opts.Env, manifest)
	if err != nil {
		return err
	}
	var results []workspaceRegressionResult
	for _, f := range skipped {
		results = append(results, workspaceRegressionResult{
			RepoKey: f.RepoKey, Name: f.Name, Freshness: f, Error: workspaceSkippedResultError(f),
		})
	}
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceRegressionResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		if workspace.FreshnessBlocksScan(freshness) {
			result.Error = "skipped (unsafe): " + freshness.Detail
		} else {
			repoPath, anomalies, scanned, warnings, truncated, scanErr := scanWorkspaceRepoRegressions(cmd.Context(), opts, repo, ro, query)
			result.RepoPath = repoPath
			result.Anomalies = anomalies
			result.FilesScanned = scanned
			result.HistoryTruncated = truncated
			result.Checked = scanErr == "" && scanned > 0
			result.Warnings = warnings
			result.Error = scanErr
			if warn := workspaceFreshnessWarning(freshness); warn != "" {
				result.Warnings = append([]string{warn}, result.Warnings...)
			}
		}
		results = append(results, result)
	}
	render := func() error {
		if ro.json {
			return writeJSON(cmd, struct {
				Workspace     string                      `json:"workspace"`
				SchemaVersion int                         `json:"schema_version"`
				Results       []workspaceRegressionResult `json:"results"`
			}{Workspace: manifest.Name, SchemaVersion: reviewReportSchemaVersion, Results: results})
		}
		out := cmd.OutOrStdout()
		total := 0
		scanned := 0
		for _, result := range results {
			if result.Checked {
				scanned++
			}
			// Surface per-repo freshness so a stale/degraded pairing is never silently trusted.
			state := result.Freshness.State
			if state == "" {
				state = "unknown"
			}
			if workspaceRepoBlockNeedsHeader(state, len(result.Anomalies), len(result.Warnings), result.Error) {
				fmt.Fprintf(out, "%s [%s]\n", result.RepoKey, state)
			}
			if result.Error != "" {
				fmt.Fprintf(out, "  %s\n", result.Error)
			}
			for _, w := range result.Warnings {
				fmt.Fprintf(out, "  warning: %s\n", w)
			}
			for _, a := range result.Anomalies {
				total++
				fmt.Fprintf(out, "  %s:%d [%s, conf %.2f] %s\n", a.File, a.Line, a.Kind, a.Confidence, a.Identifier)
				if a.Expected != "" {
					fmt.Fprintf(out, "    expected: %s\n    current:  %s\n", a.Expected, a.Current)
				}
			}
		}
		if total == 0 {
			// COVERAGE, not membership. The count used to be len(results), so a
			// workspace whose members were skipped unsafe or never resolved still
			// reported "No suspected regressions across 6 repo(s)" -- a clean bill of
			// health over repos nothing ever looked at.
			//
			// COMPLETENESS is the same defect one layer in, and it is why the
			// truncated count is here: a repo whose history scan stopped at a
			// cap WAS scanned, so it counts toward coverage, but it was not
			// scanned to the end. Without this the headline reads as clean over
			// a scan that never finished.
			truncated := 0
			for _, result := range results {
				if result.HistoryTruncated {
					truncated++
				}
			}
			switch {
			case scanned == 0:
				fmt.Fprintf(out, "INCONCLUSIVE: no file in any of %d repo(s) in %q was compared — this is not a clean result. See the warnings above.\n",
					len(results), manifest.Name)
			case truncated > 0:
				fmt.Fprintf(out, "PARTIAL for %q: nothing flagged across %d of %d repo(s), but %d repo(s) stopped at a history cap before the end of the brain, so this is not a clean result. See the per-repo lines above.\n",
					manifest.Name, scanned, len(results), truncated)
			case scanned == len(results):
				fmt.Fprintf(out, "No suspected regressions across %d repo(s) in %q.\n", scanned, manifest.Name)
			default:
				fmt.Fprintf(out, "No suspected regressions across %d of %d repo(s) in %q; %d not scanned (see the per-repo lines above).\n",
					scanned, len(results), manifest.Name, len(results)-scanned)
			}
		}
		return nil
	}
	return bufferRetrievalCommandOutput(cmd, privacyPolicies, render)
}

// workspaceReviewSummary is the per-repo verdict for a repo that produced no
// findings, and it exists because "no findings" has two completely different
// causes that were being reported with one sentence.
//
// The sentence was:
//
//	no suspected regressions (current tree matches the brain's memory).
//
// printed from a default: branch that never asked whether anything had been
// compared -- while the warnings that say nothing was ("no code identifiers
// found in query", "history holds no precise code assertions for these
// identifiers", "no current files to verify against") were being assembled
// three lines away. detectRegressionAnomalies returns the file count for
// exactly this purpose and the caller discarded it. So a brain that had never
// been built, and a query naming nothing the detector could anchor, both
// reported a clean tree -- an assertion about a comparison that never ran.
//
// An agent or script reads `summary`. INCONCLUSIVE is the honest verdict when
// the scan read zero files, and `checked`/`files_scanned` carry the same fact
// in machine-readable form so nothing has to parse this prose at all.
func workspaceReviewSummary(checked bool, filesScanned int, historyTruncated bool) string {
	if checked && filesScanned > 0 {
		if historyTruncated {
			// A capped scan has not seen the whole brain, so no findings is not
			// a clean result. This surface could not say so while the caps were
			// discarded by the uncapped detector wrapper.
			return fmt.Sprintf("PARTIAL: nothing flagged in what was compared (%d file(s)), but the history scan stopped at a cap before the end of the brain, so this is not a clean result. See the warnings.", filesScanned)
		}
		return fmt.Sprintf("no suspected regressions (compared %d file(s) against the brain's memory).", filesScanned)
	}
	return "INCONCLUSIVE: nothing was compared — no files were read, so this is not a clean result. See the warnings."
}

func runWorkspaceReview(cmd *cobra.Command, opts Options, ro regressionDetectorOptions, workspaceName, query string) error {
	manifest, err := workspaceStore(opts.Env).LoadManifest(workspaceName)
	if err != nil {
		return err
	}
	return runWorkspaceReviewManifest(cmd, opts, ro, manifest, query, nil)
}

func runWorkspaceReviewManifest(cmd *cobra.Command, opts Options, ro regressionDetectorOptions, manifest workspace.Manifest, query string, skipped []workspace.RepoFreshness) error {
	if ro.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	privacyPolicies, _, err := captureWorkspaceRetrievalPrivacyPolicies(opts.Env, manifest)
	if err != nil {
		return err
	}
	var results []workspaceReviewResult
	reposWithFindings := 0
	totalFindings := 0
	for _, f := range skipped {
		results = append(results, workspaceReviewResult{
			RepoKey: f.RepoKey, Name: f.Name, Freshness: f,
			Error:    workspaceSkippedResultError(f),
			Summary:  "skipped: this member could not be verified against a checkout in scope.",
			Findings: []reviewFinding{},
		})
	}
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceReviewResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		if workspace.FreshnessBlocksScan(freshness) {
			result.Error = "skipped (unsafe): " + freshness.Detail
			result.Summary = "skipped: brain and working tree cannot be trusted to match."
			results = append(results, result)
			continue
		}
		repoPath, anomalies, scanned, warnings, truncated, scanErr := scanWorkspaceRepoRegressions(cmd.Context(), opts, repo, ro, query)
		result.RepoPath = repoPath
		result.FilesScanned = scanned
		result.HistoryTruncated = truncated
		result.Checked = scanErr == "" && scanned > 0
		result.Warnings = warnings
		result.Error = scanErr
		if warn := workspaceFreshnessWarning(freshness); warn != "" {
			result.Warnings = append([]string{warn}, result.Warnings...)
		}
		result.Findings = []reviewFinding{} // [] not null in JSON, even when empty
		for _, a := range anomalies {
			result.Findings = append(result.Findings, anomalyToReviewFinding(a))
		}
		switch {
		case scanErr != "":
			// A scan error means the repo wasn't reviewed — don't claim it matches the brain's memory.
			result.Summary = "not reviewed: " + scanErr
		case len(result.Findings) > 0:
			reposWithFindings++
			totalFindings += len(result.Findings)
			result.Summary = fmt.Sprintf("%d suspected regression(s) — verify each before acting.", len(result.Findings))
		default:
			result.Summary = workspaceReviewSummary(result.Checked, result.FilesScanned, result.HistoryTruncated)
		}
		if result.HistoryTruncated && len(result.Findings) > 0 {
			result.Summary = reviewSummary(len(result.Findings), scanned, true)
		}
		results = append(results, result)
	}
	// "%d/%d repo(s)" is a statement about REVIEWED repos, so the denominator
	// has to be the reviewed ones. It used to be len(results), which counts the
	// members that were skipped unsafe or never resolved -- so a workspace with
	// two dead members reported "0 suspected regression(s) across 0/6 repo(s)",
	// a headline that reads as six repos inspected and all six clean. The
	// unreviewed ones are now named in their own clause instead of padding the
	// total.
	// REVIEWED means COMPARED. Counting "no error" as reviewed re-introduced the
	// same lie one level up: a member whose scan read zero files raised no error,
	// so it padded the denominator of a headline that reads as coverage.
	reviewed := 0
	for _, result := range results {
		if result.Checked {
			reviewed++
		}
	}
	summary := fmt.Sprintf("Cross-repo diff-less review: %d suspected regression(s) across %d/%d repo(s).", totalFindings, reposWithFindings, reviewed)
	switch {
	case reviewed == 0:
		summary = fmt.Sprintf("INCONCLUSIVE: no file in any of %d member(s) was compared — this is not a clean result. See each repo's warnings.", len(results))
	case reviewed != len(results):
		summary += fmt.Sprintf(" %d of %d member(s) were not reviewed.", len(results)-reviewed, len(results))
	}
	partialMembers := 0
	for _, result := range results {
		if result.HistoryTruncated {
			partialMembers++
		}
	}
	if partialMembers > 0 {
		summary = fmt.Sprintf("PARTIAL: %s History scans stopped at a cap in %d member(s); this is not a clean result.", summary, partialMembers)
	}
	render := func() error {
		if ro.json {
			return writeJSON(cmd, struct {
				Workspace     string                  `json:"workspace"`
				SchemaVersion int                     `json:"schema_version"`
				Mode          string                  `json:"mode"`
				Summary       string                  `json:"summary"`
				Results       []workspaceReviewResult `json:"results"`
			}{Workspace: manifest.Name, SchemaVersion: reviewReportSchemaVersion, Mode: "diff-less (brain memory vs current tree)", Summary: summary, Results: results})
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, summary)
		for _, result := range results {
			// Surface per-repo freshness so a stale/degraded or skipped pairing is visible, not silent.
			state := result.Freshness.State
			if state == "" {
				state = "unknown"
			}
			if len(result.Findings) == 0 && result.Error == "" && len(result.Warnings) == 0 && state == "ok" {
				continue
			}
			label := result.RepoKey
			if result.Name != "" {
				label = result.Name + " (" + result.RepoKey + ")"
			}
			fmt.Fprintf(out, "\n%s [%s]\n", label, state)
			if result.Error != "" {
				fmt.Fprintf(out, "  error: %s\n", result.Error)
				continue
			}
			for _, w := range result.Warnings {
				fmt.Fprintf(out, "  warning: %s\n", w)
			}
			for _, f := range result.Findings {
				fmt.Fprintf(out, "  [%s] %s\n    %s:%d\n    %s\n    evidence: %s\n",
					strings.ToUpper(f.Severity), f.Title, f.File, f.Line, f.Detail, f.Evidence)
			}
		}
		return nil
	}
	return bufferRetrievalCommandOutput(cmd, privacyPolicies, render)
}

// workspaceGraphGeneration binds cached edges to membership and member source
// generations. A builder that finishes after either changes writes a stale token,
// which readers reject even if that builder overwrites a newer cache file.
func workspaceGraphGeneration(env EntireEnv, manifest workspace.Manifest) (string, error) {
	type memberSource struct {
		Repo        workspace.Repo  `json:"repo"`
		Manifest    *exportManifest `json:"manifest"`
		Unavailable bool            `json:"unavailable,omitempty"`
		GraphDigest string          `json:"graph_digest,omitempty"`
	}
	members := make([]memberSource, 0, len(manifest.Repos))
	for _, repo := range manifest.Repos {
		brainDir, err := brainDirForKey(env, repo.RepoKey)
		if err != nil {
			return "", err
		}
		source, err := loadBrainManifest(brainDir)
		// The builder reports source failures per member and still serves healthy
		// repositories. Bind that degraded state too, so recovery invalidates it.
		member := memberSource{Repo: repo, Manifest: source, Unavailable: err != nil}
		// Graph can be rebuilt independently of Brain. Bind derived cross-repo
		// edges to the current provider entry, including its missing state.
		if repo.LocalPathHint != "" {
			d, graphErr := fetchGraphStore(context.Background(), Options{Runner: ExecRunner{}}, repo.LocalPathHint, "", "full")
			if graphErr == nil {
				member.GraphDigest = d.EntryDigest
			} else {
				// The reason matters, not just the fact of failure: collapsing
				// every cause to the same "unavailable" sentinel meant a member
				// that went from "Graph absent" to "Graph present but schema
				// mismatched" (or vice versa) produced an unchanged digest, so
				// the generation never invalidated the cross-repo cache.
				member.GraphDigest = "unavailable: " + graphErr.Error()
			}
		}
		members = append(members, member)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Repo.RepoKey < members[j].Repo.RepoKey })
	data, err := json.Marshal(members)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
