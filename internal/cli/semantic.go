package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	semanticDirName        = "semantic"
	semanticSnapshotsDir   = "snapshots"
	semanticBundleDir      = "bundles"
	semanticAuditLogName   = "audit.jsonl"
	semanticLockDir        = "locks"
	semanticIndexLockName  = "index.lock"
	semanticSnapshotName   = "snapshot.ndjson"
	semanticManifestName   = "manifest.json"
	semanticSupportedMajor = "1"
)

var (
	semanticBundleMaxEntry int64 = 128 * 1024 * 1024
	semanticBundleMaxTotal int64 = 512 * 1024 * 1024
	semanticBundleMaxFiles       = 10000
	semanticMaxRecordBytes       = 64 * 1024 * 1024
)

type semanticSourceManifest struct {
	GeneratedAt      time.Time         `json:"generated_at"`
	Commit           string            `json:"commit,omitempty"`
	Tree             string            `json:"tree,omitempty"`
	Branch           string            `json:"branch,omitempty"`
	DefaultBranch    string            `json:"default_branch,omitempty"`
	DirtyWorktree    bool              `json:"dirty_worktree"`
	Provider         string            `json:"provider,omitempty"`
	ProviderVersion  string            `json:"provider_version,omitempty"`
	SchemaVersion    string            `json:"schema_version,omitempty"`
	SnapshotPath     string            `json:"snapshot_path,omitempty"`
	Symbols          int               `json:"symbols"`
	Relations        int               `json:"relations"`
	Warnings         []semanticWarning `json:"warnings,omitempty"`
	PartialFailures  []semanticWarning `json:"partial_failures,omitempty"`
	Capabilities     []string          `json:"capabilities,omitempty"`
	NoEgressVerified bool              `json:"no_egress_verified"`
	WorktreeMode     string            `json:"worktree_mode,omitempty"`
	WorktreeHash     string            `json:"worktree_hash,omitempty"`
}

type semanticWarning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Effect   string `json:"effect,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type semanticHeader struct {
	SchemaVersion   string            `json:"schema_version"`
	Provider        string            `json:"provider"`
	ProviderVersion string            `json:"provider_version"`
	RepoRoot        string            `json:"repo_root"`
	RepoKey         string            `json:"repo_key"`
	Commit          string            `json:"commit"`
	Tree            string            `json:"tree"`
	Languages       []string          `json:"languages"`
	Capabilities    []string          `json:"capabilities"`
	Warnings        []semanticWarning `json:"warnings"`
	PartialFailures []semanticWarning `json:"partial_failures"`
}

type semanticRecord struct {
	RecordType      string   `json:"record_type"`
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Name            string   `json:"name"`
	QualifiedName   string   `json:"qualified_name"`
	FilePath        string   `json:"file_path"`
	StartLine       int      `json:"start_line"`
	EndLine         int      `json:"end_line"`
	Path            string   `json:"path"`
	Signature       string   `json:"signature"`
	Language        string   `json:"language"`
	FromID          string   `json:"from_id"`
	ToID            string   `json:"to_id"`
	Type            string   `json:"type"`
	WarningCodes    []string `json:"warning_codes"`
	Confidence      float64  `json:"confidence"`
	Reason          string   `json:"reason"`
	StableIDVersion string   `json:"stable_id_version"`
}

type semanticIndexOptions struct {
	force     bool
	semBinary string
	skipSem   bool
	worktree  bool
}

func newSemanticIndexCommand(opts Options) *cobra.Command {
	indexOpts := semanticIndexOptions{semBinary: "entire"}
	cmd := &cobra.Command{
		Use:   "index [path]",
		Short: "Build a local semantic brain index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSemanticIndex(cmd.Context(), cmd, opts, indexOpts, target)
		},
	}
	cmd.Flags().BoolVar(&indexOpts.force, "force", false, "Replace the current semantic snapshot even when one exists")
	cmd.Flags().StringVar(&indexOpts.semBinary, "sem-binary", "entire", "Entire CLI binary that exposes `sem` provider commands")
	cmd.Flags().BoolVar(&indexOpts.skipSem, "skip-sem", false, "Record semantic metadata without invoking the semantic provider")
	cmd.Flags().BoolVar(&indexOpts.worktree, "worktree", false, "Index the dirty worktree instead of committed HEAD")
	return cmd
}

func runSemanticIndex(ctx context.Context, cmd *cobra.Command, opts Options, indexOpts semanticIndexOptions, target string) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("semantic index requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	existing, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if !indexOpts.force && existing.Sources != nil && existing.Sources.Semantic != nil {
		return errors.New("semantic index already exists; use --force to replace it")
	}

	ignore, err := loadBrainIgnore(repoDir)
	if err != nil {
		return err
	}
	head, err := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolve HEAD for semantic index: %w", err)
	}
	tree, err := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return fmt.Errorf("resolve HEAD tree for semantic index: %w", err)
	}
	branch, _ := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current")
	defaultBranch, defaultWarnings := detectDefaultBranch(ctx, opts.Runner, repoDir)
	dirty, err := worktreeDirty(ctx, opts.Runner, repoDir)
	if err != nil {
		return fmt.Errorf("check worktree dirtiness for semantic index: %w", err)
	}
	if indexOpts.worktree {
		return errors.New("worktree indexing is not supported until the semantic provider exposes an explicit worktree snapshot mode")
	}
	if dirty && !indexOpts.worktree {
		return errors.New("dirty_worktree: refusing to index uncommitted content without --worktree")
	}

	var raw []byte
	var header semanticHeader
	counts := semanticCounts{}
	noEgress := false
	var warnings []semanticWarning
	for _, warning := range defaultWarnings {
		warnings = append(warnings, semanticWarning{Code: "default_branch_unknown", Severity: "warning", Effect: "freshness", Detail: warning})
	}
	if indexOpts.skipSem {
		header = semanticHeader{
			SchemaVersion: "1.0",
			Provider:      "skipped",
			RepoRoot:      repoDir,
			RepoKey:       storage.Key,
			Commit:        head,
			Tree:          tree,
			Warnings:      []semanticWarning{{Code: "provider_skipped", Severity: "warning", Effect: "semantic facts unavailable", Detail: "semantic provider was skipped"}},
		}
		raw, err = json.Marshal(header)
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		warnings = append(warnings, header.Warnings...)
	} else {
		if strings.TrimSpace(indexOpts.semBinary) == "" {
			return errors.New("--sem-binary must not be empty")
		}
		var doctorWarnings []semanticWarning
		noEgress, doctorWarnings = runSemanticDoctor(ctx, opts.Runner, repoDir, indexOpts.semBinary)
		warnings = append(warnings, doctorWarnings...)
		if !noEgress {
			code := "provider_no_egress_unverified"
			if len(doctorWarnings) > 0 && doctorWarnings[0].Code != "" {
				code = doctorWarnings[0].Code
			}
			return fmt.Errorf("%s: semantic provider no-egress status is not verified", code)
		}
		raw, err = runSemanticSnapshot(ctx, opts.Runner, repoDir, indexOpts.semBinary)
		if err != nil {
			return err
		}
		header, counts, raw, err = filterSemanticSnapshot(raw, ignore)
		if err != nil {
			return err
		}
		if err := validateSemanticSchema(header.SchemaVersion); err != nil {
			return err
		}
		if err := validateLiveSemanticHeader(header, storage.Key, head, tree); err != nil {
			return err
		}
		warnings = append(warnings, header.Warnings...)
	}
	if header.Commit == "" {
		header.Commit = head
	}
	if header.Tree == "" {
		header.Tree = tree
	}
	if header.Provider == "" {
		header.Provider = "entire-sem"
	}
	if header.RepoKey == "" {
		header.RepoKey = storage.Key
	}

	snapshotID := header.Commit
	worktreeMode := "head"
	worktreeHash := ""
	sum := sha256.Sum256(raw)
	if indexOpts.worktree {
		snapshotID = "worktree-" + hex.EncodeToString(sum[:8])
		worktreeMode = "worktree"
		worktreeHash = worktreeFingerprint(ctx, opts.Runner, repoDir)
	} else if snapshotID == "" {
		snapshotID = "unknown"
	} else {
		snapshotID = snapshotID + "-" + hex.EncodeToString(sum[:8])
	}
	snapshotRel := filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir, snapshotID, semanticSnapshotName))
	snapshotPath := filepath.Join(storage.BrainDir, filepath.FromSlash(snapshotRel))
	if err := rejectExistingSymlinkPathComponents(storage.BrainDir, filepath.FromSlash(snapshotRel)); err != nil {
		return err
	}
	if err := writeFileAtomic(snapshotPath, raw, 0o600); err != nil {
		return fmt.Errorf("write semantic snapshot: %w", err)
	}
	source := &semanticSourceManifest{
		GeneratedAt:      opts.Now().UTC(),
		Commit:           header.Commit,
		Tree:             header.Tree,
		Branch:           branch,
		DefaultBranch:    defaultBranch,
		DirtyWorktree:    dirty,
		Provider:         header.Provider,
		ProviderVersion:  header.ProviderVersion,
		SchemaVersion:    header.SchemaVersion,
		SnapshotPath:     snapshotRel,
		Symbols:          counts.Symbols,
		Relations:        counts.Relations,
		Warnings:         warnings,
		PartialFailures:  header.PartialFailures,
		Capabilities:     header.Capabilities,
		NoEgressVerified: noEgress || indexOpts.skipSem,
		WorktreeMode:     worktreeMode,
		WorktreeHash:     worktreeHash,
	}
	if err := writeBrainSemanticSource(storage.BrainDir, storage.Key, source); err != nil {
		return err
	}
	if err := writeSemanticSnapshotManifest(storage.BrainDir, snapshotID, source); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "indexed semantic brain: %s\n", storage.BrainDir)
	fmt.Fprintf(cmd.OutOrStdout(), "snapshot: %s\n", snapshotRel)
	fmt.Fprintf(cmd.OutOrStdout(), "symbols: %d\nrelations: %d\n", source.Symbols, source.Relations)
	if len(source.Warnings) > 0 || len(source.PartialFailures) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "warnings: %d\n", len(source.Warnings)+len(source.PartialFailures))
	}
	return nil
}

type semanticCounts struct {
	Symbols   int
	Relations int
}

func writeBrainSemanticSource(outputDir, repoKey string, semantic *semanticSourceManifest) error {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.SchemaVersion = brainManifestSchemaVersion
	manifest.RepoKey = repoKey
	manifest.GeneratedAt = semantic.GeneratedAt
	manifest.Sources.Semantic = semantic
	applySessionSourceAliases(manifest)
	return writeBrainManifestAndReadme(outputDir, *manifest)
}

func acquireSemanticIndexLock(brainDir string) (func(), error) {
	lockDir := filepath.Join(brainDir, semanticLockDir)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create semantic lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, semanticIndexLockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("index_locked: semantic index lock exists at %s", lockPath)
		}
		return nil, fmt.Errorf("create semantic index lock: %w", err)
	}
	_, _ = fmt.Fprintf(f, "pid=%d\ncreated_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	_ = f.Close()
	return func() { _ = os.Remove(lockPath) }, nil
}

func runSemanticDoctor(ctx context.Context, runner CommandRunner, repoDir, semBinary string) (bool, []semanticWarning) {
	stdout, _, err := runner.Run(ctx, repoDir, semBinary, "sem", "doctor", "--json")
	if err != nil {
		return false, []semanticWarning{{Code: "provider_doctor_failed", Severity: "warning", Effect: "provider diagnostics unavailable", Detail: err.Error()}}
	}
	var data map[string]any
	if err := json.Unmarshal(stdout, &data); err != nil {
		return false, []semanticWarning{{Code: "provider_doctor_malformed", Severity: "warning", Effect: "provider diagnostics unavailable", Detail: err.Error()}}
	}
	if boolValue(data, "network_egress") || boolValue(data, "requires_network") || boolValue(data, "network_required") {
		return false, []semanticWarning{{Code: "provider_network_required", Severity: "error", Effect: "phase 1 local-only guarantee not verified", Detail: "provider doctor reports network access is required"}}
	}
	if boolValue(data, "no_egress") || boolValue(data, "no_egress_verified") || boolValue(data, "local_only") {
		return true, nil
	}
	return false, []semanticWarning{{Code: "provider_no_egress_unknown", Severity: "warning", Effect: "phase 1 local-only guarantee not fully verified", Detail: "provider doctor did not report no-egress status"}}
}

func boolValue(data map[string]any, key string) bool {
	value, ok := data[key]
	if !ok {
		return false
	}
	b, _ := value.(bool)
	return b
}

func runSemanticSnapshot(ctx context.Context, runner CommandRunner, repoDir, semBinary string) ([]byte, error) {
	stdout, _, err := runner.Run(ctx, repoDir, semBinary, "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")
	if err != nil {
		return nil, fmt.Errorf("semantic provider snapshot failed: %w", err)
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		return nil, errors.New("semantic provider snapshot produced no output")
	}
	return stdout, nil
}

func validateLiveSemanticHeader(header semanticHeader, repoKey, commit, tree string) error {
	if header.Commit == "" {
		return errors.New("semantic snapshot header missing commit")
	}
	if header.Tree == "" {
		return errors.New("semantic snapshot header missing tree")
	}
	if header.RepoKey == "" {
		return errors.New("semantic snapshot header missing repo_key")
	}
	if header.RepoKey != repoKey {
		return fmt.Errorf("semantic snapshot repo_key %q does not match current repo %q", header.RepoKey, repoKey)
	}
	if header.Commit != "" && commit != "" && header.Commit != commit {
		return fmt.Errorf("semantic snapshot commit %q does not match HEAD %q", header.Commit, commit)
	}
	if header.Tree != "" && tree != "" && header.Tree != tree {
		return fmt.Errorf("semantic snapshot tree %q does not match HEAD tree %q", header.Tree, tree)
	}
	return nil
}

func filterSemanticSnapshot(raw []byte, ignore brainIgnore) (semanticHeader, semanticCounts, []byte, error) {
	scanner := newSemanticScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, err
		}
		return semanticHeader{}, semanticCounts{}, nil, errors.New("semantic snapshot missing header")
	}
	var header semanticHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot header: %w", err)
	}
	header.Warnings = ignore.FilterWarnings(header.Warnings)
	header.PartialFailures = ignore.FilterWarnings(header.PartialFailures)
	headerLine, marshalErr := json.Marshal(header)
	if marshalErr != nil {
		return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("encode filtered semantic snapshot header: %w", marshalErr)
	}
	ignoredIDs := make(map[string]struct{})
	line := 1
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		if ignore.Ignored(record.semanticPath()) && record.ID != "" {
			ignoredIDs[record.ID] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, err
	}
	var filtered bytes.Buffer
	filtered.Write(headerLine)
	filtered.WriteByte('\n')
	var counts semanticCounts
	scanner = newSemanticScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		return semanticHeader{}, semanticCounts{}, nil, errors.New("semantic snapshot missing header")
	}
	line = 1
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		if ignore.Ignored(record.semanticPath()) {
			continue
		}
		if record.RecordType == "relation" {
			if ignore.MentionsIgnoredPath(record.FromID) || ignore.MentionsIgnoredPath(record.ToID) {
				continue
			}
			if _, ok := ignoredIDs[record.FromID]; ok {
				continue
			}
			if _, ok := ignoredIDs[record.ToID]; ok {
				continue
			}
		}
		switch record.RecordType {
		case "symbol":
			counts.Symbols++
		case "relation":
			counts.Relations++
		}
		filtered.Write(text)
		filtered.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, err
	}
	return header, counts, filtered.Bytes(), nil
}

func newSemanticScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), semanticMaxRecordBytes)
	return scanner
}

func (r semanticRecord) semanticPath() string {
	if r.FilePath != "" {
		return r.FilePath
	}
	return r.Path
}

func validateSemanticSchema(version string) error {
	major, _, ok := strings.Cut(version, ".")
	if !ok {
		return fmt.Errorf("unsupported semantic schema version %q", version)
	}
	if major != semanticSupportedMajor {
		return fmt.Errorf("unsupported semantic schema major version %q", major)
	}
	return nil
}

func writeSemanticSnapshotManifest(brainDir, snapshotID string, source *semanticSourceManifest) error {
	data, err := json.MarshalIndent(source, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	rel := filepath.Join(semanticDirName, semanticSnapshotsDir, snapshotID, semanticManifestName)
	if err := rejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(brainDir, rel), data, 0o600)
}

type brainIgnore struct {
	patterns []string
}

func loadBrainIgnore(repoDir string) (brainIgnore, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, ".brainignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return brainIgnore{}, nil
		}
		return brainIgnore{}, fmt.Errorf("read .brainignore: %w", err)
	}
	var ignore brainIgnore
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ignore.patterns = append(ignore.patterns, filepath.ToSlash(line))
	}
	return ignore, nil
}

func (i brainIgnore) Ignored(path string) bool {
	path = filepath.ToSlash(strings.TrimPrefix(path, "./"))
	if path == ".git" || strings.HasPrefix(path, ".git/") {
		return true
	}
	for _, segment := range []string{"node_modules", "vendor", "dist", "build", "target", ".entire"} {
		if pathHasSegment(path, segment) {
			return true
		}
	}
	for _, pattern := range []string{".env", ".pem", ".key"} {
		if path == pattern || strings.Contains(path, "/"+pattern) || strings.HasSuffix(path, pattern) {
			return true
		}
	}
	for _, pattern := range i.patterns {
		if pattern == path || strings.HasPrefix(path, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
		if ok, _ := filepath.Match(pattern, path); ok {
			return true
		}
	}
	return false
}

func pathHasSegment(path, segment string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == segment {
			return true
		}
	}
	return false
}

func (i brainIgnore) FilterWarnings(warnings []semanticWarning) []semanticWarning {
	if len(warnings) == 0 {
		return nil
	}
	filtered := make([]semanticWarning, 0, len(warnings))
	for _, warning := range warnings {
		if i.Ignored(warning.Path) || i.MentionsIgnoredPath(warning.Detail) || i.MentionsIgnoredPath(warning.Effect) {
			continue
		}
		filtered = append(filtered, warning)
	}
	return filtered
}

func (i brainIgnore) MentionsIgnoredPath(text string) bool {
	text = filepath.ToSlash(text)
	if text == "" {
		return false
	}
	for _, pattern := range append([]string{".env", ".pem", ".key"}, i.patterns...) {
		pattern = strings.Trim(filepath.ToSlash(pattern), "/")
		if pattern != "" && strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

func gitScalar(ctx context.Context, runner CommandRunner, repoDir string, args ...string) (string, error) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(stdout)), nil
}

func worktreeDirty(ctx context.Context, runner CommandRunner, repoDir string) (bool, error) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(stdout)) != "", nil
}

func worktreeFingerprint(ctx context.Context, runner CommandRunner, repoDir string) string {
	status, _, _ := runner.Run(ctx, repoDir, "git", "status", "--porcelain")
	diff, _, _ := runner.Run(ctx, repoDir, "git", "diff", "--binary", "HEAD")
	cached, _, _ := runner.Run(ctx, repoDir, "git", "diff", "--cached", "--binary", "HEAD")
	content := append(append(status, diff...), cached...)
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type staleReport struct {
	Severity string               `json:"severity"`
	Axes     map[string]staleAxis `json:"axes"`
	Warnings []semanticWarning    `json:"warnings,omitempty"`
}

type staleAxis struct {
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	Current string `json:"current,omitempty"`
	Indexed string `json:"indexed,omitempty"`
}

func newSemanticStaleCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stale [path]",
		Short: "Report semantic brain freshness",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSemanticStale(cmd.Context(), cmd, opts, target, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runSemanticStale(ctx context.Context, cmd *cobra.Command, opts Options, target string, jsonOut bool) error {
	report, err := semanticStaleReport(ctx, opts, target)
	if err != nil {
		return err
	}
	if jsonOut {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "semantic freshness: %s\n", report.Severity)
	keys := make([]string, 0, len(report.Axes))
	for key := range report.Axes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		axis := report.Axes[key]
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %s", key, axis.State)
		if axis.Detail != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " (%s)", axis.Detail)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
	return nil
}

func semanticStaleReport(ctx context.Context, opts Options, target string) (staleReport, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return staleReport{}, err
	}
	if !local {
		return staleReport{}, fmt.Errorf("stale requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return staleReport{}, err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return staleReport{}, err
	}
	axes := map[string]staleAxis{}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		axes["semantic"] = staleAxis{State: "missing", Detail: "no semantic source in manifest"}
		return staleReport{Severity: "unsafe", Axes: axes}, nil
	}
	source := manifest.Sources.Semantic
	if source.SnapshotPath == "" {
		axes["snapshot"] = staleAxis{State: "unsafe", Detail: "semantic snapshot_path is missing"}
	} else if _, err := validateSemanticSnapshotPath(source.SnapshotPath); err != nil {
		axes["snapshot"] = staleAxis{State: "unsafe", Detail: err.Error()}
	} else if err := rejectSymlinkPathComponents(storage.BrainDir, filepath.FromSlash(source.SnapshotPath)); err != nil {
		axes["snapshot"] = staleAxis{State: "unsafe", Detail: err.Error()}
	} else {
		snapshotPath := filepath.Join(storage.BrainDir, filepath.FromSlash(source.SnapshotPath))
		snapshotHeader, snapshotCounts, snapshotErr := readSemanticSnapshotSummary(snapshotPath, storage.Key)
		if snapshotErr != nil {
			axes["snapshot"] = staleAxis{State: "unsafe", Detail: snapshotErr.Error()}
		} else if err := validateSemanticSourceMatchesSnapshot(source, snapshotHeader, snapshotCounts); err != nil {
			axes["snapshot"] = staleAxis{State: "unsafe", Detail: err.Error()}
		} else {
			axes["snapshot"] = staleAxis{State: "ok", Detail: source.SnapshotPath}
		}
	}
	head, _ := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD")
	branch, _ := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current")
	dirty, dirtyErr := worktreeDirty(ctx, opts.Runner, repoDir)
	if source.Commit == head {
		axes["head"] = staleAxis{State: "ok", Current: head, Indexed: source.Commit}
	} else {
		axes["head"] = staleAxis{State: "stale", Current: head, Indexed: source.Commit}
	}
	if source.Branch == "" || source.Branch == branch {
		axes["branch_tip"] = staleAxis{State: "ok", Current: branch, Indexed: source.Branch}
	} else {
		axes["branch_tip"] = staleAxis{State: "stale", Current: branch, Indexed: source.Branch}
	}
	switch {
	case dirtyErr != nil:
		axes["worktree"] = staleAxis{State: "unsafe", Detail: "worktree status unavailable: " + dirtyErr.Error()}
	case dirty && source.WorktreeMode != "worktree":
		axes["worktree"] = staleAxis{State: "dirty-unindexed", Detail: "semantic index is based on committed HEAD"}
	case dirty && source.WorktreeHash != "" && source.WorktreeHash != worktreeFingerprint(ctx, opts.Runner, repoDir):
		axes["worktree"] = staleAxis{State: "dirty-stale", Detail: "dirty worktree changed since semantic indexing"}
	case dirty:
		axes["worktree"] = staleAxis{State: "dirty-indexed"}
	case source.WorktreeMode == "worktree":
		axes["worktree"] = staleAxis{State: "worktree-overlay-stale", Detail: "active semantic index was built from dirty content but worktree is now clean"}
	default:
		axes["worktree"] = staleAxis{State: "clean"}
	}
	if source.Provider == "skipped" || semanticWarningsContainCode(source.Warnings, "provider_skipped") {
		axes["provider"] = staleAxis{State: "degraded", Detail: "semantic provider was skipped"}
	} else if err := validateSemanticSchema(source.SchemaVersion); err != nil {
		axes["provider"] = staleAxis{State: "unsafe", Detail: err.Error()}
	} else if !source.NoEgressVerified {
		axes["provider"] = staleAxis{State: "degraded", Detail: "provider no-egress status was not verified"}
	} else {
		axes["provider"] = staleAxis{State: "ok", Detail: source.Provider + " " + source.ProviderVersion}
	}
	if source.Provider == "skipped" || semanticWarningsContainCode(source.Warnings, "provider_skipped") {
		axes["semantic_completeness"] = staleAxis{State: "unsafe", Detail: "semantic facts unavailable"}
	} else if len(source.PartialFailures) > 0 {
		axes["semantic_completeness"] = staleAxis{State: "degraded", Detail: strconv.Itoa(len(source.PartialFailures)) + " partial failures"}
	} else {
		axes["semantic_completeness"] = staleAxis{State: "ok"}
	}
	severity := aggregateStaleSeverity(axes)
	return staleReport{Severity: severity, Axes: axes, Warnings: source.Warnings}, nil
}

func semanticWarningsContainCode(warnings []semanticWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func aggregateStaleSeverity(axes map[string]staleAxis) string {
	severity := "ok"
	for _, axis := range axes {
		switch axis.State {
		case "missing", "unsafe", "stale", "dirty-stale", "worktree-overlay-stale":
			return "unsafe"
		case "degraded", "dirty-unindexed":
			severity = "degraded"
		}
	}
	return severity
}

type semanticQueryOptions struct {
	limit int
	json  bool
}

func newSemanticQueryCommand(opts Options) *cobra.Command {
	queryOpts := semanticQueryOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "query <symbol-or-text>",
		Short: "Search the local semantic brain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticQuery(cmd.Context(), cmd, opts, queryOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&queryOpts.limit, "limit", 20, "Maximum results to return")
	cmd.Flags().BoolVar(&queryOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runSemanticQuery(ctx context.Context, cmd *cobra.Command, opts Options, queryOpts semanticQueryOptions, query string) error {
	if queryOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain index`")
	}
	snapshotPath, err := validateSemanticSnapshotPath(manifest.Sources.Semantic.SnapshotPath)
	if err != nil {
		return err
	}
	if err := rejectSymlinkPathComponents(storage.BrainDir, snapshotPath); err != nil {
		return err
	}
	results, err := findSemanticSymbols(filepath.Join(storage.BrainDir, snapshotPath), query, queryOpts.limit)
	if err != nil {
		return err
	}
	if queryOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness staleReport      `json:"freshness"`
			Results   []semanticRecord `json:"results"`
		}{Freshness: freshness, Results: results}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "semantic freshness: %s\n", freshness.Severity)
	}
	for _, result := range results {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s:%d-%d\n", result.Kind, displaySymbolName(result), result.FilePath, result.StartLine, result.EndLine)
	}
	return nil
}

func findSemanticSymbols(snapshotPath, query string, limit int) ([]semanticRecord, error) {
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("open semantic snapshot: %w", err)
	}
	defer f.Close()
	query = strings.ToLower(strings.TrimSpace(query))
	scanner := newSemanticScanner(f)
	first := true
	var results []semanticRecord
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			return nil, err
		}
		if record.RecordType != "symbol" {
			continue
		}
		fields := []string{record.Name, record.QualifiedName, record.Signature, record.FilePath}
		for _, field := range fields {
			if strings.Contains(strings.ToLower(field), query) {
				results = append(results, record)
				break
			}
		}
		if len(results) >= limit {
			break
		}
	}
	return results, scanner.Err()
}

func displaySymbolName(record semanticRecord) string {
	if record.QualifiedName != "" {
		return record.QualifiedName
	}
	return record.Name
}

func newSemanticGCCommand(opts Options) *cobra.Command {
	var olderThan string
	cmd := &cobra.Command{
		Use:   "gc [path]",
		Short: "Prune old local semantic overlays and snapshots",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSemanticGC(cmd.Context(), cmd, opts, target, olderThan)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "30d", "Prune semantic snapshots older than this duration")
	return cmd
}

func runSemanticGC(ctx context.Context, cmd *cobra.Command, opts Options, target, olderThan string) error {
	cutoff, err := parseAgeCutoff(olderThan, opts.Now().UTC())
	if err != nil {
		return err
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("gc requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	active := ""
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Semantic != nil {
		active = filepath.Clean(filepath.Join(storage.BrainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath), ".."))
	}
	snapshotsRoot := filepath.Join(storage.BrainDir, semanticDirName, semanticSnapshotsDir)
	if err := rejectExistingSymlinkPathComponents(storage.BrainDir, filepath.Join(semanticDirName, semanticSnapshotsDir)); err != nil {
		return err
	}
	entries, err := os.ReadDir(snapshotsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(cmd.OutOrStdout(), "pruned semantic snapshots: 0")
			return nil
		}
		return err
	}
	pruned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(snapshotsRoot, entry.Name())
		if active != "" && filepath.Clean(path) == active {
			continue
		}
		if err := rejectSymlinkPathComponents(storage.BrainDir, filepath.Join(semanticDirName, semanticSnapshotsDir, entry.Name())); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
			pruned++
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "pruned semantic snapshots: %d\n", pruned)
	return nil
}

func parseAgeCutoff(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil || days < 0 {
			return time.Time{}, fmt.Errorf("invalid --older-than value: %s", value)
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --older-than value: %s", value)
	}
	return now.Add(-duration), nil
}

func newSemanticBundleCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Import or export local brain bundles",
	}
	var output string
	var expectedChecksum string
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export a local brain bundle",
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return errors.New("--output is required")
			}
			return runSemanticBundleExport(cmd.Context(), cmd, opts, output)
		},
	}
	exportCmd.Flags().StringVar(&output, "output", "", "Local output archive path")
	importCmd := &cobra.Command{
		Use:   "import <archive>",
		Short: "Import a local brain bundle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if expectedChecksum == "" {
				return errors.New("--sha256 is required for bundle import")
			}
			return runSemanticBundleImport(cmd.Context(), cmd, opts, args[0], expectedChecksum)
		},
	}
	importCmd.Flags().StringVar(&expectedChecksum, "sha256", "", "Expected SHA-256 checksum for the local bundle archive")
	cmd.AddCommand(exportCmd, importCmd)
	return cmd
}

func runSemanticBundleExport(ctx context.Context, cmd *cobra.Command, opts Options, output string) error {
	if err := rejectRemoteBundlePath(output); err != nil {
		return err
	}
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := rejectBundleOutputInsideBrain(output, storage.BrainDir); err != nil {
		return err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain index`")
	}
	activeSnapshot, err := validateSemanticSnapshotPath(manifest.Sources.Semantic.SnapshotPath)
	if err != nil {
		return err
	}
	activeSnapshotPath := filepath.Join(storage.BrainDir, activeSnapshot)
	info, err := os.Lstat(activeSnapshotPath)
	if err != nil {
		return fmt.Errorf("active semantic snapshot missing: %w", err)
	}
	if err := rejectSymlinkPathComponents(storage.BrainDir, activeSnapshot); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("active semantic snapshot must not be a symlink: %s", manifest.Sources.Semantic.SnapshotPath)
	}
	if info.IsDir() {
		return fmt.Errorf("active semantic snapshot must be a file: %s", manifest.Sources.Semantic.SnapshotPath)
	}
	if err := validateSnapshotFileSchema(activeSnapshotPath, storage.Key); err != nil {
		return err
	}
	if err := rejectBundleOutputHardLink(output, storage.BrainDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	tw := tar.NewWriter(mw)
	if err := addBundleManifest(tw, exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		RepoKey:       storage.Key,
		Sources:       &brainSources{Semantic: manifest.Sources.Semantic},
	}); err != nil {
		_ = f.Close()
		return err
	}
	if err := addBundlePath(tw, storage.BrainDir, activeSnapshot); err != nil {
		_ = f.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))
	if err := appendSemanticAudit(storage.BrainDir, "bundle_export", output, checksum); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "exported bundle: %s\nsha256: %s\n", output, checksum)
	return nil
}

func addBundleManifest(tw *tar.Writer, manifest exportManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	header := &tar.Header{Name: exportManifestFileName, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now().UTC()}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

func addBundlePath(tw *tar.Writer, root, rel string) error {
	if err := rejectSymlinkPathComponents(root, rel); err != nil {
		return err
	}
	path := filepath.Join(root, rel)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundle export refuses symlink: %s", rel)
	}
	if info.IsDir() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	header := &tar.Header{Name: filepath.ToSlash(rel), Mode: 0o600, Size: int64(len(data)), ModTime: info.ModTime()}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

func rejectSymlinkPathComponents(root, rel string) error {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside root: %s", rel)
	}
	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component must not be a symlink: %s", rel)
		}
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	pathResolved, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return err
	}
	if !pathInside(rootResolved, pathResolved) {
		return fmt.Errorf("path escapes root through symlinks: %s", rel)
	}
	return nil
}

func rejectExistingSymlinkPathComponents(root, rel string) error {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside root: %s", rel)
	}
	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component must not be a symlink: %s", rel)
		}
	}
	return nil
}

func rejectBundleOutputInsideBrain(output, brainDir string) error {
	outputAbs, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	brainAbs, err := filepath.Abs(brainDir)
	if err != nil {
		return err
	}
	if pathInside(brainAbs, outputAbs) {
		return fmt.Errorf("bundle output must be outside the active brain directory: %s", output)
	}
	outputResolved, err := resolvedOutputPath(output)
	if err != nil {
		return err
	}
	brainResolved, err := filepath.EvalSymlinks(brainDir)
	if err != nil {
		brainResolved = brainAbs
	}
	if pathInside(brainResolved, outputResolved) {
		return fmt.Errorf("bundle output must be outside the active brain directory: %s", output)
	}
	return nil
}

func rejectBundleOutputHardLink(output, brainDir string) error {
	outputInfo, err := os.Stat(output)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if outputInfo.IsDir() {
		return fmt.Errorf("bundle output path is a directory: %s", output)
	}
	return filepath.WalkDir(brainDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if os.SameFile(outputInfo, info) {
			return fmt.Errorf("bundle output must not be a hard link to brain file: %s", path)
		}
		return nil
	})
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func resolvedOutputPath(output string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(output); err == nil {
		return filepath.Abs(resolved)
	}
	absOutput, err := filepath.Abs(output)
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(absOutput)
	rest := strings.TrimPrefix(absOutput, volume)
	parts := strings.Split(strings.Trim(rest, string(filepath.Separator)), string(filepath.Separator))
	existing := volume + string(filepath.Separator)
	var missing []string
	for i, part := range parts {
		candidate := filepath.Join(existing, part)
		if _, err := os.Lstat(candidate); err != nil {
			if os.IsNotExist(err) {
				missing = parts[i:]
				break
			}
			return "", err
		}
		existing = candidate
	}
	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	for _, part := range missing {
		resolvedExisting = filepath.Join(resolvedExisting, part)
	}
	return filepath.Abs(resolvedExisting)
}

func runSemanticBundleImport(ctx context.Context, cmd *cobra.Command, opts Options, archive, expectedChecksum string) error {
	if err := rejectRemoteBundlePath(archive); err != nil {
		return err
	}
	expectedChecksum = strings.ToLower(strings.TrimSpace(expectedChecksum))
	if len(expectedChecksum) != sha256.Size*2 {
		return errors.New("expected bundle checksum must be a hex SHA-256 digest")
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	sum, err := readerSHA256(f)
	if err != nil {
		return err
	}
	if sum != expectedChecksum {
		return fmt.Errorf("bundle checksum mismatch: got %s, want %s", sum, expectedChecksum)
	}
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(storage.BrainDir), 0o700); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(storage.BrainDir), ".bundle-import-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	tr := tar.NewReader(f)
	imported := 0
	var totalBytes int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := validateBundleEntry(header); err != nil {
			return err
		}
		if header.Size > semanticBundleMaxEntry {
			return fmt.Errorf("bundle entry too large: %s", header.Name)
		}
		imported++
		if imported > semanticBundleMaxFiles {
			return fmt.Errorf("bundle has too many entries: %d", imported)
		}
		totalBytes += header.Size
		if totalBytes > semanticBundleMaxTotal {
			return fmt.Errorf("bundle total size exceeds limit: %d", totalBytes)
		}
		target := filepath.Join(tmpDir, filepath.FromSlash(header.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(out, tr, header.Size)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	importManifest, err := validateImportedBundle(tmpDir, storage.Key)
	if err != nil {
		return err
	}
	if err := filepath.WalkDir(tmpDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(tmpDir, path)
		if err != nil {
			return err
		}
		if rel == exportManifestFileName {
			return nil
		}
		if filepath.ToSlash(rel) != importManifest.Sources.Semantic.SnapshotPath {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := rejectExistingSymlinkPathComponents(storage.BrainDir, rel); err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(storage.BrainDir, rel), content, 0o600)
	}); err != nil {
		return err
	}
	if err := writeBrainSemanticSource(storage.BrainDir, storage.Key, importManifest.Sources.Semantic); err != nil {
		return err
	}
	if err := appendSemanticAudit(storage.BrainDir, "bundle_import", archive, sum); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "imported bundle entries: %d\nsha256: %s\n", imported, sum)
	return nil
}

func readerSHA256(r io.Reader) (string, error) {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateImportedBundle(root, repoKey string) (*exportManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, exportManifestFileName))
	if err != nil {
		return nil, fmt.Errorf("bundle missing manifest: %w", err)
	}
	var manifest exportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse bundle manifest: %w", err)
	}
	symbolsPresent, relationsPresent, err := bundleSemanticCountPresence(data)
	if err != nil {
		return nil, err
	}
	if manifest.RepoKey == "" {
		return nil, errors.New("bundle manifest missing repo_key")
	}
	if manifest.RepoKey != repoKey {
		return nil, fmt.Errorf("bundle repo_key %q does not match current repo %q", manifest.RepoKey, repoKey)
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return nil, errors.New("bundle manifest missing semantic source")
	}
	if err := validateSemanticSchema(manifest.Sources.Semantic.SchemaVersion); err != nil {
		return nil, fmt.Errorf("bundle semantic schema unsupported: %w", err)
	}
	snapshotPath := manifest.Sources.Semantic.SnapshotPath
	if snapshotPath == "" {
		return nil, errors.New("bundle manifest missing semantic snapshot_path")
	}
	clean, err := validateSemanticSnapshotPath(snapshotPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filepath.Join(root, clean))
	if err != nil {
		return nil, fmt.Errorf("bundle missing referenced semantic snapshot: %w", err)
	}
	if info.IsDir() {
		return nil, errors.New("bundle semantic snapshot_path points to a directory")
	}
	snapshotHeader, snapshotCounts, err := readSemanticSnapshotSummary(filepath.Join(root, clean), repoKey)
	if err != nil {
		return nil, err
	}
	if !symbolsPresent {
		manifest.Sources.Semantic.Symbols = snapshotCounts.Symbols
	}
	if !relationsPresent {
		manifest.Sources.Semantic.Relations = snapshotCounts.Relations
	}
	if manifest.Sources.Semantic.Commit == "" {
		manifest.Sources.Semantic.Commit = snapshotHeader.Commit
	}
	if manifest.Sources.Semantic.Tree == "" {
		manifest.Sources.Semantic.Tree = snapshotHeader.Tree
	}
	if manifest.Sources.Semantic.Provider == "" {
		manifest.Sources.Semantic.Provider = snapshotHeader.Provider
	}
	if manifest.Sources.Semantic.ProviderVersion == "" {
		manifest.Sources.Semantic.ProviderVersion = snapshotHeader.ProviderVersion
	}
	if err := validateSemanticSourceMatchesSnapshot(manifest.Sources.Semantic, snapshotHeader, snapshotCounts); err != nil {
		return nil, fmt.Errorf("bundle manifest does not match semantic snapshot: %w", err)
	}
	return &manifest, nil
}

func bundleSemanticCountPresence(data []byte) (bool, bool, error) {
	var raw struct {
		Sources struct {
			Semantic map[string]json.RawMessage `json:"semantic"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, false, fmt.Errorf("parse bundle manifest count fields: %w", err)
	}
	_, symbolsPresent := raw.Sources.Semantic["symbols"]
	_, relationsPresent := raw.Sources.Semantic["relations"]
	return symbolsPresent, relationsPresent, nil
}

func validateSemanticSnapshotPath(snapshotPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(snapshotPath))
	cleanSlash := filepath.ToSlash(clean)
	if cleanSlash != snapshotPath {
		return "", fmt.Errorf("semantic snapshot_path must be canonical: %s", snapshotPath)
	}
	if strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) || !strings.HasPrefix(cleanSlash, filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir))+"/") {
		return "", fmt.Errorf("semantic snapshot_path is unsafe: %s", snapshotPath)
	}
	if filepath.Base(clean) != semanticSnapshotName {
		return "", fmt.Errorf("semantic snapshot_path must end with %s: %s", semanticSnapshotName, snapshotPath)
	}
	return clean, nil
}

func validateSnapshotFileSchema(path, repoKey string) error {
	_, _, err := readSemanticSnapshotSummary(path, repoKey)
	return err
}

func readSemanticSnapshotSummary(path, repoKey string) (semanticHeader, semanticCounts, error) {
	f, err := os.Open(path)
	if err != nil {
		return semanticHeader{}, semanticCounts{}, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return semanticHeader{}, semanticCounts{}, err
		}
		return semanticHeader{}, semanticCounts{}, errors.New("semantic snapshot missing header")
	}
	var header semanticHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return semanticHeader{}, semanticCounts{}, fmt.Errorf("parse semantic snapshot header: %w", err)
	}
	if header.RepoKey == "" {
		return semanticHeader{}, semanticCounts{}, errors.New("semantic snapshot header missing repo_key")
	}
	if header.RepoKey != repoKey {
		return semanticHeader{}, semanticCounts{}, fmt.Errorf("semantic snapshot repo_key %q does not match current repo %q", header.RepoKey, repoKey)
	}
	if err := validateSemanticSchema(header.SchemaVersion); err != nil {
		return semanticHeader{}, semanticCounts{}, fmt.Errorf("semantic snapshot schema unsupported: %w", err)
	}
	line := 1
	var counts semanticCounts
	for scanner.Scan() {
		line++
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			return semanticHeader{}, semanticCounts{}, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		switch record.RecordType {
		case "symbol":
			counts.Symbols++
		case "relation":
			counts.Relations++
		}
	}
	if err := scanner.Err(); err != nil {
		return semanticHeader{}, semanticCounts{}, err
	}
	return header, counts, nil
}

func validateSemanticSourceMatchesSnapshot(source *semanticSourceManifest, header semanticHeader, counts semanticCounts) error {
	if source == nil {
		return errors.New("semantic source is missing")
	}
	if source.WorktreeMode == "worktree" || source.DirtyWorktree || source.WorktreeHash != "" {
		return errors.New("bundle semantic source contains unsupported worktree overlay metadata")
	}
	if source.SchemaVersion != "" && header.SchemaVersion != source.SchemaVersion {
		return fmt.Errorf("schema_version %q does not match snapshot %q", source.SchemaVersion, header.SchemaVersion)
	}
	if source.Commit != "" && header.Commit != source.Commit {
		return fmt.Errorf("commit %q does not match snapshot %q", source.Commit, header.Commit)
	}
	if source.Tree != "" && header.Tree != source.Tree {
		return fmt.Errorf("tree %q does not match snapshot %q", source.Tree, header.Tree)
	}
	if source.Provider != "" && header.Provider != "" && header.Provider != source.Provider {
		return fmt.Errorf("provider %q does not match snapshot %q", source.Provider, header.Provider)
	}
	if source.ProviderVersion != "" && header.ProviderVersion != "" && header.ProviderVersion != source.ProviderVersion {
		return fmt.Errorf("provider_version %q does not match snapshot %q", source.ProviderVersion, header.ProviderVersion)
	}
	if source.Symbols != counts.Symbols {
		return fmt.Errorf("symbol count %d does not match snapshot %d", source.Symbols, counts.Symbols)
	}
	if source.Relations != counts.Relations {
		return fmt.Errorf("relation count %d does not match snapshot %d", source.Relations, counts.Relations)
	}
	return nil
}

func rejectRemoteBundlePath(path string) error {
	lower := strings.ToLower(strings.TrimSpace(path))
	for _, prefix := range []string{"http://", "https://", "s3://", "git+ssh://"} {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf("bundle path must be local: %s", path)
		}
	}
	if strings.HasPrefix(lower, "file://") && !strings.HasPrefix(lower, "file:///") {
		return fmt.Errorf("bundle file URL must not include a hostname: %s", path)
	}
	return nil
}

func validateBundleEntry(header *tar.Header) error {
	if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
		return fmt.Errorf("bundle entry must be a regular file: %s", header.Name)
	}
	if strings.HasPrefix(header.Name, "/") || strings.Contains(header.Name, "..") {
		return fmt.Errorf("unsafe bundle path: %s", header.Name)
	}
	clean := filepath.Clean(filepath.FromSlash(header.Name))
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe bundle path: %s", header.Name)
	}
	cleanSlash := filepath.ToSlash(clean)
	if clean != exportManifestFileName && !strings.HasPrefix(cleanSlash, filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir))+"/") {
		return fmt.Errorf("bundle entry outside allowed paths: %s", header.Name)
	}
	return nil
}

func appendSemanticAudit(brainDir, action, path, checksum string) error {
	entry := map[string]string{
		"action":       action,
		"path":         path,
		"sha256":       checksum,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	auditPath := filepath.Join(brainDir, semanticDirName, semanticAuditLogName)
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}
