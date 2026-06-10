package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

const (
	semanticDirName                     = "semantic"
	semanticSnapshotsDir                = "snapshots"
	semanticGenerationsDir              = "generations"
	semanticBundleDir                   = "bundles"
	semanticAuditLogName                = "audit.jsonl"
	semanticLockDir                     = "locks"
	semanticIndexLockName               = "index.lock"
	semanticSnapshotName                = "snapshot.ndjson"
	semanticManifestName                = "manifest.json"
	semanticSQLiteName                  = "semantic.sqlite"
	semanticMetricsName                 = "metrics.json"
	semanticSupportedMajor              = "1"
	semanticContextMaxLines             = 80
	semanticContextMaxBytes             = 64 * 1024
	semanticParseCacheMaxFile           = 16 * 1024 * 1024
	semanticWorktreeFingerprintMaxFile  = 16 * 1024 * 1024
	semanticWorktreeFingerprintMaxTotal = 128 * 1024 * 1024
	semanticDoctorTimeout               = 30 * time.Second
	semanticSnapshotTimeout             = 2 * time.Minute
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
	StorePath        string            `json:"store_path,omitempty"`
	GenerationPath   string            `json:"generation_path,omitempty"`
	MetricsPath      string            `json:"metrics_path,omitempty"`
	ParseCachePath   string            `json:"parse_cache_path,omitempty"`
	OverlayPath      string            `json:"overlay_path,omitempty"`
	Symbols          int               `json:"symbols"`
	Relations        int               `json:"relations"`
	Files            int               `json:"files,omitempty"`
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

// UnmarshalJSON accepts both entire-brain's canonical keys (path, effect) and the
// semantic provider's wire keys (file_path, effect_on_semantic_completeness). The
// provider attaches the failing file to each partial failure via file_path; without
// this alias those fields are silently dropped, leaving degraded brains unable to
// report which files failed to parse. Canonical keys win when both are present.
func (w *semanticWarning) UnmarshalJSON(data []byte) error {
	type alias semanticWarning
	var raw struct {
		alias
		ProviderPath   string `json:"file_path"`
		ProviderEffect string `json:"effect_on_semantic_completeness"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*w = semanticWarning(raw.alias)
	if w.Path == "" {
		w.Path = raw.ProviderPath
	}
	if w.Effect == "" {
		w.Effect = raw.ProviderEffect
	}
	return nil
}

type semanticHeader struct {
	SchemaVersion   string            `json:"schema_version"`
	Provider        string            `json:"provider"`
	ProviderVersion string            `json:"provider_version"`
	RepoRoot        string            `json:"repo_root,omitempty"`
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
	Blob            string   `json:"blob"`
	Score           int      `json:"score,omitempty"`
}

type semanticIndexOptions struct {
	force          bool
	semBinary      string
	skipSem        bool
	worktree       bool
	outputDir      string
	outputExplicit bool
	// progress, when set, is called at each phase boundary of the index
	// (verifying provider, snapshotting, building store, …) so long-running
	// indexing reports something more useful than a static spinner.
	progress func(phase string)
}

func (o semanticIndexOptions) reportPhase(phase string) {
	if o.progress != nil {
		o.progress(phase)
	}
}

type semanticResetOptions struct {
	force        bool
	semanticOnly bool
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

func newSemanticRepairCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair [path]",
		Short: "Rebuild local semantic brain derived indexes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSemanticRepair(cmd.Context(), cmd, opts, target)
		},
	}
	return cmd
}

func newSemanticResetCommand(opts Options) *cobra.Command {
	resetOpts := semanticResetOptions{}
	cmd := &cobra.Command{
		Use:   "reset [path]",
		Short: "Remove local generated brain data",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSemanticReset(cmd.Context(), cmd, opts, resetOpts, target)
		},
	}
	cmd.Flags().BoolVar(&resetOpts.semanticOnly, "semantic-only", false, "Remove only semantic artifacts and manifest source metadata")
	cmd.Flags().BoolVar(&resetOpts.force, "force", false, "Confirm removal without an interactive prompt")
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
	if indexOpts.outputExplicit {
		outputDir, err := filepath.Abs(indexOpts.outputDir)
		if err != nil {
			return fmt.Errorf("resolve output directory: %w", err)
		}
		storage.BrainDir = outputDir
	}
	if err := ensureSemanticAuditPathSafe(storage.BrainDir); err != nil {
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
	providerIgnoreFiles, err := semanticProviderIgnoreFiles(repoDir)
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
	dirty, err := worktreeDirtyWithIgnore(ctx, opts.Runner, repoDir, ignore)
	if err != nil {
		return fmt.Errorf("check worktree dirtiness for semantic index: %w", err)
	}
	if dirty && !indexOpts.worktree {
		return errors.New("dirty_worktree: refusing to index uncommitted content without --worktree")
	}
	worktreeHashBefore := ""
	if indexOpts.worktree {
		worktreeHashBefore, err = worktreeFingerprint(ctx, opts.Runner, repoDir)
		if err != nil {
			return fmt.Errorf("fingerprint worktree for semantic index: %w", err)
		}
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
		indexOpts.reportPhase("verifying provider")
		noEgress, doctorWarnings = runSemanticDoctor(ctx, opts.Runner, repoDir, indexOpts.semBinary)
		warnings = append(warnings, doctorWarnings...)
		if !noEgress {
			code := "provider_no_egress_unverified"
			if len(doctorWarnings) > 0 && doctorWarnings[0].Code != "" {
				code = doctorWarnings[0].Code
			}
			return fmt.Errorf("%s: semantic provider no-egress status is not verified", code)
		}
		indexOpts.reportPhase("parsing sources")
		var snapshotWarnings []semanticWarning
		raw, snapshotWarnings, err = runSemanticSnapshot(ctx, opts.Runner, repoDir, indexOpts.semBinary, indexOpts.worktree, providerIgnoreFiles)
		if err != nil {
			return err
		}
		warnings = append(warnings, snapshotWarnings...)
		indexOpts.reportPhase("filtering snapshot")
		header, counts, raw, err = filterSemanticSnapshot(raw, ignore, repoDir)
		if err != nil {
			return err
		}
		if err := verifySemanticWorktreeStable(ctx, opts.Runner, repoDir, indexOpts.worktree, dirty, worktreeHashBefore); err != nil {
			return err
		}
		if err := validateSemanticSchema(header.SchemaVersion); err != nil {
			return err
		}
		if err := validateLiveSemanticHeader(header, storage.Key, head, tree, indexOpts.worktree && dirty); err != nil {
			return err
		}
		header.Warnings = sanitizeSemanticWarnings(header.Warnings, repoDir)
		header.PartialFailures = sanitizeSemanticWarnings(header.PartialFailures, repoDir)
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
	if indexOpts.worktree && dirty {
		snapshotID = "worktree-" + hex.EncodeToString(sum[:8])
		worktreeMode = "worktree"
		worktreeHash = worktreeHashBefore
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
	indexOpts.reportPhase("building store")
	generation, metrics, err := buildSemanticGeneration(storage.BrainDir, repoDir, snapshotID, raw, header, counts, opts.Now().UTC())
	if err != nil {
		return err
	}
	overlayPath, err := writeSemanticBranchOverlay(ctx, opts.Runner, storage.BrainDir, repoDir, branch, defaultBranch, header.Commit, snapshotRel, opts.Now().UTC())
	if err != nil {
		return err
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
		StorePath:        filepath.ToSlash(filepath.Join(generation, semanticSQLiteName)),
		GenerationPath:   generation,
		MetricsPath:      filepath.ToSlash(filepath.Join(generation, semanticMetricsName)),
		ParseCachePath:   filepath.ToSlash(filepath.Join(generation, "parse-cache")),
		OverlayPath:      overlayPath,
		Symbols:          counts.Symbols,
		Relations:        counts.Relations,
		Files:            counts.Files,
		Warnings:         sanitizeSemanticWarnings(warnings, repoDir),
		PartialFailures:  sanitizeSemanticWarnings(header.PartialFailures, repoDir),
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
	fmt.Fprintf(cmd.OutOrStdout(), "store: %s\n", source.StorePath)
	fmt.Fprintf(cmd.OutOrStdout(), "build_ms: %d\n", metrics.BuildMillis)
	return nil
}

func runSemanticRepair(ctx context.Context, cmd *cobra.Command, opts Options, target string) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("repair requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	if err := ensureSemanticAuditPathSafe(storage.BrainDir); err != nil {
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
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	source := manifest.Sources.Semantic
	snapshotRel, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return err
	}
	if err := rejectSymlinkPathComponents(storage.BrainDir, snapshotRel); err != nil {
		return err
	}
	snapshotPath := filepath.Join(storage.BrainDir, snapshotRel)
	info, err := os.Lstat(snapshotPath)
	if err != nil {
		return fmt.Errorf("active semantic snapshot missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("active semantic snapshot must not be a symlink: %s", source.SnapshotPath)
	}
	if info.IsDir() {
		return fmt.Errorf("active semantic snapshot must be a file: %s", source.SnapshotPath)
	}
	raw, err := os.ReadFile(snapshotPath)
	if err != nil {
		return fmt.Errorf("read active semantic snapshot: %w", err)
	}
	header, counts, err := readSemanticSnapshotSummary(snapshotPath, storage.Key)
	if err != nil {
		return err
	}
	if err := validateSemanticSourceMatchesSnapshot(source, header, counts); err != nil {
		return err
	}
	counts.Files = source.Files
	if counts.Files == 0 {
		counts.Files = semanticSnapshotFileCount(raw)
	}
	snapshotID := filepath.Base(filepath.Dir(snapshotRel))
	if snapshotID == "." || snapshotID == string(filepath.Separator) || snapshotID == "" {
		return fmt.Errorf("semantic snapshot path has no generation id: %s", source.SnapshotPath)
	}
	generation, metrics, err := buildSemanticGeneration(storage.BrainDir, repoDir, snapshotID, raw, header, counts, opts.Now().UTC())
	if err != nil {
		return err
	}
	repaired := *source
	repaired.GeneratedAt = opts.Now().UTC()
	repaired.StorePath = filepath.ToSlash(filepath.Join(generation, semanticSQLiteName))
	repaired.GenerationPath = generation
	repaired.MetricsPath = filepath.ToSlash(filepath.Join(generation, semanticMetricsName))
	repaired.ParseCachePath = filepath.ToSlash(filepath.Join(generation, "parse-cache"))
	repaired.Symbols = counts.Symbols
	repaired.Relations = counts.Relations
	repaired.Files = counts.Files
	if err := writeBrainSemanticSource(storage.BrainDir, storage.Key, &repaired); err != nil {
		return err
	}
	if err := writeSemanticSnapshotManifest(storage.BrainDir, snapshotID, &repaired); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "repaired semantic brain: %s\n", storage.BrainDir)
	fmt.Fprintf(cmd.OutOrStdout(), "snapshot: %s\n", source.SnapshotPath)
	fmt.Fprintf(cmd.OutOrStdout(), "store: %s\n", repaired.StorePath)
	fmt.Fprintf(cmd.OutOrStdout(), "build_ms: %d\n", metrics.BuildMillis)
	return nil
}

func semanticSnapshotFileCount(raw []byte) int {
	files := map[string]struct{}{}
	scanner := newSemanticScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		return 0
	}
	for scanner.Scan() {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			continue
		}
		switch record.RecordType {
		case "file", "symbol":
			if path := record.semanticPath(); path != "" {
				files[path] = struct{}{}
			}
		}
	}
	return len(files)
}

func runSemanticReset(ctx context.Context, cmd *cobra.Command, opts Options, resetOpts semanticResetOptions, target string) error {
	if !resetOpts.force {
		return errors.New("reset requires --force")
	}
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("reset requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	if err := rejectSymlinkedBrainRoot(storage.BrainDir); err != nil {
		return err
	}
	if resetOpts.semanticOnly {
		unlock, err := acquireSemanticIndexLock(storage.BrainDir)
		if err != nil {
			return err
		}
		defer unlock()
		if err := withBrainWriteLock(storage.BrainDir, func() error {
			manifest, err := loadBrainManifest(storage.BrainDir)
			if err != nil {
				return err
			}
			if manifest.Sources == nil {
				manifest.Sources = &brainSources{}
			}
			manifest.Sources.Semantic = nil
			applySessionSourceAliases(manifest)
			if err := rejectExistingSymlinkPathComponents(storage.BrainDir, semanticDirName); err != nil {
				return err
			}
			if err := writeBrainManifestAndReadme(storage.BrainDir, *manifest); err != nil {
				return err
			}
			if err := os.RemoveAll(filepath.Join(storage.BrainDir, semanticDirName)); err != nil {
				return fmt.Errorf("remove semantic artifacts: %w", err)
			}
			return syncParentDir(filepath.Join(storage.BrainDir, semanticDirName))
		}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "reset semantic brain: %s\n", storage.BrainDir)
		return nil
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(storage.BrainDir); err != nil {
		unlock()
		if os.IsNotExist(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "reset brain: %s\n", storage.BrainDir)
			return nil
		}
		return err
	}
	unlockBrain, err := acquireBrainWriteLock(storage.BrainDir)
	if err != nil {
		unlock()
		return err
	}
	if err := resetBrainDirectoryContents(storage.BrainDir); err != nil {
		unlockBrain()
		unlock()
		return err
	}
	unlockBrain()
	unlock()
	if err := syncParentDir(storage.BrainDir); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reset brain: %s\n", storage.BrainDir)
	return nil
}

func resetBrainDirectoryContents(brainDir string) error {
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(brainDir, 0o700)
		}
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == brainLockDirName {
			continue
		}
		if err := removeAllWithRetry(filepath.Join(brainDir, name)); err != nil {
			return fmt.Errorf("remove brain entry %s: %w", name, err)
		}
	}
	return syncParentDir(brainDir)
}

func removeAllWithRetry(path string) error {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.RemoveAll(path); err != nil {
			last = err
			time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
			continue
		}
		return nil
	}
	return last
}

type semanticCounts struct {
	Files     int
	Symbols   int
	Relations int
}

type semanticBuildMetrics struct {
	GeneratedAt     time.Time `json:"generated_at"`
	GenerationID    string    `json:"generation_id"`
	SnapshotSHA256  string    `json:"snapshot_sha256,omitempty"`
	Commit          string    `json:"commit,omitempty"`
	Tree            string    `json:"tree,omitempty"`
	Files           int       `json:"files"`
	Symbols         int       `json:"symbols"`
	Relations       int       `json:"relations"`
	ReverseEdges    int       `json:"reverse_edges"`
	Warnings        int       `json:"warnings"`
	PartialFailures int       `json:"partial_failures"`
	BuildMillis     int64     `json:"build_millis"`
	StoreBytes      int64     `json:"store_bytes"`
	ParseCacheFiles int       `json:"parse_cache_files"`
}

func writeBrainSemanticSource(outputDir, repoKey string, semantic *semanticSourceManifest) error {
	return withBrainWriteLock(outputDir, func() error {
		return writeBrainSemanticSourceLocked(outputDir, repoKey, semantic)
	})
}

func writeBrainSemanticSourceLocked(outputDir, repoKey string, semantic *semanticSourceManifest) error {
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
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(brainDir, semanticLockDir)
	if err := rejectExistingSymlinkPathComponents(brainDir, semanticLockDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create semantic lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, semanticIndexLockName)
	lock, err := acquireFileLock(lockPath, "index_locked", 0)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

func runSemanticDoctor(ctx context.Context, runner CommandRunner, repoDir, semBinary string) (bool, []semanticWarning) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, semanticDoctorTimeout)
	defer cancel()
	stdout, _, err := runner.Run(runCtx, repoDir, semBinary, "sem", "doctor", "--json")
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return false, []semanticWarning{{Code: "provider_doctor_timeout", Severity: "warning", Effect: "provider diagnostics unavailable", Detail: fmt.Sprintf("semantic provider doctor timed out after %s", semanticDoctorTimeout)}}
	}
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

func runSemanticSnapshot(ctx context.Context, runner CommandRunner, repoDir, semBinary string, worktree bool, ignoreFiles []string) ([]byte, []semanticWarning, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	args := semanticSnapshotArgs(repoDir, worktree, ignoreFiles)
	stdout, stderr, err := runSemanticSnapshotCommand(ctx, runner, repoDir, semBinary, args)
	if err != nil && countNonEmptyStrings(ignoreFiles) > 0 && semanticSnapshotRejectsIgnoreFile(stderr, err) {
		stdout, stderr, err = runSemanticSnapshotCommand(ctx, runner, repoDir, semBinary, semanticSnapshotArgs(repoDir, worktree, nil))
		if err == nil {
			warning := semanticWarning{
				Code:     "provider_ignore_file_unsupported",
				Severity: "warning",
				Effect:   "provider-side ignore prefilter unavailable; brainignore applied after snapshot",
				Detail:   fmt.Sprintf("semantic provider rejected --ignore-file; retried without %d provider ignore file(s)", countNonEmptyStrings(ignoreFiles)),
			}
			if len(bytes.TrimSpace(stdout)) == 0 {
				return nil, []semanticWarning{warning}, errors.New("semantic provider snapshot produced no output")
			}
			return stdout, []semanticWarning{warning}, nil
		}
	}
	if err != nil {
		if len(bytes.TrimSpace(stderr)) > 0 {
			return nil, nil, fmt.Errorf("semantic provider snapshot failed: %w: %s", err, strings.TrimSpace(string(stderr)))
		}
		return nil, nil, fmt.Errorf("semantic provider snapshot failed: %w", err)
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		return nil, nil, errors.New("semantic provider snapshot produced no output")
	}
	return stdout, nil, nil
}

func semanticSnapshotArgs(repoDir string, worktree bool, ignoreFiles []string) []string {
	args := []string{"sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"}
	for _, path := range ignoreFiles {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		args = append(args, "--ignore-file", path)
	}
	if worktree {
		args = append(args, "--worktree")
	}
	return args
}

func runSemanticSnapshotCommand(ctx context.Context, runner CommandRunner, repoDir, semBinary string, args []string) ([]byte, []byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, semanticSnapshotTimeout)
	defer cancel()
	stdout, stderr, err := runner.Run(runCtx, repoDir, semBinary, args...)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return stdout, nil, fmt.Errorf("semantic provider snapshot timed out after %s", semanticSnapshotTimeout)
	}
	return stdout, stderr, err
}

func semanticSnapshotRejectsIgnoreFile(stderr []byte, err error) bool {
	text := strings.ToLower(strings.TrimSpace(string(stderr)))
	if err != nil {
		if text != "" {
			text += " "
		}
		text += strings.ToLower(err.Error())
	}
	if !strings.Contains(text, "ignore-file") {
		return false
	}
	for _, phrase := range []string{
		"unexpected argument",
		"unexpected arguments",
		"unknown flag",
		"unknown option",
		"unrecognized option",
		"flag provided but not defined",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func countNonEmptyStrings(values []string) int {
	var count int
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	return count
}

func validateLiveSemanticHeader(header semanticHeader, repoKey, commit, tree string, allowWorktreeTree bool) error {
	if header.Commit == "" {
		return errors.New("semantic snapshot header missing commit")
	}
	if header.Tree == "" {
		return errors.New("semantic snapshot header missing tree")
	}
	if header.RepoKey == "" {
		return errors.New("semantic snapshot header missing repo_key")
	}
	if !semanticRepoKeyEqual(header.RepoKey, repoKey) {
		return fmt.Errorf("semantic snapshot repo_key %q does not match current repo %q", header.RepoKey, repoKey)
	}
	if header.Commit != "" && commit != "" && header.Commit != commit {
		return fmt.Errorf("semantic snapshot commit %q does not match HEAD %q", header.Commit, commit)
	}
	if header.Tree != "" && tree != "" && header.Tree != tree && !allowWorktreeTree {
		return fmt.Errorf("semantic snapshot tree %q does not match HEAD tree %q", header.Tree, tree)
	}
	return nil
}

func filterSemanticSnapshot(raw []byte, ignore brainIgnore, repoDir string) (semanticHeader, semanticCounts, []byte, error) {
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
	header.RepoRoot = ""
	header.Warnings = ignore.FilterWarnings(header.Warnings)
	header.PartialFailures = ignore.FilterWarnings(header.PartialFailures)
	header.Warnings = sanitizeSemanticWarnings(header.Warnings, repoDir)
	header.PartialFailures = sanitizeSemanticWarnings(header.PartialFailures, repoDir)
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
		if err := validateSemanticRecordPath(&record); err != nil {
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
	files := make(map[string]struct{})
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
		if err := validateSemanticRecordPath(&record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		if ignore.Ignored(record.semanticPath()) {
			continue
		}
		if record.RecordType == "relation" {
			if p := semanticEndpointPath(record.FromID); p != "" && ignore.Ignored(p) {
				continue
			}
			if p := semanticEndpointPath(record.ToID); p != "" && ignore.Ignored(p) {
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
		case "file":
			if path := record.semanticPath(); path != "" {
				files[path] = struct{}{}
			}
		case "symbol":
			if path := record.semanticPath(); path != "" {
				files[path] = struct{}{}
			}
			counts.Symbols++
		case "relation":
			counts.Relations++
		}
		filteredRecord, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("encode semantic snapshot line %d: %w", line, marshalErr)
		}
		filtered.Write(filteredRecord)
		filtered.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, err
	}
	counts.Files = len(files)
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

func mergeSemanticFileRecord(existing, incoming semanticRecord) semanticRecord {
	if existing.RecordType == "" {
		return incoming
	}
	if existing.Blob == "" {
		existing.Blob = incoming.Blob
	}
	if existing.Language == "" {
		existing.Language = incoming.Language
	}
	if existing.semanticPath() == "" {
		existing.setSemanticPath(incoming.semanticPath())
	}
	return existing
}

// semanticEndpointPath extracts the repository-relative file path encoded in a
// semantic relation endpoint ID, or "" when the ID carries no path. Internal
// IDs are "<repo-key>:<lang>:<path>:<kind>:<name>"; the repo key, language, and
// kind segments never contain ":", so the path is always the third field.
// External endpoints ("external:<kind>:<value>") carry no file path. Endpoint
// IDs are filtered against the ignore set by file path so a substring in the
// repo key or symbol name cannot accidentally redact unrelated relations.
func semanticEndpointPath(id string) string {
	if id == "" || strings.HasPrefix(id, "external:") {
		return ""
	}
	parts := strings.Split(id, ":")
	if len(parts) < 5 {
		return ""
	}
	return parts[2]
}

func (r *semanticRecord) setSemanticPath(path string) {
	if r.RecordType == "symbol" {
		r.FilePath = path
		r.Path = ""
		return
	}
	if r.FilePath != "" {
		r.FilePath = path
		return
	}
	r.Path = path
}

func validateSemanticRecordPath(record *semanticRecord) error {
	switch record.RecordType {
	case "file", "symbol":
	default:
		return nil
	}
	path := record.semanticPath()
	if path == "" {
		return nil
	}
	clean, err := validateSemanticProviderPath(path)
	if err != nil {
		return err
	}
	record.setSemanticPath(clean)
	return nil
}

func validateSemanticProviderPath(providerPath string) (string, error) {
	if providerPath == "" {
		return "", nil
	}
	if looksLikeWindowsDrivePath(providerPath) {
		return "", fmt.Errorf("semantic provider path escapes repository: %s", providerPath)
	}
	if strings.Contains(providerPath, "\\") {
		return "", fmt.Errorf("semantic provider path must use slash separators: %s", providerPath)
	}
	clean := filepath.Clean(filepath.FromSlash(providerPath))
	cleanSlash := filepath.ToSlash(clean)
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("semantic provider path escapes repository: %s", providerPath)
	}
	if cleanSlash != providerPath {
		return "", fmt.Errorf("semantic provider path must be canonical: %s", providerPath)
	}
	return cleanSlash, nil
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

func buildSemanticGeneration(brainDir, repoDir, generationID string, raw []byte, header semanticHeader, counts semanticCounts, now time.Time) (string, semanticBuildMetrics, error) {
	start := time.Now()
	snapshotSHA := semanticSnapshotSHA256(raw)
	generationsRoot := filepath.Join(brainDir, semanticDirName, semanticGenerationsDir)
	if err := rejectExistingSymlinkPathComponents(brainDir, filepath.Join(semanticDirName, semanticGenerationsDir)); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if err := os.MkdirAll(generationsRoot, 0o700); err != nil {
		return "", semanticBuildMetrics{}, fmt.Errorf("create semantic generations dir: %w", err)
	}
	generationRel := filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, generationID))
	finalDir := filepath.Join(generationsRoot, generationID)
	if semanticGenerationReady(finalDir, generationID, snapshotSHA, header, counts) {
		return generationRel, semanticBuildMetrics{GenerationID: generationID}, nil
	}
	if _, err := os.Lstat(finalDir); err == nil {
		generationID = uniqueSemanticGenerationID(generationsRoot, generationID, raw)
		generationRel = filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, generationID))
		finalDir = filepath.Join(generationsRoot, generationID)
	} else if !os.IsNotExist(err) {
		return "", semanticBuildMetrics{}, err
	}

	tmpDir, err := os.MkdirTemp(generationsRoot, ".tmp-"+generationID+"-*")
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	dbPath := filepath.Join(tmpDir, semanticSQLiteName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		return "", semanticBuildMetrics{}, err
	}
	metrics, err := populateSemanticSQLite(db, repoDir, generationID, snapshotSHA, raw, header, counts, now, tmpDir)
	if closeErr := db.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	metrics.StoreBytes = info.Size()
	metrics.BuildMillis = time.Since(start).Milliseconds()
	metricsData, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	metricsData = append(metricsData, '\n')
	if err := writeFileAtomic(filepath.Join(tmpDir, semanticMetricsName), metricsData, 0o600); err != nil {
		return "", semanticBuildMetrics{}, err
	}

	if err := rejectExistingSymlinkPathComponents(brainDir, filepath.Join(semanticDirName, semanticGenerationsDir, generationID)); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if semanticGenerationReady(finalDir, generationID, snapshotSHA, header, counts) {
		return generationRel, metrics, nil
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if err := syncParentDir(finalDir); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	cleanup = false
	return generationRel, metrics, nil
}

func semanticSnapshotSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func semanticGenerationReady(dir, expectedGenerationID, expectedSnapshotSHA string, header semanticHeader, counts semanticCounts) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	metricsPath := filepath.Join(dir, semanticMetricsName)
	metricsInfo, err := os.Lstat(metricsPath)
	if err != nil || metricsInfo.IsDir() || metricsInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	var metrics semanticBuildMetrics
	data, err := os.ReadFile(metricsPath)
	if err != nil || json.Unmarshal(data, &metrics) != nil || metrics.GenerationID != expectedGenerationID || metrics.SnapshotSHA256 != expectedSnapshotSHA {
		return false
	}
	if metrics.Files != counts.Files || metrics.Symbols != counts.Symbols || metrics.Relations != counts.Relations {
		return false
	}
	return semanticSQLiteStoreReady(filepath.Join(dir, semanticSQLiteName), expectedGenerationID, expectedSnapshotSHA, header, counts)
}

func semanticSQLiteStoreReady(path, expectedGenerationID, expectedSnapshotSHA string, header semanticHeader, counts semanticCounts) bool {
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return false
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return false
	}
	meta, err := semanticSQLiteMetaMap(db)
	if err != nil {
		return false
	}
	if meta["generation_id"] != expectedGenerationID || meta["snapshot_sha256"] != expectedSnapshotSHA {
		return false
	}
	if meta["schema_version"] != header.SchemaVersion || meta["provider"] != header.Provider || meta["provider_version"] != header.ProviderVersion {
		return false
	}
	files, err := semanticSQLiteTableCount(db, "files")
	if err != nil || files != counts.Files {
		return false
	}
	symbols, err := semanticSQLiteTableCount(db, "symbols")
	if err != nil || symbols != counts.Symbols {
		return false
	}
	relations, err := semanticSQLiteTableCount(db, "relations")
	if err != nil || relations != counts.Relations {
		return false
	}
	if _, err := semanticSQLiteTableCount(db, "metrics"); err != nil {
		return false
	}
	return true
}

func uniqueSemanticGenerationID(generationsRoot, baseID string, content []byte) string {
	sum := sha256.Sum256(content)
	prefix := baseID + "-rebuild-" + hex.EncodeToString(sum[:6])
	for i := 0; ; i++ {
		candidate := prefix
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", prefix, i+1)
		}
		if _, err := os.Lstat(filepath.Join(generationsRoot, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
}

func initializeSemanticSQLite(db *sql.DB) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE files (path TEXT PRIMARY KEY, blob TEXT, content_hash TEXT, language TEXT)`,
		`CREATE TABLE symbols (id TEXT PRIMARY KEY, kind TEXT, name TEXT, qualified_name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, signature TEXT, language TEXT, stable_id_version TEXT)`,
		`CREATE TABLE relations (id INTEGER PRIMARY KEY AUTOINCREMENT, from_id TEXT NOT NULL, to_id TEXT NOT NULL, type TEXT NOT NULL, confidence REAL, reason TEXT, warning_codes TEXT)`,
		`CREATE TABLE reverse_relations (to_id TEXT NOT NULL, from_id TEXT NOT NULL, type TEXT NOT NULL)`,
		`CREATE INDEX idx_symbols_name ON symbols(name)`,
		`CREATE INDEX idx_symbols_qualified_name ON symbols(qualified_name)`,
		`CREATE INDEX idx_symbols_file_path ON symbols(file_path)`,
		`CREATE INDEX idx_relations_from ON relations(from_id)`,
		`CREATE INDEX idx_relations_to ON relations(to_id)`,
		`CREATE INDEX idx_reverse_to ON reverse_relations(to_id)`,
		`CREATE TABLE symbol_fts (id TEXT PRIMARY KEY, text TEXT NOT NULL)`,
		`CREATE TABLE parse_cache (path TEXT PRIMARY KEY, blob TEXT, content_hash TEXT, artifact_path TEXT NOT NULL)`,
		`CREATE TABLE warnings (id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT, severity TEXT, path TEXT, effect TEXT, detail TEXT, source TEXT)`,
		`CREATE TABLE metrics (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("initialize semantic sqlite: %w", err)
		}
	}
	return nil
}

func populateSemanticSQLite(db *sql.DB, repoDir, generationID, snapshotSHA string, raw []byte, header semanticHeader, counts semanticCounts, now time.Time, generationDir string) (semanticBuildMetrics, error) {
	tx, err := db.Begin()
	if err != nil {
		return semanticBuildMetrics{}, err
	}
	commit := func(err error) (semanticBuildMetrics, error) {
		if err != nil {
			_ = tx.Rollback()
			return semanticBuildMetrics{}, err
		}
		if err := tx.Commit(); err != nil {
			return semanticBuildMetrics{}, err
		}
		return semanticBuildMetrics{}, nil
	}

	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?), (?, ?), (?, ?), (?, ?), (?, ?)`,
		"schema_version", header.SchemaVersion,
		"provider", header.Provider,
		"provider_version", header.ProviderVersion,
		"generation_id", generationID,
		"snapshot_sha256", snapshotSHA,
	); err != nil {
		return commit(err)
	}
	for _, warning := range header.Warnings {
		if err := insertSemanticWarning(tx, warning, "warning"); err != nil {
			return commit(err)
		}
	}
	for _, failure := range header.PartialFailures {
		if err := insertSemanticWarning(tx, failure, "partial_failure"); err != nil {
			return commit(err)
		}
	}

	files := make(map[string]semanticRecord)
	scanner := newSemanticScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		return commit(errors.New("semantic snapshot missing header"))
	}
	for scanner.Scan() {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var record semanticRecord
		if err := json.Unmarshal(text, &record); err != nil {
			return commit(err)
		}
		switch record.RecordType {
		case "file":
			path := record.semanticPath()
			if path != "" {
				path, err = validateSemanticProviderPath(path)
				if err != nil {
					return commit(err)
				}
				record.setSemanticPath(path)
				files[path] = mergeSemanticFileRecord(files[path], record)
			}
		case "symbol":
			path := record.semanticPath()
			if path != "" {
				path, err = validateSemanticProviderPath(path)
				if err != nil {
					return commit(err)
				}
				record.setSemanticPath(path)
				if _, ok := files[path]; !ok {
					files[path] = semanticRecord{RecordType: "file", FilePath: path, Language: record.Language}
				} else if files[path].Language == "" && record.Language != "" {
					existing := files[path]
					existing.Language = record.Language
					files[path] = existing
				}
			}
			if err := insertSemanticSymbol(tx, record); err != nil {
				return commit(err)
			}
		case "relation":
			if err := insertSemanticRelation(tx, record); err != nil {
				return commit(err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return commit(err)
	}

	parseCacheFiles := 0
	for path, record := range files {
		contentHash := semanticFileContentHash(repoDir, path)
		if contentHash == "" && record.Blob != "" {
			contentHash = "git-blob:" + record.Blob
		}
		artifactRel, err := writeSemanticParseCacheArtifact(generationDir, path, record.Blob, contentHash)
		if err != nil {
			return commit(err)
		}
		parseCacheFiles++
		if _, err := tx.Exec(`INSERT OR REPLACE INTO files(path, blob, content_hash, language) VALUES (?, ?, ?, ?)`, path, record.Blob, contentHash, record.Language); err != nil {
			return commit(err)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO parse_cache(path, blob, content_hash, artifact_path) VALUES (?, ?, ?, ?)`, path, record.Blob, contentHash, artifactRel); err != nil {
			return commit(err)
		}
	}

	metrics := semanticBuildMetrics{
		GeneratedAt:     now,
		GenerationID:    generationID,
		SnapshotSHA256:  snapshotSHA,
		Commit:          header.Commit,
		Tree:            header.Tree,
		Files:           len(files),
		Symbols:         counts.Symbols,
		Relations:       counts.Relations,
		ReverseEdges:    counts.Relations,
		Warnings:        len(header.Warnings),
		PartialFailures: len(header.PartialFailures),
		ParseCacheFiles: parseCacheFiles,
	}
	metricsJSON, _ := json.Marshal(metrics)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO metrics(key, value) VALUES (?, ?)`, "build", string(metricsJSON)); err != nil {
		return commit(err)
	}
	if err := tx.Commit(); err != nil {
		return semanticBuildMetrics{}, err
	}
	return metrics, nil
}

func insertSemanticWarning(tx *sql.Tx, warning semanticWarning, source string) error {
	_, err := tx.Exec(`INSERT INTO warnings(code, severity, path, effect, detail, source) VALUES (?, ?, ?, ?, ?, ?)`,
		warning.Code, warning.Severity, warning.Path, warning.Effect, warning.Detail, source)
	return err
}

func insertSemanticSymbol(tx *sql.Tx, record semanticRecord) error {
	if record.ID == "" {
		return nil
	}
	_, err := tx.Exec(`INSERT OR REPLACE INTO symbols(id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Kind, record.Name, record.QualifiedName, record.FilePath, record.StartLine, record.EndLine, record.Signature, record.Language, record.StableIDVersion)
	if err != nil {
		return err
	}
	searchText := strings.Join([]string{record.Name, record.QualifiedName, record.Signature, record.FilePath, record.Kind, record.Language}, " ")
	_, err = tx.Exec(`INSERT OR REPLACE INTO symbol_fts(id, text) VALUES (?, ?)`, record.ID, searchText)
	return err
}

func insertSemanticRelation(tx *sql.Tx, record semanticRecord) error {
	if record.FromID == "" || record.ToID == "" || record.Type == "" {
		return nil
	}
	warningCodes, err := json.Marshal(record.WarningCodes)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO relations(from_id, to_id, type, confidence, reason, warning_codes) VALUES (?, ?, ?, ?, ?, ?)`,
		record.FromID, record.ToID, record.Type, record.Confidence, record.Reason, string(warningCodes)); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO reverse_relations(to_id, from_id, type) VALUES (?, ?, ?)`, record.ToID, record.FromID, record.Type)
	return err
}

func semanticFileContentHash(repoDir, path string) string {
	if path == "" {
		return ""
	}
	clean, err := validateSemanticProviderPath(path)
	if err != nil {
		return ""
	}
	if err := rejectSymlinkPathComponents(repoDir, filepath.FromSlash(clean)); err != nil {
		return ""
	}
	f, err := os.Open(filepath.Join(repoDir, filepath.FromSlash(clean)))
	if err != nil {
		return ""
	}
	defer f.Close()
	hasher := sha256.New()
	limited := io.LimitReader(f, semanticParseCacheMaxFile+1)
	n, err := io.Copy(hasher, limited)
	if err != nil || n > semanticParseCacheMaxFile {
		return ""
	}
	sum := hasher.Sum(nil)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeSemanticParseCacheArtifact(generationDir, path, blob, contentHash string) (string, error) {
	key := contentHash
	if key == "" {
		sum := sha256.Sum256([]byte(path + "\x00" + blob))
		key = "sha256:" + hex.EncodeToString(sum[:])
	}
	if !strings.HasPrefix(key, "sha256:") {
		sum := sha256.Sum256([]byte(path + "\x00" + blob + "\x00" + key))
		key = "sha256:" + hex.EncodeToString(sum[:])
	}
	cleanKey := strings.TrimPrefix(key, "sha256:")
	if len(cleanKey) < 4 {
		cleanKey = hex.EncodeToString([]byte(cleanKey))
	}
	rel := filepath.Join("parse-cache", cleanKey[:2], cleanKey[2:4], cleanKey+".json")
	payload := map[string]string{
		"path":         path,
		"blob":         blob,
		"content_hash": key,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := writeFileAtomic(filepath.Join(generationDir, rel), data, 0o600); err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func writeSemanticBranchOverlay(ctx context.Context, runner CommandRunner, brainDir, repoDir, branch, defaultBranch, head, snapshotRel string, now time.Time) (string, error) {
	if branch == "" || defaultBranch == "" || head == "" || branch == defaultBranch {
		return "", nil
	}
	base := strings.TrimSpace(string(runGitOutput(ctx, runner, repoDir, "rev-parse", "refs/heads/"+defaultBranch)))
	if base == "" {
		base = strings.TrimSpace(string(runGitOutput(ctx, runner, repoDir, "rev-parse", "refs/remotes/origin/"+defaultBranch)))
	}
	if base == "" || base == head {
		return "", nil
	}
	return writeSemanticOverlayFile(brainDir, base, head, branch, snapshotRel, now)
}

func writeSemanticOverlayFile(brainDir, base, head, branch, snapshotRel string, now time.Time) (string, error) {
	name := base + ".." + head + ".json"
	rel := filepath.ToSlash(filepath.Join(semanticDirName, "overlays", name))
	payload := map[string]string{
		"base":         base,
		"head":         head,
		"branch":       branch,
		"generated_at": now.Format(time.RFC3339),
	}
	if snapshotRel != "" {
		payload["snapshot_path"] = snapshotRel
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := rejectExistingSymlinkPathComponents(brainDir, filepath.FromSlash(rel)); err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(brainDir, filepath.FromSlash(rel)), data, 0o600); err != nil {
		return "", err
	}
	return rel, nil
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

func (i brainIgnore) gitPathspecExclusions() []string {
	patterns := []string{"node_modules/**", "vendor/**", "dist/**", "build/**", "target/**", ".entire/**", ".git/**", ".env", "*.pem", "*.key"}
	for _, pattern := range i.patterns {
		pattern = strings.TrimSpace(filepath.ToSlash(pattern))
		if pattern == "" {
			continue
		}
		pattern = strings.TrimPrefix(pattern, "./")
		if strings.HasSuffix(pattern, "/") {
			pattern += "**"
		}
		patterns = append(patterns, pattern)
	}
	result := make([]string, 0, len(patterns))
	seen := map[string]struct{}{}
	for _, pattern := range patterns {
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		result = append(result, ":(exclude)"+pattern)
	}
	return result
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

func semanticProviderIgnoreFiles(repoDir string) ([]string, error) {
	path := filepath.Join(repoDir, ".brainignore")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat .brainignore: %w", err)
	}
	if info.IsDir() {
		return nil, errors.New(".brainignore is a directory")
	}
	return []string{path}, nil
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
		if brainIgnorePatternMatches(pattern, path) {
			return true
		}
	}
	return false
}

func brainIgnorePatternMatches(pattern, target string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	pattern = strings.TrimPrefix(pattern, "./")
	target = filepath.ToSlash(strings.TrimPrefix(target, "./"))
	if pattern == "" || target == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		prefix := strings.TrimSuffix(pattern, "/")
		return target == prefix || strings.HasPrefix(target, prefix+"/")
	}
	if strings.Contains(pattern, "**") {
		return brainIgnoreSegmentsMatch(strings.Split(pattern, "/"), strings.Split(target, "/"))
	}
	if pattern == target || strings.HasPrefix(target, pattern+"/") {
		return true
	}
	if ok, _ := pathpkg.Match(pattern, target); ok {
		return true
	}
	return false
}

func brainIgnoreSegmentsMatch(pattern, target []string) bool {
	if len(pattern) == 0 {
		return len(target) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(target); i++ {
			if brainIgnoreSegmentsMatch(pattern[1:], target[i:]) {
				return true
			}
		}
		return false
	}
	if len(target) == 0 {
		return false
	}
	ok, err := pathpkg.Match(pattern[0], target[0])
	if err != nil || !ok {
		return false
	}
	return brainIgnoreSegmentsMatch(pattern[1:], target[1:])
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

func sanitizeSemanticWarnings(warnings []semanticWarning, repoDir string) []semanticWarning {
	if len(warnings) == 0 {
		return nil
	}
	sanitized := make([]semanticWarning, 0, len(warnings))
	for _, warning := range warnings {
		warning.Path = sanitizeSemanticWarningPath(warning.Path)
		warning.Effect = sanitizeSemanticWarningText(warning.Effect, repoDir)
		warning.Detail = sanitizeSemanticWarningText(warning.Detail, repoDir)
		sanitized = append(sanitized, warning)
	}
	return sanitized
}

func sanitizeSemanticSourceForExport(source *semanticSourceManifest, repoDir string) *semanticSourceManifest {
	if source == nil {
		return nil
	}
	sanitized := *source
	sanitized.Warnings = sanitizeSemanticWarnings(source.Warnings, repoDir)
	sanitized.PartialFailures = sanitizeSemanticWarnings(source.PartialFailures, repoDir)
	return &sanitized
}

func sanitizeSemanticWarningPath(path string) string {
	if path == "" {
		return ""
	}
	clean, err := validateSemanticProviderPath(filepath.ToSlash(path))
	if err != nil {
		return "<redacted>"
	}
	return clean
}

func sanitizeSemanticWarningText(text, repoDir string) string {
	if text == "" {
		return ""
	}
	replacements := []string{}
	if repoDir != "" {
		replacements = append(replacements, repoDir, filepath.ToSlash(repoDir))
	}
	if containsUnixAbsolutePathText(text) || containsWindowsDrivePathText(text) {
		return "<redacted>"
	}
	for _, replacement := range replacements {
		if replacement != "" {
			text = strings.ReplaceAll(text, replacement, "<repo>")
		}
	}
	return text
}

func containsWindowsDrivePathText(text string) bool {
	for i := 0; i+2 < len(text); i++ {
		if !isASCIIAlpha(text[i]) || text[i+1] != ':' {
			continue
		}
		if i > 0 && !isPathBoundary(text[i-1]) {
			continue
		}
		next := text[i+2]
		if next == '\\' || next == '/' || !isPathBoundary(next) {
			return true
		}
	}
	return false
}

func containsUnixAbsolutePathText(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] != '/' {
			continue
		}
		if i > 0 && !isPathBoundary(text[i-1]) {
			continue
		}
		if i+1 >= len(text) || text[i+1] == '/' || isPathBoundary(text[i+1]) {
			continue
		}
		if hasHTTPMethodPrefix(text[:i]) {
			continue
		}
		end := i + 1
		for end < len(text) && !isPathBoundary(text[end]) {
			end++
		}
		candidate := strings.TrimRight(text[i:end], ".:")
		if looksLikeUnixAbsolutePath(candidate) {
			return true
		}
	}
	return false
}

func looksLikeUnixAbsolutePath(candidate string) bool {
	if strings.Count(candidate, "/") < 2 {
		return false
	}
	return !strings.Contains(candidate, "{")
}

func hasHTTPMethodPrefix(prefix string) bool {
	for _, method := range []string{"GET ", "POST ", "PUT ", "PATCH ", "DELETE ", "HEAD ", "OPTIONS "} {
		if strings.HasSuffix(prefix, method) {
			return true
		}
	}
	return false
}

func isASCIIAlpha(ch byte) bool {
	return (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
}

func isPathBoundary(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\r', '"', '\'', '(', ')', '[', ']', '{', '}', ',', ';':
		return true
	default:
		return false
	}
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
	ignore, err := loadBrainIgnore(repoDir)
	if err != nil {
		return false, err
	}
	return worktreeDirtyWithIgnore(ctx, runner, repoDir, ignore)
}

func worktreeDirtyWithIgnore(ctx context.Context, runner CommandRunner, repoDir string, ignore brainIgnore) (bool, error) {
	stdout, err := gitStatusPorcelainAll(ctx, runner, repoDir)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(filterWorktreeStatus(stdout, ignore))) != "", nil
}

func verifySemanticWorktreeStable(ctx context.Context, runner CommandRunner, repoDir string, worktreeMode, dirtyBefore bool, hashBefore string) error {
	dirtyAfter, err := worktreeDirty(ctx, runner, repoDir)
	if err != nil {
		return fmt.Errorf("recheck worktree dirtiness for semantic index: %w", err)
	}
	if !worktreeMode {
		if dirtyAfter {
			return errors.New("worktree_changed: worktree became dirty during semantic indexing")
		}
		return nil
	}
	if dirtyBefore != dirtyAfter {
		return errors.New("worktree_changed: worktree dirtiness changed during semantic indexing")
	}
	hashAfter, err := worktreeFingerprint(ctx, runner, repoDir)
	if err != nil {
		return fmt.Errorf("fingerprint worktree after semantic index: %w", err)
	}
	if hashBefore != hashAfter {
		return errors.New("worktree_changed: worktree content changed during semantic indexing")
	}
	return nil
}

func worktreeFingerprint(ctx context.Context, runner CommandRunner, repoDir string) (string, error) {
	status, err := gitStatusPorcelainAll(ctx, runner, repoDir)
	if err != nil {
		return "", err
	}
	ignore, err := loadBrainIgnore(repoDir)
	if err != nil {
		return "", err
	}
	diff, err := gitDiffBinary(ctx, runner, repoDir, false, ignore)
	if err != nil {
		return "", err
	}
	cached, err := gitDiffBinary(ctx, runner, repoDir, true, ignore)
	if err != nil {
		return "", err
	}
	status = filterWorktreeStatus(status, ignore)
	hasher := sha256.New()
	total := int64(0)
	for _, data := range [][]byte{status, diff, cached} {
		if err := writeWorktreeFingerprintBytes(hasher, &total, data); err != nil {
			return "", err
		}
	}
	if err := appendUntrackedWorktreeContent(hasher, &total, repoDir, status, ignore); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func gitStatusPorcelainAll(ctx context.Context, runner CommandRunner, repoDir string) ([]byte, error) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", "status", "--porcelain", "--untracked-files=all")
	if err == nil {
		return stdout, nil
	}
	stdout, _, fallbackErr := runner.Run(ctx, repoDir, "git", "status", "--porcelain")
	if fallbackErr != nil {
		return nil, err
	}
	return stdout, nil
}

func gitDiffBinary(ctx context.Context, runner CommandRunner, repoDir string, cached bool, ignore brainIgnore) ([]byte, error) {
	args := []string{"diff"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, "--binary", "HEAD")
	exclusions := ignore.gitPathspecExclusions()
	if len(exclusions) > 0 {
		args = append(args, "--", ".")
		args = append(args, exclusions...)
	}
	stdout, _, err := runner.Run(ctx, repoDir, "git", args...)
	if err == nil {
		return stdout, nil
	}
	args = []string{"diff"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, "--binary", "HEAD")
	stdout, _, fallbackErr := runner.Run(ctx, repoDir, "git", args...)
	if fallbackErr != nil {
		return nil, err
	}
	return stdout, nil
}

func filterWorktreeStatus(status []byte, ignore brainIgnore) []byte {
	var filtered []string
	for _, line := range strings.Split(string(status), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		paths := worktreeStatusPaths(line)
		if len(paths) > 0 {
			allIgnored := true
			for _, path := range paths {
				if !ignore.Ignored(path) {
					allIgnored = false
					break
				}
			}
			if allIgnored {
				continue
			}
		}
		filtered = append(filtered, line)
	}
	if len(filtered) == 0 {
		return nil
	}
	return []byte(strings.Join(filtered, "\n") + "\n")
}

func worktreeStatusPaths(line string) []string {
	if strings.HasPrefix(line, "?? ") && len(line) > 3 {
		return []string{decodeGitPorcelainPath(strings.TrimSpace(line[3:]))}
	}
	if len(line) > 3 {
		path := strings.TrimSpace(line[3:])
		if before, after, ok := strings.Cut(path, " -> "); ok {
			return []string{decodeGitPorcelainPath(strings.TrimSpace(before)), decodeGitPorcelainPath(strings.TrimSpace(after))}
		}
		return []string{decodeGitPorcelainPath(path)}
	}
	return nil
}

func decodeGitPorcelainPath(path string) string {
	if len(path) >= 2 && path[0] == '"' && path[len(path)-1] == '"' {
		if unquoted, err := strconv.Unquote(path); err == nil {
			return unquoted
		}
	}
	return path
}

func writeWorktreeFingerprintBytes(hasher hash.Hash, total *int64, data []byte) error {
	*total += int64(len(data))
	if *total > semanticWorktreeFingerprintMaxTotal {
		return errors.New("worktree fingerprint exceeds total size limit")
	}
	_, err := hasher.Write(data)
	return err
}

func appendUntrackedWorktreeContent(hasher hash.Hash, total *int64, repoDir string, status []byte, ignore brainIgnore) error {
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "?? ") {
			continue
		}
		path := decodeGitPorcelainPath(strings.TrimSpace(strings.TrimPrefix(line, "?? ")))
		clean, err := validateSemanticProviderPath(path)
		if err != nil {
			continue
		}
		if ignore.Ignored(clean) {
			continue
		}
		if err := appendWorktreePathContent(hasher, total, repoDir, clean, ignore); err != nil {
			return err
		}
	}
	return nil
}

func appendWorktreePathContent(hasher hash.Hash, total *int64, repoDir, clean string, ignore brainIgnore) error {
	path := filepath.Join(repoDir, filepath.FromSlash(clean))
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		return filepath.WalkDir(path, func(child string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(repoDir, child)
			if err != nil {
				return nil
			}
			relSlash := filepath.ToSlash(rel)
			if _, err := validateSemanticProviderPath(relSlash); err != nil {
				return nil
			}
			if d.IsDir() {
				if ignore.Ignored(relSlash) {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if ignore.Ignored(relSlash) {
				return nil
			}
			return appendFileFingerprintContent(hasher, total, child, relSlash)
		})
	}
	return appendFileFingerprintContent(hasher, total, path, clean)
}

func appendFileFingerprintContent(hasher hash.Hash, total *int64, path, rel string) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if err := writeWorktreeFingerprintBytes(hasher, total, []byte("\x00untracked:"+rel+"\x00")); err != nil {
		return err
	}
	limited := io.LimitReader(f, semanticWorktreeFingerprintMaxFile+1)
	n, err := io.Copy(hasher, limited)
	if err != nil {
		return err
	}
	if n > semanticWorktreeFingerprintMaxFile {
		return fmt.Errorf("untracked file exceeds worktree fingerprint size limit: %s", rel)
	}
	*total += n
	if *total > semanticWorktreeFingerprintMaxTotal {
		return errors.New("worktree fingerprint exceeds total size limit")
	}
	return nil
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
	var blindSpots bool
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
			return runSemanticStale(cmd.Context(), cmd, opts, target, jsonOut, blindSpots)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&blindSpots, "blind-spots", false, "List the files the semantic provider failed to fully index")
	return cmd
}

// staleReportWithBlindSpots augments a freshness report with the concrete list
// of files the semantic provider could not fully index, so an agent knows
// exactly where its semantic answers are untrustworthy instead of only seeing
// an aggregate "N partial failures" count.
type staleReportWithBlindSpots struct {
	staleReport
	BlindSpots []brainBlindSpot `json:"blind_spots,omitempty"`
}

type brainBlindSpot struct {
	Path   string `json:"path"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func brainBlindSpotsForRepo(ctx context.Context, opts Options, target string) ([]brainBlindSpot, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return nil, err
	}
	if !local {
		return nil, fmt.Errorf("blind-spots requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return nil, err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return nil, nil
	}
	seen := map[string]struct{}{}
	var spots []brainBlindSpot
	unknownPaths := 0
	for _, failure := range manifest.Sources.Semantic.PartialFailures {
		if strings.TrimSpace(failure.Path) == "" {
			unknownPaths++
			continue
		}
		key := failure.Path + "|" + failure.Code
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		spots = append(spots, brainBlindSpot{Path: failure.Path, Code: failure.Code, Detail: failure.Detail})
	}
	sort.Slice(spots, func(i, j int) bool { return spots[i].Path < spots[j].Path })
	// Brains indexed before file paths were preserved on partial failures only
	// record the count. Surface that explicitly so the gap reads as "refresh to
	// locate these" rather than "only one file affected".
	if unknownPaths > 0 {
		spots = append(spots, brainBlindSpot{
			Code:   "E_PATH_UNAVAILABLE",
			Detail: fmt.Sprintf("%d partial failures without a recorded path; run `entire brain refresh` to locate them", unknownPaths),
		})
	}
	return spots, nil
}

func runSemanticStale(ctx context.Context, cmd *cobra.Command, opts Options, target string, jsonOut, blindSpots bool) error {
	report, err := semanticStaleReport(ctx, opts, target)
	if err != nil {
		return err
	}
	var spots []brainBlindSpot
	if blindSpots {
		spots, err = brainBlindSpotsForRepo(ctx, opts, target)
		if err != nil {
			return err
		}
	}
	if jsonOut {
		var payload any = report
		if blindSpots {
			payload = staleReportWithBlindSpots{staleReport: report, BlindSpots: spots}
		}
		data, err := json.MarshalIndent(payload, "", "  ")
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
	if blindSpots {
		if len(spots) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "blind-spots: none")
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "blind-spots: %d files not fully indexed\n", len(spots))
			for _, spot := range spots {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s", spot.Path)
				if spot.Code != "" {
					fmt.Fprintf(cmd.OutOrStdout(), " [%s]", spot.Code)
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
		}
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
	if source.StorePath != "" {
		if source.GenerationPath == "" {
			axes["store"] = staleAxis{State: "unsafe", Detail: "semantic generation_path is missing"}
		} else if generationPath, err := validateSemanticGenerationPath(source.GenerationPath); err != nil {
			axes["store"] = staleAxis{State: "unsafe", Detail: err.Error()}
		} else if storePath, err := validateSemanticGenerationFilePath(source.StorePath, generationPath, semanticSQLiteName); err != nil {
			axes["store"] = staleAxis{State: "unsafe", Detail: err.Error()}
		} else if err := rejectSymlinkPathComponents(storage.BrainDir, storePath); err != nil {
			axes["store"] = staleAxis{State: "unsafe", Detail: err.Error()}
		} else if err := validateSemanticSQLiteStore(filepath.Join(storage.BrainDir, storePath), source.Files, source.Symbols, source.Relations); err != nil {
			axes["store"] = staleAxis{State: "unsafe", Detail: err.Error()}
		} else {
			axes["store"] = staleAxis{State: "ok", Detail: source.StorePath}
		}
	}
	head, headErr := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD")
	branch, branchErr := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current")
	dirty, dirtyErr := worktreeDirty(ctx, opts.Runner, repoDir)
	currentWorktreeHash := ""
	var currentWorktreeHashErr error
	if dirty && source.WorktreeHash != "" {
		currentWorktreeHash, currentWorktreeHashErr = worktreeFingerprint(ctx, opts.Runner, repoDir)
	}
	if headErr != nil {
		axes["head"] = staleAxis{State: "unsafe", Detail: "HEAD unavailable: " + headErr.Error(), Indexed: source.Commit}
	} else if source.Commit == head {
		axes["head"] = staleAxis{State: "ok", Current: head, Indexed: source.Commit}
	} else {
		axes["head"] = staleAxis{State: "stale", Current: head, Indexed: source.Commit}
	}
	if branchErr != nil {
		axes["branch_tip"] = staleAxis{State: "unsafe", Detail: "branch unavailable: " + branchErr.Error(), Indexed: source.Branch}
	} else if source.Branch == "" || source.Branch == branch {
		axes["branch_tip"] = staleAxis{State: "ok", Current: branch, Indexed: source.Branch}
	} else {
		axes["branch_tip"] = staleAxis{State: "stale", Current: branch, Indexed: source.Branch}
	}
	switch {
	case dirtyErr != nil:
		axes["worktree"] = staleAxis{State: "unsafe", Detail: "worktree status unavailable: " + dirtyErr.Error()}
	case dirty && source.WorktreeMode != "worktree":
		axes["worktree"] = staleAxis{State: "dirty-unindexed", Detail: "semantic index is based on committed HEAD"}
	case currentWorktreeHashErr != nil:
		axes["worktree"] = staleAxis{State: "unsafe", Detail: "worktree fingerprint unavailable: " + currentWorktreeHashErr.Error()}
	case dirty && source.WorktreeHash != "" && source.WorktreeHash != currentWorktreeHash:
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
		axes["semantic_completeness"] = semanticCompletenessAxis(source)
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

// semanticParseErrorCode marks files where the tree-sitter parse produced error
// nodes but symbols were still extracted (error-tolerant parse). A small number
// of these is expected for any real codebase — newer language syntax the bundled
// grammar version predates — and does not make the indexed facts unsafe to use.
const semanticParseErrorCode = "E_PARSE_ERROR"

// semanticParseErrorTolerance is the fraction of indexed files that may carry
// only benign parse errors before the brain is reported as degraded. Below this
// threshold the completeness axis stays ok; the failures remain recorded in the
// semantic source so they can still be inspected and acted on. Above it, the
// volume is high enough to suspect a grammar/provider problem worth surfacing.
const semanticParseErrorTolerance = 0.10

// semanticCompletenessAxis classifies partial provider failures. Failures that
// are exclusively benign parse errors and stay under the tolerance fraction keep
// the axis ok; anything else (a non-parse failure code, an uncountable file set,
// or too many parse errors) degrades the brain.
func semanticCompletenessAxis(source *semanticSourceManifest) staleAxis {
	failures := len(source.PartialFailures)
	parseErrors := 0
	for _, failure := range source.PartialFailures {
		if failure.Code == semanticParseErrorCode {
			parseErrors++
		}
	}
	if parseErrors == failures && source.Files > 0 {
		fraction := float64(failures) / float64(source.Files)
		if fraction < semanticParseErrorTolerance {
			return staleAxis{State: "ok", Detail: fmt.Sprintf("%d/%d files had tolerated parse errors", failures, source.Files)}
		}
		return staleAxis{State: "degraded", Detail: fmt.Sprintf("%d partial failures in %.0f%% of files", failures, fraction*100)}
	}
	return staleAxis{State: "degraded", Detail: strconv.Itoa(failures) + " partial failures"}
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
	limit  int
	offset int
	json   bool
}

type semanticContextOptions struct {
	limit          int
	offset         int
	includeContent bool
	json           bool
}

type semanticContextResult struct {
	Symbols   []semanticRecord  `json:"symbols"`
	Relations []semanticRecord  `json:"relations"`
	Neighbors []semanticRecord  `json:"neighbors,omitempty"`
	Content   []semanticContent `json:"content,omitempty"`
}

// nonNilRecords guarantees a JSON array (`[]`) rather than `null` for an empty
// result, so consumers can use one uniform shape across every brain command
// instead of special-casing null per field.
func nonNilRecords(records []semanticRecord) []semanticRecord {
	if records == nil {
		return []semanticRecord{}
	}
	return records
}

type semanticContent struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

type semanticImpactOptions struct {
	limit int
	depth int
	json  bool
}

type semanticImpactResult struct {
	Roots     []semanticRecord `json:"roots"`
	Symbols   []semanticRecord `json:"symbols"`
	Relations []semanticRecord `json:"relations"`
}

type semanticChangesOptions struct {
	limit int
	json  bool
}

type semanticChangesReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Files       []string         `json:"files"`
	Symbols     []semanticRecord `json:"symbols"`
}

type semanticBoundaryOptions struct {
	limit int
	json  bool
}

type semanticBoundarySpec struct {
	Name          string
	Use           string
	Short         string
	SymbolKinds   []string
	RelationTypes []string
}

type semanticBoundaryResult struct {
	Boundaries []semanticRecord `json:"boundaries"`
	Handlers   []semanticRecord `json:"handlers"`
	Relations  []semanticRecord `json:"relations"`
}

type semanticTestsOptions struct {
	limit int
	json  bool
}

type semanticTestSuggestion struct {
	Symbol semanticRecord `json:"symbol"`
	Reason string         `json:"reason"`
}

type semanticTestsResult struct {
	Roots       []semanticRecord         `json:"roots"`
	Suggestions []semanticTestSuggestion `json:"suggestions"`
}

func runSemanticQuery(ctx context.Context, cmd *cobra.Command, opts Options, queryOpts semanticQueryOptions, query string) error {
	if queryOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	if queryOpts.offset < 0 {
		return errors.New("--offset must be zero or greater")
	}
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	snapshotPath, err := validateSemanticSnapshotPath(manifest.Sources.Semantic.SnapshotPath)
	if err != nil {
		return err
	}
	if err := rejectSymlinkPathComponents(storage.BrainDir, snapshotPath); err != nil {
		return err
	}
	var results []semanticRecord
	if manifest.Sources.Semantic.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(storage.BrainDir, manifest.Sources.Semantic)
		if err != nil {
			return err
		}
		results, err = findSemanticSymbolsInSQLite(storePath, query, queryOpts.limit, queryOpts.offset)
		if err != nil {
			return err
		}
	} else {
		results, err = findSemanticSymbols(filepath.Join(storage.BrainDir, snapshotPath), query, queryOpts.limit, queryOpts.offset)
		if err != nil {
			return err
		}
	}
	if queryOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness  staleReport      `json:"freshness"`
			Pagination semanticPage     `json:"pagination"`
			Results    []semanticRecord `json:"results"`
		}{Freshness: freshness, Pagination: semanticPage{Limit: queryOpts.limit, Offset: queryOpts.offset, Count: len(results)}, Results: nonNilRecords(results)}, "", "  ")
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

func runSemanticContext(ctx context.Context, cmd *cobra.Command, opts Options, contextOpts semanticContextOptions, query string) error {
	if contextOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	if contextOpts.offset < 0 {
		return errors.New("--offset must be zero or greater")
	}
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	if contextOpts.includeContent && freshness.Severity != "ok" {
		return fmt.Errorf("semantic context content requires fresh semantic data: %s", freshness.Severity)
	}
	symbols, relations, neighbors, err := semanticContextFacts(storage.BrainDir, manifest.Sources.Semantic, query, contextOpts.limit, contextOpts.offset)
	if err != nil {
		return err
	}
	result := semanticContextResult{Symbols: nonNilRecords(symbols), Relations: nonNilRecords(relations), Neighbors: nonNilRecords(neighbors)}
	if contextOpts.includeContent {
		result.Content = semanticContextContent(repoDir, symbols)
	}
	if contextOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness  staleReport           `json:"freshness"`
			Pagination semanticPage          `json:"pagination"`
			Context    semanticContextResult `json:"context"`
		}{Freshness: freshness, Pagination: semanticPage{Limit: contextOpts.limit, Offset: contextOpts.offset, Count: len(symbols)}, Context: result}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "semantic freshness: %s\n", freshness.Severity)
	}
	for _, symbol := range result.Symbols {
		fmt.Fprintf(cmd.OutOrStdout(), "symbol %s %s:%d-%d\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
	}
	for _, relation := range result.Relations {
		fmt.Fprintf(cmd.OutOrStdout(), "relation %s -> %s %s\n", relation.FromID, relation.ToID, relation.Type)
	}
	for _, neighbor := range result.Neighbors {
		fmt.Fprintf(cmd.OutOrStdout(), "neighbor %s %s:%d-%d\n", displaySymbolName(neighbor), neighbor.FilePath, neighbor.StartLine, neighbor.EndLine)
	}
	for _, content := range result.Content {
		fmt.Fprintf(cmd.OutOrStdout(), "content %s:%d-%d\n%s\n", content.Path, content.StartLine, content.EndLine, content.Text)
	}
	return nil
}

func runSemanticImpact(ctx context.Context, cmd *cobra.Command, opts Options, impactOpts semanticImpactOptions, query string) error {
	if impactOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	if impactOpts.depth <= 0 {
		return errors.New("--depth must be greater than zero")
	}
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	roots, symbols, relations, err := semanticImpactFacts(storage.BrainDir, manifest.Sources.Semantic, query, impactOpts.depth, impactOpts.limit)
	if err != nil {
		return err
	}
	result := semanticImpactResult{Roots: nonNilRecords(roots), Symbols: nonNilRecords(symbols), Relations: nonNilRecords(relations)}
	if impactOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness staleReport          `json:"freshness"`
			Impact    semanticImpactResult `json:"impact"`
		}{Freshness: freshness, Impact: result}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "semantic freshness: %s\n", freshness.Severity)
	}
	for _, symbol := range symbols {
		fmt.Fprintf(cmd.OutOrStdout(), "symbol %s %s:%d-%d\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
	}
	for _, relation := range relations {
		fmt.Fprintf(cmd.OutOrStdout(), "relation %s -> %s %s\n", relation.FromID, relation.ToID, relation.Type)
	}
	return nil
}

func runSemanticChanges(ctx context.Context, cmd *cobra.Command, opts Options, changesOpts semanticChangesOptions) error {
	if changesOpts.limit <= 0 {
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
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	files, err := changedSemanticFiles(ctx, opts.Runner, repoDir)
	if err != nil {
		return err
	}
	symbols, err := semanticSymbolsForFiles(storage.BrainDir, manifest.Sources.Semantic, files, changesOpts.limit)
	if err != nil {
		return err
	}
	report := semanticChangesReport{GeneratedAt: opts.Now().UTC(), Files: files, Symbols: symbols}
	if err := writeSemanticChangesReport(storage.BrainDir, report); err != nil {
		return err
	}
	if changesOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness staleReport           `json:"freshness"`
			Changes   semanticChangesReport `json:"changes"`
		}{Freshness: freshness, Changes: report}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "semantic freshness: %s\n", freshness.Severity)
	}
	for _, file := range files {
		fmt.Fprintf(cmd.OutOrStdout(), "file %s\n", file)
	}
	for _, symbol := range symbols {
		fmt.Fprintf(cmd.OutOrStdout(), "symbol %s %s:%d-%d\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
	}
	return nil
}

func runSemanticBoundary(ctx context.Context, cmd *cobra.Command, opts Options, boundaryOpts semanticBoundaryOptions, spec semanticBoundarySpec) error {
	if boundaryOpts.limit <= 0 {
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
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	result, err := semanticBoundaryFacts(storage.BrainDir, manifest.Sources.Semantic, spec, boundaryOpts.limit)
	if err != nil {
		return err
	}
	result.Boundaries = nonNilRecords(result.Boundaries)
	result.Handlers = nonNilRecords(result.Handlers)
	result.Relations = nonNilRecords(result.Relations)
	if boundaryOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness staleReport            `json:"freshness"`
			Boundary  semanticBoundaryResult `json:"boundary"`
		}{Freshness: freshness, Boundary: result}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "semantic freshness: %s\n", freshness.Severity)
	}
	if len(result.Boundaries) == 0 && len(result.Relations) == 0 && len(result.Handlers) == 0 {
		// Distinguish "the index has no such boundaries" from a silent failure:
		// these surfaces only exist if the semantic provider emitted them.
		fmt.Fprintf(cmd.OutOrStdout(), "no %s found in the semantic index\n", spec.Name)
		return nil
	}
	for _, boundary := range result.Boundaries {
		if boundary.FilePath == "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", boundary.Kind, displaySymbolName(boundary))
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s:%d-%d\n", boundary.Kind, displaySymbolName(boundary), boundary.FilePath, boundary.StartLine, boundary.EndLine)
	}
	for _, relation := range result.Relations {
		fmt.Fprintf(cmd.OutOrStdout(), "relation %s -> %s %s\n", relation.FromID, relation.ToID, relation.Type)
	}
	for _, handler := range result.Handlers {
		fmt.Fprintf(cmd.OutOrStdout(), "handler %s %s:%d-%d\n", displaySymbolName(handler), handler.FilePath, handler.StartLine, handler.EndLine)
	}
	return nil
}

func runSemanticTests(ctx context.Context, cmd *cobra.Command, opts Options, testsOpts semanticTestsOptions, query string) error {
	if testsOpts.limit <= 0 {
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
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	result, err := semanticTestFacts(storage.BrainDir, manifest.Sources.Semantic, query, testsOpts.limit)
	if err != nil {
		return err
	}
	result.Roots = nonNilRecords(result.Roots)
	if result.Suggestions == nil {
		result.Suggestions = []semanticTestSuggestion{}
	}
	if testsOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness staleReport         `json:"freshness"`
			Tests     semanticTestsResult `json:"tests"`
		}{Freshness: freshness, Tests: result}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "semantic freshness: %s\n", freshness.Severity)
	}
	for _, suggestion := range result.Suggestions {
		symbol := suggestion.Symbol
		fmt.Fprintf(cmd.OutOrStdout(), "test %s %s:%d-%d %s\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine, suggestion.Reason)
	}
	return nil
}

func semanticContextFacts(brainDir string, source *semanticSourceManifest, query string, limit, offset int) ([]semanticRecord, []semanticRecord, []semanticRecord, error) {
	if source.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(brainDir, source)
		if err != nil {
			return nil, nil, nil, err
		}
		symbols, err := findSemanticSymbolsInSQLite(storePath, query, limit, offset)
		if err != nil {
			return nil, nil, nil, err
		}
		relations, err := findSemanticRelationsForSymbolsInSQLite(storePath, symbols, limit*4)
		if err != nil {
			return nil, nil, nil, err
		}
		symbolsByID, err := loadSemanticSymbolsByIDSQLite(storePath)
		if err != nil {
			return nil, nil, nil, err
		}
		neighbors := semanticNeighborRecords(symbols, relations, symbolsByID, limit)
		return symbols, relations, neighbors, nil
	}
	snapshotPath, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotPath); err != nil {
		return nil, nil, nil, err
	}
	fullPath := filepath.Join(brainDir, snapshotPath)
	symbols, err := findSemanticSymbols(fullPath, query, limit, offset)
	if err != nil {
		return nil, nil, nil, err
	}
	relations, err := findSemanticRelationsForSymbols(fullPath, symbols, limit*4)
	if err != nil {
		return nil, nil, nil, err
	}
	symbolsByID, err := loadSemanticSymbolsByIDSnapshot(fullPath)
	if err != nil {
		return nil, nil, nil, err
	}
	neighbors := semanticNeighborRecords(symbols, relations, symbolsByID, limit)
	return symbols, relations, neighbors, nil
}

func semanticNeighborRecords(symbols, relations []semanticRecord, symbolsByID map[string]semanticRecord, limit int) []semanticRecord {
	if limit <= 0 || len(relations) == 0 || len(symbolsByID) == 0 {
		return nil
	}
	roots := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		if symbol.ID != "" {
			roots[symbol.ID] = struct{}{}
		}
	}
	seen := map[string]struct{}{}
	neighbors := make([]semanticRecord, 0, min(limit, len(relations)))
	add := func(id string) {
		if id == "" || len(neighbors) >= limit {
			return
		}
		if _, ok := roots[id]; ok {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		record, ok := symbolsByID[id]
		if !ok {
			return
		}
		seen[id] = struct{}{}
		neighbors = append(neighbors, record)
	}
	for _, relation := range relations {
		add(relation.FromID)
		add(relation.ToID)
		if len(neighbors) >= limit {
			break
		}
	}
	return neighbors
}

func semanticImpactFacts(brainDir string, source *semanticSourceManifest, query string, depth, limit int) ([]semanticRecord, []semanticRecord, []semanticRecord, error) {
	if source.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(brainDir, source)
		if err != nil {
			return nil, nil, nil, err
		}
		roots, err := findSemanticSymbolsInSQLite(storePath, query, limit, 0)
		if err != nil {
			return nil, nil, nil, err
		}
		symbols, relations, err := traverseSemanticImpactSQLite(storePath, roots, depth, limit)
		return roots, symbols, relations, err
	}
	snapshotPath, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotPath); err != nil {
		return nil, nil, nil, err
	}
	fullPath := filepath.Join(brainDir, snapshotPath)
	roots, err := findSemanticSymbols(fullPath, query, limit, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	symbols, relations, err := traverseSemanticImpactSnapshot(fullPath, roots, depth, limit)
	return roots, symbols, relations, err
}

func semanticSymbolsForFiles(brainDir string, source *semanticSourceManifest, files []string, limit int) ([]semanticRecord, error) {
	if source.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(brainDir, source)
		if err != nil {
			return nil, err
		}
		return findSemanticSymbolsForFilesSQLite(storePath, files, limit)
	}
	snapshotPath, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return nil, err
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotPath); err != nil {
		return nil, err
	}
	return findSemanticSymbolsForFiles(filepath.Join(brainDir, snapshotPath), files, limit)
}

// externalBoundaryRecord turns an "external:<kind>:<value>" node id into a
// synthetic boundary record when its kind matches the spec. Providers emit
// route/tool/workflow boundaries either as in-repo symbols (kind=route, with a
// file path) or as external endpoint nodes (no file path); this recovers the
// latter. Returns ok=false for non-external ids or kinds outside the set.
func externalBoundaryRecord(id string, kindSet map[string]struct{}) (semanticRecord, bool) {
	const prefix = "external:"
	if !strings.HasPrefix(id, prefix) {
		return semanticRecord{}, false
	}
	rest := id[len(prefix):]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return semanticRecord{}, false
	}
	kind := rest[:colon]
	if !semanticKindInSet(kind, kindSet) {
		return semanticRecord{}, false
	}
	name := rest[colon+1:]
	if name == "" {
		name = id
	}
	return semanticRecord{RecordType: "external", ID: id, Kind: kind, Name: name, QualifiedName: name}, true
}

func semanticBoundaryFacts(brainDir string, source *semanticSourceManifest, spec semanticBoundarySpec, limit int) (semanticBoundaryResult, error) {
	symbolsByID, relations, err := loadSemanticBoundaryRecords(brainDir, source, spec.RelationTypes)
	if err != nil {
		return semanticBoundaryResult{}, err
	}
	kindSet := lowerSet(spec.SymbolKinds)

	// isBoundaryEligible reports whether an endpoint could be a boundary —
	// either a kind-tagged in-repo symbol or an external:<kind>:* node matching
	// the spec. Used to keep limit-dropped boundaries from resurfacing as
	// handlers.
	isBoundaryEligible := func(id string) bool {
		if symbol, ok := symbolsByID[id]; ok {
			return semanticKindInSet(symbol.Kind, kindSet)
		}
		_, ok := externalBoundaryRecord(id, kindSet)
		return ok
	}

	// Phase 1: gather boundary candidates. Providers emit boundaries either as
	// in-repo symbols (kind=route, with a file path) or as external endpoint
	// nodes referenced by the handler relations; collect both.
	var candidates []semanticRecord
	seenCandidate := map[string]struct{}{}
	addCandidate := func(record semanticRecord) {
		if record.ID == "" {
			return
		}
		if _, ok := seenCandidate[record.ID]; ok {
			return
		}
		seenCandidate[record.ID] = struct{}{}
		candidates = append(candidates, record)
	}
	for _, symbol := range sortedSemanticSymbols(symbolsByID) {
		if semanticKindInSet(symbol.Kind, kindSet) {
			addCandidate(symbol)
		}
	}
	externalByID := map[string]semanticRecord{}
	var externalIDs []string
	for _, relation := range relations {
		for _, id := range []string{relation.FromID, relation.ToID} {
			if _, ok := symbolsByID[id]; ok {
				continue
			}
			if _, ok := externalByID[id]; ok {
				continue
			}
			if record, ok := externalBoundaryRecord(id, kindSet); ok {
				externalByID[id] = record
				externalIDs = append(externalIDs, id)
			}
		}
	}
	sort.Strings(externalIDs)
	for _, id := range externalIDs {
		addCandidate(externalByID[id])
	}

	// Phase 2: apply the limit to the boundary set.
	var result semanticBoundaryResult
	included := map[string]struct{}{}
	for _, record := range candidates {
		if len(result.Boundaries) >= limit {
			break
		}
		included[record.ID] = struct{}{}
		result.Boundaries = append(result.Boundaries, record)
	}

	// Phase 3: keep relations that touch an included boundary; the other,
	// non-boundary endpoint is its handler when it is a real indexed symbol.
	seenHandlers := map[string]struct{}{}
	for _, relation := range relations {
		_, fromIncluded := included[relation.FromID]
		_, toIncluded := included[relation.ToID]
		if !fromIncluded && !toIncluded {
			continue
		}
		result.Relations = append(result.Relations, relation)
		for _, id := range []string{relation.FromID, relation.ToID} {
			if _, ok := included[id]; ok {
				continue
			}
			if isBoundaryEligible(id) {
				continue
			}
			symbol, ok := symbolsByID[id]
			if !ok {
				continue
			}
			if _, seen := seenHandlers[id]; seen {
				continue
			}
			seenHandlers[id] = struct{}{}
			result.Handlers = append(result.Handlers, symbol)
		}
	}
	return result, nil
}

func semanticTestFacts(brainDir string, source *semanticSourceManifest, query string, limit int) (semanticTestsResult, error) {
	roots, _, relations, symbolsByID, err := semanticGraphFacts(brainDir, source, query, 1, limit)
	if err != nil {
		return semanticTestsResult{}, err
	}
	related := map[string]struct{}{}
	for _, root := range roots {
		related[root.ID] = struct{}{}
	}
	for _, relation := range relations {
		if _, ok := related[relation.FromID]; ok {
			related[relation.ToID] = struct{}{}
		}
		if _, ok := related[relation.ToID]; ok {
			related[relation.FromID] = struct{}{}
		}
	}
	rootDirs := map[string]struct{}{}
	rootNames := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.FilePath != "" {
			rootDirs[pathDirSlash(root.FilePath)] = struct{}{}
		}
		name := strings.ToLower(root.Name)
		if name != "" {
			rootNames = append(rootNames, name)
		}
	}
	result := semanticTestsResult{Roots: roots}
	seen := map[string]struct{}{}
	for _, symbol := range sortedSemanticSymbols(symbolsByID) {
		if !isSemanticTestSymbol(symbol) {
			continue
		}
		reason := semanticTestReason(symbol, related, rootDirs, rootNames)
		if reason == "" {
			continue
		}
		if _, ok := seen[symbol.ID]; ok {
			continue
		}
		seen[symbol.ID] = struct{}{}
		result.Suggestions = append(result.Suggestions, semanticTestSuggestion{Symbol: symbol, Reason: reason})
		if len(result.Suggestions) >= limit {
			break
		}
	}
	return result, nil
}

func semanticGraphFacts(brainDir string, source *semanticSourceManifest, query string, depth, limit int) ([]semanticRecord, []semanticRecord, []semanticRecord, map[string]semanticRecord, error) {
	if source.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(brainDir, source)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		roots, err := findSemanticSymbolsInSQLite(storePath, query, limit, 0)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		symbols, relations, err := traverseSemanticImpactSQLite(storePath, roots, depth, limit)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		symbolsByID, err := loadSemanticSymbolsByIDSQLite(storePath)
		return roots, symbols, relations, symbolsByID, err
	}
	snapshotPath, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotPath); err != nil {
		return nil, nil, nil, nil, err
	}
	fullPath := filepath.Join(brainDir, snapshotPath)
	roots, err := findSemanticSymbols(fullPath, query, limit, 0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	symbols, relations, err := traverseSemanticImpactSnapshot(fullPath, roots, depth, limit)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	symbolsByID, err := loadSemanticSymbolsByIDSnapshot(fullPath)
	return roots, symbols, relations, symbolsByID, err
}

func loadSemanticBoundaryRecords(brainDir string, source *semanticSourceManifest, relationTypes []string) (map[string]semanticRecord, []semanticRecord, error) {
	if source.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(brainDir, source)
		if err != nil {
			return nil, nil, err
		}
		symbolsByID, err := loadSemanticSymbolsByIDSQLite(storePath)
		if err != nil {
			return nil, nil, err
		}
		relations, err := findSemanticRelationsByTypesSQLite(storePath, relationTypes)
		return symbolsByID, relations, err
	}
	snapshotPath, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return nil, nil, err
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotPath); err != nil {
		return nil, nil, err
	}
	fullPath := filepath.Join(brainDir, snapshotPath)
	symbolsByID, err := loadSemanticSymbolsByIDSnapshot(fullPath)
	if err != nil {
		return nil, nil, err
	}
	relations, err := findSemanticRelationsByTypesSnapshot(fullPath, relationTypes)
	return symbolsByID, relations, err
}

func validateSemanticDeclaredStore(brainDir string, source *semanticSourceManifest) (string, error) {
	if source.GenerationPath == "" {
		return "", errors.New("semantic generation_path is missing")
	}
	generationPath, err := validateSemanticGenerationPath(source.GenerationPath)
	if err != nil {
		return "", err
	}
	storePath, err := validateSemanticGenerationFilePath(source.StorePath, generationPath, semanticSQLiteName)
	if err != nil {
		return "", err
	}
	if err := rejectSymlinkPathComponents(brainDir, storePath); err != nil {
		return "", err
	}
	fullPath := filepath.Join(brainDir, storePath)
	if err := validateSemanticSQLiteStore(fullPath, source.Files, source.Symbols, source.Relations); err != nil {
		return "", err
	}
	return fullPath, nil
}

type semanticPage struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Count  int `json:"count"`
}

// semanticQueryTokens reduces a free-text query to the distinct identifier-ish
// tokens worth searching the symbol index for. It powers the tokenized fallback
// so a natural-language task ("change how feeds are refreshed") matches symbols
// on its content words instead of only as a verbatim substring.
func semanticQueryTokens(query string) []string {
	seen := map[string]struct{}{}
	var tokens []string
	for _, raw := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		raw = strings.Trim(raw, "_")
		if len(raw) < 3 {
			continue
		}
		if historyQueryStopword(raw) {
			continue
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		tokens = append(tokens, raw)
		if len(tokens) >= 8 {
			break
		}
	}
	return tokens
}

// tokenIDFWeight scores a token by inverse document frequency: a token matching
// few symbols (e.g. "authentication") is far more discriminating than one
// matching many (e.g. "handler", "work"), so it must contribute more to a
// symbol's relevance. Returns a small positive integer for use as a SQL literal
// weight. Smoothed so a token matching nothing still ranks above none.
func tokenIDFWeight(total, df int) int {
	if total < 0 {
		total = 0
	}
	if df < 0 {
		df = 0
	}
	idf := math.Log(float64(total+1)/float64(df+1)) + 1
	weight := int(math.Round(idf * 10))
	if weight < 1 {
		weight = 1
	}
	return weight
}

// findSemanticSymbolsTokenizedSQLite ranks symbols by the inverse-document-
// frequency-weighted sum of query tokens appearing in their full-text body. It
// is the fallback for multi-word queries that never appear verbatim in any one
// symbol; weighting by rarity keeps a specific term (matched by few symbols)
// from being tied with a generic filler term (matched by many).
func findSemanticSymbolsTokenizedSQLite(db *sql.DB, tokens []string, limit, offset int) ([]semanticRecord, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM symbols`).Scan(&total); err != nil {
		return nil, err
	}
	weights := make([]int, len(tokens))
	for i, token := range tokens {
		var df int
		if err := db.QueryRow(`SELECT count(*) FROM symbol_fts WHERE instr(lower(text), ?) > 0`, token).Scan(&df); err != nil {
			return nil, err
		}
		weights[i] = tokenIDFWeight(total, df)
	}
	var conds, score strings.Builder
	args := make([]any, 0, len(tokens)*2+2)
	for i := range tokens {
		if i > 0 {
			conds.WriteString(" OR ")
			score.WriteString(" + ")
		}
		conds.WriteString("instr(lower(f.text), ?) > 0")
		// Weights are computed integers, safe to inline as SQL literals.
		fmt.Fprintf(&score, "(CASE WHEN instr(lower(f.text), ?) > 0 THEN %d ELSE 0 END)", weights[i])
	}
	// Token args are consumed once for the score expression and once for the
	// WHERE clause, in that order.
	for _, token := range tokens {
		args = append(args, token)
	}
	for _, token := range tokens {
		args = append(args, token)
	}
	args = append(args, limit, offset)
	q := `SELECT s.id, s.kind, s.name, s.qualified_name, s.file_path, s.start_line, s.end_line, s.signature, s.language, s.stable_id_version, (` + score.String() + `) AS hits
FROM symbols s
JOIN symbol_fts f ON f.id = s.id
WHERE ` + conds.String() + `
ORDER BY hits DESC, s.qualified_name
LIMIT ? OFFSET ?`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []semanticRecord
	for rows.Next() {
		var record semanticRecord
		record.RecordType = "symbol"
		var hits int
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion, &hits); err != nil {
			return nil, err
		}
		record.Score = hits
		results = append(results, record)
	}
	return results, rows.Err()
}

// semanticQueryLooksLikePath reports whether a query is a file path rather than
// free text. Path queries must resolve exactly: tokenizing "missing/file.ts"
// into ["missing","file"] would otherwise match half the index on the common
// token "file" and report a bogus blast radius for a path that does not exist.
func semanticQueryLooksLikePath(query string) bool {
	query = strings.TrimSpace(query)
	if strings.ContainsAny(query, "/\\") {
		return true
	}
	switch strings.ToLower(filepath.Ext(query)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".sql", ".java", ".rb", ".mjs", ".cjs":
		return true
	}
	return false
}

func findSemanticSymbolsInSQLite(storePath, query string, limit, offset int) ([]semanticRecord, error) {
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	literal := strings.ToLower(strings.TrimSpace(query))
	// Match the full-text body OR an exact id/qualified-name. The id branch lets
	// an agent paste a record id straight from `search`/`overview` results into
	// `context`/`impact` and get that exact symbol, instead of an empty result
	// because the colon-delimited id never appears as a substring of the body.
	rows, err := db.Query(`SELECT s.id, s.kind, s.name, s.qualified_name, s.file_path, s.start_line, s.end_line, s.signature, s.language, s.stable_id_version
FROM symbols s
JOIN symbol_fts f ON f.id = s.id
WHERE instr(lower(f.text), ?) > 0 OR lower(s.id) = ? OR lower(s.qualified_name) = ?
ORDER BY CASE WHEN lower(s.id) = ? THEN 0 WHEN lower(s.name) = lower(?) THEN 1 WHEN lower(s.qualified_name) = lower(?) THEN 2 ELSE 3 END, s.qualified_name
LIMIT ? OFFSET ?`, literal, literal, literal, literal, query, query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []semanticRecord
	for rows.Next() {
		var record semanticRecord
		record.RecordType = "symbol"
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
			return nil, err
		}
		results = append(results, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Fall back to token-overlap ranking only when the verbatim query matched
	// nothing and the query is multi-word. This leaves every single-symbol and
	// substring lookup untouched while making natural-language queries (and the
	// task strings `brief` passes here) match on their content words.
	if len(results) == 0 && !semanticQueryLooksLikePath(query) {
		tokens := semanticQueryTokens(query)
		if len(tokens) > 1 {
			return findSemanticSymbolsTokenizedSQLite(db, tokens, limit, offset)
		}
	}
	return results, nil
}

func findSemanticSymbols(snapshotPath, query string, limit, offset int) ([]semanticRecord, error) {
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("open semantic snapshot: %w", err)
	}
	defer f.Close()
	query = strings.ToLower(strings.TrimSpace(query))
	scanner := newSemanticScanner(f)
	first := true
	var results []semanticRecord
	seen := 0
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
		if strings.ToLower(record.ID) == query {
			if seen < offset {
				seen++
				continue
			}
			results = append(results, record)
			seen++
			if len(results) >= limit {
				break
			}
			continue
		}
		fields := []string{record.Name, record.QualifiedName, record.Signature, record.FilePath}
		for _, field := range fields {
			if strings.Contains(strings.ToLower(field), query) {
				if seen < offset {
					seen++
					break
				}
				results = append(results, record)
				seen++
				break
			}
		}
		if len(results) >= limit {
			break
		}
	}
	return results, scanner.Err()
}

func findSemanticRelationsForSymbolsInSQLite(storePath string, symbols []semanticRecord, limit int) ([]semanticRecord, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	ids := make([]any, 0, len(symbols)*2)
	placeholders := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		ids = append(ids, symbol.ID)
		placeholders = append(placeholders, "?")
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	args := append(append([]any{}, ids...), ids...)
	args = append(args, limit)
	inClause := strings.Join(placeholders, ",")
	rows, err := db.Query(`SELECT from_id, to_id, type, confidence, reason FROM relations
WHERE from_id IN (`+inClause+`) OR to_id IN (`+inClause+`)
ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var relations []semanticRecord
	for rows.Next() {
		var record semanticRecord
		record.RecordType = "relation"
		if err := rows.Scan(&record.FromID, &record.ToID, &record.Type, &record.Confidence, &record.Reason); err != nil {
			return nil, err
		}
		relations = append(relations, record)
	}
	return relations, rows.Err()
}

func findSemanticRelationsByTypesSQLite(storePath string, relationTypes []string) ([]semanticRecord, error) {
	if len(relationTypes) == 0 {
		return nil, nil
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	args := make([]any, 0, len(relationTypes))
	placeholders := make([]string, 0, len(relationTypes))
	for _, relationType := range relationTypes {
		args = append(args, relationType)
		placeholders = append(placeholders, "?")
	}
	rows, err := db.Query(`SELECT from_id, to_id, type, confidence, reason FROM relations WHERE type IN (`+strings.Join(placeholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var relations []semanticRecord
	for rows.Next() {
		var record semanticRecord
		record.RecordType = "relation"
		if err := rows.Scan(&record.FromID, &record.ToID, &record.Type, &record.Confidence, &record.Reason); err != nil {
			return nil, err
		}
		relations = append(relations, record)
	}
	return relations, rows.Err()
}

func traverseSemanticImpactSQLite(storePath string, roots []semanticRecord, depth, limit int) ([]semanticRecord, []semanticRecord, error) {
	if len(roots) == 0 {
		return nil, nil, nil
	}
	symbolsByID, err := loadSemanticSymbolsByIDSQLite(storePath)
	if err != nil {
		return nil, nil, err
	}
	frontier := map[string]struct{}{}
	seen := map[string]struct{}{}
	var symbols []semanticRecord
	for _, root := range roots {
		frontier[root.ID] = struct{}{}
		if _, ok := seen[root.ID]; !ok {
			seen[root.ID] = struct{}{}
			symbols = append(symbols, root)
		}
	}
	var relations []semanticRecord
	for level := 0; level < depth && len(frontier) > 0; level++ {
		current := make([]semanticRecord, 0, len(frontier))
		for id := range frontier {
			current = append(current, semanticRecord{ID: id})
		}
		edges, err := findSemanticRelationsForSymbolsInSQLite(storePath, current, limit*4)
		if err != nil {
			return nil, nil, err
		}
		next := map[string]struct{}{}
		for _, edge := range edges {
			relations = append(relations, edge)
			for _, id := range []string{edge.FromID, edge.ToID} {
				if _, ok := seen[id]; ok {
					continue
				}
				if symbol, ok := symbolsByID[id]; ok && len(symbols) < limit {
					seen[id] = struct{}{}
					next[id] = struct{}{}
					symbols = append(symbols, symbol)
					if len(symbols) >= limit {
						break
					}
				}
			}
		}
		frontier = next
	}
	return symbols, relations, nil
}

func loadSemanticSymbolsByIDSQLite(storePath string) (map[string]semanticRecord, error) {
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version FROM symbols`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	symbols := map[string]semanticRecord{}
	for rows.Next() {
		var record semanticRecord
		record.RecordType = "symbol"
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
			return nil, err
		}
		symbols[record.ID] = record
	}
	return symbols, rows.Err()
}

func traverseSemanticImpactSnapshot(snapshotPath string, roots []semanticRecord, depth, limit int) ([]semanticRecord, []semanticRecord, error) {
	if len(roots) == 0 {
		return nil, nil, nil
	}
	symbolsByID, err := loadSemanticSymbolsByIDSnapshot(snapshotPath)
	if err != nil {
		return nil, nil, err
	}
	frontier := map[string]struct{}{}
	seen := map[string]struct{}{}
	var symbols []semanticRecord
	for _, root := range roots {
		frontier[root.ID] = struct{}{}
		if _, ok := seen[root.ID]; !ok {
			seen[root.ID] = struct{}{}
			symbols = append(symbols, root)
		}
	}
	var relations []semanticRecord
	for level := 0; level < depth && len(frontier) > 0; level++ {
		current := make([]semanticRecord, 0, len(frontier))
		for id := range frontier {
			current = append(current, semanticRecord{ID: id})
		}
		edges, err := findSemanticRelationsForSymbols(snapshotPath, current, limit*4)
		if err != nil {
			return nil, nil, err
		}
		next := map[string]struct{}{}
		for _, edge := range edges {
			relations = append(relations, edge)
			for _, id := range []string{edge.FromID, edge.ToID} {
				if _, ok := seen[id]; ok {
					continue
				}
				if symbol, ok := symbolsByID[id]; ok && len(symbols) < limit {
					seen[id] = struct{}{}
					next[id] = struct{}{}
					symbols = append(symbols, symbol)
					if len(symbols) >= limit {
						break
					}
				}
			}
		}
		frontier = next
	}
	return symbols, relations, nil
}

func loadSemanticSymbolsByIDSnapshot(snapshotPath string) (map[string]semanticRecord, error) {
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	first := true
	symbols := map[string]semanticRecord{}
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
		if record.RecordType == "symbol" {
			symbols[record.ID] = record
		}
	}
	return symbols, scanner.Err()
}

func findSemanticSymbolsForFilesSQLite(storePath string, files []string, limit int) ([]semanticRecord, error) {
	if len(files) == 0 {
		return nil, nil
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	args := make([]any, 0, len(files)+1)
	placeholders := make([]string, 0, len(files))
	for _, file := range files {
		args = append(args, file)
		placeholders = append(placeholders, "?")
	}
	args = append(args, limit)
	rows, err := db.Query(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version
FROM symbols WHERE file_path IN (`+strings.Join(placeholders, ",")+`) ORDER BY file_path, start_line LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var symbols []semanticRecord
	for rows.Next() {
		var record semanticRecord
		record.RecordType = "symbol"
		if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
			return nil, err
		}
		symbols = append(symbols, record)
	}
	return symbols, rows.Err()
}

func changedSemanticFiles(ctx context.Context, runner CommandRunner, repoDir string) ([]string, error) {
	ignore, err := loadBrainIgnore(repoDir)
	if err != nil {
		return nil, err
	}
	diffOutput, _, err := runner.Run(ctx, repoDir, "git", "diff", "--name-status", "-M", "-C", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("list changed files: %w", err)
	}
	statusOutput, _, err := runner.Run(ctx, repoDir, "git", "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list worktree status: %w", err)
	}
	seen := map[string]struct{}{}
	var files []string
	add := func(path string) {
		clean, err := validateSemanticProviderPath(path)
		if err != nil {
			return
		}
		if ignore.Ignored(clean) {
			return
		}
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		files = append(files, clean)
	}
	for _, path := range changedPathsFromNameStatus(string(diffOutput)) {
		add(path)
	}
	for _, line := range strings.Split(string(statusOutput), "\n") {
		if strings.HasPrefix(line, "?? ") && len(line) > 3 {
			path := decodeGitPorcelainPath(strings.TrimSpace(line[3:]))
			if strings.HasSuffix(path, "/") {
				for _, child := range untrackedDirectoryFiles(repoDir, path, ignore) {
					add(child)
				}
				continue
			}
			add(path)
		}
	}
	sort.Strings(files)
	return files, nil
}

func changedPathsFromNameStatus(output string) []string {
	seen := map[string]struct{}{}
	var paths []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		if (strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C")) && len(fields) >= 3 {
			add(decodeGitPorcelainPath(fields[1]))
			add(decodeGitPorcelainPath(fields[2]))
			continue
		}
		add(decodeGitPorcelainPath(fields[1]))
	}
	return paths
}

func untrackedDirectoryFiles(repoDir, relDir string, ignore brainIgnore) []string {
	clean, err := validateSemanticProviderPath(strings.TrimSuffix(relDir, "/"))
	if err != nil {
		return nil
	}
	if ignore.Ignored(clean) {
		return nil
	}
	root := filepath.Join(repoDir, filepath.FromSlash(clean))
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(repoDir, path)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if d.IsDir() && ignore.Ignored(relSlash) {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if _, err := validateSemanticProviderPath(relSlash); err != nil {
			return nil
		}
		if ignore.Ignored(relSlash) {
			return nil
		}
		files = append(files, relSlash)
		return nil
	})
	return files
}

func findSemanticSymbolsForFiles(snapshotPath string, files []string, limit int) ([]semanticRecord, error) {
	if len(files) == 0 {
		return nil, nil
	}
	wanted := map[string]struct{}{}
	for _, file := range files {
		wanted[file] = struct{}{}
	}
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	first := true
	var symbols []semanticRecord
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
		if _, ok := wanted[record.FilePath]; ok {
			symbols = append(symbols, record)
			if len(symbols) >= limit {
				break
			}
		}
	}
	return symbols, scanner.Err()
}

func writeSemanticChangesReport(brainDir string, report semanticChangesReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	rel := filepath.Join(semanticDirName, "changes", "latest.json")
	if err := rejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(brainDir, rel), data, 0o600)
}

func findSemanticRelationsForSymbols(snapshotPath string, symbols []semanticRecord, limit int) ([]semanticRecord, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	ids := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		ids[symbol.ID] = struct{}{}
	}
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	first := true
	var relations []semanticRecord
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
		if record.RecordType != "relation" {
			continue
		}
		_, from := ids[record.FromID]
		_, to := ids[record.ToID]
		if from || to {
			relations = append(relations, record)
			if len(relations) >= limit {
				break
			}
		}
	}
	return relations, scanner.Err()
}

func findSemanticRelationsByTypesSnapshot(snapshotPath string, relationTypes []string) ([]semanticRecord, error) {
	if len(relationTypes) == 0 {
		return nil, nil
	}
	typeSet := lowerSet(relationTypes)
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	first := true
	var relations []semanticRecord
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
		if record.RecordType == "relation" && semanticKindInSet(record.Type, typeSet) {
			relations = append(relations, record)
		}
	}
	return relations, scanner.Err()
}

func lowerSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func semanticKindInSet(value string, set map[string]struct{}) bool {
	_, ok := set[strings.ToLower(strings.TrimSpace(value))]
	return ok
}

func sortedSemanticSymbols(symbols map[string]semanticRecord) []semanticRecord {
	result := make([]semanticRecord, 0, len(symbols))
	for _, symbol := range symbols {
		result = append(result, symbol)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].FilePath != result[j].FilePath {
			return result[i].FilePath < result[j].FilePath
		}
		if result[i].StartLine != result[j].StartLine {
			return result[i].StartLine < result[j].StartLine
		}
		if result[i].QualifiedName != result[j].QualifiedName {
			return result[i].QualifiedName < result[j].QualifiedName
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func isSemanticTestSymbol(symbol semanticRecord) bool {
	kind := strings.ToLower(symbol.Kind)
	path := strings.ToLower(symbol.FilePath)
	name := strings.ToLower(symbol.Name)
	return strings.Contains(kind, "test") ||
		strings.HasSuffix(path, "_test.go") ||
		strings.Contains(path, "/test/") ||
		strings.Contains(path, "/tests/") ||
		strings.HasPrefix(name, "test")
}

func semanticTestReason(symbol semanticRecord, related map[string]struct{}, rootDirs map[string]struct{}, rootNames []string) string {
	if _, ok := related[symbol.ID]; ok {
		return "semantic relation"
	}
	if _, ok := rootDirs[pathDirSlash(symbol.FilePath)]; ok {
		return "same directory"
	}
	lowerName := strings.ToLower(symbol.Name)
	for _, rootName := range rootNames {
		if rootName != "" && strings.Contains(lowerName, rootName) {
			return "name match"
		}
	}
	return ""
}

func pathDirSlash(path string) string {
	dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(path)))
	if dir == "." {
		return ""
	}
	return dir
}

func semanticContextContent(repoDir string, symbols []semanticRecord) []semanticContent {
	var content []semanticContent
	for _, symbol := range symbols {
		if symbol.FilePath == "" || symbol.StartLine <= 0 {
			continue
		}
		clean, err := validateSemanticProviderPath(symbol.FilePath)
		if err != nil {
			continue
		}
		if err := rejectSymlinkPathComponents(repoDir, filepath.FromSlash(clean)); err != nil {
			continue
		}
		text, truncated, err := readSemanticContextSnippet(filepath.Join(repoDir, filepath.FromSlash(clean)), symbol.StartLine, symbol.EndLine)
		if err != nil {
			continue
		}
		content = append(content, semanticContent{Path: clean, StartLine: symbol.StartLine, EndLine: symbol.EndLine, Text: text, Truncated: truncated})
	}
	return content
}

func readSemanticContextSnippet(path string, start, end int) (string, bool, error) {
	if start <= 0 {
		return "", false, errors.New("start line must be positive")
	}
	if end < start {
		end = start
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	var lines []string
	line := 0
	bytesUsed := 0
	truncated := false
	for scanner.Scan() {
		line++
		if line < start {
			continue
		}
		if line > end {
			break
		}
		text := scanner.Text()
		if len(lines) >= semanticContextMaxLines || bytesUsed+len(text) > semanticContextMaxBytes {
			truncated = true
			break
		}
		lines = append(lines, text)
		bytesUsed += len(text)
	}
	if err := scanner.Err(); err != nil {
		return "", false, err
	}
	if len(lines) == 0 {
		return "", false, errors.New("line range outside file")
	}
	return strings.Join(lines, "\n"), truncated, nil
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
	if err := ensureSemanticAuditPathSafe(storage.BrainDir); err != nil {
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
	activeSnapshot := ""
	activeGeneration := ""
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Semantic != nil {
		activeSnapshot = filepath.Clean(filepath.Join(storage.BrainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath), ".."))
		if manifest.Sources.Semantic.GenerationPath != "" {
			activeGeneration = filepath.Clean(filepath.Join(storage.BrainDir, filepath.FromSlash(manifest.Sources.Semantic.GenerationPath)))
		}
	}
	prunedSnapshots, err := pruneSemanticChildDirs(storage.BrainDir, filepath.Join(semanticDirName, semanticSnapshotsDir), activeSnapshot, cutoff)
	if err != nil {
		return err
	}
	prunedGenerations, err := pruneSemanticChildDirs(storage.BrainDir, filepath.Join(semanticDirName, semanticGenerationsDir), activeGeneration, cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "pruned semantic snapshots: %d\n", prunedSnapshots)
	fmt.Fprintf(cmd.OutOrStdout(), "pruned semantic generations: %d\n", prunedGenerations)
	return nil
}

func pruneSemanticChildDirs(brainDir, rootRel, active string, cutoff time.Time) (int, error) {
	if err := rejectExistingSymlinkPathComponents(brainDir, rootRel); err != nil {
		return 0, err
	}
	root := filepath.Join(brainDir, rootRel)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pruned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if active != "" && filepath.Clean(path) == active {
			continue
		}
		if err := rejectSymlinkPathComponents(brainDir, filepath.Join(rootRel, entry.Name())); err != nil {
			return 0, err
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.RemoveAll(path); err != nil {
				return 0, err
			}
			pruned++
		}
	}
	return pruned, nil
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
	if err := rejectBundleOutputSymlink(output); err != nil {
		return err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain refresh index`")
	}
	if manifest.Sources.Semantic.WorktreeMode == "worktree" || manifest.Sources.Semantic.DirtyWorktree || manifest.Sources.Semantic.WorktreeHash != "" {
		return errors.New("semantic bundle export does not support worktree-backed semantic indexes; refresh a clean semantic index first")
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
	generationPaths, err := collectSemanticGenerationBundlePaths(storage.BrainDir, manifest.Sources.Semantic)
	if err != nil {
		return err
	}
	if manifest.Sources.Semantic.GenerationPath != "" {
		if err := validateSemanticGenerationReferences(storage.BrainDir, manifest.Sources.Semantic); err != nil {
			return err
		}
	}
	f, tempPath, finalPath, err := createBundleOutputTemp(output, storage.BrainDir)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tempPath)
		}
	}()
	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)
	tw := tar.NewWriter(mw)
	if err := addBundleManifest(tw, exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		RepoKey:       storage.Key,
		Sources:       &brainSources{Semantic: sanitizeSemanticSourceForExport(manifest.Sources.Semantic, repoDir)},
	}); err != nil {
		_ = f.Close()
		return err
	}
	if err := addSanitizedSnapshotBundlePath(tw, storage.BrainDir, activeSnapshot, repoDir); err != nil {
		_ = f.Close()
		return err
	}
	for _, rel := range generationPaths {
		if filepath.ToSlash(rel) == manifest.Sources.Semantic.StorePath {
			generationID := pathpkg.Base(filepath.ToSlash(manifest.Sources.Semantic.GenerationPath))
			if err := addSanitizedSemanticStoreBundlePath(tw, rel, activeSnapshotPath, repoDir, generationID, opts.Now()); err != nil {
				_ = f.Close()
				return err
			}
			continue
		}
		if err := addBundlePath(tw, storage.BrainDir, rel); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return err
	}
	renamed = true
	checksum := hex.EncodeToString(hasher.Sum(nil))
	if err := appendSemanticAudit(storage.BrainDir, "bundle_export", output, checksum); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "exported bundle: %s\nsha256: %s\n", output, checksum)
	return nil
}

func collectSemanticGenerationBundlePaths(brainDir string, source *semanticSourceManifest) ([]string, error) {
	if source == nil || source.GenerationPath == "" {
		return nil, nil
	}
	generationPath, err := validateSemanticGenerationPath(source.GenerationPath)
	if err != nil {
		return nil, err
	}
	if err := rejectSymlinkPathComponents(brainDir, generationPath); err != nil {
		return nil, err
	}
	generationRoot := filepath.Join(brainDir, generationPath)
	info, err := os.Lstat(generationRoot)
	if err != nil {
		return nil, fmt.Errorf("semantic generation missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("semantic generation must not be a symlink: %s", source.GenerationPath)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("semantic generation must be a directory: %s", source.GenerationPath)
	}
	var paths []string
	err = filepath.WalkDir(generationRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(brainDir, path)
		if err != nil {
			return err
		}
		if err := rejectSymlinkPathComponents(brainDir, rel); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func validateSemanticGenerationReferences(brainDir string, source *semanticSourceManifest) error {
	generationPath, err := validateSemanticGenerationPath(source.GenerationPath)
	if err != nil {
		return err
	}
	for _, ref := range []struct {
		name     string
		path     string
		basename string
	}{
		{name: "semantic sqlite store", path: source.StorePath, basename: semanticSQLiteName},
		{name: "semantic metrics", path: source.MetricsPath, basename: semanticMetricsName},
	} {
		if ref.path == "" {
			continue
		}
		clean, err := validateSemanticGenerationFilePath(ref.path, generationPath, ref.basename)
		if err != nil {
			return err
		}
		if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
			return err
		}
		if info, err := os.Lstat(filepath.Join(brainDir, clean)); err != nil {
			return fmt.Errorf("%s missing: %w", ref.name, err)
		} else if info.IsDir() {
			return fmt.Errorf("%s must be a file: %s", ref.name, ref.path)
		} else if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must not be a symlink: %s", ref.name, ref.path)
		}
	}
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

func addSanitizedSnapshotBundlePath(tw *tar.Writer, root, rel, repoDir string) error {
	if err := rejectSymlinkPathComponents(root, rel); err != nil {
		return err
	}
	path := filepath.Join(root, rel)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundle export refuses symlink: %s", rel)
	}
	if info.IsDir() {
		return fmt.Errorf("active semantic snapshot must be a file: %s", rel)
	}
	data, err := sanitizedSemanticSnapshotForBundle(path, repoDir)
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

func addSanitizedSemanticStoreBundlePath(tw *tar.Writer, rel, snapshotPath, repoDir, generationID string, now time.Time) error {
	raw, err := sanitizedSemanticSnapshotForBundle(snapshotPath, repoDir)
	if err != nil {
		return err
	}
	header, counts, raw, err := filterSemanticSnapshot(raw, brainIgnore{}, repoDir)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "entire-brain-bundle-store-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, semanticSQLiteName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		return err
	}
	_, err = populateSemanticSQLite(db, repoDir, generationID, semanticSnapshotSHA256(raw), raw, header, counts, now, tmpDir)
	if closeErr := db.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return err
	}
	headerTar := &tar.Header{Name: filepath.ToSlash(rel), Mode: 0o600, Size: int64(len(data)), ModTime: now}
	if err := tw.WriteHeader(headerTar); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

func sanitizedSemanticSnapshotForBundle(path, repoDir string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("semantic snapshot missing header")
	}
	var header semanticHeader
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return nil, fmt.Errorf("parse semantic snapshot header: %w", err)
	}
	header.RepoRoot = ""
	header.Warnings = sanitizeSemanticWarnings(header.Warnings, repoDir)
	header.PartialFailures = sanitizeSemanticWarnings(header.PartialFailures, repoDir)
	headerLine, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Write(headerLine)
	out.WriteByte('\n')
	idMap := make(map[string]string)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		sanitizedLine, err := sanitizeSemanticSnapshotRecordForBundle(line, repoDir, idMap)
		if err != nil {
			return nil, err
		}
		out.Write(sanitizedLine)
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func sanitizeSemanticSnapshotRecordForBundle(line []byte, repoDir string, idMap map[string]string) ([]byte, error) {
	var record map[string]any
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, fmt.Errorf("parse semantic snapshot record: %w", err)
	}
	for field, raw := range record {
		value, ok := raw.(string)
		if !ok {
			continue
		}
		switch field {
		case "id", "from_id", "to_id":
			record[field] = sanitizeSemanticRecordID(value, repoDir, idMap)
		default:
			record[field] = sanitizeSemanticWarningText(value, repoDir)
		}
	}
	return json.Marshal(record)
}

func sanitizeSemanticRecordID(value, repoDir string, idMap map[string]string) string {
	if value == "" {
		return ""
	}
	if sanitized, ok := idMap[value]; ok {
		return sanitized
	}
	sanitized := sanitizeSemanticWarningText(value, repoDir)
	if sanitized == value {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	sanitized = "redacted:" + hex.EncodeToString(sum[:8])
	idMap[value] = sanitized
	return sanitized
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

func rejectSymlinkedBrainRoot(brainDir string) error {
	info, err := os.Lstat(brainDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("brain directory must not be a symlink: %s", brainDir)
	}
	return nil
}

func rejectBrainRootPathSymlinks(brainRoot, rel string) error {
	if err := rejectSymlinkedBrainRoot(brainRoot); err != nil {
		return err
	}
	return rejectExistingSymlinkPathComponents(brainRoot, rel)
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

func createBundleOutputTemp(output, brainDir string) (*os.File, string, string, error) {
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return nil, "", "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return nil, "", "", err
	}
	finalPath := filepath.Join(dir, filepath.Base(output))
	if err := rejectBundleOutputInsideBrain(finalPath, brainDir); err != nil {
		return nil, "", "", err
	}
	if err := rejectBundleOutputSymlink(finalPath); err != nil {
		return nil, "", "", err
	}
	if err := rejectBundleOutputHardLink(finalPath, brainDir); err != nil {
		return nil, "", "", err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(output)+".tmp-*")
	if err != nil {
		return nil, "", "", err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, "", "", err
	}
	return f, f.Name(), finalPath, nil
}

func rejectBundleOutputSymlink(output string) error {
	info, err := os.Lstat(output)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundle output must not be a symlink: %s", output)
	}
	return nil
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
	if err := ensureSemanticAuditPathSafe(storage.BrainDir); err != nil {
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
	if err := publishImportedSemanticSnapshot(storage.BrainDir, tmpDir, importManifest.Sources.Semantic); err != nil {
		return err
	}
	if importManifest.Sources.Semantic.GenerationPath != "" {
		if err := replaceImportedSemanticGeneration(storage.BrainDir, tmpDir, repoDir, importManifest.Sources.Semantic, opts.Now()); err != nil {
			return err
		}
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

func publishImportedSemanticSnapshot(brainDir, bundleRoot string, source *semanticSourceManifest) error {
	if source == nil {
		return nil
	}
	snapshotRel, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return err
	}
	sourcePath := filepath.Join(bundleRoot, snapshotRel)
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read bundle semantic snapshot: %w", err)
	}
	targetRel := snapshotRel
	targetPath := filepath.Join(brainDir, targetRel)
	if info, err := os.Lstat(targetPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("semantic snapshot must not be a symlink: %s", source.SnapshotPath)
		}
		if info.IsDir() {
			return fmt.Errorf("semantic snapshot must be a file: %s", source.SnapshotPath)
		}
		existing, err := os.ReadFile(targetPath)
		if err != nil {
			return err
		}
		if bytes.Equal(existing, content) {
			return nil
		}
		targetRel = uniqueSemanticSnapshotRel(brainDir, snapshotRel, content)
		source.SnapshotPath = filepath.ToSlash(targetRel)
		targetPath = filepath.Join(brainDir, targetRel)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, targetRel); err != nil {
		return err
	}
	return writeFileAtomic(targetPath, content, 0o600)
}

func uniqueSemanticSnapshotRel(brainDir, snapshotRel string, content []byte) string {
	sum := sha256.Sum256(content)
	dir := pathpkg.Dir(filepath.ToSlash(snapshotRel))
	base := pathpkg.Base(dir)
	parent := pathpkg.Dir(dir)
	prefix := base + "-import-" + hex.EncodeToString(sum[:6])
	for i := 0; ; i++ {
		candidate := prefix
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", prefix, i+1)
		}
		rel := filepath.ToSlash(filepath.Join(parent, candidate, semanticSnapshotName))
		if _, err := os.Lstat(filepath.Join(brainDir, filepath.FromSlash(rel))); os.IsNotExist(err) {
			return rel
		}
	}
}

func replaceImportedSemanticGeneration(brainDir, bundleRoot, repoDir string, source *semanticSourceManifest, now time.Time) error {
	generationPath, err := validateSemanticGenerationPath(source.GenerationPath)
	if err != nil {
		return err
	}
	sourceRoot := filepath.Join(bundleRoot, generationPath)
	info, err := os.Stat(sourceRoot)
	if err != nil {
		return fmt.Errorf("bundle missing semantic generation: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("bundle semantic generation must be a directory: %s", source.GenerationPath)
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, generationPath); err != nil {
		return err
	}
	targetPath := filepath.Join(brainDir, generationPath)
	parent := filepath.Dir(targetPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	targetGenerationPath := generationPath
	if _, err := os.Lstat(targetPath); err == nil {
		targetGenerationPath = uniqueImportedSemanticGenerationPath(brainDir, generationPath, source.SnapshotPath)
		rewriteSemanticGenerationSourcePaths(source, generationPath, targetGenerationPath)
		parent = filepath.Dir(filepath.Join(brainDir, targetGenerationPath))
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	targetPath = filepath.Join(brainDir, targetGenerationPath)
	staging, err := os.MkdirTemp(parent, ".import-"+filepath.Base(targetGenerationPath)+"-*")
	if err != nil {
		return err
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := copyImportedGenerationDir(sourceRoot, staging); err != nil {
		return err
	}
	if err := removeImportedSemanticParseCache(staging, targetGenerationPath, source.ParseCachePath); err != nil {
		return err
	}
	if source.StorePath != "" {
		if err := rebuildImportedSemanticStoreInGeneration(brainDir, repoDir, source, staging, now); err != nil {
			return err
		}
		storePath, err := semanticGenerationStagingFilePath(staging, source.GenerationPath, source.StorePath, semanticSQLiteName)
		if err != nil {
			return err
		}
		if err := validateSemanticSQLiteStore(storePath, source.Files, source.Symbols, source.Relations); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, targetPath); err != nil {
		return err
	}
	if err := syncParentDir(targetPath); err != nil {
		return err
	}
	cleanupStaging = false
	return nil
}

func uniqueImportedSemanticGenerationPath(brainDir, generationPath, salt string) string {
	sum := sha256.Sum256([]byte(generationPath + "\x00" + salt))
	parent := pathpkg.Dir(filepath.ToSlash(generationPath))
	base := pathpkg.Base(filepath.ToSlash(generationPath))
	prefix := base + "-import-" + hex.EncodeToString(sum[:6])
	for i := 0; ; i++ {
		candidate := prefix
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", prefix, i+1)
		}
		rel := filepath.ToSlash(filepath.Join(parent, candidate))
		if _, err := os.Lstat(filepath.Join(brainDir, filepath.FromSlash(rel))); os.IsNotExist(err) {
			return rel
		}
	}
}

func rewriteSemanticGenerationSourcePaths(source *semanticSourceManifest, oldGenerationPath, newGenerationPath string) {
	replace := func(value string) string {
		if value == "" {
			return ""
		}
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
		oldSlash := filepath.ToSlash(oldGenerationPath)
		if clean == oldSlash {
			return filepath.ToSlash(newGenerationPath)
		}
		if strings.HasPrefix(clean, oldSlash+"/") {
			return filepath.ToSlash(filepath.Join(newGenerationPath, strings.TrimPrefix(clean, oldSlash+"/")))
		}
		return value
	}
	source.GenerationPath = filepath.ToSlash(newGenerationPath)
	source.StorePath = replace(source.StorePath)
	source.MetricsPath = replace(source.MetricsPath)
	source.ParseCachePath = replace(source.ParseCachePath)
}

func removeImportedSemanticParseCache(generationRoot, generationPath, parseCachePath string) error {
	expected := filepath.ToSlash(filepath.Join(generationPath, "parse-cache"))
	if strings.TrimSpace(parseCachePath) != "" && filepath.ToSlash(parseCachePath) != expected {
		return fmt.Errorf("semantic parse_cache_path must be %s: %s", expected, parseCachePath)
	}
	target := filepath.Join(generationRoot, "parse-cache")
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bundle semantic parse-cache must not be a symlink: %s", parseCachePath)
	}
	return os.RemoveAll(target)
}

func rebuildImportedSemanticStoreInGeneration(brainDir, repoDir string, source *semanticSourceManifest, generationRoot string, now time.Time) error {
	if source == nil || source.StorePath == "" {
		return nil
	}
	snapshotRel, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return err
	}
	generationRel, err := validateSemanticGenerationPath(source.GenerationPath)
	if err != nil {
		return err
	}
	storeRel, err := validateSemanticGenerationFilePath(source.StorePath, generationRel, semanticSQLiteName)
	if err != nil {
		return err
	}
	if source.MetricsPath == "" {
		source.MetricsPath = filepath.ToSlash(filepath.Join(generationRel, semanticMetricsName))
	}
	metricsRel, err := validateSemanticGenerationFilePath(source.MetricsPath, generationRel, semanticMetricsName)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(brainDir, snapshotRel))
	if err != nil {
		return err
	}
	header, counts, raw, err := filterSemanticSnapshot(raw, brainIgnore{}, repoDir)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "entire-brain-import-store-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, semanticSQLiteName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		return err
	}
	snapshotSHA := semanticSnapshotSHA256(raw)
	metrics, err := populateSemanticSQLite(db, repoDir, pathpkg.Base(filepath.ToSlash(generationRel)), snapshotSHA, raw, header, counts, now, generationRoot)
	if closeErr := db.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		return err
	}
	metrics.StoreBytes = int64(len(data))
	metricsData, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return err
	}
	metricsData = append(metricsData, '\n')
	storeSubpath, err := filepath.Rel(filepath.Join(brainDir, generationRel), filepath.Join(brainDir, storeRel))
	if err != nil {
		return err
	}
	storePath := filepath.Join(generationRoot, storeSubpath)
	if err := writeFileAtomic(storePath, data, 0o600); err != nil {
		return err
	}
	metricsSubpath, err := filepath.Rel(filepath.Join(brainDir, generationRel), filepath.Join(brainDir, metricsRel))
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(generationRoot, metricsSubpath), metricsData, 0o600)
}

func semanticGenerationStagingFilePath(generationRoot, generationPath, filePath, basename string) (string, error) {
	generationRel, err := validateSemanticGenerationPath(generationPath)
	if err != nil {
		return "", err
	}
	fileRel, err := validateSemanticGenerationFilePath(filePath, generationRel, basename)
	if err != nil {
		return "", err
	}
	generationSlash := filepath.ToSlash(generationRel)
	fileSlash := filepath.ToSlash(fileRel)
	if fileSlash == generationSlash {
		return generationRoot, nil
	}
	return filepath.Join(generationRoot, filepath.FromSlash(strings.TrimPrefix(fileSlash, generationSlash+"/"))), nil
}

func copyImportedGenerationDir(sourceRoot, targetRoot string) error {
	return filepath.WalkDir(sourceRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceRoot {
			return nil
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(targetRoot, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle semantic generation entry must not be a symlink: %s", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return writeFileAtomic(target, data, 0o600)
	})
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
	if !semanticRepoKeyEqual(manifest.RepoKey, repoKey) {
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
	if manifest.Sources.Semantic.GenerationPath == "" &&
		(manifest.Sources.Semantic.StorePath != "" || manifest.Sources.Semantic.MetricsPath != "" || manifest.Sources.Semantic.ParseCachePath != "") {
		return nil, errors.New("bundle semantic generation_path is required when generated store paths are present")
	}
	if manifest.Sources.Semantic.GenerationPath != "" {
		generationPath, err := validateSemanticGenerationPath(manifest.Sources.Semantic.GenerationPath)
		if err != nil {
			return nil, err
		}
		if manifest.Sources.Semantic.StorePath != "" {
			storePath, err := validateSemanticGenerationFilePath(manifest.Sources.Semantic.StorePath, generationPath, semanticSQLiteName)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(filepath.Join(root, storePath)); err != nil {
				return nil, fmt.Errorf("bundle missing semantic sqlite store: %w", err)
			}
		}
		if err := validateImportedGenerationEntries(root, manifest.Sources.Semantic, generationPath); err != nil {
			return nil, err
		}
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
	if manifest.Sources.Semantic.StorePath != "" {
		storeFullPath := filepath.Join(root, filepath.FromSlash(manifest.Sources.Semantic.StorePath))
		if err := validateSemanticSQLiteStore(storeFullPath, manifest.Sources.Semantic.Files, manifest.Sources.Semantic.Symbols, manifest.Sources.Semantic.Relations); err != nil {
			return nil, err
		}
	}
	if err := validateBundleSemanticSourceMatchesSnapshot(manifest.Sources.Semantic, snapshotHeader, snapshotCounts); err != nil {
		return nil, fmt.Errorf("bundle manifest does not match semantic snapshot: %w", err)
	}
	return &manifest, nil
}

func validateImportedGenerationEntries(root string, source *semanticSourceManifest, generationPath string) error {
	generationRoot := filepath.Join(root, generationPath)
	allowedFiles := map[string]struct{}{}
	for _, rel := range []string{source.StorePath, source.MetricsPath} {
		if rel == "" {
			continue
		}
		clean, err := validateSemanticGenerationFilePath(rel, generationPath, "")
		if err != nil {
			return err
		}
		allowedFiles[filepath.ToSlash(clean)] = struct{}{}
	}
	parseCachePath := ""
	if source.ParseCachePath != "" {
		expectedParseCache := filepath.ToSlash(filepath.Join(generationPath, "parse-cache"))
		if source.ParseCachePath != expectedParseCache {
			return fmt.Errorf("semantic parse_cache_path must be %s: %s", expectedParseCache, source.ParseCachePath)
		}
		clean, err := validateSemanticGenerationFilePath(source.ParseCachePath+"/.placeholder", generationPath, "")
		if err != nil {
			return err
		}
		parseCachePath = strings.TrimSuffix(filepath.ToSlash(clean), "/.placeholder")
	}
	return filepath.WalkDir(generationRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if _, ok := allowedFiles[relSlash]; ok {
			return nil
		}
		if parseCachePath != "" && strings.HasPrefix(relSlash, parseCachePath+"/") {
			return nil
		}
		return fmt.Errorf("bundle contains unreferenced semantic generation entry: %s", relSlash)
	})
}

func validateSemanticSQLiteStore(path string, expectedFiles, expectedSymbols, expectedRelations int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("semantic sqlite store missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("semantic sqlite store must not be a symlink: %s", path)
	}
	if info.IsDir() {
		return fmt.Errorf("semantic sqlite store must be a file: %s", path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open semantic sqlite store: %w", err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("validate semantic sqlite integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("semantic sqlite integrity check failed: %s", integrity)
	}
	files, err := semanticSQLiteTableCount(db, "files")
	if err != nil {
		return err
	}
	symbols, err := semanticSQLiteTableCount(db, "symbols")
	if err != nil {
		return err
	}
	relations, err := semanticSQLiteTableCount(db, "relations")
	if err != nil {
		return err
	}
	if expectedFiles > 0 && files != expectedFiles {
		return fmt.Errorf("semantic sqlite file count %d does not match manifest %d", files, expectedFiles)
	}
	if symbols != expectedSymbols {
		return fmt.Errorf("semantic sqlite symbol count %d does not match manifest %d", symbols, expectedSymbols)
	}
	if relations != expectedRelations {
		return fmt.Errorf("semantic sqlite relation count %d does not match manifest %d", relations, expectedRelations)
	}
	return nil
}

func semanticSQLiteTableCount(db *sql.DB, table string) (int, error) {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		return 0, fmt.Errorf("validate semantic sqlite table %s: %w", table, err)
	}
	return count, nil
}

func semanticSQLiteMetaMap(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("read semantic sqlite meta: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("scan semantic sqlite meta: %w", err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan semantic sqlite meta: %w", err)
	}
	return out, nil
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

func validateSemanticGenerationPath(generationPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(generationPath))
	cleanSlash := filepath.ToSlash(clean)
	if cleanSlash != generationPath {
		return "", fmt.Errorf("semantic generation_path must be canonical: %s", generationPath)
	}
	if strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) || !strings.HasPrefix(cleanSlash, filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir))+"/") {
		return "", fmt.Errorf("semantic generation_path is unsafe: %s", generationPath)
	}
	return clean, nil
}

func validateSemanticGenerationFilePath(path, generationPath, basename string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(path))
	cleanSlash := filepath.ToSlash(clean)
	if cleanSlash != path {
		return "", fmt.Errorf("semantic generation file path must be canonical: %s", path)
	}
	if !strings.HasPrefix(cleanSlash, filepath.ToSlash(generationPath)+"/") {
		return "", fmt.Errorf("semantic generation file path %s is outside generation %s", path, generationPath)
	}
	if basename != "" && filepath.Base(clean) != basename {
		return "", fmt.Errorf("semantic generation file path must end with %s: %s", basename, path)
	}
	return clean, nil
}

func semanticBundleGenerationRelAllowed(source *semanticSourceManifest, rel string) bool {
	if source == nil || source.GenerationPath == "" {
		return false
	}
	generationPath, err := validateSemanticGenerationPath(source.GenerationPath)
	if err != nil {
		return false
	}
	return strings.HasPrefix(rel, filepath.ToSlash(generationPath)+"/")
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
	if !semanticRepoKeyEqual(header.RepoKey, repoKey) {
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
		if err := validateSemanticRecordPath(&record); err != nil {
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

func semanticRepoKeyEqual(a, b string) bool {
	return a == b || strings.EqualFold(a, b)
}

func validateSemanticSourceMatchesSnapshot(source *semanticSourceManifest, header semanticHeader, counts semanticCounts) error {
	if source == nil {
		return errors.New("semantic source is missing")
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

func validateBundleSemanticSourceMatchesSnapshot(source *semanticSourceManifest, header semanticHeader, counts semanticCounts) error {
	if source == nil {
		return errors.New("semantic source is missing")
	}
	if source.WorktreeMode == "worktree" || source.DirtyWorktree || source.WorktreeHash != "" {
		return errors.New("bundle semantic source contains unsupported worktree overlay metadata")
	}
	return validateSemanticSourceMatchesSnapshot(source, header, counts)
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
	if clean != exportManifestFileName &&
		!strings.HasPrefix(cleanSlash, filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir))+"/") &&
		!strings.HasPrefix(cleanSlash, filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir))+"/") {
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
	return withBrainWriteLock(brainDir, func() error {
		if err := ensureSemanticAuditPathSafe(brainDir); err != nil {
			return err
		}
		existing, err := os.ReadFile(auditPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		existing = append(existing, data...)
		existing = append(existing, '\n')
		return writeFileAtomic(auditPath, existing, 0o600)
	})
}

func ensureSemanticAuditPathSafe(brainDir string) error {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	auditPath := filepath.Join(brainDir, semanticDirName, semanticAuditLogName)
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, filepath.Join(semanticDirName, semanticAuditLogName)); err != nil {
		return err
	}
	return nil
}
