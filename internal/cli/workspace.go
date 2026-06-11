package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

type workspaceImpactOptions struct {
	limit int
	depth int
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
	return cmd
}

func newWorkspaceGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var branch string
	cmd := &cobra.Command{
		Use:   "get <workspace> <repo-key/id>...",
		Short: "Fetch items in full by repo-qualified id (e.g. gh/owner/repo/fact:…, as printed by workspace search)",
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
			result.Symbols = nonNilRecords(symbols)
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

func runWorkspaceRetrieve(cmd *cobra.Command, opts Options, retrieveOpts workspaceRetrieveOptions, mode retrievalMode, workspaceName, query string) error {
	if retrieveOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
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
		found, err := retrieveUnified(brainDir, branch, query, retrieveOpts.limit, mode)
		if err != nil {
			result.Error = err.Error()
		} else if found != nil {
			result.Results = found
		}
		results = append(results, result)
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
			// The id is printed repo-qualified so it can be pasted into `workspace get`.
			fmt.Fprintf(out, "[%s] %s/%s  %s\n    %s\n", r.Source, result.RepoKey, r.ID, loc, ex)
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
	members := make(map[string]bool, len(manifest.Repos))
	for _, repo := range manifest.Repos {
		members[repo.RepoKey] = true
	}
	// Group ids by repo key, preserving first-appearance order of repos and the
	// input order of ids within each repo.
	var repoOrder []string
	idsByRepo := make(map[string][]string)
	for _, qualified := range qualifiedIDs {
		repoKey, id, err := splitWorkspaceID(qualified)
		if err != nil {
			return err
		}
		if !members[repoKey] {
			return fmt.Errorf("repo %s is not a member of workspace %s", repoKey, manifest.Name)
		}
		if _, seen := idsByRepo[repoKey]; !seen {
			repoOrder = append(repoOrder, repoKey)
		}
		idsByRepo[repoKey] = append(idsByRepo[repoKey], id)
	}
	var results []workspaceGetResult
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
		found, missing, err := getUnifiedBatch(brainDir, branch, idsByRepo[repoKey])
		if err != nil {
			result.Error = err.Error()
			results = append(results, result)
			continue
		}
		if found != nil {
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
			fmt.Fprintf(out, "[%s] %s/%s  %s\n%s\n\n", r.Source, result.RepoKey, r.ID, loc, r.Text)
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
	for _, prefix := range []string{"fact:", "history:", "doc:"} {
		if strings.HasPrefix(qualified, prefix) {
			return "", "", fmt.Errorf("id %q is missing its repo key (expected <repo-key>/%s…)", qualified, prefix)
		}
		if i := strings.Index(qualified, "/"+prefix); i > 0 {
			return qualified[:i], qualified[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("unrecognized id %q (expected <repo-key>/fact:…, <repo-key>/history:…, or <repo-key>/doc:…)", qualified)
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
	fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return nil
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
