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
	// semanticSnapshotTimeout is the default overall deadline for a provider
	// snapshot. It is intentionally generous (large repositories can take many
	// minutes) and is paired with an inactivity timeout so a hung provider still
	// aborts promptly. Both are configurable via flags.
	semanticSnapshotTimeout           = 30 * time.Minute
	semanticSnapshotInactivityTimeout = 5 * time.Minute
	semanticIndexLockTimeout          = 10 * time.Second
)

var (
	semanticBundleMaxEntry int64 = 128 * 1024 * 1024
	semanticBundleMaxTotal int64 = 512 * 1024 * 1024
	semanticBundleMaxFiles       = 10000
	// semanticMaxRecordBytes bounds a single NDJSON record from the (untrusted)
	// entire-graph stream. A per-symbol record is realistically kilobytes; the cap is
	// kept generous but far below the previous 64 MiB so one crafted line cannot
	// force a huge buffer allocation. Operators with unusually large records can
	// raise it via ENTIRE_BRAIN_MAX_RECORD_BYTES.
	semanticMaxRecordBytes = 16 * 1024 * 1024
)

func semanticRecordMaxBytes() int {
	if v := os.Getenv("ENTIRE_BRAIN_MAX_RECORD_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return semanticMaxRecordBytes
}

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
	Externals        int               `json:"externals,omitempty"`
	Warnings         []semanticWarning `json:"warnings,omitempty"`
	PartialFailures  []semanticWarning `json:"partial_failures,omitempty"`
	Capabilities     []string          `json:"capabilities,omitempty"`
	NoEgressVerified bool              `json:"no_egress_verified"`
	WorktreeMode     string            `json:"worktree_mode,omitempty"`
	WorktreeHash     string            `json:"worktree_hash,omitempty"`

	// Aggregate metadata sourced from the provider's authoritative summary
	// record. SummaryPresent records whether a summary was received, so a
	// downgraded (lean header, no summary) provider is distinguishable from one
	// that reported no warnings.
	Languages []string `json:"languages,omitempty"`
	// LanguageTiers maps each language present in the repo to "semantic" or
	// "inventory-only" (from the provider), so retrieval can be scoped per
	// language, not just by the repo-wide completeness/trust fields below.
	LanguageTiers           map[string]string `json:"language_tiers,omitempty"`
	Profile                 string            `json:"profile,omitempty"`
	RelationSet             []string          `json:"relation_set,omitempty"`
	SkippedRelationFamilies []string          `json:"skipped_relation_families,omitempty"`
	Completeness            json.RawMessage   `json:"completeness,omitempty"`
	ProfileLimits           json.RawMessage   `json:"profile_limits,omitempty"`
	Stats                   json.RawMessage   `json:"stats,omitempty"`
	SummaryPresent          bool              `json:"summary_present"`

	// Retrieval-trust diagnostics derived from the provider's completeness so an
	// agent reading this index knows how much to trust its semantic facts.
	// CompletenessLevel is the provider's level (ok/degraded/unsafe); Trust is a
	// coarse label (trusted/partial/low/unknown). See trustForCompleteness.
	CompletenessLevel string `json:"completeness_level,omitempty"`
	Trust             string `json:"trust,omitempty"`
}

// completenessLevelFromStats extracts completeness_level from the provider's
// trailing summary stats blob (json.RawMessage), or "" if absent.
func completenessLevelFromStats(stats json.RawMessage) string {
	if len(stats) == 0 {
		return ""
	}
	var s struct {
		CompletenessLevel string `json:"completeness_level"`
	}
	if err := json.Unmarshal(stats, &s); err != nil {
		return ""
	}
	return s.CompletenessLevel
}

// trustForCompleteness maps a provider completeness level to a coarse
// retrieval-trust label agents can branch on.
func trustForCompleteness(level string) string {
	switch level {
	case "ok":
		return "trusted"
	case "degraded":
		return "partial"
	case "unsafe":
		return "low"
	default:
		return "unknown"
	}
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
	LanguageTiers   map[string]string `json:"language_tiers,omitempty"`
	Capabilities    []string          `json:"capabilities"`
	Warnings        []semanticWarning `json:"warnings"`
	PartialFailures []semanticWarning `json:"partial_failures"`

	// Aggregate metadata carried by the authoritative trailing summary record
	// (see semanticSummary / mergeSemanticSummary). These are omitempty so a
	// lean streaming header round-trips unchanged and older provider headers
	// without a summary are unaffected.
	Profile                 string          `json:"profile,omitempty"`
	RelationSet             []string        `json:"relation_set,omitempty"`
	SkippedRelationFamilies []string        `json:"skipped_relation_families,omitempty"`
	Completeness            json.RawMessage `json:"completeness,omitempty"`
	ProfileLimits           json.RawMessage `json:"profile_limits,omitempty"`
	Stats                   json.RawMessage `json:"stats,omitempty"`
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

	// Schema 1.1 fields. These are omitempty so older (1.0) snapshots round-trip
	// unchanged, but they must be modeled so the streaming filter does not
	// silently drop them when re-marshaling file/symbol/relation records.
	Bytes         int                `json:"bytes,omitempty"`          // file record: source size
	ContainerID   string             `json:"container_id,omitempty"`   // symbol record: enclosing symbol
	BodyHash      string             `json:"body_hash,omitempty"`      // symbol record: content hash
	RelationScope string             `json:"relation_scope,omitempty"` // relation record: file|external|...
	Resolution    string             `json:"resolution,omitempty"`     // relation record: exact|type_inferred|name_only
	TargetKind    string             `json:"target_kind,omitempty"`    // relation record: symbol|external
	Evidence      []semanticEvidence `json:"evidence,omitempty"`       // relation record: supporting spans
}

// semanticEvidence is a single supporting span for a relation (schema 1.1).
type semanticEvidence struct {
	Kind      string `json:"kind,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

type semanticIndexOptions struct {
	force          bool
	graphBinary    string
	skipGraph      bool
	worktree       bool
	outputDir      string
	outputExplicit bool
	// timeout is the overall deadline for the provider snapshot. Zero selects
	// semanticSnapshotTimeout. inactivityTimeout aborts the snapshot when no
	// records arrive for the given duration; zero selects
	// semanticSnapshotInactivityTimeout.
	timeout           time.Duration
	inactivityTimeout time.Duration
	// profile selects the provider snapshot profile (e.g. full|syntax-only). It
	// is only passed to the provider when non-empty so the provider default
	// applies otherwise.
	profile string
	// progress, when set, is called at each phase boundary of the index
	// (verifying provider, snapshotting, building store, …) so long-running
	// indexing reports something more useful than a static spinner.
	progress func(phase string)
	// containRoot, when set, requires the resolved repository directory to stay
	// inside it. The MCP server sets this to the bound repo root so an untrusted
	// client cannot index a directory outside it — checked against the *resolved*
	// git toplevel, not just the requested path, so a path that resolves upward
	// still cannot escape.
	containRoot string
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
	indexOpts := semanticIndexOptions{graphBinary: "entire"}
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
	cmd.Flags().StringVar(&indexOpts.graphBinary, "graph-binary", "entire", "Entire CLI binary that exposes `graph` provider commands")
	cmd.Flags().BoolVar(&indexOpts.skipGraph, "skip-graph", false, "Record semantic metadata without invoking the semantic provider")
	cmd.Flags().BoolVar(&indexOpts.worktree, "worktree", false, "Index the dirty worktree instead of committed HEAD")
	cmd.Flags().DurationVar(&indexOpts.timeout, "graph-timeout", 0, "Overall deadline for the semantic provider snapshot (0 uses the default)")
	cmd.Flags().DurationVar(&indexOpts.inactivityTimeout, "graph-inactivity-timeout", 0, "Abort the snapshot if the provider emits no records for this long (0 uses the default)")
	cmd.Flags().StringVar(&indexOpts.profile, "profile", "", "Semantic provider snapshot profile (e.g. full, syntax-only); empty uses the provider default")
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
	if err := enforceIndexContainment(indexOpts.containRoot, repoDir); err != nil {
		return err
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

	var header semanticHeader
	var summary *semanticSummary
	counts := semanticCounts{}
	var streamCounts semanticStreamCounts
	noEgress := false
	var warnings []semanticWarning
	for _, warning := range defaultWarnings {
		warnings = append(warnings, semanticWarning{Code: "default_branch_unknown", Severity: "warning", Effect: "freshness", Detail: warning})
	}

	// Stream the filtered snapshot to a temp file rather than buffering it. The
	// final snapshot path depends on the SHA-256 of the filtered output, so we
	// hash while writing and rename into place once the digest is known.
	snapshotsRoot := filepath.Join(storage.BrainDir, semanticDirName, semanticSnapshotsDir)
	if err := rejectExistingSymlinkPathComponents(storage.BrainDir, filepath.Join(semanticDirName, semanticSnapshotsDir)); err != nil {
		return err
	}
	if err := os.MkdirAll(snapshotsRoot, 0o700); err != nil {
		return fmt.Errorf("create semantic snapshots dir: %w", err)
	}
	tmp, err := os.CreateTemp(snapshotsRoot, ".tmp-snapshot-*.ndjson")
	if err != nil {
		return fmt.Errorf("create semantic snapshot temp file: %w", err)
	}
	tmpPath := tmp.Name()
	tmpClosed := false
	finalized := false
	defer func() {
		if !tmpClosed {
			_ = tmp.Close()
		}
		if !finalized {
			_ = os.Remove(tmpPath)
		}
	}()
	hasher := sha256.New()
	out := io.MultiWriter(tmp, hasher)

	if indexOpts.skipGraph {
		header = semanticHeader{
			SchemaVersion: "1.0",
			Provider:      "skipped",
			RepoKey:       storage.Key,
			Commit:        head,
			Tree:          tree,
			Warnings:      []semanticWarning{{Code: "provider_skipped", Severity: "warning", Effect: "semantic facts unavailable", Detail: "semantic provider was skipped"}},
		}
		headerLine, err := json.Marshal(header)
		if err != nil {
			return err
		}
		if err := writeNDJSONLine(out, headerLine); err != nil {
			return err
		}
		warnings = append(warnings, header.Warnings...)
	} else {
		if strings.TrimSpace(indexOpts.graphBinary) == "" {
			return errors.New("--graph-binary must not be empty")
		}
		var doctorWarnings []semanticWarning
		indexOpts.reportPhase("verifying provider")
		noEgress, doctorWarnings = runSemanticDoctor(ctx, opts.Runner, repoDir, indexOpts.graphBinary)
		warnings = append(warnings, doctorWarnings...)
		if !noEgress {
			code := "provider_no_egress_unverified"
			if len(doctorWarnings) > 0 && doctorWarnings[0].Code != "" {
				code = doctorWarnings[0].Code
			}
			return fmt.Errorf("%s: semantic provider no-egress status is not verified", code)
		}
		indexOpts.reportPhase("parsing sources")
		res, serr := streamSemanticSnapshot(ctx, opts.Runner, repoDir, indexOpts, providerIgnoreFiles, ignore, out)
		if serr != nil && len(providerIgnoreFiles) > 0 && semanticSnapshotRejectsIgnoreFile(serr) {
			warnings = append(warnings, semanticWarning{
				Code:     "provider_ignore_file_unsupported",
				Severity: "warning",
				Effect:   "semantic provider retried without .brainignore; local filtering still applies",
				Detail:   serr.Error(),
			})
			if err := resetSnapshotTempFile(tmp, hasher); err != nil {
				return err
			}
			res, serr = streamSemanticSnapshot(ctx, opts.Runner, repoDir, indexOpts, nil, ignore, out)
		}
		if serr != nil {
			return serr
		}
		header = res.header
		summary = res.summary
		counts = res.counts
		streamCounts = res.stream
		warnings = append(warnings, res.extraWarnings...)
		mergeSemanticSummary(&header, summary)
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
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flush semantic snapshot temp file: %w", err)
	}
	tmpClosed = true
	if header.Commit == "" {
		header.Commit = head
	}
	if header.Tree == "" {
		header.Tree = tree
	}
	if header.Provider == "" {
		header.Provider = "entire-graph"
	}
	if header.RepoKey == "" {
		header.RepoKey = storage.Key
	}

	sum := hasher.Sum(nil)
	contentSuffix := hex.EncodeToString(sum[:8])
	snapshotID := header.Commit
	worktreeMode := "head"
	worktreeHash := ""
	if indexOpts.worktree && dirty {
		snapshotID = "worktree-" + contentSuffix
		worktreeMode = "worktree"
		worktreeHash = worktreeHashBefore
	} else if snapshotID == "" {
		snapshotID = "unknown"
	} else {
		snapshotID = snapshotID + "-" + contentSuffix
	}
	snapshotRel := filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir, snapshotID, semanticSnapshotName))
	snapshotPath := filepath.Join(storage.BrainDir, filepath.FromSlash(snapshotRel))
	if err := rejectExistingSymlinkPathComponents(storage.BrainDir, filepath.FromSlash(snapshotRel)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return fmt.Errorf("create semantic snapshot dir: %w", err)
	}
	if err := os.Rename(tmpPath, snapshotPath); err != nil {
		return fmt.Errorf("write semantic snapshot: %w", err)
	}
	finalized = true
	if err := os.Chmod(snapshotPath, 0o600); err != nil {
		return fmt.Errorf("set semantic snapshot permissions: %w", err)
	}
	indexOpts.reportPhase("building store")
	snapshotFile, err := os.Open(snapshotPath)
	if err != nil {
		return fmt.Errorf("open semantic snapshot for store build: %w", err)
	}
	generation, metrics, err := buildSemanticGeneration(storage.BrainDir, repoDir, snapshotID, snapshotFile, contentSuffix, header, counts, opts.Now().UTC())
	_ = snapshotFile.Close()
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
		Externals:        streamCounts.Externals,
		Warnings:         sanitizeSemanticWarnings(warnings, repoDir),
		PartialFailures:  sanitizeSemanticWarnings(header.PartialFailures, repoDir),
		Capabilities:     header.Capabilities,
		NoEgressVerified: noEgress || indexOpts.skipGraph,
		WorktreeMode:     worktreeMode,
		WorktreeHash:     worktreeHash,

		Languages:               header.Languages,
		LanguageTiers:           header.LanguageTiers,
		Profile:                 header.Profile,
		RelationSet:             header.RelationSet,
		SkippedRelationFamilies: header.SkippedRelationFamilies,
		Completeness:            header.Completeness,
		ProfileLimits:           header.ProfileLimits,
		Stats:                   header.Stats,
		SummaryPresent:          summary != nil,
		CompletenessLevel:       completenessLevelFromStats(header.Stats),
		Trust:                   trustForCompleteness(completenessLevelFromStats(header.Stats)),
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
	if source.CompletenessLevel != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "completeness: %s\ntrust: %s\n", source.CompletenessLevel, source.Trust)
	}
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
		return errors.New("semantic index missing; run `entire brain index`")
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
	raw, err := safeReadFile(snapshotPath, semanticSnapshotMaxBytes())
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
	generation, metrics, err := buildSemanticGeneration(storage.BrainDir, repoDir, snapshotID, bytes.NewReader(raw), semanticGenerationContentSuffix(raw), header, counts, opts.Now().UTC())
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
		if err := os.RemoveAll(filepath.Join(storage.BrainDir, semanticDirName)); err != nil {
			return fmt.Errorf("remove semantic artifacts: %w", err)
		}
		if err := writeBrainManifestAndReadme(storage.BrainDir, *manifest); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "reset semantic brain: %s\n", storage.BrainDir)
		return nil
	}
	unlockBrain, err := acquireBrainWriteLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlockBrain()
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := removeBrainEntriesExceptLocks(storage.BrainDir); err != nil {
		return err
	}
	lockDir := filepath.Join(storage.BrainDir, brainLockDirName)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return fmt.Errorf("recreate brain lock directory: %w", err)
	}
	if f, err := os.OpenFile(filepath.Join(lockDir, brainWriteLockName), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600); err != nil {
		return fmt.Errorf("recreate brain write lock metadata: %w", err)
	} else {
		writeLockMetadata(f)
		if err := f.Close(); err != nil {
			return fmt.Errorf("close brain write lock metadata: %w", err)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reset brain: %s\n", storage.BrainDir)
	return nil
}

func removeBrainEntriesExceptLocks(brainDir string) error {
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read brain directory for reset: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == brainLockDirName {
			continue
		}
		path := filepath.Join(brainDir, entry.Name())
		if err := removeAllWithRetry(path); err != nil {
			return fmt.Errorf("remove brain entry %s: %w", entry.Name(), err)
		}
	}
	return nil
}

type semanticCounts struct {
	Files     int
	Symbols   int
	Relations int
}

type semanticBuildMetrics struct {
	GeneratedAt     time.Time `json:"generated_at"`
	GenerationID    string    `json:"generation_id"`
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
	if err := rejectExistingSymlinkPathComponents(brainDir, semanticLockDir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(brainDir, semanticLockDir, semanticIndexLockName)
	lock, err := acquireFileLock(lockPath, "index_locked", semanticIndexLockTimeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

func runSemanticDoctor(ctx context.Context, runner CommandRunner, repoDir, graphBinary string) (bool, []semanticWarning) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, semanticDoctorTimeout)
	defer cancel()
	stdout, _, err := runner.Run(runCtx, repoDir, graphBinary, "graph", "doctor", "--json")
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

func semanticSnapshotRejectsIgnoreFile(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "ignore-file") &&
		(strings.Contains(text, "unknown") ||
			strings.Contains(text, "unsupported") ||
			strings.Contains(text, "unexpected") ||
			strings.Contains(text, "flag provided but not defined"))
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
		recordRaw := append([]byte(nil), text...)
		var record semanticRecord
		if err := json.Unmarshal(recordRaw, &record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		if err := validateSemanticRecordPath(&record); err != nil {
			return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot line %d: %w", line, err)
		}
		switch record.RecordType {
		case "file", "symbol", "relation":
			if ignore.Ignored(record.semanticPath()) {
				continue
			}
			if record.RecordType == "relation" && relationEndpointIgnored(record, ignore, ignoredIDs) {
				continue
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
		case "summary":
			// The authoritative summary overrides the lean header for aggregate
			// metadata. Preserve it (sanitized) and fold it into the returned
			// header so re-ingestion paths report summary-accurate metadata.
			var summary semanticSummary
			if err := json.Unmarshal(recordRaw, &summary); err != nil {
				return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("parse semantic snapshot summary (line %d): %w", line, err)
			}
			summary.Warnings = sanitizeSemanticWarnings(ignore.FilterWarnings(summary.Warnings), repoDir)
			summary.PartialFailures = sanitizeSemanticWarnings(ignore.FilterWarnings(summary.PartialFailures), repoDir)
			mergeSemanticSummary(&header, &summary)
			encoded, marshalErr := json.Marshal(summary)
			if marshalErr != nil {
				return semanticHeader{}, semanticCounts{}, nil, fmt.Errorf("encode semantic snapshot summary (line %d): %w", line, marshalErr)
			}
			filtered.Write(encoded)
			filtered.WriteByte('\n')
		default:
			// external + unknown future record types: preserve verbatim so the
			// re-filtered snapshot stays forward compatible.
			filtered.Write(recordRaw)
			filtered.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return semanticHeader{}, semanticCounts{}, nil, err
	}
	counts.Files = len(files)
	return header, counts, filtered.Bytes(), nil
}

func newSemanticScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), semanticRecordMaxBytes())
	return scanner
}

func (r semanticRecord) semanticPath() string {
	if r.FilePath != "" {
		return r.FilePath
	}
	return r.Path
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

// buildSemanticGeneration ingests the filtered snapshot into a fresh SQLite
// generation. snapshot is scanned record-by-record (never fully buffered);
// contentSuffix is the hex digest fragment used to disambiguate the generation
// directory when a base ID already exists.
func buildSemanticGeneration(brainDir, repoDir, generationID string, snapshot io.Reader, contentSuffix string, header semanticHeader, counts semanticCounts, now time.Time) (string, semanticBuildMetrics, error) {
	start := time.Now()
	generationsRoot := filepath.Join(brainDir, semanticDirName, semanticGenerationsDir)
	if err := rejectExistingSymlinkPathComponents(brainDir, filepath.Join(semanticDirName, semanticGenerationsDir)); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if err := os.MkdirAll(generationsRoot, 0o700); err != nil {
		return "", semanticBuildMetrics{}, fmt.Errorf("create semantic generations dir: %w", err)
	}
	finalID := semanticAvailableGenerationID(generationsRoot, generationID, contentSuffix)
	tmpDir, err := os.MkdirTemp(generationsRoot, ".tmp-"+finalID+"-*")
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
	db, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		return "", semanticBuildMetrics{}, err
	}
	metrics, err := populateSemanticSQLite(db, repoDir, finalID, snapshot, header, counts, now, tmpDir)
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

	finalDir := filepath.Join(generationsRoot, finalID)
	if err := rejectExistingSymlinkPathComponents(brainDir, filepath.Join(semanticDirName, semanticGenerationsDir, finalID)); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	if _, err := os.Lstat(finalDir); err == nil {
		return "", semanticBuildMetrics{}, fmt.Errorf("semantic generation target already exists: %s", finalDir)
	} else if !os.IsNotExist(err) {
		return "", semanticBuildMetrics{}, err
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", semanticBuildMetrics{}, err
	}
	cleanup = false
	return filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, finalID)), metrics, nil
}

func semanticAvailableGenerationID(root, base, suffix string) string {
	candidate := base
	if _, err := os.Lstat(filepath.Join(root, candidate)); os.IsNotExist(err) {
		return candidate
	}
	candidate = base + "-" + suffix
	if _, err := os.Lstat(filepath.Join(root, candidate)); os.IsNotExist(err) {
		return candidate
	}
	for i := 1; ; i++ {
		candidate = fmt.Sprintf("%s-%s-%d", base, suffix, i)
		if _, err := os.Lstat(filepath.Join(root, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
}

func semanticGenerationContentSuffix(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:8])
}

func initializeSemanticSQLite(db *sql.DB) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE files (path TEXT PRIMARY KEY, blob TEXT, content_hash TEXT, language TEXT)`,
		`CREATE TABLE symbols (id TEXT PRIMARY KEY, kind TEXT, name TEXT, qualified_name TEXT, file_path TEXT, start_line INTEGER, end_line INTEGER, signature TEXT, language TEXT, stable_id_version TEXT)`,
		`CREATE TABLE relations (id INTEGER PRIMARY KEY AUTOINCREMENT, from_id TEXT NOT NULL, to_id TEXT NOT NULL, type TEXT NOT NULL, confidence REAL, reason TEXT, warning_codes TEXT)`,
		`CREATE TABLE reverse_relations (to_id TEXT NOT NULL, from_id TEXT NOT NULL, type TEXT NOT NULL)`,
		`CREATE TABLE runtime_traces (id INTEGER PRIMARY KEY AUTOINCREMENT, imported_at TEXT NOT NULL, source_path TEXT NOT NULL, from_id TEXT NOT NULL, to_id TEXT NOT NULL, observed_type TEXT, matched_static_edge INTEGER NOT NULL)`,
		`CREATE INDEX idx_symbols_name ON symbols(name)`,
		`CREATE INDEX idx_symbols_qualified_name ON symbols(qualified_name)`,
		`CREATE INDEX idx_symbols_file_path ON symbols(file_path)`,
		`CREATE INDEX idx_relations_from ON relations(from_id)`,
		`CREATE INDEX idx_relations_to ON relations(to_id)`,
		`CREATE INDEX idx_reverse_to ON reverse_relations(to_id)`,
		`CREATE INDEX idx_runtime_traces_from ON runtime_traces(from_id)`,
		`CREATE INDEX idx_runtime_traces_to ON runtime_traces(to_id)`,
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

func populateSemanticSQLite(db *sql.DB, repoDir, generationID string, snapshot io.Reader, header semanticHeader, counts semanticCounts, now time.Time, generationDir string) (semanticBuildMetrics, error) {
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

	if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?), (?, ?), (?, ?), (?, ?)`,
		"schema_version", header.SchemaVersion,
		"provider", header.Provider,
		"provider_version", header.ProviderVersion,
		"generation_id", generationID,
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
	scanner := newSemanticScanner(snapshot)
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
				files[path] = record
			}
		case "symbol":
			path := record.semanticPath()
			if path != "" {
				path, err = validateSemanticProviderPath(path)
				if err != nil {
					return commit(err)
				}
				record.setSemanticPath(path)
				if existing, ok := files[path]; ok {
					if strings.TrimSpace(existing.Language) == "" && strings.TrimSpace(record.Language) != "" {
						existing.Language = record.Language
						files[path] = existing
					}
				} else {
					files[path] = semanticRecord{RecordType: "file", FilePath: path}
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
		if _, err := tx.Exec(`INSERT OR REPLACE INTO files(path, blob, content_hash, language) VALUES (?, ?, ?, ?)`, path, record.Blob, contentHash, strings.TrimSpace(record.Language)); err != nil {
			return commit(err)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO parse_cache(path, blob, content_hash, artifact_path) VALUES (?, ?, ?, ?)`, path, record.Blob, contentHash, artifactRel); err != nil {
			return commit(err)
		}
	}

	metrics := semanticBuildMetrics{
		GeneratedAt:     now,
		GenerationID:    generationID,
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
		} else if err := validateSemanticSQLiteStore(filepath.Join(storage.BrainDir, storePath), source.Symbols, source.Relations); err != nil {
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

// nonNil guarantees a JSON array (`[]`) rather than `null` for an empty result,
// so consumers can use one uniform shape across every brain command instead of
// special-casing null per field: it returns values unchanged unless it is nil,
// in which case it returns an empty (non-nil) slice.
func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
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
	GeneratedAt time.Time `json:"generated_at"`
	// Clean is true when there are no *semantically-relevant* changed files since
	// the indexed HEAD — i.e. after changedSemanticFiles applies the built-in
	// exclusions and `.brainignore`. It lets a consumer distinguish "nothing to
	// map" from a broken command without inferring it from empty arrays; it is
	// NOT a `git status`-clean signal (changes confined to ignored paths still
	// report clean=true).
	Clean   bool             `json:"clean"`
	Files   []string         `json:"files"`
	Symbols []semanticRecord `json:"symbols"`
	// Facts are durable facts whose locus names a changed file/symbol — "what the
	// brain already knows about the code you're touching". Omitted when none.
	Facts []factRecord `json:"facts,omitempty"`
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
		return errors.New("semantic index missing; run `entire brain index`")
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
			Freshness         staleReport       `json:"freshness"`
			CompletenessLevel string            `json:"completeness_level,omitempty"`
			Trust             string            `json:"trust,omitempty"`
			LanguageTiers     map[string]string `json:"language_tiers,omitempty"`
			Pagination        semanticPage      `json:"pagination"`
			Results           []semanticRecord  `json:"results"`
		}{Freshness: freshness, CompletenessLevel: manifest.Sources.Semantic.CompletenessLevel, Trust: manifest.Sources.Semantic.Trust, LanguageTiers: manifest.Sources.Semantic.LanguageTiers, Pagination: semanticPage{Limit: queryOpts.limit, Offset: queryOpts.offset, Count: len(results)}, Results: nonNil(results)}, "", "  ")
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
		return errors.New("semantic index missing; run `entire brain index`")
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
	result := semanticContextResult{Symbols: nonNil(symbols), Relations: nonNil(relations), Neighbors: nonNil(neighbors)}
	if contextOpts.includeContent {
		result.Content = semanticContextContent(repoDir, symbols)
	}
	if contextOpts.json {
		data, err := json.MarshalIndent(struct {
			Freshness         staleReport           `json:"freshness"`
			CompletenessLevel string                `json:"completeness_level,omitempty"`
			Trust             string                `json:"trust,omitempty"`
			LanguageTiers     map[string]string     `json:"language_tiers,omitempty"`
			Pagination        semanticPage          `json:"pagination"`
			Context           semanticContextResult `json:"context"`
		}{Freshness: freshness, CompletenessLevel: manifest.Sources.Semantic.CompletenessLevel, Trust: manifest.Sources.Semantic.Trust, LanguageTiers: manifest.Sources.Semantic.LanguageTiers, Pagination: semanticPage{Limit: contextOpts.limit, Offset: contextOpts.offset, Count: len(symbols)}, Context: result}, "", "  ")
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
		return errors.New("semantic index missing; run `entire brain index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	roots, symbols, relations, err := semanticImpactFacts(storage.BrainDir, manifest.Sources.Semantic, query, impactOpts.depth, impactOpts.limit)
	if err != nil {
		return err
	}
	result := semanticImpactResult{Roots: nonNil(roots), Symbols: nonNil(symbols), Relations: nonNil(relations)}
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
		return errors.New("semantic index missing; run `entire brain index`")
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
	if files == nil {
		files = []string{}
	}
	report := semanticChangesReport{GeneratedAt: opts.Now().UTC(), Clean: len(files) == 0, Files: files, Symbols: nonNil(symbols)}
	// Surface durable facts about the code being touched. Best-effort: a missing
	// facts source or a branch lookup failure simply yields no facts, never an
	// error on the changes command. Skipped on a clean tree — no changed files
	// means there is nothing to match a fact's locus against.
	if !report.Clean {
		if branch, branchErr := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); branchErr == nil && strings.TrimSpace(branch) != "" {
			if facts, factsErr := loadFacts(storage.BrainDir, strings.TrimSpace(branch)); factsErr == nil {
				report.Facts = factsRelevantToChange(files, symbols, facts, changesOpts.limit)
			}
		}
	}
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
	if report.Clean {
		fmt.Fprintln(cmd.OutOrStdout(), "no changes since the indexed HEAD")
		return nil
	}
	for _, file := range files {
		fmt.Fprintf(cmd.OutOrStdout(), "file %s\n", file)
	}
	for _, symbol := range symbols {
		fmt.Fprintf(cmd.OutOrStdout(), "symbol %s %s:%d-%d\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
	}
	for _, fact := range report.Facts {
		fmt.Fprintf(cmd.OutOrStdout(), "fact %s %s %s\n", fact.ID, factKindOrInferred(fact), fact.Text)
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
		return errors.New("semantic index missing; run `entire brain index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	result, err := semanticBoundaryFacts(storage.BrainDir, manifest.Sources.Semantic, spec, boundaryOpts.limit)
	if err != nil {
		return err
	}
	result.Boundaries = nonNil(result.Boundaries)
	result.Handlers = nonNil(result.Handlers)
	result.Relations = nonNil(result.Relations)
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
		return errors.New("semantic index missing; run `entire brain index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	result, err := semanticTestFacts(storage.BrainDir, manifest.Sources.Semantic, query, testsOpts.limit)
	if err != nil {
		return err
	}
	result.Roots = nonNil(result.Roots)
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

// neighborCandidateIDs returns the relation endpoint ids that are not already in
// symbols — the only ids neighbor materialization needs to load, so resolution
// stays proportional to the fan-out rather than the whole symbol table. Ordered
// (relations order) and deduped.
func neighborCandidateIDs(symbols, relations []semanticRecord) []string {
	inSymbols := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		inSymbols[symbol.ID] = struct{}{}
	}
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := inSymbols[id]; ok {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, relation := range relations {
		add(relation.FromID)
		add(relation.ToID)
	}
	return ids
}

// resolveContextNeighbors materializes the relation endpoints that are not
// already in symbols into full records, so an agent gets "who calls this / what
// this calls" without a follow-up query per bare from_id/to_id. symbolsByID need
// only contain the candidate endpoints (see neighborCandidateIDs); ordering
// follows the relations and the result is capped at limit to bound fan-out on a
// hot symbol.
func resolveContextNeighbors(symbols, relations []semanticRecord, symbolsByID map[string]semanticRecord, limit int) []semanticRecord {
	if limit <= 0 || len(relations) == 0 || len(symbolsByID) == 0 {
		return nil
	}
	inSymbols := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		inSymbols[symbol.ID] = struct{}{}
	}
	seen := map[string]struct{}{}
	var neighbors []semanticRecord
	// add materializes one endpoint; it returns true once limit is reached so the
	// caller can stop. Handling FromID/ToID directly avoids a per-relation slice
	// allocation on this hot path.
	add := func(id string) bool {
		if id == "" {
			return false
		}
		if _, ok := inSymbols[id]; ok {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		record, ok := symbolsByID[id]
		if !ok {
			return false
		}
		seen[id] = struct{}{}
		neighbors = append(neighbors, record)
		return len(neighbors) >= limit
	}
	for _, relation := range relations {
		if add(relation.FromID) || add(relation.ToID) {
			return neighbors
		}
	}
	return neighbors
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
		var neighbors []semanticRecord
		if ids := neighborCandidateIDs(symbols, relations); len(ids) > 0 {
			symbolsByID, err := loadSemanticSymbolsByIDsSQLite(storePath, ids)
			if err != nil {
				return nil, nil, nil, err
			}
			neighbors = resolveContextNeighbors(symbols, relations, symbolsByID, limit)
		}
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
	var neighbors []semanticRecord
	if ids := neighborCandidateIDs(symbols, relations); len(ids) > 0 {
		symbolsByID, err := loadSemanticSymbolsByIDsSnapshot(fullPath, ids)
		if err != nil {
			return nil, nil, nil, err
		}
		neighbors = resolveContextNeighbors(symbols, relations, symbolsByID, limit)
	}
	return symbols, relations, neighbors, nil
}

func semanticRuntimeTraceFacts(brainDir string, source *semanticSourceManifest, query string, limit int) ([]semanticRecord, error) {
	if limit <= 0 {
		return []semanticRecord{}, nil
	}
	if source.StorePath == "" {
		return []semanticRecord{}, nil
	}
	storePath, err := validateSemanticDeclaredStore(brainDir, source)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureSemanticRuntimeTraceTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT rt.from_id,
       COALESCE(to_sym.id, rt.to_id) AS to_id,
       rt.observed_type,
       rt.matched_static_edge,
       rt.source_path,
       COALESCE(from_sym.file_path, '') AS from_file,
       COALESCE(to_sym.file_path, '') AS to_file,
       COALESCE(from_sym.name, '') AS from_name,
       COALESCE(from_sym.qualified_name, '') AS from_qualified,
       COALESCE(to_sym.name, '') AS to_name,
       COALESCE(to_sym.qualified_name, '') AS to_qualified
FROM runtime_traces rt
LEFT JOIN symbols from_sym ON from_sym.id = rt.from_id OR from_sym.name = rt.from_id OR from_sym.qualified_name = rt.from_id
LEFT JOIN symbols to_sym ON to_sym.id = rt.to_id OR to_sym.name = rt.to_id OR to_sym.qualified_name = rt.to_id
ORDER BY rt.imported_at DESC, rt.id DESC
LIMIT ?`, limit*8)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	terms := brainBriefFileMatchTerms(query)
	var records []semanticRecord
	for rows.Next() {
		var record semanticRecord
		var observedType, sourcePath, fromFile, toFile, fromName, fromQualified, toName, toQualified string
		var matched int
		if err := rows.Scan(&record.FromID, &record.ToID, &observedType, &matched, &sourcePath, &fromFile, &toFile, &fromName, &fromQualified, &toName, &toQualified); err != nil {
			return nil, err
		}
		record.RecordType = "runtime_trace"
		record.Type = "RUNTIME_TRACE"
		record.Confidence = 1
		record.FilePath = semanticFirstNonEmpty(fromFile, toFile)
		record.Path = record.FilePath
		record.Reason = strings.TrimSpace("runtime trace observed " + observedType)
		if record.Reason == "runtime trace observed" {
			record.Reason = "runtime trace observed edge"
		}
		if matched == 0 {
			record.Confidence = 0.5
			record.WarningCodes = []string{"UNMATCHED_STATIC_EDGE"}
		} else {
			record.WarningCodes = []string{}
		}
		record.Evidence = []semanticEvidence{{Kind: "runtime_trace_import", FilePath: sourcePath, Detail: observedType}}
		record.Score = runtimeTraceTaskScore(record, terms, fromFile, toFile, fromName, fromQualified, toName, toQualified, observedType)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Score == records[j].Score {
			if records[i].Confidence == records[j].Confidence {
				if records[i].FilePath == records[j].FilePath {
					if records[i].FromID == records[j].FromID {
						return records[i].ToID < records[j].ToID
					}
					return records[i].FromID < records[j].FromID
				}
				return records[i].FilePath < records[j].FilePath
			}
			return records[i].Confidence > records[j].Confidence
		}
		return records[i].Score > records[j].Score
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return nonNil(records), nil
}

func runtimeTraceTaskScore(record semanticRecord, terms []string, fields ...string) int {
	haystack := strings.ToLower(strings.Join(append([]string{record.FromID, record.ToID, record.FilePath, record.Reason}, fields...), " "))
	score := 0
	for _, term := range terms {
		if term != "" && strings.Contains(haystack, strings.ToLower(term)) {
			score += 3
		}
	}
	if record.Confidence >= 1 {
		score++
	}
	return score
}

func semanticFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
	rootNames := make([]string, 0, len(roots))
	rootFiles := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.FilePath != "" {
			rootFiles = append(rootFiles, root.FilePath)
		}
		name := strings.TrimSpace(root.Name)
		if name != "" {
			rootNames = append(rootNames, name)
		}
	}
	result := semanticTestsResult{Roots: roots}
	result.Suggestions = rankSemanticTestSuggestions(sortedSemanticSymbols(symbolsByID), related, rootNames, rootFiles, limit)
	return result, nil
}

func rankSemanticTestSuggestions(symbols []semanticRecord, related map[string]struct{}, rootNames, rootFiles []string, limit int) []semanticTestSuggestion {
	seen := map[string]struct{}{}
	type candidate struct {
		suggestion semanticTestSuggestion
		score      int
	}
	var candidates []candidate
	for _, symbol := range symbols {
		if !isSemanticTestSymbol(symbol) {
			continue
		}
		reason, score := semanticTestRelevance(symbol, related, rootNames, rootFiles)
		if score == 0 {
			continue
		}
		if _, ok := seen[symbol.ID]; ok {
			continue
		}
		seen[symbol.ID] = struct{}{}
		candidates = append(candidates, candidate{
			suggestion: semanticTestSuggestion{Symbol: symbol, Reason: reason},
			score:      score,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		a, b := candidates[i].suggestion.Symbol, candidates[j].suggestion.Symbol
		if a.FilePath != b.FilePath {
			return a.FilePath < b.FilePath
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		return a.ID < b.ID
	})
	var suggestions []semanticTestSuggestion
	for _, candidate := range candidates {
		suggestions = append(suggestions, candidate.suggestion)
		if len(suggestions) >= limit {
			break
		}
	}
	return suggestions
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
	if err := validateSemanticSQLiteStore(fullPath, source.Symbols, source.Relations); err != nil {
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
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
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
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// Chunk the IN-clause. Each id is bound twice (from_id and to_id), so a single
	// query over the whole set blows past SQLITE_MAX_VARIABLE_NUMBER (~32766) once
	// the symbol set exceeds ~16k — which the viz graph reaches at scale. Query in
	// batches and merge by the relation rowid so the global "ORDER BY id LIMIT"
	// still holds: each batch returns its own first `limit` rows by id, and any row
	// a batch drops has a larger id than `limit` rows it kept, so it can never
	// belong in the global top-N. A set that fits one batch behaves exactly like
	// the old single query. (loadSemanticSymbolsByIDsSQLite chunks the same way.)
	const chunk = 900
	type keyedRel struct {
		id  int64
		rec semanticRecord
	}
	var merged []keyedRel
	seen := make(map[int64]bool)
	// trimMerged keeps peak memory O(limit) instead of O(chunks x limit): once
	// merged holds well past `limit` rows, only the smallest `limit` rowids can
	// still make the final cut, so the rest can be dropped early. A trimmed row
	// re-arriving from a later batch is deduped away again by the final trim.
	trimMerged := func() {
		if limit <= 0 || len(merged) <= limit*2 {
			return
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].id < merged[j].id })
		merged = merged[:limit]
		seen = make(map[int64]bool, limit)
		for i := range merged {
			seen[merged[i].id] = true
		}
	}
	for start := 0; start < len(symbols); start += chunk {
		end := start + chunk
		if end > len(symbols) {
			end = len(symbols)
		}
		batch := symbols[start:end]
		placeholders := make([]string, len(batch))
		for i := range batch {
			placeholders[i] = "?"
		}
		args := make([]any, 0, len(batch)*2+1)
		for _, symbol := range batch {
			args = append(args, symbol.ID)
		}
		for _, symbol := range batch {
			args = append(args, symbol.ID)
		}
		args = append(args, limit)
		inClause := strings.Join(placeholders, ",")
		if err := func() error {
			rows, err := db.Query(`SELECT id, from_id, to_id, type, confidence, reason FROM relations
WHERE from_id IN (`+inClause+`) OR to_id IN (`+inClause+`)
ORDER BY id LIMIT ?`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var record semanticRecord
				record.RecordType = "relation"
				if err := rows.Scan(&id, &record.FromID, &record.ToID, &record.Type, &record.Confidence, &record.Reason); err != nil {
					return err
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				merged = append(merged, keyedRel{id: id, rec: record})
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
		trimMerged()
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].id < merged[j].id })
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	relations := make([]semanticRecord, len(merged))
	for i := range merged {
		relations[i] = merged[i].rec
	}
	return relations, nil
}

func findSemanticRelationsByTypesSQLite(storePath string, relationTypes []string) ([]semanticRecord, error) {
	if len(relationTypes) == 0 {
		return nil, nil
	}
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
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
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
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

// loadSemanticSymbolsByIDsSQLite loads only the requested symbol ids via an
// indexed `id IN (...)` lookup (chunked to stay under SQLite's parameter cap),
// so neighbor materialization is O(fan-out), not O(total symbols).
func loadSemanticSymbolsByIDsSQLite(storePath string, ids []string) (map[string]semanticRecord, error) {
	out := map[string]semanticRecord{}
	if len(ids) == 0 {
		return out, nil
	}
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	const chunk = 900
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		if err := func() error {
			rows, err := db.Query(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version FROM symbols WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var record semanticRecord
				record.RecordType = "symbol"
				if err := rows.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
					return err
				}
				out[record.ID] = record
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return out, nil
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

// loadSemanticSymbolsByIDsSnapshot scans the snapshot but retains only the
// requested ids and stops as soon as all are found, so neighbor materialization
// does not build a map of every symbol just to look up a handful of endpoints.
func loadSemanticSymbolsByIDsSnapshot(snapshotPath string, ids []string) (map[string]semanticRecord, error) {
	out := map[string]semanticRecord{}
	if len(ids) == 0 {
		return out, nil
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	f, err := os.Open(snapshotPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	first := true
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
		if _, ok := want[record.ID]; !ok {
			continue
		}
		out[record.ID] = record
		if len(out) == len(want) {
			break
		}
	}
	return out, scanner.Err()
}

func findSemanticSymbolsForFilesSQLite(storePath string, files []string, limit int) ([]semanticRecord, error) {
	if len(files) == 0 {
		return nil, nil
	}
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
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

func semanticTestRelevance(symbol semanticRecord, related map[string]struct{}, rootNames, rootFiles []string) (string, int) {
	if _, ok := related[symbol.ID]; ok {
		return "semantic relation", 1000
	}
	lowerName := strings.ToLower(symbol.Name)
	for _, rootName := range rootNames {
		if rootName != "" && strings.Contains(lowerName, strings.ToLower(rootName)) {
			return "name match", 800
		}
	}
	testTerms := lowerStringSet(historyQueryTerms(symbol.Name))
	bestOverlap := 0
	for _, rootName := range rootNames {
		overlap := 0
		for _, term := range historyQueryTerms(rootName) {
			if _, ok := testTerms[term]; ok {
				overlap++
			}
		}
		if overlap > bestOverlap {
			bestOverlap = overlap
		}
	}
	if bestOverlap >= 2 {
		return "name terms", 100 + bestOverlap*10
	}
	lowerTestPath := strings.ToLower(filepath.ToSlash(symbol.FilePath))
	for _, rootFile := range rootFiles {
		stem := strings.TrimSuffix(strings.ToLower(filepath.Base(rootFile)), strings.ToLower(filepath.Ext(rootFile)))
		if len(stem) >= 3 && strings.Contains(filepath.Base(lowerTestPath), stem) {
			return "file match", 50
		}
	}
	return "", 0
}

func lowerStringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[strings.ToLower(value)] = struct{}{}
	}
	return set
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
		return errors.New("semantic index missing; run `entire brain index`")
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
	data, err := safeReadFile(path, semanticSnapshotMaxBytes())
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
	db, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		return err
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		return err
	}
	_, err = populateSemanticSQLite(db, repoDir, generationID, bytes.NewReader(raw), header, counts, now, tmpDir)
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
	deepestExisting := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				break // tolerate not-yet-created leaves on the write path
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component must not be a symlink: %s", rel)
		}
		deepestExisting = current
	}
	// Parity with rejectSymlinkPathComponents (the read path): verify the existing
	// prefix still resolves inside root once symlinks in root's own ancestry are
	// normalized. The per-component checks alone miss an ancestor-symlink escape,
	// which is exactly the gap the write path previously had. The root may not be
	// created yet on a first write — there is nothing to escape through then, so a
	// not-yet-existing prefix is treated as safe.
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	existingResolved, err := filepath.EvalSymlinks(deepestExisting)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !pathInside(rootResolved, existingResolved) {
		return fmt.Errorf("path escapes root through symlinks: %s", rel)
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

// enforceIndexContainment verifies that the resolved repository directory stays
// inside containRoot. An empty containRoot disables the check (the CLI path,
// where the operator is trusted). Both sides are symlink-resolved so neither a
// symlinked target nor a git toplevel that walks above the root can escape.
func enforceIndexContainment(containRoot, repoDir string) error {
	if strings.TrimSpace(containRoot) == "" {
		return nil
	}
	rootAbs, err := filepath.Abs(containRoot)
	if err != nil {
		return fmt.Errorf("resolve containment root: %w", err)
	}
	repoAbs, err := filepath.Abs(repoDir)
	if err != nil {
		return fmt.Errorf("resolve repository directory: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	if resolved, err := filepath.EvalSymlinks(repoAbs); err == nil {
		repoAbs = resolved
	}
	if !pathInside(rootAbs, repoAbs) {
		return fmt.Errorf("indexed repository %q is outside the bound repository root", repoDir)
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
		relSlash := filepath.ToSlash(rel)
		if relSlash != importManifest.Sources.Semantic.SnapshotPath {
			return nil
		}
		content, err := safeReadFile(path, semanticSnapshotMaxBytes())
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
	if importManifest.Sources.Semantic.GenerationPath != "" {
		if err := replaceImportedSemanticGeneration(storage.BrainDir, tmpDir, importManifest.Sources.Semantic); err != nil {
			return err
		}
		if err := rebuildImportedSemanticStore(storage.BrainDir, repoDir, importManifest.Sources.Semantic, opts.Now()); err != nil {
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

func replaceImportedSemanticGeneration(brainDir, bundleRoot string, source *semanticSourceManifest) error {
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
	targetGenerationPath := generationPath
	target := filepath.Join(brainDir, targetGenerationPath)
	if _, err := os.Lstat(target); err == nil {
		root := filepath.Join(brainDir, semanticDirName, semanticGenerationsDir)
		targetID := semanticAvailableGenerationID(root, pathpkg.Base(filepath.ToSlash(generationPath))+"-import", semanticGenerationContentSuffix([]byte(source.SnapshotPath+"\x00"+source.StorePath)))
		targetGenerationPath = filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, targetID))
		target = filepath.Join(brainDir, targetGenerationPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".import-"+filepath.Base(generationPath)+"-*")
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
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("semantic import target already exists: %s", target)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(staging, target); err != nil {
		return err
	}
	cleanupStaging = false
	rewriteSemanticGenerationRefs(source, targetGenerationPath)
	return nil
}

func rewriteSemanticGenerationRefs(source *semanticSourceManifest, generationPath string) {
	source.GenerationPath = filepath.ToSlash(generationPath)
	source.StorePath = filepath.ToSlash(filepath.Join(generationPath, semanticSQLiteName))
	source.MetricsPath = filepath.ToSlash(filepath.Join(generationPath, semanticMetricsName))
	source.ParseCachePath = filepath.ToSlash(filepath.Join(generationPath, "parse-cache"))
}

func rebuildImportedSemanticStore(brainDir, repoDir string, source *semanticSourceManifest, now time.Time) error {
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
	raw, err := safeReadFile(filepath.Join(brainDir, snapshotRel), semanticSnapshotMaxBytes())
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
	db, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		return err
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		return err
	}
	_, err = populateSemanticSQLite(db, repoDir, pathpkg.Base(filepath.ToSlash(generationRel)), bytes.NewReader(raw), header, counts, now, filepath.Join(brainDir, generationRel))
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
	storePath := filepath.Join(brainDir, storeRel)
	if err := os.Remove(storePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return writeFileAtomic(storePath, data, 0o600)
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
		data, err := safeReadFile(path, semanticSnapshotMaxBytes())
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
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
		if err := validateSemanticSQLiteStore(storeFullPath, manifest.Sources.Semantic.Symbols, manifest.Sources.Semantic.Relations); err != nil {
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

func validateSemanticSQLiteStore(path string, expectedCounts ...int) error {
	expectedFiles := -1
	expectedSymbols := 0
	expectedRelations := 0
	switch len(expectedCounts) {
	case 2:
		expectedSymbols = expectedCounts[0]
		expectedRelations = expectedCounts[1]
	case 3:
		expectedFiles = expectedCounts[0]
		expectedSymbols = expectedCounts[1]
		expectedRelations = expectedCounts[2]
	default:
		return fmt.Errorf("validate semantic sqlite store: expected 2 or 3 count arguments, got %d", len(expectedCounts))
	}
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
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(path))
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
	if expectedFiles >= 0 {
		files, err := semanticSQLiteTableCount(db, "files")
		if err != nil {
			return err
		}
		if files != expectedFiles {
			return fmt.Errorf("semantic sqlite file count %d does not match manifest %d", files, expectedFiles)
		}
	}
	symbols, err := semanticSQLiteTableCount(db, "symbols")
	if err != nil {
		return err
	}
	relations, err := semanticSQLiteTableCount(db, "relations")
	if err != nil {
		return err
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
		case "summary":
			// The trailing summary is authoritative for aggregate metadata. Merge
			// it so rebuild/validation paths see the same summary-merged header the
			// initial index produced. Warnings on disk are already sanitized; the
			// idempotent re-sanitize (empty repoDir still redacts absolute paths)
			// guards against a hand-authored or stale snapshot.
			var summary semanticSummary
			if err := json.Unmarshal(text, &summary); err != nil {
				return semanticHeader{}, semanticCounts{}, fmt.Errorf("parse semantic snapshot summary (line %d): %w", line, err)
			}
			summary.Warnings = sanitizeSemanticWarnings(summary.Warnings, "")
			summary.PartialFailures = sanitizeSemanticWarnings(summary.PartialFailures, "")
			mergeSemanticSummary(&header, &summary)
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
	if err := ensureSemanticAuditPathSafe(brainDir); err != nil {
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
