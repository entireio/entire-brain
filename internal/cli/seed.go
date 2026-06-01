package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	seedDirName             = "seed"
	seedAgentDirName        = "agent"
	seedDocsDirName         = "docs"
	seedCursorFileName      = "seed.json"
	defaultSeedMaxFileBytes = 256 * 1024
	defaultSeedMaxFiles     = 2000
	defaultAgentMaxInput    = 500000
	defaultAgentMaxOutput   = 200000
)

type seedCommandOptions struct {
	outputDir          string
	update             bool
	force              bool
	includeTests       bool
	maxFileBytes       int
	maxFiles           int
	format             string
	worktree           bool
	agent              string
	agentCommand       []string
	agentQuickTimeout  time.Duration
	agentDeepTimeout   time.Duration
	agentTimeoutAction string
	agentMaxInputBytes int
	interactive        bool
	noInteractive      bool
	requireAgent       bool
}

type seedSourceManifest struct {
	GeneratedAt       time.Time            `json:"generated_at"`
	Commit            string               `json:"commit,omitempty"`
	WorktreeMode      string               `json:"worktree_mode"`
	FileFingerprint   string               `json:"file_fingerprint"`
	SummaryPath       string               `json:"summary_path"`
	Documents         []seedDocument       `json:"documents,omitempty"`
	Entrypoints       []string             `json:"entrypoints,omitempty"`
	Commands          []seedCommand        `json:"commands,omitempty"`
	HistoryBaseline   *seedHistoryBaseline `json:"history_baseline,omitempty"`
	HistoryCoverage   *seedHistoryCoverage `json:"history_coverage,omitempty"`
	Agent             *seedAgentManifest   `json:"agent,omitempty"`
	Warnings          []string             `json:"warnings,omitempty"`
	DeterministicPath []string             `json:"deterministic_paths,omitempty"`
}

type seedDocument struct {
	Path      string `json:"path"`
	SeedPath  string `json:"seed_path,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type seedCommand struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Source  string `json:"source"`
}

type seedHistoryBaseline struct {
	OldestCommitAt  *time.Time `json:"oldest_commit_at,omitempty"`
	OldestSessionAt *time.Time `json:"oldest_session_at,omitempty"`
	SeedRequired    bool       `json:"seed_required"`
	Reason          string     `json:"reason"`
	Confidence      string     `json:"confidence"`
}

type seedHistoryCoverage struct {
	Path                          string              `json:"path"`
	TotalCommits                  int                 `json:"total_commits"`
	PreSessionCommits             int                 `json:"pre_session_commits"`
	CoveredCommits                int                 `json:"covered_commits"`
	CheckpointedUnexportedCommits int                 `json:"checkpointed_unexported_commits"`
	MissingSessionCommits         int                 `json:"missing_session_commits"`
	NoSessionHistoryCommits       int                 `json:"no_session_history_commits"`
	MergeCommits                  int                 `json:"merge_commits"`
	OldestSessionAt               *time.Time          `json:"oldest_session_at,omitempty"`
	ExportedCheckpoints           int                 `json:"exported_checkpoints"`
	UncoveredCommits              []seedCoveredCommit `json:"uncovered_commits,omitempty"`
	GeneratedFrom                 string              `json:"generated_from"`
}

type seedCoveredCommit struct {
	Hash        string    `json:"hash"`
	CommittedAt time.Time `json:"committed_at"`
	AuthorName  string    `json:"author_name,omitempty"`
	AuthorEmail string    `json:"author_email,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	Coverage    string    `json:"coverage"`
	Checkpoints []string  `json:"checkpoints,omitempty"`
	Merge       bool      `json:"merge,omitempty"`
	Parents     []string  `json:"parents,omitempty"`
	BodyExcerpt string    `json:"body_excerpt,omitempty"`
}

type seedAgentManifest struct {
	Mode  string         `json:"mode"`
	Quick seedAgentPhase `json:"quick"`
	Deep  seedAgentPhase `json:"deep"`
}

type seedAgentPhase struct {
	Status           string    `json:"status"`
	Timeout          string    `json:"timeout"`
	TimeoutAction    string    `json:"timeout_action,omitempty"`
	InputFingerprint string    `json:"input_fingerprint,omitempty"`
	OutputPaths      []string  `json:"output_paths,omitempty"`
	Model            string    `json:"model,omitempty"`
	Warnings         []string  `json:"warnings,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	CompletedAt      time.Time `json:"completed_at,omitempty"`
}

type seedCursor struct {
	SchemaVersion                  int                  `json:"schema_version"`
	UpdatedAt                      time.Time            `json:"updated_at"`
	RepoRoot                       string               `json:"repo_root"`
	RepoKey                        string               `json:"repo_key"`
	LastCommit                     string               `json:"last_commit,omitempty"`
	WorktreeMode                   string               `json:"worktree_mode"`
	Files                          []seedFileIndexEntry `json:"files"`
	DeterministicPacketFingerprint string               `json:"deterministic_packet_fingerprint"`
	QuickAgent                     seedAgentPhase       `json:"quick_agent,omitempty"`
	DeepAgent                      seedAgentPhase       `json:"deep_agent,omitempty"`
}

type seedFileIndexEntry struct {
	Path      string `json:"path"`
	Category  string `json:"category"`
	Bytes     int64  `json:"bytes,omitempty"`
	Hash      string `json:"hash,omitempty"`
	Included  bool   `json:"included"`
	Reason    string `json:"reason"`
	SeedPath  string `json:"seed_path,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type seedScanResult struct {
	RepoDir     string
	RepoKey     string
	Commit      string
	Files       []seedFileIndexEntry
	Docs        []seedDocument
	Entrypoints []string
	Commands    []seedCommand
	Coverage    *seedHistoryCoverage
	Warnings    []string
	Fingerprint string
}

func newSeedCommand(opts Options) *cobra.Command {
	seedOpts := seedCommandOptions{
		includeTests:       true,
		maxFileBytes:       defaultSeedMaxFileBytes,
		maxFiles:           defaultSeedMaxFiles,
		format:             "markdown+json",
		agent:              "none",
		agentQuickTimeout:  2 * time.Minute,
		agentDeepTimeout:   10 * time.Minute,
		agentTimeoutAction: "keep-quick",
		agentMaxInputBytes: defaultAgentMaxInput,
	}
	cmd := &cobra.Command{
		Use:   "seed [path]",
		Short: "Seed a brain from repository docs and code structure",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSeed(cmd.Context(), cmd, opts, seedOpts, target)
		},
	}
	cmd.Flags().StringVarP(&seedOpts.outputDir, "output", "o", "", "Output directory for seed (default: persistent brain directory)")
	cmd.Flags().BoolVar(&seedOpts.update, "update", false, "Refresh an existing seed")
	cmd.Flags().BoolVar(&seedOpts.force, "force", false, "Overwrite existing seed output")
	cmd.Flags().BoolVar(&seedOpts.includeTests, "include-tests", true, "Include tests in inventory and summaries")
	cmd.Flags().IntVar(&seedOpts.maxFileBytes, "max-file-bytes", defaultSeedMaxFileBytes, "Maximum bytes to copy from any source/doc file")
	cmd.Flags().IntVar(&seedOpts.maxFiles, "max-files", defaultSeedMaxFiles, "Maximum files to scan")
	cmd.Flags().StringVar(&seedOpts.format, "format", "markdown+json", "Seed output format")
	cmd.Flags().BoolVar(&seedOpts.worktree, "worktree", false, "Include selected untracked instruction/docs files")
	cmd.Flags().StringVar(&seedOpts.agent, "agent", "none", "Agent synthesis mode: none, command, codex, or claude-code")
	cmd.Flags().StringArrayVar(&seedOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().DurationVar(&seedOpts.agentQuickTimeout, "agent-quick-timeout", 2*time.Minute, "Timeout for quick agent synthesis")
	cmd.Flags().DurationVar(&seedOpts.agentDeepTimeout, "agent-deep-timeout", 10*time.Minute, "Timeout for deep agent synthesis")
	cmd.Flags().StringVar(&seedOpts.agentTimeoutAction, "agent-timeout-action", "keep-quick", "Deep timeout action: keep-quick, continue, or fail")
	cmd.Flags().IntVar(&seedOpts.agentMaxInputBytes, "agent-max-input-bytes", defaultAgentMaxInput, "Maximum bytes in agent input packet")
	cmd.Flags().BoolVar(&seedOpts.interactive, "interactive", false, "Allow timeout prompts when stdin is a terminal")
	cmd.Flags().BoolVar(&seedOpts.noInteractive, "no-interactive", false, "Disable timeout prompts")
	cmd.Flags().BoolVar(&seedOpts.requireAgent, "require-agent", false, "Fail if required agent synthesis does not complete")
	return cmd
}

func runSeed(ctx context.Context, cmd *cobra.Command, opts Options, seedOpts seedCommandOptions, target string) error {
	if seedOpts.maxFileBytes <= 0 {
		return errors.New("--max-file-bytes must be greater than zero")
	}
	if seedOpts.maxFiles <= 0 {
		return errors.New("--max-files must be greater than zero")
	}
	if seedOpts.format != "markdown+json" {
		return errors.New("--format must be markdown+json")
	}
	if seedOpts.agentTimeoutAction != "keep-quick" && seedOpts.agentTimeoutAction != "continue" && seedOpts.agentTimeoutAction != "fail" {
		return errors.New("--agent-timeout-action must be keep-quick, continue, or fail")
	}

	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("seed target must be an existing local path: %s", target)
	}

	outputExplicit := cmd.Flags().Changed("output")
	persistentSeed := !outputExplicit
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	outputDir := seedOpts.outputDir
	if !outputExplicit {
		outputDir = storage.BrainDir
	}
	if outputExplicit {
		if seedOpts.force {
			if err := os.RemoveAll(outputDir); err != nil {
				return fmt.Errorf("remove forced output directory: %w", err)
			}
		} else if _, err := validateExportDirAvailable(outputDir); err != nil {
			return err
		}
	}
	outputDir, err = prepareExportDir(outputDir, true)
	if err != nil {
		return err
	}

	scan, err := scanSeedRepository(ctx, opts.Runner, repoDir, storage.Key, seedOpts)
	if err != nil {
		return err
	}
	scan.Coverage = buildSeedHistoryCoverage(ctx, opts.Runner, repoDir, outputDir)
	if scan.Coverage != nil && scan.Coverage.MissingSessionCommits > 0 {
		scan.Warnings = append(scan.Warnings, fmt.Sprintf("%d commits after oldest session have no Entire checkpoint trailer; see seed/history-gaps.md", scan.Coverage.MissingSessionCommits))
	}
	if err := writeSeedArtifacts(outputDir, scan); err != nil {
		return err
	}

	seedManifest := &seedSourceManifest{
		GeneratedAt:       opts.Now().UTC(),
		Commit:            scan.Commit,
		WorktreeMode:      seedWorktreeMode(seedOpts),
		FileFingerprint:   scan.Fingerprint,
		SummaryPath:       filepath.ToSlash(filepath.Join(seedDirName, "repo-overview.md")),
		Documents:         scan.Docs,
		Entrypoints:       scan.Entrypoints,
		Commands:          scan.Commands,
		HistoryBaseline:   buildSeedHistoryBaseline(ctx, opts.Runner, repoDir, outputDir),
		HistoryCoverage:   scan.Coverage,
		Warnings:          scan.Warnings,
		DeterministicPath: []string{"seed/repo-overview.md", "seed/architecture.md", "seed/commands.md", "seed/conventions.md", "seed/risks.md", "seed/history-gaps.md", "seed/file-index.json"},
	}

	if seedOpts.agent != "none" {
		agentManifest, err := runSeedAgent(ctx, repoDir, outputDir, scan, seedOpts)
		if err != nil {
			if seedOpts.requireAgent {
				return err
			}
			seedManifest.Warnings = append(seedManifest.Warnings, err.Error())
		}
		if agentManifest != nil {
			seedManifest.Agent = agentManifest
		}
	}

	if err := writeBrainSeedSource(outputDir, storage.Key, seedManifest); err != nil {
		return err
	}
	if persistentSeed {
		cursor := seedCursor{
			SchemaVersion:                  1,
			UpdatedAt:                      seedManifest.GeneratedAt,
			RepoRoot:                       repoDir,
			RepoKey:                        storage.Key,
			LastCommit:                     scan.Commit,
			WorktreeMode:                   seedManifest.WorktreeMode,
			Files:                          scan.Files,
			DeterministicPacketFingerprint: scan.Fingerprint,
		}
		if seedManifest.Agent != nil {
			cursor.QuickAgent = seedManifest.Agent.Quick
			cursor.DeepAgent = seedManifest.Agent.Deep
		}
		if err := writeSeedCursor(storage.HeadPath, cursor); err != nil {
			return err
		}
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "seeded brain from %d files\n", len(scan.Files))
	fmt.Fprintf(out, "output: %s\n", outputDir)
	if len(seedManifest.Warnings) > 0 {
		fmt.Fprintf(out, "warnings: %d\n", len(seedManifest.Warnings))
	}
	return nil
}

func scanSeedRepository(ctx context.Context, runner CommandRunner, repoDir, repoKey string, opts seedCommandOptions) (seedScanResult, error) {
	paths, warnings := listSeedFiles(ctx, runner, repoDir, opts)
	if len(paths) > opts.maxFiles {
		warnings = append(warnings, fmt.Sprintf("file scan capped at %d files", opts.maxFiles))
		paths = paths[:opts.maxFiles]
	}
	commit := strings.TrimSpace(string(runGitOutput(ctx, runner, repoDir, "rev-parse", "HEAD")))
	result := seedScanResult{RepoDir: repoDir, RepoKey: repoKey, Commit: commit, Warnings: warnings}
	for _, rel := range paths {
		entry := inspectSeedFile(repoDir, rel, opts)
		result.Files = append(result.Files, entry)
		if !entry.Included {
			continue
		}
		if isHighSignalDoc(rel) {
			result.Docs = append(result.Docs, seedDocument{
				Path:      filepath.ToSlash(rel),
				SeedPath:  filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, rel)),
				Bytes:     entry.Bytes,
				Truncated: entry.Truncated,
				Reason:    entry.Reason,
			})
		}
		if isEntrypointPath(rel) {
			result.Entrypoints = append(result.Entrypoints, filepath.ToSlash(rel))
		}
		if filepath.Base(rel) == "package.json" {
			commands, err := packageJSONCommands(filepath.Join(repoDir, rel), rel)
			if err != nil {
				result.Warnings = append(result.Warnings, err.Error())
			}
			result.Commands = append(result.Commands, commands...)
		}
		if filepath.Base(rel) == "mise.toml" {
			commands, err := miseCommands(filepath.Join(repoDir, rel), rel)
			if err != nil {
				result.Warnings = append(result.Warnings, err.Error())
			}
			result.Commands = append(result.Commands, commands...)
		}
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	sort.Slice(result.Docs, func(i, j int) bool { return result.Docs[i].Path < result.Docs[j].Path })
	sort.Strings(result.Entrypoints)
	sort.Slice(result.Commands, func(i, j int) bool { return result.Commands[i].Name < result.Commands[j].Name })
	result.Fingerprint = seedFingerprint(result)
	return result, nil
}

func listSeedFiles(ctx context.Context, runner CommandRunner, repoDir string, opts seedCommandOptions) ([]string, []string) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", "ls-files")
	if err == nil {
		paths := splitNonEmptyLines(stdout)
		if opts.worktree {
			extra, _, extraErr := runner.Run(ctx, repoDir, "git", "ls-files", "--others", "--exclude-standard")
			if extraErr == nil {
				for _, path := range splitNonEmptyLines(extra) {
					if isSelectedUntrackedSeedFile(path) {
						paths = append(paths, path)
					}
				}
			}
		}
		sort.Strings(paths)
		return compactSortedStrings(paths), nil
	}
	var paths []string
	var warnings []string
	err = filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			warnings = append(warnings, err.Error())
			return nil
		}
		if path == repoDir {
			return nil
		}
		rel, relErr := filepath.Rel(repoDir, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() && isDeniedSeedDir(rel) {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		warnings = append(warnings, "walk repository: "+err.Error())
	}
	sort.Strings(paths)
	return paths, warnings
}

func splitNonEmptyLines(data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(filepath.ToSlash(line))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func inspectSeedFile(repoDir, rel string, opts seedCommandOptions) seedFileIndexEntry {
	entry := seedFileIndexEntry{Path: filepath.ToSlash(rel), Category: seedCategory(rel), Included: true, Reason: "included"}
	if isDeniedSeedPath(rel) {
		entry.Included = false
		entry.Reason = "denied path"
		return entry
	}
	if !opts.includeTests && isTestPath(rel) {
		entry.Included = false
		entry.Reason = "tests excluded"
		return entry
	}
	info, err := os.Stat(filepath.Join(repoDir, rel))
	if err != nil {
		entry.Included = false
		entry.Reason = "stat failed"
		return entry
	}
	entry.Bytes = info.Size()
	if info.Size() > int64(opts.maxFileBytes) {
		entry.Truncated = true
		if !isHighSignalDoc(rel) {
			entry.Included = false
			entry.Reason = "too large"
			return entry
		}
		entry.Reason = "included truncated"
	}
	if isLikelyBinaryPath(rel) {
		entry.Included = false
		entry.Reason = "binary or media"
		return entry
	}
	data, err := os.ReadFile(filepath.Join(repoDir, rel))
	if err == nil {
		if bytes.IndexByte(data, 0) >= 0 {
			entry.Included = false
			entry.Reason = "binary content"
			return entry
		}
		sum := sha256.Sum256(data)
		entry.Hash = "sha256:" + hex.EncodeToString(sum[:])
	}
	return entry
}

func seedCategory(path string) string {
	path = filepath.ToSlash(path)
	switch {
	case isHighSignalDoc(path):
		return "docs"
	case isTestPath(path):
		return "tests"
	case strings.HasPrefix(path, ".github/") || strings.HasPrefix(path, ".codex/") || strings.HasPrefix(path, ".cursor/"):
		return "config"
	case strings.HasPrefix(path, "src/") || strings.HasPrefix(path, "internal/") || strings.HasPrefix(path, "cmd/"):
		return "source"
	default:
		return "other"
	}
}

func isDeniedSeedPath(path string) bool {
	path = filepath.ToSlash(path)
	if isDeniedSeedDir(path) || strings.Contains(path, "/.git/") {
		return true
	}
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.Contains(base, "credential") || strings.Contains(base, "secret") || strings.Contains(base, "token") {
		return true
	}
	switch filepath.Ext(base) {
	case ".pem", ".key", ".p12", ".pfx", ".crt", ".cer":
		return true
	}
	if base == "package-lock.json" || base == "yarn.lock" || base == "pnpm-lock.yaml" || base == "go.sum" {
		return true
	}
	return false
}

func isDeniedSeedDir(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, part := range parts {
		switch part {
		case ".git", "node_modules", "dist", "build", "coverage", "vendor", ".next", ".turbo":
			return true
		}
	}
	return false
}

func isLikelyBinaryPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".tar", ".tgz", ".mp4", ".mov", ".woff", ".woff2", ".ttf", ".otf":
		return true
	default:
		return false
	}
}

func isHighSignalDoc(path string) bool {
	path = filepath.ToSlash(path)
	base := strings.ToLower(filepath.Base(path))
	if strings.HasPrefix(base, "readme") || base == "claude.md" || base == "agents.md" || base == "contributing.md" || base == "security.md" {
		return true
	}
	return path == ".github/copilot-instructions.md" || strings.HasPrefix(path, "docs/") && strings.HasSuffix(strings.ToLower(path), ".md")
}

func isSelectedUntrackedSeedFile(path string) bool {
	return isHighSignalDoc(path) || strings.HasPrefix(filepath.ToSlash(path), ".codex/") || strings.HasPrefix(filepath.ToSlash(path), ".cursor/")
}

func isEntrypointPath(path string) bool {
	base := filepath.Base(path)
	if base == "main.go" || base == "server.js" || base == "index.js" || base == "index.ts" || base == "index.tsx" || base == "main.jsx" || base == "main.tsx" || base == "App.jsx" || base == "App.tsx" {
		return true
	}
	return strings.HasPrefix(filepath.ToSlash(path), "bin/")
}

func isTestPath(path string) bool {
	path = filepath.ToSlash(path)
	base := filepath.Base(path)
	return strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") || strings.Contains(path, "__tests__") || strings.Contains(base, "_test.") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

func packageJSONCommands(path, rel string) ([]seedCommand, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
		Bin     any               `json:"bin"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	var commands []seedCommand
	for name, script := range pkg.Scripts {
		commands = append(commands, seedCommand{Name: "npm run " + name, Command: script, Source: filepath.ToSlash(rel)})
	}
	return commands, nil
}

func miseCommands(path, rel string) ([]seedCommand, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	var commands []seedCommand
	var current string
	var collecting bool
	var multiline strings.Builder
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if collecting {
			if end, _, ok := strings.Cut(line, "'''"); ok {
				multiline.WriteString(strings.TrimSpace(end))
				commands = append(commands, seedCommand{Name: "mise run " + current, Command: strings.TrimSpace(multiline.String()), Source: filepath.ToSlash(rel)})
				collecting = false
				multiline.Reset()
				continue
			}
			multiline.WriteString(strings.TrimSpace(raw))
			multiline.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(line, "[tasks.") && strings.HasSuffix(line, "]") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "[tasks."), "]")
			name = strings.Trim(name, `"`)
			current = name
			continue
		}
		if current == "" || !strings.HasPrefix(line, "run") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "run" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "'''") {
			value = strings.TrimPrefix(value, "'''")
			if before, _, ok := strings.Cut(value, "'''"); ok {
				commands = append(commands, seedCommand{Name: "mise run " + current, Command: strings.TrimSpace(before), Source: filepath.ToSlash(rel)})
				continue
			}
			collecting = true
			multiline.WriteString(strings.TrimSpace(value))
			if strings.TrimSpace(value) != "" {
				multiline.WriteByte('\n')
			}
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		commands = append(commands, seedCommand{Name: "mise run " + current, Command: value, Source: filepath.ToSlash(rel)})
	}
	return commands, nil
}

func seedFingerprint(scan seedScanResult) string {
	var b strings.Builder
	b.WriteString(scan.Commit)
	for _, file := range scan.Files {
		b.WriteString("\n")
		b.WriteString(file.Path)
		b.WriteString("\x00")
		b.WriteString(file.Hash)
		b.WriteString("\x00")
		b.WriteString(file.Reason)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeSeedArtifacts(outputDir string, scan seedScanResult) error {
	if err := os.MkdirAll(filepath.Join(outputDir, seedDirName, seedDocsDirName), 0o700); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(outputDir, seedDirName, "file-index.json"), scan.Files); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, seedDirName, "repo-overview.md"), []byte(renderSeedOverview(scan)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, seedDirName, "architecture.md"), []byte(renderSeedArchitecture(scan)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, seedDirName, "commands.md"), []byte(renderSeedCommands(scan)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, seedDirName, "conventions.md"), []byte(renderSeedConventions(scan)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, seedDirName, "risks.md"), []byte(renderSeedRisks(scan)), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, seedDirName, "history-gaps.md"), []byte(renderSeedHistoryGaps(scan)), 0o600); err != nil {
		return err
	}
	for _, doc := range scan.Docs {
		src := filepath.Join(scan.RepoDir, filepath.FromSlash(doc.Path))
		dst := filepath.Join(outputDir, filepath.FromSlash(doc.SeedPath))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if len(data) > defaultSeedMaxFileBytes {
			data = data[:defaultSeedMaxFileBytes]
			data = append(data, []byte("\n\n[truncated]\n")...)
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func renderSeedOverview(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Seeded Repository Overview")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Repo: `%s`\n", scan.RepoDir)
	if scan.Commit != "" {
		fmt.Fprintf(&b, "- Commit: `%s`\n", scan.Commit)
	}
	fmt.Fprintf(&b, "- Files indexed: %d\n", len(scan.Files))
	fmt.Fprintf(&b, "- Documents copied: %d\n", len(scan.Docs))
	fmt.Fprintf(&b, "- Entrypoints detected: %d\n", len(scan.Entrypoints))
	fmt.Fprintf(&b, "- Commands detected: %d\n", len(scan.Commands))
	if scan.Coverage != nil {
		fmt.Fprintf(&b, "- Commits without session coverage after oldest session: %d\n", scan.Coverage.MissingSessionCommits)
	}
	return b.String()
}

func renderSeedArchitecture(scan seedScanResult) string {
	counts := make(map[string]int)
	for _, file := range scan.Files {
		counts[file.Category]++
	}
	var b strings.Builder
	fmt.Fprintln(&b, "# Architecture")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## File Categories")
	fmt.Fprintln(&b)
	for _, key := range sortedMapKeys(counts) {
		fmt.Fprintf(&b, "- %s: %d\n", key, counts[key])
	}
	if len(scan.Entrypoints) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Entrypoints")
		fmt.Fprintln(&b)
		for _, path := range scan.Entrypoints {
			fmt.Fprintf(&b, "- `%s`\n", path)
		}
	}
	return b.String()
}

func renderSeedCommands(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Commands")
	fmt.Fprintln(&b)
	if len(scan.Commands) == 0 {
		fmt.Fprintln(&b, "No commands were detected from recognized project metadata.")
		return b.String()
	}
	for _, command := range scan.Commands {
		fmt.Fprintf(&b, "- `%s`: `%s` (%s)\n", command.Name, command.Command, command.Source)
	}
	return b.String()
}

func renderSeedConventions(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Conventions")
	fmt.Fprintln(&b)
	if len(scan.Docs) == 0 {
		fmt.Fprintln(&b, "No high-signal convention documents were detected.")
		return b.String()
	}
	fmt.Fprintln(&b, "Review these copied documents first:")
	for _, doc := range scan.Docs {
		fmt.Fprintf(&b, "- `%s`\n", doc.SeedPath)
	}
	return b.String()
}

func renderSeedRisks(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Risks And Gaps")
	fmt.Fprintln(&b)
	for _, warning := range scan.Warnings {
		fmt.Fprintf(&b, "- %s\n", warning)
	}
	var skipped int
	for _, file := range scan.Files {
		if !file.Included {
			skipped++
		}
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "- %d files were skipped. See `file-index.json` for reasons.\n", skipped)
	}
	if scan.Coverage != nil && scan.Coverage.MissingSessionCommits > 0 {
		fmt.Fprintf(&b, "- %d commits after the oldest exported session have no Entire checkpoint trailer. See `history-gaps.md`.\n", scan.Coverage.MissingSessionCommits)
	}
	if skipped == 0 && len(scan.Warnings) == 0 && (scan.Coverage == nil || scan.Coverage.MissingSessionCommits == 0) {
		fmt.Fprintln(&b, "No deterministic seed risks were detected.")
	}
	return b.String()
}

func renderSeedHistoryGaps(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# History Coverage")
	fmt.Fprintln(&b)
	if scan.Coverage == nil {
		fmt.Fprintln(&b, "Commit coverage could not be computed.")
		return b.String()
	}
	coverage := scan.Coverage
	fmt.Fprintf(&b, "- Total commits: %d\n", coverage.TotalCommits)
	fmt.Fprintf(&b, "- Pre-session commits: %d\n", coverage.PreSessionCommits)
	fmt.Fprintf(&b, "- Covered by exported session checkpoint: %d\n", coverage.CoveredCommits)
	fmt.Fprintf(&b, "- Checkpointed but not directly exported: %d\n", coverage.CheckpointedUnexportedCommits)
	fmt.Fprintf(&b, "- Missing session coverage after oldest session: %d\n", coverage.MissingSessionCommits)
	fmt.Fprintf(&b, "- Commits with no session history available: %d\n", coverage.NoSessionHistoryCommits)
	fmt.Fprintf(&b, "- Merge commits: %d\n", coverage.MergeCommits)
	if coverage.OldestSessionAt != nil {
		fmt.Fprintf(&b, "- Oldest exported session: `%s`\n", coverage.OldestSessionAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "- Exported checkpoints known to manifest: %d\n", coverage.ExportedCheckpoints)
	fmt.Fprintln(&b)
	if len(coverage.UncoveredCommits) == 0 {
		fmt.Fprintln(&b, "No commits need fallback history coverage.")
		return b.String()
	}
	fmt.Fprintln(&b, "## Commits Needing Fallback Context")
	fmt.Fprintln(&b)
	for _, commit := range coverage.UncoveredCommits {
		hash := shortCommitHash(commit.Hash)
		fmt.Fprintf(&b, "- `%s` `%s` %s", hash, commit.CommittedAt.Format(time.RFC3339), commit.Subject)
		if commit.Merge {
			fmt.Fprint(&b, " (merge)")
		}
		fmt.Fprintf(&b, " [%s]\n", commit.Coverage)
		if len(commit.Checkpoints) > 0 {
			fmt.Fprintf(&b, "  Checkpoints: `%s`\n", strings.Join(commit.Checkpoints, "`, `"))
		}
		if commit.BodyExcerpt != "" && commit.BodyExcerpt != commit.Subject {
			fmt.Fprintf(&b, "  Notes: %s\n", commit.BodyExcerpt)
		}
	}
	return b.String()
}

func sortedMapKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func buildSeedHistoryBaseline(ctx context.Context, runner CommandRunner, repoDir, outputDir string) *seedHistoryBaseline {
	var baseline seedHistoryBaseline
	baseline.Confidence = "heuristic"
	for _, root := range splitNonEmptyLines(runGitOutput(ctx, runner, repoDir, "rev-list", "--max-parents=0", "HEAD")) {
		stdout := runGitOutput(ctx, runner, repoDir, "show", "-s", "--format=%aI", root)
		if text := strings.TrimSpace(string(stdout)); text != "" {
			if parsed, err := time.Parse(time.RFC3339, text); err == nil {
				parsed = parsed.UTC()
				if baseline.OldestCommitAt == nil || parsed.Before(*baseline.OldestCommitAt) {
					baseline.OldestCommitAt = &parsed
				}
			}
		}
	}
	manifest, err := loadBrainManifest(outputDir)
	if err == nil && manifest.Sources != nil && manifest.Sources.Sessions != nil {
		baseline.OldestSessionAt = manifest.Sources.Sessions.OldestSessionAt
	}
	switch {
	case baseline.OldestCommitAt != nil && baseline.OldestSessionAt != nil && baseline.OldestCommitAt.Before(*baseline.OldestSessionAt):
		baseline.SeedRequired = true
		baseline.Reason = "oldest commit predates oldest session"
	case baseline.OldestSessionAt == nil:
		baseline.SeedRequired = true
		baseline.Reason = "no session history available"
	default:
		baseline.Reason = "session history covers initial commit or timestamp unavailable"
	}
	return &baseline
}

func buildSeedHistoryCoverage(ctx context.Context, runner CommandRunner, repoDir, outputDir string) *seedHistoryCoverage {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return &seedHistoryCoverage{
			Path:          filepath.ToSlash(filepath.Join(seedDirName, "history-gaps.md")),
			GeneratedFrom: "git log",
		}
	}
	var sessions *sessionSourceManifest
	if manifest.Sources != nil {
		sessions = manifest.Sources.Sessions
	}
	exportedCheckpoints := make(map[string]struct{})
	var oldestSession *time.Time
	if sessions != nil {
		oldestSession = sessions.OldestSessionAt
		for _, session := range sessions.Sessions {
			if session.LatestCheckpoint != "" {
				exportedCheckpoints[session.LatestCheckpoint] = struct{}{}
			}
		}
	}

	coverage := &seedHistoryCoverage{
		Path:                filepath.ToSlash(filepath.Join(seedDirName, "history-gaps.md")),
		OldestSessionAt:     oldestSession,
		ExportedCheckpoints: len(exportedCheckpoints),
		GeneratedFrom:       "git log",
	}
	commits := parseSeedGitLog(runGitOutput(ctx, runner, repoDir, "log", "--reverse", "--format=%H%x00%P%x00%aI%x00%an%x00%ae%x00%B%x1e"))
	for _, commit := range commits {
		classifySeedCommitCoverage(&commit, oldestSession, exportedCheckpoints)
		coverage.TotalCommits++
		if commit.Merge {
			coverage.MergeCommits++
		}
		switch commit.Coverage {
		case "pre_session":
			coverage.PreSessionCommits++
		case "covered":
			coverage.CoveredCommits++
		case "checkpointed_unexported":
			coverage.CheckpointedUnexportedCommits++
			coverage.UncoveredCommits = append(coverage.UncoveredCommits, commit)
		case "missing_session":
			coverage.MissingSessionCommits++
			coverage.UncoveredCommits = append(coverage.UncoveredCommits, commit)
		case "no_session_history":
			coverage.NoSessionHistoryCommits++
			coverage.UncoveredCommits = append(coverage.UncoveredCommits, commit)
		}
	}
	return coverage
}

func parseSeedGitLog(data []byte) []seedCoveredCommit {
	var commits []seedCoveredCommit
	for _, raw := range strings.Split(string(data), gitLogRecordSeparator) {
		raw = strings.Trim(raw, "\n")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		fields := strings.SplitN(raw, gitLogFieldSeparator, 6)
		if len(fields) < 6 {
			continue
		}
		committedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[2]))
		if err != nil {
			continue
		}
		body := strings.TrimSpace(fields[5])
		parents := splitCommitParents(fields[1])
		commits = append(commits, seedCoveredCommit{
			Hash:        strings.TrimSpace(fields[0]),
			Parents:     parents,
			CommittedAt: committedAt.UTC(),
			AuthorName:  strings.TrimSpace(fields[3]),
			AuthorEmail: strings.TrimSpace(fields[4]),
			Subject:     firstCommitSubject(body),
			Checkpoints: uniqueStrings(checkpointTrailers(body)),
			Merge:       len(parents) > 1,
			BodyExcerpt: commitBodyExcerpt(body),
		})
	}
	return commits
}

func classifySeedCommitCoverage(commit *seedCoveredCommit, oldestSession *time.Time, exportedCheckpoints map[string]struct{}) {
	if oldestSession == nil {
		commit.Coverage = "no_session_history"
		return
	}
	if commit.CommittedAt.Before(*oldestSession) {
		commit.Coverage = "pre_session"
		return
	}
	for _, checkpoint := range commit.Checkpoints {
		if _, ok := exportedCheckpoints[checkpoint]; ok {
			commit.Coverage = "covered"
			return
		}
	}
	if len(commit.Checkpoints) > 0 {
		commit.Coverage = "checkpointed_unexported"
		return
	}
	commit.Coverage = "missing_session"
}

func splitCommitParents(value string) []string {
	return strings.Fields(strings.TrimSpace(value))
}

func checkpointTrailers(body string) []string {
	var checkpoints []string
	for _, match := range checkpointTrailerRegex.FindAllStringSubmatch(body, -1) {
		if len(match) > 1 {
			checkpoints = append(checkpoints, match[1])
		}
	}
	return checkpoints
}

func firstCommitSubject(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func commitBodyExcerpt(body string) string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, checkpointTrailerKey+":") {
			continue
		}
		lines = append(lines, line)
		if len(strings.Join(lines, " ")) >= 240 {
			break
		}
	}
	excerpt := strings.Join(lines, " ")
	if len(excerpt) > 240 {
		excerpt = excerpt[:240]
	}
	return excerpt
}

func shortCommitHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func seedWorktreeMode(opts seedCommandOptions) string {
	if opts.worktree {
		return "worktree"
	}
	return "tracked"
}

func writeSeedCursor(headPath string, cursor seedCursor) error {
	path := filepath.Join(filepath.Dir(headPath), seedCursorFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeJSONFile(path, cursor)
}

func renderCombinedBrainReadme(manifest exportManifest) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Entire Brain")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "This brain combines seeded repository context with Entire session history when available.")
	if manifest.Sources != nil && manifest.Sources.Seed != nil {
		seed := manifest.Sources.Seed
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Seeded Baseline")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Generated at: `%s`\n", seed.GeneratedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Summary: `%s`\n", seed.SummaryPath)
		fmt.Fprintf(&b, "- Documents: %d\n", len(seed.Documents))
		fmt.Fprintf(&b, "- Entrypoints: %d\n", len(seed.Entrypoints))
		fmt.Fprintf(&b, "- Commands: %d\n", len(seed.Commands))
		if seed.HistoryBaseline != nil {
			fmt.Fprintf(&b, "- Historical gap: %s (confidence: %s)\n", seed.HistoryBaseline.Reason, seed.HistoryBaseline.Confidence)
		}
		if seed.HistoryCoverage != nil {
			fmt.Fprintf(&b, "- Commit coverage: %d total, %d missing session coverage after oldest session, %d checkpointed but not directly exported (`%s`)\n", seed.HistoryCoverage.TotalCommits, seed.HistoryCoverage.MissingSessionCommits, seed.HistoryCoverage.CheckpointedUnexportedCommits, seed.HistoryCoverage.Path)
		}
		if seed.Agent != nil {
			fmt.Fprintf(&b, "- Agent quick status: %s\n", seed.Agent.Quick.Status)
			if seed.Agent.Deep.Status != "" {
				fmt.Fprintf(&b, "- Agent deep status: %s\n", seed.Agent.Deep.Status)
			}
		}
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		sessions := manifest.Sources.Sessions
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Session History")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Sessions: %d\n", len(sessions.Sessions))
		fmt.Fprintf(&b, "- Checkpoints scanned: %d\n", sessions.CheckpointsScanned)
		if sessions.OldestSessionAt != nil {
			fmt.Fprintf(&b, "- Oldest session: `%s`\n", sessions.OldestSessionAt.Format(time.RFC3339))
		}
		for _, branch := range sessions.Branches {
			label := branch.Branch
			if label == "" {
				label = "(unknown)"
			}
			fmt.Fprintf(&b, "- `%s`: %d sessions in `%s`\n", label, branch.SessionCount, branch.Directory)
		}
	}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		history := manifest.Sources.History
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## History Index")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Generated at: `%s`\n", history.GeneratedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Index: `%s`\n", history.IndexPath)
		fmt.Fprintf(&b, "- Records: %d\n", history.Records)
		fmt.Fprintf(&b, "- Decisions: %d\n", history.Decisions)
		fmt.Fprintf(&b, "- Learnings: %d\n", history.Learnings)
		fmt.Fprintf(&b, "- Validations: %d\n", history.Validations)
		fmt.Fprintf(&b, "- Tool calls: %d\n", history.ToolCalls)
	}
	if manifest.Sources != nil && manifest.Sources.Semantic != nil {
		semantic := manifest.Sources.Semantic
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Semantic Index")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Generated at: `%s`\n", semantic.GeneratedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Provider: `%s`", semantic.Provider)
		if semantic.ProviderVersion != "" {
			fmt.Fprintf(&b, " `%s`", semantic.ProviderVersion)
		}
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Schema: `%s`\n", semantic.SchemaVersion)
		fmt.Fprintf(&b, "- Snapshot: `%s`\n", semantic.SnapshotPath)
		fmt.Fprintf(&b, "- Symbols: %d\n", semantic.Symbols)
		fmt.Fprintf(&b, "- Relations: %d\n", semantic.Relations)
		if semantic.Commit != "" {
			fmt.Fprintf(&b, "- Commit: `%s`\n", semantic.Commit)
		}
		if semantic.Branch != "" {
			fmt.Fprintf(&b, "- Branch: `%s`\n", semantic.Branch)
		}
		fmt.Fprintln(&b, "- Intake: run `entire brain query <symbol> --json` then `entire brain context <symbol> --json` for task-specific semantic context.")
	}
	var warnings []string
	warnings = append(warnings, manifest.Warnings...)
	if manifest.Sources != nil && manifest.Sources.Seed != nil {
		warnings = append(warnings, manifest.Sources.Seed.Warnings...)
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		warnings = append(warnings, manifest.Sources.Sessions.Warnings...)
	}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		warnings = append(warnings, manifest.Sources.History.Warnings...)
	}
	if manifest.Sources != nil && manifest.Sources.Semantic != nil {
		for _, warning := range manifest.Sources.Semantic.Warnings {
			warnings = append(warnings, warning.Code)
		}
		for _, failure := range manifest.Sources.Semantic.PartialFailures {
			warnings = append(warnings, failure.Code)
		}
	}
	warnings = uniqueStrings(warnings)
	if len(warnings) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Warnings")
		fmt.Fprintln(&b)
		for _, warning := range warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}
	return b.String()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

type seedAgentInput struct {
	SchemaVersion          int                  `json:"schema_version"`
	Phase                  string               `json:"phase"`
	Repo                   map[string]string    `json:"repo"`
	Limits                 map[string]int       `json:"limits"`
	Inventory              []seedFileIndexEntry `json:"inventory"`
	Documents              []seedDocument       `json:"documents"`
	Metadata               map[string]any       `json:"metadata"`
	DeterministicSummaries map[string]string    `json:"deterministic_summaries"`
	QuickResult            map[string]string    `json:"quick_result,omitempty"`
}

type seedAgentOutput struct {
	SchemaVersion int               `json:"schema_version"`
	Status        string            `json:"status"`
	Model         string            `json:"model,omitempty"`
	Artifacts     map[string]string `json:"artifacts"`
	Warnings      []string          `json:"warnings,omitempty"`
}

func runSeedAgent(ctx context.Context, repoDir, outputDir string, scan seedScanResult, opts seedCommandOptions) (*seedAgentManifest, error) {
	manifest := &seedAgentManifest{Mode: opts.agent}
	quickInput, err := buildAgentInput("quick", repoDir, scan, nil, opts)
	if err != nil {
		return manifest, err
	}
	quick, quickArtifacts, err := runSeedAgentPhase(ctx, repoDir, outputDir, opts, "quick", opts.agentQuickTimeout, quickInput, []string{"quick-overview.md"})
	manifest.Quick = quick
	if err != nil {
		if opts.requireAgent {
			return manifest, err
		}
		manifest.Deep = seedAgentPhase{Status: "skipped", Timeout: opts.agentDeepTimeout.String()}
		return manifest, err
	}
	deepInput, err := buildAgentInput("deep", repoDir, scan, quickArtifacts, opts)
	if err != nil {
		return manifest, err
	}
	deep, _, err := runSeedAgentPhase(ctx, repoDir, outputDir, opts, "deep", opts.agentDeepTimeout, deepInput, []string{"overview.md", "architecture.md", "risks.md", "maintenance-guide.md", "open-questions.md"})
	manifest.Deep = deep
	if err != nil && opts.requireAgent {
		return manifest, err
	}
	return manifest, nil
}

func buildAgentInput(phase, repoDir string, scan seedScanResult, quick map[string]string, opts seedCommandOptions) ([]byte, error) {
	input := seedAgentInput{
		SchemaVersion: 1,
		Phase:         phase,
		Repo: map[string]string{
			"root":   repoDir,
			"key":    scan.RepoKey,
			"commit": scan.Commit,
		},
		Limits:    map[string]int{"max_output_bytes": defaultAgentMaxOutput},
		Inventory: scan.Files,
		Documents: scan.Docs,
		Metadata: map[string]any{
			"entrypoints":      scan.Entrypoints,
			"commands":         scan.Commands,
			"history_coverage": scan.Coverage,
		},
		DeterministicSummaries: map[string]string{
			"overview":      renderSeedOverview(scan),
			"architecture":  renderSeedArchitecture(scan),
			"commands":      renderSeedCommands(scan),
			"conventions":   renderSeedConventions(scan),
			"risks_and_gap": renderSeedRisks(scan),
			"history_gaps":  renderSeedHistoryGaps(scan),
		},
		QuickResult: quick,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(data) > opts.agentMaxInputBytes {
		return nil, fmt.Errorf("agent input packet exceeds --agent-max-input-bytes (%d > %d)", len(data), opts.agentMaxInputBytes)
	}
	return data, nil
}

func runSeedAgentPhase(ctx context.Context, repoDir, outputDir string, opts seedCommandOptions, phase string, timeout time.Duration, input []byte, required []string) (seedAgentPhase, map[string]string, error) {
	started := time.Now().UTC()
	phaseManifest := seedAgentPhase{Status: "running", Timeout: timeout.String(), StartedAt: started, InputFingerprint: bytesFingerprint(input)}
	args, err := seedAgentCommandArgs(repoDir, phase, opts)
	if err != nil {
		phaseManifest.Status = "failed"
		return phaseManifest, nil, err
	}
	for {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		command := exec.CommandContext(runCtx, args[0], args[1:]...)
		command.Dir = repoDir
		command.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		err = command.Run()
		cancel()
		phaseManifest.CompletedAt = time.Now().UTC()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			phaseManifest.Status = "timeout"
			action := opts.agentTimeoutAction
			if phase == "deep" {
				action = promptSeedTimeoutAction(opts, timeout, action)
			}
			phaseManifest.TimeoutAction = action
			if phase == "quick" {
				return phaseManifest, nil, fmt.Errorf("agent quick synthesis timed out after %s", timeout)
			}
			if action == "continue" {
				continue
			}
			if action == "fail" {
				return phaseManifest, nil, fmt.Errorf("agent %s synthesis timed out after %s", phase, timeout)
			}
			return phaseManifest, nil, nil
		}
		if err != nil {
			phaseManifest.Status = "failed"
			warning := strings.TrimSpace(stderr.String())
			if warning == "" {
				warning = strings.TrimSpace(stdout.String())
			}
			if warning != "" {
				phaseManifest.Warnings = append(phaseManifest.Warnings, truncateAgentWarning(warning))
			}
			return phaseManifest, nil, fmt.Errorf("agent %s synthesis failed: %w", phase, err)
		}
		if stdout.Len() > defaultAgentMaxOutput {
			phaseManifest.Status = "failed"
			return phaseManifest, nil, fmt.Errorf("agent %s output exceeds limit", phase)
		}
		var out seedAgentOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, nil, fmt.Errorf("parse agent %s output: %w", phase, err)
		}
		if out.Artifacts == nil {
			out.Artifacts = make(map[string]string)
		}
		if err := validateSeedAgentOutput(out, phase); err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, out.Artifacts, err
		}
		for _, name := range required {
			if _, ok := out.Artifacts[name]; !ok {
				phaseManifest.Status = "incomplete"
				return phaseManifest, out.Artifacts, fmt.Errorf("agent %s output missing required artifact %s", phase, name)
			}
		}
		if err := validateSeedAgentArtifactSet(out.Artifacts, required, phase); err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, out.Artifacts, err
		}
		written, err := writeAgentArtifacts(outputDir, out.Artifacts)
		if err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, out.Artifacts, err
		}
		phaseManifest.Status = "success"
		phaseManifest.Model = out.Model
		phaseManifest.Warnings = append(phaseManifest.Warnings, out.Warnings...)
		phaseManifest.OutputPaths = written
		return phaseManifest, out.Artifacts, nil
	}
}

func validateSeedAgentArtifactSet(artifacts map[string]string, required []string, phase string) error {
	allowed := make(map[string]struct{}, len(required))
	for _, name := range required {
		allowed[name] = struct{}{}
	}
	for name := range artifacts {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("agent %s output included unexpected artifact %s", phase, name)
		}
	}
	return nil
}

func validateSeedAgentOutput(out seedAgentOutput, phase string) error {
	if out.SchemaVersion != 1 {
		return fmt.Errorf("agent %s output has unsupported schema_version %d", phase, out.SchemaVersion)
	}
	if out.Status != "success" {
		return fmt.Errorf("agent %s output status %q is not success", phase, out.Status)
	}
	return nil
}

func truncateAgentWarning(warning string) string {
	const maxWarningBytes = 4000
	if len(warning) <= maxWarningBytes {
		return warning
	}
	return warning[:maxWarningBytes] + "\n[truncated]"
}

func promptSeedTimeoutAction(opts seedCommandOptions, timeout time.Duration, fallback string) string {
	if opts.noInteractive || !stdinIsTerminal() {
		return fallback
	}
	fmt.Fprintf(os.Stderr, "Deep synthesis has been running for %s. Keep quick result, continue, or fail?\n[k]eep quick / [c]ontinue / [f]ail: ", timeout)
	var response string
	if _, err := fmt.Fscanln(os.Stdin, &response); err != nil {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(response)) {
	case "c", "continue":
		return "continue"
	case "f", "fail":
		return "fail"
	default:
		return "keep-quick"
	}
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}

func seedAgentCommandArgs(repoDir, phase string, opts seedCommandOptions) ([]string, error) {
	switch opts.agent {
	case "command":
		if len(opts.agentCommand) == 0 {
			return nil, errors.New("--agent-command is required when --agent command")
		}
		return append([]string(nil), opts.agentCommand...), nil
	case "codex":
		return []string{"codex", "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", seedAgentPrompt(phase)}, nil
	case "claude-code":
		return []string{"claude", "--print", "--no-session-persistence", "--setting-sources", "user", "--strict-mcp-config", "--mcp-config", "{}", "--disable-slash-commands", "--permission-mode", "dontAsk", "--tools", "", "--system-prompt", seedAgentPrompt(phase)}, nil
	default:
		return nil, fmt.Errorf("unsupported --agent %q", opts.agent)
	}
}

func seedAgentPrompt(phase string) string {
	required := strings.Join(seedAgentRequiredArtifacts(phase), ", ")
	common := fmt.Sprintf(`Read the JSON seed packet on stdin and return only raw JSON with keys schema_version, status, model, artifacts, and warnings.
artifacts must be an object whose keys are artifact filenames and values are markdown content.
For phase %q, artifacts must include exactly these required filenames: %s.
Do not include markdown fences or prose outside the JSON object.

Use the deterministic summaries, documents list, inventory, commands, entrypoints, and history_coverage metadata.
Pay special attention to history gaps:
- distinguish pre-session commits, checkpointed-but-unexported commits, and missing_session commits.
- explain missing history as uncertainty, not as if session transcripts exist.
- use commit subjects/notes from history_gaps when describing likely development history.
- call out skipped files and denied paths only when they affect confidence.

Keep all artifacts factual, repo-specific, and useful for a future coding agent.
Do not invent APIs, test results, CI status, release process, or rationale not present in the packet.
Prefer concrete filenames, exported symbols, commands, renderer names, and documented constraints over generic advice.
Use compact markdown.`, phase, required)
	switch phase {
	case "quick":
		return common + `

quick-overview.md must contain these sections:
# Quick Overview
## Project
## Entry Points
## Core Modules
## Commands
## History Coverage
## Immediate Risks`
	case "deep":
		return common + `

overview.md must summarize purpose, public surface, commands, tests, docs, and history confidence.
architecture.md must explain module boundaries and data flow across the actual files in the packet.
risks.md must rank concrete code/process risks and include history-gap risk separately.
maintenance-guide.md must tell a future agent where to edit, what to run, and what parity checks matter.
open-questions.md must list only questions supported by packet evidence; include any history backfill question caused by missing_session commits.`
	default:
		return common
	}
}

func seedAgentRequiredArtifacts(phase string) []string {
	switch phase {
	case "quick":
		return []string{"quick-overview.md"}
	case "deep":
		return []string{"overview.md", "architecture.md", "risks.md", "maintenance-guide.md", "open-questions.md"}
	default:
		return nil
	}
}

func writeAgentArtifacts(outputDir string, artifacts map[string]string) ([]string, error) {
	var written []string
	for name, content := range artifacts {
		clean := filepath.Clean(name)
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("reject unsafe agent artifact path %q", name)
		}
		rel := filepath.Join(seedDirName, seedAgentDirName, clean)
		abs := filepath.Join(outputDir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			return nil, err
		}
		written = append(written, filepath.ToSlash(rel))
	}
	sort.Strings(written)
	return written, nil
}

func bytesFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
