package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	workspaceDirName       = "workspaces"
	workspaceManifestName  = "workspace.json"
	workspaceReadmeName    = "README.md"
	workspaceGraphName     = "graph.json"
	workspaceSchemaVersion = 1
)

var workspaceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

type workspaceManifest struct {
	SchemaVersion int                      `json:"schema_version"`
	Name          string                   `json:"name"`
	Repos         []workspaceRepo          `json:"repos"`
	RefreshedAt   time.Time                `json:"refreshed_at,omitempty"`
	Freshness     []workspaceRepoFreshness `json:"freshness,omitempty"`
}

type workspaceRepo struct {
	RepoKey       string `json:"repo_key"`
	Name          string `json:"name,omitempty"`
	LocalPathHint string `json:"local_path_hint,omitempty"`
}

type workspaceRepoFreshness struct {
	RepoKey string `json:"repo_key"`
	Name    string `json:"name,omitempty"`
	State   string `json:"state"`
	// PairingUnsafe is the scan-block signal: true only when the brain<->tree IDENTITY can't be
	// trusted (the local_path_hint's repo_key no longer matches, or it could not be derived), so a
	// regression scan would compare one repo's brain against an unrelated tree. This is distinct from
	// State == "unsafe", which also covers a stale/broken semantic index (a valid pairing the scan
	// can still run against raw sessions). See workspaceFreshnessBlocksScan.
	PairingUnsafe  bool   `json:"pairing_unsafe,omitempty"`
	Detail         string `json:"detail,omitempty"`
	ContractState  string `json:"contract_state,omitempty"`
	ContractDetail string `json:"contract_detail,omitempty"`
}

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
	source  string
	after   string
	before  string
	session string
	agent   string
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
	RepoKey   string                 `json:"repo_key"`
	Name      string                 `json:"name,omitempty"`
	Freshness workspaceRepoFreshness `json:"freshness"`
	Symbols   []semanticRecord       `json:"symbols"`
	Error     string                 `json:"error,omitempty"`
}

type workspaceRetrieveResult struct {
	RepoKey   string                 `json:"repo_key"`
	Name      string                 `json:"name,omitempty"`
	Branch    string                 `json:"branch,omitempty"`
	Freshness workspaceRepoFreshness `json:"freshness"`
	Results   []unifiedResult        `json:"results"`
	Error     string                 `json:"error,omitempty"`
}

type workspaceGetResult struct {
	RepoKey string          `json:"repo_key"`
	Branch  string          `json:"branch,omitempty"`
	Results []unifiedResult `json:"results"`
	Missing []string        `json:"missing"`
	Error   string          `json:"error,omitempty"`
}

type workspaceImpactResult struct {
	RepoKey   string                 `json:"repo_key"`
	Name      string                 `json:"name,omitempty"`
	Freshness workspaceRepoFreshness `json:"freshness"`
	Roots     []semanticRecord       `json:"roots"`
	Symbols   []semanticRecord       `json:"symbols"`
	Relations []semanticRecord       `json:"relations"`
	Error     string                 `json:"error,omitempty"`
}

type workspaceGraphResult struct {
	RepoKey   string                 `json:"repo_key"`
	Name      string                 `json:"name,omitempty"`
	Freshness workspaceRepoFreshness `json:"freshness"`
	Counts    map[string]int         `json:"counts,omitempty"`
	Languages []string               `json:"languages,omitempty"`
	// Retrieval-trust diagnostics carried from each repo's semantic source
	// manifest so an agent can tell a trusted index from a degraded one per repo.
	CompletenessLevel string            `json:"completeness_level,omitempty"`
	Trust             string            `json:"trust,omitempty"`
	LanguageTiers     map[string]string `json:"language_tiers,omitempty"`
	RelationTypes     []string          `json:"relation_types,omitempty"`
	Metrics           graphMetrics      `json:"metrics,omitempty"`
	Error             string            `json:"error,omitempty"`
}

type workspaceGraphContract struct {
	Endpoint string   `json:"endpoint"`
	Type     string   `json:"type"`
	Repos    []string `json:"repos"`
	Count    int      `json:"count"`
}

type workspaceGraphSymbolRef struct {
	RepoKey       string `json:"repo_key"`
	ID            string `json:"id"`
	Kind          string `json:"kind,omitempty"`
	Name          string `json:"name,omitempty"`
	QualifiedName string `json:"qualified_name,omitempty"`
	FilePath      string `json:"file_path,omitempty"`
	Direction     string `json:"direction,omitempty"`
	Count         int    `json:"count,omitempty"`
}

type workspaceGraphCrossEdge struct {
	Endpoint     string                  `json:"endpoint"`
	Type         string                  `json:"type"`
	FromRepo     string                  `json:"from_repo"`
	ToRepo       string                  `json:"to_repo"`
	FromSymbol   workspaceGraphSymbolRef `json:"from_symbol"`
	ToSymbol     workspaceGraphSymbolRef `json:"to_symbol"`
	SharedCount  int                     `json:"shared_count"`
	RelationKind string                  `json:"relation_kind"`
}

type workspaceExternalContractAggregate struct {
	Endpoint     string
	Type         string
	RepoCounts   map[string]int
	Participants []workspaceGraphSymbolRef
}

type workspaceRepoGraphIndex struct {
	RepoKey         string
	Imports         []workspaceGraphImportRef
	ExternalSymbols []workspaceGraphExternalSymbolRef
	Candidates      []workspaceGraphSymbolRef
}

type workspaceGraphImportRef struct {
	Spec   string
	Source workspaceGraphSymbolRef
	Count  int
}

type workspaceGraphExternalSymbolRef struct {
	Spec   string
	Type   string
	Source workspaceGraphSymbolRef
	Count  int
}

type workspaceGraphPayload struct {
	Workspace   string                    `json:"workspace"`
	GeneratedAt time.Time                 `json:"generated_at"`
	Results     []workspaceGraphResult    `json:"results"`
	Contracts   []workspaceGraphContract  `json:"contracts"`
	CrossEdges  []workspaceGraphCrossEdge `json:"cross_edges"`
}

type workspaceSummary struct {
	Name  string `json:"name"`
	Repos int    `json:"repos"`
}

type workspaceRegressionResult struct {
	RepoKey   string                 `json:"repo_key"`
	Name      string                 `json:"name,omitempty"`
	Freshness workspaceRepoFreshness `json:"freshness"`
	RepoPath  string                 `json:"repo_path,omitempty"`
	Anomalies []regressionAnomaly    `json:"anomalies"`
	Warnings  []string               `json:"warnings,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

type workspaceReviewResult struct {
	RepoKey   string                 `json:"repo_key"`
	Name      string                 `json:"name,omitempty"`
	Freshness workspaceRepoFreshness `json:"freshness"`
	RepoPath  string                 `json:"repo_path,omitempty"`
	Summary   string                 `json:"summary"`
	Findings  []reviewFinding        `json:"findings"`
	Warnings  []string               `json:"warnings,omitempty"`
	Error     string                 `json:"error,omitempty"`
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
			dir, err := workspaceDir(opts.Env, args[0])
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(dir, workspaceManifestName)); err == nil {
				return fmt.Errorf("workspace already exists: %s", args[0])
			} else if err != nil && !os.IsNotExist(err) {
				return err
			}
			manifest := workspaceManifest{SchemaVersion: workspaceSchemaVersion, Name: args[0]}
			if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
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
		Use:   "watch <workspace>",
		Short: "Keep every repo in a workspace fresh automatically (deterministic refresh is free; agent steps gated)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceWatch(cmd.Context(), cmd, opts, w, args[0])
		},
	}
	bindWatchFlags(cmd, &w)
	return cmd
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
	out := cmd.OutOrStdout()
	// Stop cleanly between ticks on SIGINT/SIGTERM (same as single-repo watch).
	ctx, stop := watchSignalContext(ctx)
	defer stop()
	repoTick := func(repoDir string, agentCalls *int) {
		storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
		if err != nil {
			fmt.Fprintf(out, "  skipped: %v\n", err)
			return
		}
		cursorPath := filepath.Join(filepath.Dir(storage.HeadPath), "watch.json")
		watchTick(ctx, out, w, cursorPath, watchStepsForRepo(cmd, opts, w, repoDir, now), agentCalls)
	}
	return workspaceWatchLoop(ctx, out, opts, w, workspaceName, repoTick)
}

// workspaceWatchLoop iterates the workspace members, resolving each to a local repo and handing it to
// repoTick with a single shared agentCalls counter (so --budget caps total token spend across the whole
// workspace, not per-repo). repoTick is injected so the fan-out is testable without a real refresh.
func workspaceWatchLoop(ctx context.Context, out io.Writer, opts Options, w watchCommandOptions, workspaceName string, repoTick func(repoDir string, agentCalls *int)) error {
	if w.interval <= 0 {
		w.interval = 5 * time.Minute
	}
	agentCalls := 0
	for {
		if ctx.Err() != nil {
			fmt.Fprintln(out, "[watch] stopping")
			return nil
		}
		manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "[watch] workspace %s — %d repos, distill=%v (budget=%d)\n", manifest.Name, len(manifest.Repos), w.distill, w.budget)
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
			repoTick(repoDir, &agentCalls)
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
		Short: "Build semantic context for a symbol or query across workspace repos",
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
		Short: "Run semantic impact across workspace repos",
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
		Short: "Summarize semantic graph schemas and cross-repo contracts in a workspace",
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
	return newWorkspaceRetrieveCommand(opts, "vsearch", modeVector, "Vector (semantic) search across every workspace repo's brain (facts, docs)")
}

func newWorkspaceQueryCommand(opts Options) *cobra.Command {
	return newWorkspaceRetrieveCommand(opts, "query", modeHybrid, "Hybrid (lexical+vector, RRF) search across every workspace repo's brain")
}

// newWorkspaceRetrieveCommand mirrors the top-level search/vsearch/query verbs
// (retrieve_cmd.go) as a per-member fan-out: each repo's brain is ranked
// independently and results stay grouped by repo — scores from different brains'
// indexes are not comparable, so there is no cross-repo fusion.
func newWorkspaceRetrieveCommand(opts Options, use string, mode retrievalMode, short string) *cobra.Command {
	retrieveOpts := workspaceRetrieveOptions{limit: 10}
	cmd := &cobra.Command{
		Use:   use + " <workspace> <query>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRetrieve(cmd, opts, retrieveOpts, mode, args[0], args[1])
		},
	}
	cmd.Flags().IntVar(&retrieveOpts.limit, "limit", 10, "Maximum results per repo")
	cmd.Flags().StringVar(&retrieveOpts.branch, "branch", "", "Branch for facts in every repo (default: each repo's distill default)")
	cmd.Flags().BoolVar(&retrieveOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&retrieveOpts.source, "source", "", "Restrict retrieval to one source per repo: all, fact, history, conversation, or doc (conversation is experimental opt-in)")
	cmd.Flags().StringVar(&retrieveOpts.after, "after", "", "Conversation source only: sessions at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&retrieveOpts.before, "before", "", "Conversation source only: sessions before this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&retrieveOpts.session, "session", "", "Conversation source only: exchanges from this session id")
	cmd.Flags().StringVar(&retrieveOpts.agent, "agent", "", "Conversation source only: exchanges captured by this agent/harness")
	return cmd
}

func newWorkspaceGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var branch string
	cmd := &cobra.Command{
		Use:   "get <workspace> <repo-key/id>...",
		Short: "Fetch items in full by repo-qualified id (e.g. gh/owner/repo/fact:… or gh/owner/repo/conversation:…, as printed by workspace search)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceGet(cmd, opts, args[0], args[1:], branch, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts in every repo (default: each repo's distill default)")
	return cmd
}

func runWorkspaceAdd(ctx context.Context, cmd *cobra.Command, opts Options, addOpts workspaceAddOptions, workspaceName, repoPath string) error {
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, repoPath)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("workspace repo must be an existing local path: %s", repoPath)
	}
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		return err
	}
	key, err := repoStorageKey(ctx, opts.Runner, dirs.Config, repoDir)
	if err != nil {
		return err
	}
	repo := workspaceRepo{RepoKey: key, Name: addOpts.name, LocalPathHint: repoDir}
	replaced := false
	for i := range manifest.Repos {
		if manifest.Repos[i].RepoKey == key {
			manifest.Repos[i] = repo
			replaced = true
			break
		}
	}
	if !replaced {
		manifest.Repos = append(manifest.Repos, repo)
	}
	sortWorkspaceRepos(manifest.Repos)
	if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "added %s\n", key)
	return nil
}

func runWorkspaceRefresh(ctx context.Context, cmd *cobra.Command, opts Options, workspaceName string, full bool) error {
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	if full {
		refreshRepo := func(repoDir string) error { return watchDeterministicRefresh(ctx, cmd, opts, repoDir) }
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
	manifest.Freshness = freshness
	manifest.RefreshedAt = opts.Now().UTC()
	if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
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
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
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
		source, err := workspaceSemanticSource(brainDir)
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
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
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
		source, err := workspaceSemanticSource(brainDir)
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
	if graphOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	payload, err := buildWorkspaceGraphPayload(cmd.Context(), opts, manifest, graphOpts.limit)
	if err != nil {
		return err
	}
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

func buildWorkspaceGraphPayload(ctx context.Context, opts Options, manifest workspaceManifest, limit int) (workspaceGraphPayload, error) {
	var results []workspaceGraphResult
	contractIndex := map[string]*workspaceExternalContractAggregate{}
	var repoIndexes []workspaceRepoGraphIndex
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(ctx, opts, repo)
		result := workspaceGraphResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		repoIndex := workspaceRepoGraphIndex{RepoKey: repo.RepoKey}
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
		source, err := workspaceSemanticSource(brainDir)
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
		db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
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
			err = collectWorkspaceExternalContracts(db, repo.RepoKey, contractIndex)
		}
		if err == nil {
			repoIndex.Imports, err = collectWorkspaceImportRefs(db, repo.RepoKey)
		}
		if err == nil {
			repoIndex.ExternalSymbols, err = collectWorkspaceExternalSymbolRefs(db, repo.RepoKey)
		}
		if err == nil {
			repoIndex.Candidates, err = collectWorkspaceTargetCandidates(db, repo.RepoKey)
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
	crossEdges := workspaceGraphCrossEdges(contractIndex, limit)
	crossEdges = append(crossEdges, workspaceGraphRouteCallCrossEdges(contractIndex, limit)...)
	crossEdges = append(crossEdges, workspaceGraphGraphQLCrossEdges(contractIndex, limit)...)
	crossEdges = append(crossEdges, workspaceGraphChannelCrossEdges(contractIndex, limit)...)
	crossEdges = append(crossEdges, workspaceGraphResourceCrossEdges(contractIndex, repoIndexes, limit)...)
	crossEdges = append(crossEdges, workspaceGraphImportCrossEdges(repoIndexes, limit)...)
	crossEdges = append(crossEdges, workspaceGraphExternalSymbolCrossEdges(repoIndexes, limit)...)
	sortWorkspaceGraphCrossEdges(crossEdges)
	if len(crossEdges) > limit {
		crossEdges = crossEdges[:limit]
	}
	if crossEdges == nil {
		crossEdges = []workspaceGraphCrossEdge{}
	}
	return workspaceGraphPayload{
		Workspace:   manifest.Name,
		GeneratedAt: opts.Now().UTC(),
		Results:     results,
		Contracts:   workspaceGraphContracts(contractIndex, limit),
		CrossEdges:  crossEdges,
	}, nil
}

func writeWorkspaceGraphSnapshot(ctx context.Context, opts Options, manifest workspaceManifest, limit int) (string, error) {
	payload, err := buildWorkspaceGraphPayload(ctx, opts, manifest, limit)
	if err != nil {
		return "", err
	}
	return writeWorkspaceGraphPayload(opts.Env, payload)
}

func writeWorkspaceGraphPayload(env EntireEnv, payload workspaceGraphPayload) (string, error) {
	dir, err := workspaceDir(env, payload.Workspace)
	if err != nil {
		return "", err
	}
	rel := filepath.ToSlash(filepath.Join(workspaceDirName, payload.Workspace, workspaceGraphName))
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

func collectWorkspaceExternalContracts(db *sql.DB, repoKey string, out map[string]*workspaceExternalContractAggregate) error {
	rows, err := db.Query(`
SELECT CASE
    WHEN r.from_id LIKE 'external:%' THEN r.from_id
    ELSE r.to_id
  END AS endpoint,
  r.type,
  CASE
    WHEN r.from_id LIKE 'external:%' THEN r.to_id
    ELSE r.from_id
  END AS local_id,
  CASE
    WHEN r.from_id LIKE 'external:%' THEN 'from_endpoint'
    ELSE 'to_endpoint'
  END AS direction,
  COALESCE(s.kind, '') AS kind,
  COALESCE(s.name, '') AS name,
  COALESCE(s.qualified_name, '') AS qualified_name,
  COALESCE(s.file_path, '') AS file_path,
  COUNT(*) AS c
FROM relations r
LEFT JOIN symbols s ON s.id = CASE
  WHEN r.from_id LIKE 'external:%' THEN r.to_id
  ELSE r.from_id
END
WHERE r.from_id LIKE 'external:%' OR r.to_id LIKE 'external:%'
GROUP BY endpoint, r.type, local_id, direction, kind, name, qualified_name, file_path
ORDER BY endpoint, r.type, local_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var endpoint, typ, localID, direction, kind, name, qualifiedName, filePath string
		var count int
		if err := rows.Scan(&endpoint, &typ, &localID, &direction, &kind, &name, &qualifiedName, &filePath, &count); err != nil {
			return err
		}
		key := typ + "\x00" + endpoint
		aggregate := out[key]
		if aggregate == nil {
			aggregate = &workspaceExternalContractAggregate{Endpoint: endpoint, Type: typ, RepoCounts: map[string]int{}}
			out[key] = aggregate
		}
		aggregate.RepoCounts[repoKey] += count
		if strings.HasPrefix(localID, "external:") {
			continue
		}
		aggregate.Participants = append(aggregate.Participants, workspaceGraphSymbolRef{
			RepoKey:       repoKey,
			ID:            localID,
			Kind:          kind,
			Name:          name,
			QualifiedName: qualifiedName,
			FilePath:      filePath,
			Direction:     direction,
			Count:         count,
		})
	}
	return rows.Err()
}

func collectWorkspaceImportRefs(db *sql.DB, repoKey string) ([]workspaceGraphImportRef, error) {
	rows, err := db.Query(`
SELECT CASE
    WHEN r.from_id LIKE 'external:import:%' THEN r.from_id
    ELSE r.to_id
  END AS endpoint,
  CASE
    WHEN r.from_id LIKE 'external:import:%' THEN r.to_id
    ELSE r.from_id
  END AS local_id,
  COALESCE(s.kind, '') AS kind,
  COALESCE(s.name, '') AS name,
  COALESCE(s.qualified_name, '') AS qualified_name,
  COALESCE(s.file_path, '') AS file_path,
  COUNT(*) AS c
FROM relations r
LEFT JOIN symbols s ON s.id = CASE
  WHEN r.from_id LIKE 'external:import:%' THEN r.to_id
  ELSE r.from_id
END
WHERE r.type = 'IMPORTS'
  AND (r.from_id LIKE 'external:import:%' OR r.to_id LIKE 'external:import:%')
GROUP BY endpoint, local_id, kind, name, qualified_name, file_path
ORDER BY endpoint, local_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []workspaceGraphImportRef
	for rows.Next() {
		var endpoint, localID, kind, name, qualifiedName, filePath string
		var count int
		if err := rows.Scan(&endpoint, &localID, &kind, &name, &qualifiedName, &filePath, &count); err != nil {
			return nil, err
		}
		spec := strings.TrimPrefix(endpoint, "external:import:")
		if spec == "" || strings.HasPrefix(localID, "external:") {
			continue
		}
		refs = append(refs, workspaceGraphImportRef{
			Spec: spec,
			Source: workspaceGraphSymbolRef{
				RepoKey:       repoKey,
				ID:            localID,
				Kind:          kind,
				Name:          name,
				QualifiedName: qualifiedName,
				FilePath:      filePath,
				Direction:     "imports_external",
				Count:         count,
			},
			Count: count,
		})
	}
	if refs == nil {
		refs = []workspaceGraphImportRef{}
	}
	return refs, rows.Err()
}

func collectWorkspaceExternalSymbolRefs(db *sql.DB, repoKey string) ([]workspaceGraphExternalSymbolRef, error) {
	rows, err := db.Query(`
SELECT CASE
    WHEN r.from_id LIKE 'external:symbol:%' THEN r.from_id
    ELSE r.to_id
  END AS endpoint,
  r.type,
  CASE
    WHEN r.from_id LIKE 'external:symbol:%' THEN r.to_id
    ELSE r.from_id
  END AS local_id,
  COALESCE(s.kind, '') AS kind,
  COALESCE(s.name, '') AS name,
  COALESCE(s.qualified_name, '') AS qualified_name,
  COALESCE(s.file_path, '') AS file_path,
  COUNT(*) AS c
FROM relations r
LEFT JOIN symbols s ON s.id = CASE
  WHEN r.from_id LIKE 'external:symbol:%' THEN r.to_id
  ELSE r.from_id
END
WHERE r.from_id LIKE 'external:symbol:%' OR r.to_id LIKE 'external:symbol:%'
GROUP BY endpoint, r.type, local_id, kind, name, qualified_name, file_path
ORDER BY endpoint, r.type, local_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []workspaceGraphExternalSymbolRef
	for rows.Next() {
		var endpoint, typ, localID, kind, name, qualifiedName, filePath string
		var count int
		if err := rows.Scan(&endpoint, &typ, &localID, &kind, &name, &qualifiedName, &filePath, &count); err != nil {
			return nil, err
		}
		spec := strings.TrimPrefix(endpoint, "external:symbol:")
		if spec == "" || strings.HasPrefix(localID, "external:") {
			continue
		}
		refs = append(refs, workspaceGraphExternalSymbolRef{
			Spec: spec,
			Type: typ,
			Source: workspaceGraphSymbolRef{
				RepoKey:       repoKey,
				ID:            localID,
				Kind:          kind,
				Name:          name,
				QualifiedName: qualifiedName,
				FilePath:      filePath,
				Direction:     "external_symbol_source",
				Count:         count,
			},
			Count: count,
		})
	}
	if refs == nil {
		refs = []workspaceGraphExternalSymbolRef{}
	}
	return refs, rows.Err()
}

func collectWorkspaceTargetCandidates(db *sql.DB, repoKey string) ([]workspaceGraphSymbolRef, error) {
	rows, err := db.Query(`
SELECT id, kind, name, qualified_name, file_path
FROM symbols
ORDER BY file_path, kind, qualified_name, name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []workspaceGraphSymbolRef
	for rows.Next() {
		var candidate workspaceGraphSymbolRef
		candidate.RepoKey = repoKey
		if err := rows.Scan(&candidate.ID, &candidate.Kind, &candidate.Name, &candidate.QualifiedName, &candidate.FilePath); err != nil {
			return nil, err
		}
		candidate.Direction = "import_target_candidate"
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	fileRows, err := db.Query(`SELECT path FROM files WHERE trim(path) <> '' ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer fileRows.Close()
	for fileRows.Next() {
		var path string
		if err := fileRows.Scan(&path); err != nil {
			return nil, err
		}
		candidates = append(candidates, workspaceGraphSymbolRef{
			RepoKey:   repoKey,
			ID:        repoKey + ":file:" + filepath.ToSlash(path),
			Kind:      "file",
			Name:      filepath.Base(path),
			FilePath:  filepath.ToSlash(path),
			Direction: "import_target_candidate",
		})
	}
	if candidates == nil {
		candidates = []workspaceGraphSymbolRef{}
	}
	return candidates, fileRows.Err()
}

func workspaceGraphContracts(index map[string]*workspaceExternalContractAggregate, limit int) []workspaceGraphContract {
	var contracts []workspaceGraphContract
	for _, aggregate := range index {
		if len(aggregate.RepoCounts) < 2 {
			continue
		}
		repos := make([]string, 0, len(aggregate.RepoCounts))
		count := 0
		for repo, n := range aggregate.RepoCounts {
			repos = append(repos, repo)
			count += n
		}
		sort.Strings(repos)
		contracts = append(contracts, workspaceGraphContract{Endpoint: aggregate.Endpoint, Type: aggregate.Type, Repos: repos, Count: count})
	}
	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].Count == contracts[j].Count {
			if contracts[i].Type == contracts[j].Type {
				return contracts[i].Endpoint < contracts[j].Endpoint
			}
			return contracts[i].Type < contracts[j].Type
		}
		return contracts[i].Count > contracts[j].Count
	})
	if len(contracts) > limit {
		contracts = contracts[:limit]
	}
	if contracts == nil {
		return []workspaceGraphContract{}
	}
	return contracts
}

func workspaceGraphCrossEdges(index map[string]*workspaceExternalContractAggregate, limit int) []workspaceGraphCrossEdge {
	var edges []workspaceGraphCrossEdge
	for _, aggregate := range index {
		if len(aggregate.RepoCounts) < 2 || len(aggregate.Participants) < 2 {
			continue
		}
		participants := append([]workspaceGraphSymbolRef(nil), aggregate.Participants...)
		sort.Slice(participants, func(i, j int) bool {
			if participants[i].RepoKey == participants[j].RepoKey {
				return participants[i].ID < participants[j].ID
			}
			return participants[i].RepoKey < participants[j].RepoKey
		})
		for i := 0; i < len(participants); i++ {
			for j := i + 1; j < len(participants); j++ {
				if participants[i].RepoKey == participants[j].RepoKey {
					continue
				}
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     aggregate.Endpoint,
					Type:         aggregate.Type,
					FromRepo:     participants[i].RepoKey,
					ToRepo:       participants[j].RepoKey,
					FromSymbol:   participants[i],
					ToSymbol:     participants[j],
					SharedCount:  participants[i].Count + participants[j].Count,
					RelationKind: "shared_external_contract",
				})
			}
		}
	}
	sortWorkspaceGraphCrossEdges(edges)
	if len(edges) > limit {
		edges = edges[:limit]
	}
	if edges == nil {
		return []workspaceGraphCrossEdge{}
	}
	return edges
}

func workspaceGraphImportCrossEdges(indexes []workspaceRepoGraphIndex, limit int) []workspaceGraphCrossEdge {
	var edges []workspaceGraphCrossEdge
	// Import prefixes depend only on a repo key, so compute them once per repo
	// instead of re-deriving them for every (fromRepo, import) pair in the loop.
	prefixesByRepo := make([][]string, len(indexes))
	for i, repo := range indexes {
		prefixesByRepo[i] = workspaceRepoImportPrefixes(repo.RepoKey)
	}
	for _, fromRepo := range indexes {
		for _, imp := range fromRepo.Imports {
			normalizedSpec := workspaceNormalizeImportSpec(imp.Spec)
			for j, toRepo := range indexes {
				if fromRepo.RepoKey == toRepo.RepoKey {
					continue
				}
				subpath, ok := workspaceImportMatchesPrefixes(normalizedSpec, prefixesByRepo[j])
				if !ok {
					continue
				}
				target, ok := workspaceImportTargetCandidate(toRepo.Candidates, subpath)
				if !ok {
					continue
				}
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     "external:import:" + imp.Spec,
					Type:         "IMPORTS",
					FromRepo:     fromRepo.RepoKey,
					ToRepo:       toRepo.RepoKey,
					FromSymbol:   imp.Source,
					ToSymbol:     target,
					SharedCount:  imp.Count,
					RelationKind: "cross_repo_import_candidate",
				})
			}
		}
	}
	sortWorkspaceGraphCrossEdges(edges)
	if len(edges) > limit {
		edges = edges[:limit]
	}
	if edges == nil {
		return []workspaceGraphCrossEdge{}
	}
	return edges
}

func workspaceGraphRouteCallCrossEdges(index map[string]*workspaceExternalContractAggregate, limit int) []workspaceGraphCrossEdge {
	handlersByEndpoint := map[string][]workspaceGraphSymbolRef{}
	callersByEndpoint := map[string][]workspaceGraphSymbolRef{}
	for _, aggregate := range index {
		if !strings.HasPrefix(aggregate.Endpoint, "external:route:") {
			continue
		}
		endpoint := workspaceCanonicalRouteEndpoint(aggregate.Endpoint)
		switch aggregate.Type {
		case "HANDLES_ROUTE":
			handlersByEndpoint[endpoint] = append(handlersByEndpoint[endpoint], aggregate.Participants...)
		case "HTTP_CALLS":
			callersByEndpoint[endpoint] = append(callersByEndpoint[endpoint], aggregate.Participants...)
		}
	}
	var edges []workspaceGraphCrossEdge
	seen := map[string]bool{}
	for endpoint, callers := range callersByEndpoint {
		handlers := handlersByEndpoint[endpoint]
		if len(callers) == 0 || len(handlers) == 0 {
			continue
		}
		sortWorkspaceGraphSymbolRefs(callers)
		sortWorkspaceGraphSymbolRefs(handlers)
		for _, caller := range callers {
			for _, handler := range handlers {
				if caller.RepoKey == handler.RepoKey {
					continue
				}
				key := endpoint + "\x00" + caller.ID + "\x00" + handler.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     endpoint,
					Type:         "CALLS",
					FromRepo:     caller.RepoKey,
					ToRepo:       handler.RepoKey,
					FromSymbol:   caller,
					ToSymbol:     handler,
					SharedCount:  caller.Count + handler.Count,
					RelationKind: "cross_repo_route_call",
				})
			}
		}
	}
	sortWorkspaceGraphCrossEdges(edges)
	if len(edges) > limit {
		edges = edges[:limit]
	}
	if edges == nil {
		return []workspaceGraphCrossEdge{}
	}
	return edges
}

func workspaceCanonicalRouteEndpoint(endpoint string) string {
	const prefix = "external:route:"
	if !strings.HasPrefix(endpoint, prefix) {
		return endpoint
	}
	route := strings.TrimPrefix(endpoint, prefix)
	if route != "/" {
		route = strings.TrimRight(route, "/")
		if route == "" {
			route = "/"
		}
	}
	route = regexp.MustCompile(`\{[^}/]+\}`).ReplaceAllString(route, `{param}`)
	route = regexp.MustCompile(`<(?:(?:[A-Za-z_][A-Za-z0-9_]*):)?[A-Za-z_][A-Za-z0-9_]*>`).ReplaceAllString(route, `{param}`)
	route = regexp.MustCompile(`\[\[\.{0,3}[A-Za-z_][A-Za-z0-9_]*\]\]`).ReplaceAllString(route, `{param}`)
	route = regexp.MustCompile(`\[\.{0,3}[A-Za-z_][A-Za-z0-9_]*\]`).ReplaceAllString(route, `{param}`)
	route = regexp.MustCompile(`\*[A-Za-z_][A-Za-z0-9_]*`).ReplaceAllString(route, `{param}`)
	route = regexp.MustCompile(`:([A-Za-z_][A-Za-z0-9_]*)`).ReplaceAllString(route, `{param}`)
	return prefix + route
}

func workspaceGraphGraphQLCrossEdges(index map[string]*workspaceExternalContractAggregate, limit int) []workspaceGraphCrossEdge {
	var edges []workspaceGraphCrossEdge
	for _, aggregate := range index {
		if aggregate.Type != "HANDLES_GRAPHQL" || !strings.HasPrefix(aggregate.Endpoint, "external:graphql:") {
			continue
		}
		var targets []workspaceGraphSymbolRef
		var operations []workspaceGraphSymbolRef
		var schemaFields []workspaceGraphSymbolRef
		var resolvers []workspaceGraphSymbolRef
		for _, participant := range aggregate.Participants {
			if participant.Kind == "graphql_resolver" || participant.Kind == "graphql_schema_field" {
				targets = append(targets, participant)
				if participant.Kind == "graphql_schema_field" {
					schemaFields = append(schemaFields, participant)
				} else {
					resolvers = append(resolvers, participant)
				}
			} else {
				operations = append(operations, participant)
			}
		}
		sortWorkspaceGraphSymbolRefs(operations)
		sortWorkspaceGraphSymbolRefs(targets)
		sortWorkspaceGraphSymbolRefs(schemaFields)
		sortWorkspaceGraphSymbolRefs(resolvers)
		for _, operation := range operations {
			for _, target := range targets {
				if operation.RepoKey == target.RepoKey {
					continue
				}
				relationKind := "cross_repo_graphql_call"
				if target.Kind == "graphql_schema_field" {
					relationKind = "cross_repo_graphql_schema"
				}
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     aggregate.Endpoint,
					Type:         "CALLS",
					FromRepo:     operation.RepoKey,
					ToRepo:       target.RepoKey,
					FromSymbol:   operation,
					ToSymbol:     target,
					SharedCount:  operation.Count + target.Count,
					RelationKind: relationKind,
				})
			}
		}
		for _, schemaField := range schemaFields {
			for _, resolver := range resolvers {
				if schemaField.RepoKey == resolver.RepoKey {
					continue
				}
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     aggregate.Endpoint,
					Type:         "CALLS",
					FromRepo:     schemaField.RepoKey,
					ToRepo:       resolver.RepoKey,
					FromSymbol:   schemaField,
					ToSymbol:     resolver,
					SharedCount:  schemaField.Count + resolver.Count,
					RelationKind: "cross_repo_graphql_schema_resolver",
				})
			}
		}
	}
	sortWorkspaceGraphCrossEdges(edges)
	if len(edges) > limit {
		edges = edges[:limit]
	}
	if edges == nil {
		return []workspaceGraphCrossEdge{}
	}
	return edges
}

func workspaceGraphChannelCrossEdges(index map[string]*workspaceExternalContractAggregate, limit int) []workspaceGraphCrossEdge {
	emittersByEndpoint := map[string][]workspaceGraphSymbolRef{}
	listenersByEndpoint := map[string][]workspaceGraphSymbolRef{}
	for _, aggregate := range index {
		if !strings.HasPrefix(aggregate.Endpoint, "external:channel:") {
			continue
		}
		switch aggregate.Type {
		case "EMITS":
			emittersByEndpoint[aggregate.Endpoint] = append(emittersByEndpoint[aggregate.Endpoint], aggregate.Participants...)
		case "LISTENS_ON":
			listenersByEndpoint[aggregate.Endpoint] = append(listenersByEndpoint[aggregate.Endpoint], aggregate.Participants...)
		}
	}
	var edges []workspaceGraphCrossEdge
	seen := map[string]bool{}
	for endpoint, emitters := range emittersByEndpoint {
		listeners := listenersByEndpoint[endpoint]
		if len(emitters) == 0 || len(listeners) == 0 {
			continue
		}
		sortWorkspaceGraphSymbolRefs(emitters)
		sortWorkspaceGraphSymbolRefs(listeners)
		for _, emitter := range emitters {
			for _, listener := range listeners {
				if emitter.RepoKey == listener.RepoKey {
					continue
				}
				key := endpoint + "\x00" + emitter.ID + "\x00" + listener.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     endpoint,
					Type:         "EMITS",
					FromRepo:     emitter.RepoKey,
					ToRepo:       listener.RepoKey,
					FromSymbol:   emitter,
					ToSymbol:     listener,
					SharedCount:  emitter.Count + listener.Count,
					RelationKind: "cross_repo_channel_flow",
				})
			}
		}
	}
	sortWorkspaceGraphCrossEdges(edges)
	if len(edges) > limit {
		edges = edges[:limit]
	}
	if edges == nil {
		return []workspaceGraphCrossEdge{}
	}
	return edges
}

func workspaceGraphResourceCrossEdges(index map[string]*workspaceExternalContractAggregate, indexes []workspaceRepoGraphIndex, limit int) []workspaceGraphCrossEdge {
	candidatesByRepo := map[string][]workspaceGraphSymbolRef{}
	for _, repoIndex := range indexes {
		candidatesByRepo[repoIndex.RepoKey] = repoIndex.Candidates
	}
	var edges []workspaceGraphCrossEdge
	for _, aggregate := range index {
		if aggregate.Type != "RESOURCE_DEPENDS_ON" {
			continue
		}
		kind, name, ok := workspaceKubernetesExternalResource(aggregate.Endpoint)
		if !ok {
			continue
		}
		for _, participant := range aggregate.Participants {
			for repoKey, candidates := range candidatesByRepo {
				if repoKey == participant.RepoKey {
					continue
				}
				target, ok := workspaceResourceTargetCandidate(candidates, kind, name)
				if !ok {
					continue
				}
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     aggregate.Endpoint,
					Type:         aggregate.Type,
					FromRepo:     participant.RepoKey,
					ToRepo:       repoKey,
					FromSymbol:   participant,
					ToSymbol:     target,
					SharedCount:  participant.Count,
					RelationKind: "cross_repo_resource_candidate",
				})
			}
		}
	}
	sortWorkspaceGraphCrossEdges(edges)
	if len(edges) > limit {
		edges = edges[:limit]
	}
	if edges == nil {
		return []workspaceGraphCrossEdge{}
	}
	return edges
}

func workspaceGraphExternalSymbolCrossEdges(indexes []workspaceRepoGraphIndex, limit int) []workspaceGraphCrossEdge {
	var edges []workspaceGraphCrossEdge
	for _, fromRepo := range indexes {
		for _, ref := range fromRepo.ExternalSymbols {
			for _, toRepo := range indexes {
				if fromRepo.RepoKey == toRepo.RepoKey {
					continue
				}
				target, ok := workspaceExternalSymbolTarget(toRepo.Candidates, ref.Spec)
				if !ok {
					continue
				}
				edges = append(edges, workspaceGraphCrossEdge{
					Endpoint:     "external:symbol:" + ref.Spec,
					Type:         ref.Type,
					FromRepo:     fromRepo.RepoKey,
					ToRepo:       toRepo.RepoKey,
					FromSymbol:   ref.Source,
					ToSymbol:     target,
					SharedCount:  ref.Count,
					RelationKind: "cross_repo_external_symbol",
				})
			}
		}
	}
	sortWorkspaceGraphCrossEdges(edges)
	if len(edges) > limit {
		edges = edges[:limit]
	}
	if edges == nil {
		return []workspaceGraphCrossEdge{}
	}
	return edges
}

func sortWorkspaceGraphSymbolRefs(refs []workspaceGraphSymbolRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].RepoKey != refs[j].RepoKey {
			return refs[i].RepoKey < refs[j].RepoKey
		}
		if refs[i].FilePath != refs[j].FilePath {
			return refs[i].FilePath < refs[j].FilePath
		}
		if refs[i].QualifiedName != refs[j].QualifiedName {
			return refs[i].QualifiedName < refs[j].QualifiedName
		}
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].ID < refs[j].ID
	})
}

func workspaceImportMatchesRepo(spec, repoKey string) (string, bool) {
	return workspaceImportMatchesPrefixes(workspaceNormalizeImportSpec(spec), workspaceRepoImportPrefixes(repoKey))
}

// workspaceNormalizeImportSpec canonicalizes an import spec (slash separators,
// trimmed, `::` flattened to `/`) so it can be matched against repo prefixes.
func workspaceNormalizeImportSpec(spec string) string {
	spec = strings.Trim(strings.TrimSpace(filepath.ToSlash(spec)), "/")
	return strings.ReplaceAll(spec, "::", "/")
}

// workspaceImportMatchesPrefixes reports whether a normalized import spec
// resolves under one of a repo's precomputed import prefixes, returning the
// matched subpath. Keeping this separate from workspaceRepoImportPrefixes lets
// callers hoist the per-repo prefix computation out of hot nested loops.
func workspaceImportMatchesPrefixes(normalizedSpec string, prefixes []string) (string, bool) {
	for _, prefix := range prefixes {
		if normalizedSpec == prefix {
			return "", true
		}
		if strings.HasPrefix(normalizedSpec, prefix+"/") {
			return strings.TrimPrefix(normalizedSpec, prefix+"/"), true
		}
		if strings.HasPrefix(normalizedSpec, prefix+".") {
			return strings.ReplaceAll(strings.TrimPrefix(normalizedSpec, prefix+"."), ".", "/"), true
		}
	}
	return "", false
}

func workspaceRepoImportPrefixes(repoKey string) []string {
	repoKey = strings.Trim(strings.TrimSpace(filepath.ToSlash(repoKey)), "/")
	parts := strings.Split(repoKey, "/")
	prefixes := []string{repoKey}
	if len(parts) >= 3 && parts[0] == "gh" {
		ownerRepo := strings.Join(parts[1:3], "/")
		prefixes = append(prefixes, "github.com/"+ownerRepo, ownerRepo, "@"+ownerRepo)
		if len(parts) >= 5 && parts[3] == "packages" {
			pkg := strings.Join(parts[4:], "/")
			prefixes = append(prefixes, "github.com/"+ownerRepo+"/packages/"+pkg, ownerRepo+"/packages/"+pkg, "@"+parts[1]+"/"+pkg)
			for _, alias := range workspacePackageImportAliases("gh", pkg) {
				prefixes = append(prefixes, "github.com/"+ownerRepo+"/packages/"+alias, ownerRepo+"/packages/"+alias, "@"+parts[1]+"/"+alias)
			}
		}
	}
	if len(parts) >= 2 {
		switch parts[0] {
		case "cargo", "composer", "gem", "gomod", "npm", "nuget", "pypi":
			pkg := strings.Join(parts[1:], "/")
			prefixes = append(prefixes, pkg)
			prefixes = append(prefixes, workspacePackageImportAliases(parts[0], pkg)...)
		case "maven":
			groupArtifact := strings.Join(parts[1:], "/")
			prefixes = append(prefixes, groupArtifact, strings.ReplaceAll(groupArtifact, "/", "."))
		}
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i]) != len(prefixes[j]) {
			return len(prefixes[i]) > len(prefixes[j])
		}
		return prefixes[i] < prefixes[j]
	})
	deduped := prefixes[:0]
	seen := map[string]bool{}
	for _, prefix := range prefixes {
		if prefix == "" || seen[prefix] {
			continue
		}
		deduped = append(deduped, prefix)
		seen[prefix] = true
	}
	return deduped
}

func workspacePackageImportAliases(ecosystem, pkg string) []string {
	pkg = strings.Trim(strings.TrimSpace(filepath.ToSlash(pkg)), "/")
	if pkg == "" {
		return nil
	}
	var aliases []string
	addHyphenUnderscore := func(value string) {
		if strings.Contains(value, "-") {
			aliases = append(aliases, strings.ReplaceAll(value, "-", "_"))
		}
		if strings.Contains(value, "_") {
			aliases = append(aliases, strings.ReplaceAll(value, "_", "-"))
		}
	}
	switch ecosystem {
	case "cargo", "gh", "pypi":
		addHyphenUnderscore(pkg)
	case "gem":
		// RubyGems often use hyphenated package names and underscored require
		// paths, while existing exact gem prefixes remain preferred.
		if strings.Contains(pkg, "-") {
			aliases = append(aliases, strings.ReplaceAll(pkg, "-", "_"))
		}
	}
	return aliases
}

func workspaceKubernetesExternalResource(endpoint string) (string, string, bool) {
	endpoint = strings.TrimSpace(endpoint)
	for _, parser := range []struct {
		prefix     string
		kindPrefix string
	}{
		{prefix: "external:config:kubernetes/"},
		{prefix: "external:config:compose/", kindPrefix: "compose."},
	} {
		if !strings.HasPrefix(endpoint, parser.prefix) {
			continue
		}
		rest := strings.Trim(strings.TrimPrefix(endpoint, parser.prefix), "/")
		parts := strings.Split(rest, "/")
		if len(parts) != 2 && len(parts) != 3 {
			return "", "", false
		}
		kind := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[len(parts)-1])
		if kind == "" || name == "" {
			return "", "", false
		}
		if len(parts) == 3 {
			namespace := strings.TrimSpace(parts[1])
			if namespace == "" {
				return "", "", false
			}
			name = namespace + "/" + name
		}
		return parser.kindPrefix + kind, name, true
	}
	return "", "", false
}

func workspaceResourceTargetCandidate(candidates []workspaceGraphSymbolRef, kind, name string) (workspaceGraphSymbolRef, bool) {
	expected := workspaceResourceTargetNames(kind, name)
	for _, candidate := range candidates {
		if !strings.EqualFold(candidate.Kind, "resource") {
			continue
		}
		for _, value := range []string{candidate.Name, candidate.QualifiedName} {
			if !expected[strings.ToLower(strings.TrimSpace(value))] {
				continue
			}
			candidate.Direction = "external_resource_target"
			return candidate, true
		}
	}
	return workspaceGraphSymbolRef{}, false
}

func workspaceResourceTargetNames(kind, name string) map[string]bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	name = strings.ToLower(strings.Trim(strings.TrimSpace(name), "/"))
	out := map[string]bool{}
	if kind == "" || name == "" {
		return out
	}
	out[kind+"."+strings.ReplaceAll(name, "/", ".")] = true
	if strings.Contains(name, "/") {
		_, shortName, ok := strings.Cut(name, "/")
		if ok && shortName != "" {
			out[kind+"."+shortName] = true
		}
	}
	return out
}

func workspaceExternalSymbolTarget(candidates []workspaceGraphSymbolRef, spec string) (workspaceGraphSymbolRef, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return workspaceGraphSymbolRef{}, false
	}
	var matches []workspaceGraphSymbolRef
	for _, candidate := range candidates {
		if candidate.ID == "" || strings.EqualFold(candidate.Kind, "file") {
			continue
		}
		if workspaceExternalSymbolMatchesCandidate(spec, candidate) {
			candidate.Direction = "external_symbol_target"
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return workspaceGraphSymbolRef{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		leftRank := workspaceExternalSymbolCandidateRank(spec, matches[i])
		rightRank := workspaceExternalSymbolCandidateRank(spec, matches[j])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if matches[i].FilePath != matches[j].FilePath {
			return matches[i].FilePath < matches[j].FilePath
		}
		if matches[i].QualifiedName != matches[j].QualifiedName {
			return matches[i].QualifiedName < matches[j].QualifiedName
		}
		if matches[i].Name != matches[j].Name {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].ID < matches[j].ID
	})
	return matches[0], true
}

func workspaceExternalSymbolMatchesCandidate(spec string, candidate workspaceGraphSymbolRef) bool {
	aliases := workspaceExternalSymbolCandidateAliases(candidate)
	for _, normalized := range workspaceExternalSymbolCandidateSpecs(spec, candidate.RepoKey) {
		for _, alias := range aliases {
			if alias == normalized {
				return true
			}
		}
	}
	return false
}

func workspaceExternalSymbolCandidateRank(spec string, candidate workspaceGraphSymbolRef) int {
	normalized := map[string]bool{}
	for _, value := range workspaceExternalSymbolCandidateSpecs(spec, candidate.RepoKey) {
		normalized[value] = true
	}
	switch {
	case candidate.QualifiedName == spec:
		return 0
	case candidate.Name == spec:
		return 1
	case normalized[candidate.QualifiedName]:
		return 2
	case normalized[candidate.Name]:
		return 3
	case workspaceExternalSymbolMatchesCandidate(spec, candidate):
		return 4
	default:
		return 5
	}
}

func workspaceExternalSymbolCandidateAliases(candidate workspaceGraphSymbolRef) []string {
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.Trim(strings.TrimSpace(filepath.ToSlash(value)), "/")
		if value == "" {
			return
		}
		seen[value] = true
		seen[strings.ReplaceAll(value, "/", ".")] = true
	}
	add(candidate.QualifiedName)
	add(candidate.Name)
	if candidate.FilePath != "" && candidate.Name != "" {
		path := strings.TrimSuffix(strings.Trim(filepath.ToSlash(candidate.FilePath), "/"), filepath.Ext(candidate.FilePath))
		if path != "" {
			add(path + "/" + candidate.Name)
			add(path + "." + candidate.Name)
			parts := strings.Split(path, "/")
			for i := 1; i < len(parts); i++ {
				add(strings.Join(parts[i:], "/") + "/" + candidate.Name)
				add(strings.Join(parts[i:], ".") + "." + candidate.Name)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for alias := range seen {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

func workspaceExternalSymbolCandidateSpecs(spec, repoKey string) []string {
	spec = strings.Trim(strings.TrimSpace(filepath.ToSlash(spec)), "/")
	colonSpec := strings.ReplaceAll(spec, "::", "/")
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.Trim(strings.TrimSpace(filepath.ToSlash(value)), "/")
		if value != "" {
			seen[value] = true
			seen[strings.ReplaceAll(value, "/", ".")] = true
		}
	}
	add(spec)
	add(colonSpec)
	for _, prefix := range workspaceRepoImportPrefixes(repoKey) {
		prefix = strings.Trim(strings.TrimSpace(filepath.ToSlash(prefix)), "/")
		if prefix == "" {
			continue
		}
		for _, candidateSpec := range []string{spec, colonSpec} {
			for _, sep := range []string{"/", "."} {
				if strings.HasPrefix(candidateSpec, prefix+sep) {
					add(strings.TrimPrefix(candidateSpec, prefix+sep))
				}
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func workspaceImportTargetCandidate(candidates []workspaceGraphSymbolRef, subpath string) (workspaceGraphSymbolRef, bool) {
	subpath = strings.Trim(strings.TrimSpace(filepath.ToSlash(subpath)), "/")
	matchesSubpath := func(path string) bool {
		return workspaceImportPathMatchesSubpath(path, subpath)
	}
	var symbolMatches []workspaceGraphSymbolRef
	for _, candidate := range candidates {
		if workspaceImportSymbolMatchesSubpath(candidate, subpath) {
			candidate.Direction = "import_symbol_target"
			symbolMatches = append(symbolMatches, candidate)
		}
	}
	if len(symbolMatches) > 0 {
		sort.Slice(symbolMatches, func(i, j int) bool {
			leftRank := workspaceImportSymbolCandidateRank(symbolMatches[i], subpath)
			rightRank := workspaceImportSymbolCandidateRank(symbolMatches[j], subpath)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
			if symbolMatches[i].FilePath != symbolMatches[j].FilePath {
				return symbolMatches[i].FilePath < symbolMatches[j].FilePath
			}
			if symbolMatches[i].QualifiedName != symbolMatches[j].QualifiedName {
				return symbolMatches[i].QualifiedName < symbolMatches[j].QualifiedName
			}
			if symbolMatches[i].Name != symbolMatches[j].Name {
				return symbolMatches[i].Name < symbolMatches[j].Name
			}
			return symbolMatches[i].ID < symbolMatches[j].ID
		})
		return symbolMatches[0], true
	}
	var filtered []workspaceGraphSymbolRef
	for _, candidate := range candidates {
		if matchesSubpath(candidate.FilePath) {
			candidate.Direction = "import_path_target"
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 0 && subpath != "" {
		filtered = append(filtered, candidates...)
	}
	if len(filtered) == 0 {
		return workspaceGraphSymbolRef{}, false
	}
	sort.Slice(filtered, func(i, j int) bool {
		leftRank := workspaceImportCandidateRank(filtered[i])
		rightRank := workspaceImportCandidateRank(filtered[j])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if filtered[i].FilePath != filtered[j].FilePath {
			return filtered[i].FilePath < filtered[j].FilePath
		}
		if filtered[i].QualifiedName != filtered[j].QualifiedName {
			return filtered[i].QualifiedName < filtered[j].QualifiedName
		}
		if filtered[i].Name != filtered[j].Name {
			return filtered[i].Name < filtered[j].Name
		}
		return filtered[i].ID < filtered[j].ID
	})
	return filtered[0], true
}

func workspaceImportPathMatchesSubpath(path, subpath string) bool {
	path = strings.Trim(filepath.ToSlash(path), "/")
	subpath = strings.Trim(filepath.ToSlash(strings.ReplaceAll(subpath, "::", "/")), "/")
	path = strings.TrimSuffix(path, filepath.Ext(path))
	for _, variant := range workspaceImportSubpathVariants(subpath) {
		if variant == "" {
			return true
		}
		if path == variant || strings.HasPrefix(path, variant+"/") {
			return true
		}
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			suffix := strings.Join(parts[i:], "/")
			if suffix == variant || strings.HasPrefix(suffix, variant+"/") {
				return true
			}
		}
	}
	return false
}

func workspaceImportSubpathVariants(subpath string) []string {
	subpath = strings.Trim(filepath.ToSlash(strings.ReplaceAll(subpath, "::", "/")), "/")
	variants := []string{subpath}
	parts := strings.Split(subpath, "/")
	if len(parts) > 0 && regexp.MustCompile(`^v[2-9][0-9]*$`).MatchString(parts[0]) {
		variants = append(variants, strings.Join(parts[1:], "/"))
	}
	return variants
}

func workspaceImportSymbolMatchesSubpath(candidate workspaceGraphSymbolRef, subpath string) bool {
	if candidate.ID == "" || strings.EqualFold(candidate.Kind, "file") || subpath == "" {
		return false
	}
	for _, name := range workspaceImportSymbolNames(subpath) {
		if candidate.Name == name || candidate.QualifiedName == name {
			return true
		}
		if strings.HasSuffix(candidate.QualifiedName, "."+name) || strings.HasSuffix(candidate.QualifiedName, "/"+name) {
			return true
		}
	}
	return false
}

func workspaceImportSymbolCandidateRank(candidate workspaceGraphSymbolRef, subpath string) int {
	names := workspaceImportSymbolNames(subpath)
	for _, name := range names {
		if candidate.QualifiedName == name {
			return 0
		}
	}
	for _, name := range names {
		if candidate.Name == name {
			return 1
		}
	}
	for _, name := range names {
		if strings.HasSuffix(candidate.QualifiedName, "."+name) || strings.HasSuffix(candidate.QualifiedName, "/"+name) {
			return 2
		}
	}
	return 3
}

func workspaceImportSymbolNames(subpath string) []string {
	subpath = strings.Trim(strings.TrimSpace(filepath.ToSlash(strings.ReplaceAll(subpath, "::", "/"))), "/")
	if subpath == "" {
		return nil
	}
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.Trim(strings.TrimSpace(value), ". /")
		if value != "" {
			seen[value] = true
		}
	}
	add(subpath)
	add(strings.ReplaceAll(subpath, "/", "."))
	parts := strings.FieldsFunc(subpath, func(r rune) bool { return r == '/' || r == '.' })
	if len(parts) > 0 {
		add(parts[len(parts)-1])
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

func workspaceImportCandidateRank(candidate workspaceGraphSymbolRef) int {
	switch strings.ToLower(candidate.Kind) {
	case "package", "module", "namespace", "project":
		return 0
	case "file":
		return 2
	default:
		return 1
	}
}

func sortWorkspaceGraphCrossEdges(edges []workspaceGraphCrossEdge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].SharedCount == edges[j].SharedCount {
			if edges[i].RelationKind == edges[j].RelationKind {
				if edges[i].Type == edges[j].Type {
					if edges[i].Endpoint == edges[j].Endpoint {
						if edges[i].FromRepo == edges[j].FromRepo {
							return edges[i].ToRepo < edges[j].ToRepo
						}
						return edges[i].FromRepo < edges[j].FromRepo
					}
					return edges[i].Endpoint < edges[j].Endpoint
				}
				return edges[i].Type < edges[j].Type
			}
			return edges[i].RelationKind < edges[j].RelationKind
		}
		return edges[i].SharedCount > edges[j].SharedCount
	})
}

func runWorkspaceRetrieve(cmd *cobra.Command, opts Options, retrieveOpts workspaceRetrieveOptions, mode retrievalMode, workspaceName, query string) error {
	if retrieveOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	// Validate the source/filter contract once, before the fan-out, so an
	// invalid selector is one structured error rather than N per-repo copies.
	// Note: retrieveOpts.branch is the facts-branch selector resolved per
	// member below; the conversation branch filter is deliberately NOT wired
	// to it here (member repos are on different branches — filter per-repo
	// results by branch in the caller if needed).
	ropts, err := buildRetrievalOptions(retrieveOpts.source, retrieveOpts.after, retrieveOpts.before, retrieveOpts.session, retrieveOpts.agent, "")
	if err != nil {
		return fmt.Errorf("--%s", err.Error())
	}
	if mode == modeVector && ropts.Source == retrievalSourceConversation {
		return errConversationVectorUnsupported
	}
	if ropts.hasConversationOnlyFilters() && ropts.Source != retrievalSourceConversation {
		return fmt.Errorf(`after/before/session/agent filters require --source conversation (got %q)`, ropts.Source)
	}
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
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
		} else if found != nil {
			if repoDir == "" {
				found = annotateCurrentCodeUnavailable(found)
			}
			result.Results = found
		}
		results = append(results, result)
	}
	if mode != modeVector {
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
	if retrieveOpts.json {
		return writeJSON(cmd, struct {
			Workspace string                    `json:"workspace"`
			Query     string                    `json:"query"`
			Results   []workspaceRetrieveResult `json:"results"`
		}{Workspace: manifest.Name, Query: query, Results: results})
	}
	out := cmd.OutOrStdout()
	for _, result := range results {
		for _, r := range result.Results {
			loc := r.Path
			if r.Line > 0 {
				loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
			}
			ex := truncateString(strings.Join(strings.Fields(r.Text), " "), 200)
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
	return nil
}

func runWorkspaceGet(cmd *cobra.Command, opts Options, workspaceName string, qualifiedIDs []string, branchOverride string, jsonOut bool) error {
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	members := make(map[string]workspaceRepo, len(manifest.Repos))
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
		found, missing, err := getUnifiedBatch(repoDir, brainDir, branch, idsByRepo[repoKey])
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
		return writeJSON(cmd, struct {
			Workspace string               `json:"workspace"`
			Results   []workspaceGetResult `json:"results"`
		}{Workspace: manifest.Name, Results: results})
	}
	out := cmd.OutOrStdout()
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
	return nil
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
		score := workspaceGraphSearchScore(result.Text, query)
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
	result, ok := getWorkspaceCorpusRecord(env, workspaceName, id)
	return result, ok, nil
}

func loadWorkspaceGraphPayload(env EntireEnv, workspaceName string) (workspaceGraphPayload, error) {
	dir, err := workspaceDir(env, workspaceName)
	if err != nil {
		return workspaceGraphPayload{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, workspaceGraphName))
	if err != nil {
		return workspaceGraphPayload{}, err
	}
	var payload workspaceGraphPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return workspaceGraphPayload{}, err
	}
	return payload, nil
}

func workspaceGraphCrossEdgeUnified(edge workspaceGraphCrossEdge) unifiedResult {
	fromName := workspaceGraphSymbolDisplay(edge.FromSymbol)
	toName := workspaceGraphSymbolDisplay(edge.ToSymbol)
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
		ID:      workspaceGraphCrossEdgeID(edge),
		Path:    workspaceGraphCrossEdgePath(edge),
		Heading: edge.RelationKind,
		Text:    text,
	}
}

func workspaceGraphCrossEdgeID(edge workspaceGraphCrossEdge) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(edge.RelationKind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(edge.Type))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(edge.Endpoint))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(edge.FromRepo))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(edge.FromSymbol.ID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(edge.ToRepo))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(edge.ToSymbol.ID))
	return fmt.Sprintf("graph:%016x", h.Sum64())
}

func workspaceGraphCrossEdgePath(edge workspaceGraphCrossEdge) string {
	from := strings.TrimSpace(edge.FromSymbol.FilePath)
	to := strings.TrimSpace(edge.ToSymbol.FilePath)
	switch {
	case from != "" && to != "":
		return edge.FromRepo + ":" + from + " -> " + edge.ToRepo + ":" + to
	case from != "":
		return edge.FromRepo + ":" + from
	case to != "":
		return edge.ToRepo + ":" + to
	default:
		return edge.FromRepo + " -> " + edge.ToRepo
	}
}

func workspaceGraphSymbolDisplay(symbol workspaceGraphSymbolRef) string {
	for _, value := range []string{symbol.QualifiedName, symbol.Name, symbol.ID} {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return "(unknown)"
}

func workspaceGraphSearchScore(text, query string) float64 {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return 0
	}
	text = strings.ToLower(text)
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return 0
	}
	var matched int
	for _, term := range terms {
		if strings.Contains(text, term) {
			matched++
		}
	}
	if matched == 0 {
		return 0
	}
	return float64(matched) / float64(len(terms))
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
	for _, prefix := range []string{"fact:", "review:", "history:", "conversation:", "doc:", "pattern:", "theme:"} {
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
func workspaceFullRefresh(ctx context.Context, out io.Writer, opts Options, manifest workspaceManifest, refreshRepo func(repoDir string) error) error {
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
func resolveWorkspaceMemberRepoDir(ctx context.Context, opts Options, configDir string, repo workspaceRepo) (string, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, repo.LocalPathHint)
	if err != nil || !local {
		return "", errors.New("no resolvable local path")
	}
	hintKey, err := repoStorageKey(ctx, opts.Runner, configDir, repoDir)
	if err != nil {
		return "", fmt.Errorf("unsafe: %w", err)
	}
	if hintKey != repo.RepoKey {
		return "", fmt.Errorf("unsafe: local_path_hint repo_key mismatch: %s", hintKey)
	}
	return repoDir, nil
}

func workspaceFreshness(ctx context.Context, opts Options, manifest workspaceManifest) ([]workspaceRepoFreshness, error) {
	var result []workspaceRepoFreshness
	for _, repo := range manifest.Repos {
		result = append(result, workspaceRepoFreshnessForRepo(ctx, opts, repo))
	}
	return result, nil
}

func workspaceRepoFreshnessForRepo(ctx context.Context, opts Options, repo workspaceRepo) workspaceRepoFreshness {
	status := workspaceRepoFreshness{RepoKey: repo.RepoKey, Name: repo.Name}
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
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, repo.LocalPathHint)
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
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		status.State = "unsafe"
		status.Detail = err.Error()
		return status
	}
	hintKey, err := repoStorageKey(ctx, opts.Runner, dirs.Config, repoDir)
	if err != nil {
		// Can't derive the tree's repo_key → can't verify the pairing → unsafe to scan.
		status.State = "unsafe"
		status.PairingUnsafe = true
		status.Detail = err.Error()
		return status
	}
	if workspaceRepoKeyMismatch(repo.RepoKey, hintKey) {
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
	if axis, ok := report.Axes["snapshot"]; ok {
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

func workspaceRepoKeyMismatch(registered, hint string) bool {
	if registered == hint {
		return false
	}
	return true
}

func workspaceSemanticSource(brainDir string) (*semanticSourceManifest, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return nil, errors.New("semantic index missing")
	}
	return manifest.Sources.Semantic, nil
}

// workspaceDir resolves a workspace's home at <data>/workspaces/<name> — a
// SIBLING of the repos/ tree, not inside it. Workspaces are not repos:
// keeping them under repos/ parked a magic "workspaces" directory in the
// middle of the repo keyspace (an unknown host slug or hand-edited
// DomainSlugs entry could legitimately claim the same segment and collide),
// and forced any future repos/-enumerator to know to skip it. Manifests
// written by older builds under repos/workspaces/<name> are migrated lazily
// on first touch (a directory rename; the payload is kilobytes).
func workspaceDir(env EntireEnv, name string) (string, error) {
	if err := validateWorkspaceName(name); err != nil {
		return "", err
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	workspaceRoot := filepath.Join(dirs.Data, workspaceDirName)
	if err := rejectBrainRootPathSymlinks(workspaceRoot, name); err != nil {
		return "", err
	}
	dir := filepath.Join(workspaceRoot, name)
	if err := migrateLegacyWorkspaceDir(dirs.Data, name, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// migrateLegacyWorkspaceDir moves a pre-relocation workspace
// (<data>/repos/workspaces/<name>) to its new home on first touch. A no-op
// when the legacy dir is absent or the new dir already exists (the new copy
// wins: it is the one current builds have been writing to).
func migrateLegacyWorkspaceDir(dataDir, name, newDir string) error {
	legacyRoot := filepath.Join(dataDir, repoStoreDirName)
	legacy := filepath.Join(legacyRoot, workspaceDirName, name)
	info, err := os.Lstat(legacy)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		// Absent, unreadable, or a SYMLINK: never migrate. Renaming a symlink
		// would move the link itself into the new home, making every future
		// workspace write flow through an attacker-placed target.
		return nil
	}
	if rejectBrainRootPathSymlinks(legacyRoot, filepath.Join(workspaceDirName, name)) != nil {
		return nil // tampered legacy tree (symlinked root/components): leave it alone
	}
	if _, err := os.Stat(newDir); err == nil {
		return nil // the new copy wins: it is the one current builds write to
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o700); err != nil {
		return err
	}
	if err := os.Rename(legacy, newDir); err != nil {
		return fmt.Errorf("migrate workspace %s to %s: %w", name, newDir, err)
	}
	return nil
}

func loadWorkspaceManifest(env EntireEnv, name string) (workspaceManifest, error) {
	if err := validateWorkspaceName(name); err != nil {
		return workspaceManifest{}, err
	}
	dir, err := workspaceDir(env, name)
	if err != nil {
		return workspaceManifest{}, err
	}
	path := filepath.Join(dir, workspaceManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return workspaceManifest{}, err
	}
	var manifest workspaceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return workspaceManifest{}, err
	}
	if manifest.SchemaVersion != workspaceSchemaVersion {
		return workspaceManifest{}, fmt.Errorf("unsupported workspace schema_version: %d", manifest.SchemaVersion)
	}
	if manifest.Name != name {
		return workspaceManifest{}, fmt.Errorf("workspace name mismatch: %s", manifest.Name)
	}
	for _, repo := range manifest.Repos {
		if err := validateWorkspaceRepoKey(repo.RepoKey); err != nil {
			return workspaceManifest{}, err
		}
	}
	return manifest, nil
}

func writeWorkspaceManifest(env EntireEnv, manifest workspaceManifest) error {
	if err := validateWorkspaceName(manifest.Name); err != nil {
		return err
	}
	for _, repo := range manifest.Repos {
		if err := validateWorkspaceRepoKey(repo.RepoKey); err != nil {
			return err
		}
	}
	manifest.SchemaVersion = workspaceSchemaVersion
	sortWorkspaceRepos(manifest.Repos)
	dir, err := workspaceDir(env, manifest.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(dir, workspaceManifestName), data, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, workspaceReadmeName), []byte(renderWorkspaceReadme(manifest)), 0o600)
}

func validateWorkspaceName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Trim(name, ".") == "" || !workspaceNamePattern.MatchString(name) {
		return fmt.Errorf("workspace name must contain only letters, numbers, dots, underscores, or dashes: %s", name)
	}
	return nil
}

func validateWorkspaceRepoKey(key string) error {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(key)))
	if key == "" || key == "." || key == ".." || clean != key || strings.HasPrefix(key, "../") || strings.Contains(key, "/../") || filepath.IsAbs(filepath.FromSlash(key)) {
		return fmt.Errorf("workspace repo_key is unsafe: %s", key)
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "." || segment == ".." || segment == "" {
			return fmt.Errorf("workspace repo_key is unsafe: %s", key)
		}
	}
	return nil
}

func sortWorkspaceRepos(repos []workspaceRepo) {
	sort.Slice(repos, func(i, j int) bool {
		return repos[i].RepoKey < repos[j].RepoKey
	})
}

func renderWorkspaceReadme(manifest workspaceManifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Workspace: %s\n\n", manifest.Name)
	fmt.Fprintln(&b, "Local-only workspace brain. Repository identity is stored by repo key; path hints are local metadata.")
	if len(manifest.Repos) > 0 {
		fmt.Fprintln(&b, "\n## Repositories")
		for _, repo := range manifest.Repos {
			label := repo.RepoKey
			if repo.Name != "" {
				label = repo.Name + " (" + repo.RepoKey + ")"
			}
			fmt.Fprintf(&b, "- `%s`\n", label)
		}
	}
	return b.String()
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
	out := &stickyErrorWriter{writer: cmd.OutOrStdout()}
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
		filepath.Join(dirs.Data, workspaceDirName),
		filepath.Join(dirs.Data, repoStoreDirName, workspaceDirName),
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
	var summaries []workspaceSummary
	for name := range names {
		manifest, err := loadWorkspaceManifest(opts.Env, name)
		if err != nil {
			// Skip directories that are not valid workspaces (unreadable, bad name, schema drift).
			continue
		}
		summaries = append(summaries, workspaceSummary{Name: manifest.Name, Repos: len(manifest.Repos)})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	if asJSON {
		return writeJSON(cmd, struct {
			Workspaces []workspaceSummary `json:"workspaces"`
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
	if repoKey == "" {
		// workspaceDir validates the name and rejects symlinked paths, so RemoveAll stays inside the store.
		dir, err := workspaceDir(opts.Env, workspaceName)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, workspaceManifestName)); err != nil {
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
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	kept := make([]workspaceRepo, 0, len(manifest.Repos))
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
	if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
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

// workspaceFreshnessBlocksScan reports whether a repo's brain↔working-tree pairing is too unsafe to
// scan. It blocks ONLY on PairingUnsafe — the local_path_hint's repo_key no longer matches (the repo
// moved/was replaced) or could not be derived — because scanning would then compare one repo's brain
// against an UNRELATED tree and manufacture bogus "regressions". It deliberately does NOT block on
// State == "unsafe" alone: a valid pairing with a stale/broken semantic index is still scannable (the
// detector reads raw sessions regardless of index age) — blocking those would drop real findings.
func workspaceFreshnessBlocksScan(freshness workspaceRepoFreshness) bool {
	return freshness.PairingUnsafe
}

// workspaceFreshnessWarning returns a loud, human-facing annotation for a repo that is scannable but
// not "ok" (so the result is qualified rather than silently trusted), or "" for ok. Surfaced in both
// JSON (prepended to result.Warnings) and text output. "unsafe" never reaches here — it blocks the
// scan in workspaceFreshnessBlocksScan and is reported as a skip instead.
func workspaceFreshnessWarning(f workspaceRepoFreshness) string {
	switch f.State {
	case "unsafe":
		// Reached only for a SCANNED repo (PairingUnsafe blocks before this), so "unsafe" here means
		// a stale/broken semantic index over a still-valid pairing — the scan ran on raw sessions.
		return "semantic index unsafe/stale — scanned raw sessions only; verify findings"
	case "degraded":
		return "freshness degraded — brain/index may be stale; verify findings"
	case "missing-brain":
		// No export manifest at the brain root. The detector still scans raw sessions (the real
		// mirrored-sessions brains are exactly this shape), so this qualifies rather than blocks.
		return "no export manifest — scanned raw sessions only (run `entire brain refresh` for a full brain)"
	case "missing-semantic":
		return "semantic index missing — scanned raw sessions only"
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

// scanWorkspaceRepoRegressions runs the regression detector for a single workspace repo and returns
// the resolved working-tree path, anomalies, warnings, and a per-repo error string (empty on success).
func scanWorkspaceRepoRegressions(ctx context.Context, opts Options, repo workspaceRepo, ro regressionDetectorOptions, query string) (string, []regressionAnomaly, []string, string) {
	brainDir, err := brainDirForKey(opts.Env, repo.RepoKey)
	if err != nil {
		return "", nil, nil, err.Error()
	}
	if repo.LocalPathHint == "" {
		return "", nil, nil, "no local_path_hint (re-add the repo to record its working tree)"
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, repo.LocalPathHint)
	if err != nil {
		return "", nil, nil, err.Error()
	}
	if !local {
		return "", nil, nil, "local_path_hint unavailable"
	}
	// Semantic source is optional: a sessions-only brain (no export manifest) still scans.
	var semSource *semanticSourceManifest
	if source, srcErr := workspaceSemanticSource(brainDir); srcErr == nil {
		semSource = source
	}
	anomalies, _, warnings := detectRegressionAnomalies(brainDir, repoDir, semSource, query, ro.limit, ro.includeDeletions)
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
	return repoDir, anomalies, warnings, ""
}

func runWorkspaceRegressions(cmd *cobra.Command, opts Options, ro regressionDetectorOptions, workspaceName, query string) error {
	if ro.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	var results []workspaceRegressionResult
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceRegressionResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		if workspaceFreshnessBlocksScan(freshness) {
			result.Error = "skipped (unsafe): " + freshness.Detail
		} else {
			repoPath, anomalies, warnings, scanErr := scanWorkspaceRepoRegressions(cmd.Context(), opts, repo, ro, query)
			result.RepoPath = repoPath
			result.Anomalies = anomalies
			result.Warnings = warnings
			result.Error = scanErr
			if warn := workspaceFreshnessWarning(freshness); warn != "" {
				result.Warnings = append([]string{warn}, result.Warnings...)
			}
		}
		results = append(results, result)
	}
	if ro.json {
		return writeJSON(cmd, struct {
			Workspace     string                      `json:"workspace"`
			SchemaVersion int                         `json:"schema_version"`
			Results       []workspaceRegressionResult `json:"results"`
		}{Workspace: manifest.Name, SchemaVersion: reviewReportSchemaVersion, Results: results})
	}
	out := cmd.OutOrStdout()
	total := 0
	for _, result := range results {
		// Surface per-repo freshness so a stale/degraded pairing is never silently trusted.
		state := result.Freshness.State
		if state == "" {
			state = "unknown"
		}
		if len(result.Anomalies) > 0 || result.Error != "" || state != "ok" {
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
		fmt.Fprintf(out, "No suspected regressions across %d repo(s) in %q.\n", len(results), manifest.Name)
	}
	return nil
}

func runWorkspaceReview(cmd *cobra.Command, opts Options, ro regressionDetectorOptions, workspaceName, query string) error {
	if ro.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	var results []workspaceReviewResult
	reposWithFindings := 0
	totalFindings := 0
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceReviewResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
		if workspaceFreshnessBlocksScan(freshness) {
			result.Error = "skipped (unsafe): " + freshness.Detail
			result.Summary = "skipped: brain and working tree cannot be trusted to match."
			results = append(results, result)
			continue
		}
		repoPath, anomalies, warnings, scanErr := scanWorkspaceRepoRegressions(cmd.Context(), opts, repo, ro, query)
		result.RepoPath = repoPath
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
			result.Summary = "no suspected regressions (current tree matches the brain's memory)."
		}
		results = append(results, result)
	}
	summary := fmt.Sprintf("Cross-repo diff-less review: %d suspected regression(s) across %d/%d repo(s).", totalFindings, reposWithFindings, len(results))
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
