package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	RepoKey        string `json:"repo_key"`
	Name           string `json:"name,omitempty"`
	State          string `json:"state"`
	Detail         string `json:"detail,omitempty"`
	ContractState  string `json:"contract_state,omitempty"`
	ContractDetail string `json:"contract_detail,omitempty"`
}

type workspaceAddOptions struct {
	name string
}

type workspaceQueryOptions struct {
	limit int
	json  bool
}

type workspaceImpactOptions struct {
	limit int
	depth int
	json  bool
}

type workspaceQueryResult struct {
	RepoKey   string                 `json:"repo_key"`
	Name      string                 `json:"name,omitempty"`
	Freshness workspaceRepoFreshness `json:"freshness"`
	Symbols   []semanticRecord       `json:"symbols"`
	Error     string                 `json:"error,omitempty"`
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

func newWorkspaceCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage local multi-repo brain workspaces",
	}
	cmd.AddCommand(newWorkspaceCreateCommand(opts))
	cmd.AddCommand(newWorkspaceAddCommand(opts))
	cmd.AddCommand(newWorkspaceRefreshCommand(opts))
	cmd.AddCommand(newWorkspaceQueryCommand(opts))
	cmd.AddCommand(newWorkspaceImpactCommand(opts))
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
	return &cobra.Command{
		Use:   "refresh <workspace>",
		Short: "Refresh local workspace membership freshness",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRefresh(cmd.Context(), cmd, opts, args[0])
		},
	}
}

func newWorkspaceQueryCommand(opts Options) *cobra.Command {
	queryOpts := workspaceQueryOptions{limit: 10}
	cmd := &cobra.Command{
		Use:   "query <workspace> <symbol-or-text>",
		Short: "Query semantic symbols across workspace repos",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceQuery(cmd, opts, queryOpts, args[0], args[1])
		},
	}
	cmd.Flags().IntVar(&queryOpts.limit, "limit", 10, "Maximum symbols per repo")
	cmd.Flags().BoolVar(&queryOpts.json, "json", false, "Emit machine-readable JSON")
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

func runWorkspaceRefresh(ctx context.Context, cmd *cobra.Command, opts Options, workspaceName string) error {
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
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

func runWorkspaceQuery(cmd *cobra.Command, opts Options, queryOpts workspaceQueryOptions, workspaceName, query string) error {
	if queryOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	manifest, err := loadWorkspaceManifest(opts.Env, workspaceName)
	if err != nil {
		return err
	}
	var results []workspaceQueryResult
	for _, repo := range manifest.Repos {
		freshness := workspaceRepoFreshnessForRepo(cmd.Context(), opts, repo)
		result := workspaceQueryResult{RepoKey: repo.RepoKey, Name: repo.Name, Freshness: freshness}
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
		symbols, _, err := semanticContextFacts(brainDir, source, query, queryOpts.limit, 0)
		unlock()
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Symbols = symbols
		}
		results = append(results, result)
	}
	if queryOpts.json {
		return writeJSON(cmd, struct {
			Workspace string                 `json:"workspace"`
			Results   []workspaceQueryResult `json:"results"`
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
	if _, err := os.Stat(filepath.Join(brainDir, exportManifestFileName)); err != nil {
		if os.IsNotExist(err) {
			status.State = "missing-brain"
			return status
		}
		status.State = "unsafe"
		status.Detail = err.Error()
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
	if repo.LocalPathHint == "" {
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
		status.State = "unsafe"
		status.Detail = err.Error()
		return status
	}
	if hintKey != repo.RepoKey {
		status.State = "unsafe"
		status.Detail = "local_path_hint repo_key mismatch: " + hintKey
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

func workspaceDir(env EntireEnv, name string) (string, error) {
	if err := validateWorkspaceName(name); err != nil {
		return "", err
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	brainRoot := filepath.Join(dirs.Data, repoStoreDirName)
	if err := rejectBrainRootPathSymlinks(brainRoot, filepath.Join(workspaceDirName, name)); err != nil {
		return "", err
	}
	return filepath.Join(brainRoot, workspaceDirName, name), nil
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
