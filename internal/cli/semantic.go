package cli

import (
	"archive/tar"
	"bufio"
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
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/providerdetail"
	"github.com/spf13/cobra"
)

const (
	semanticDirName        = "semantic"
	semanticSnapshotsDir   = "snapshots"
	semanticGenerationsDir = "generations"
	semanticBundleDir      = "bundles"
	semanticAuditLogName   = "audit.jsonl"
	semanticLockDir        = "locks"
	semanticIndexLockName  = "index.lock"
	semanticSnapshotName   = "snapshot.ndjson"
	semanticManifestName   = "manifest.json"
	semanticSQLiteName     = "semantic.sqlite"
	semanticMetricsName    = "metrics.json"
	semanticSupportedMajor = "1"
	// semanticSchemaVersion is the newest provider schema this build knows how
	// to ingest in full. A newer minor is still accepted (ADR 0001: minors are
	// additive) but warns, because facts added after this version are dropped.
	//
	// 1.2 added identity_revision (consumed: internal/entityindex keys every
	// stored delta by it and re-migrates when it changes). 1.3 added
	// completeness.health (consumed: carried verbatim as the Completeness raw
	// message and persisted on the manifest) and the partial-failure language
	// field above. 1.4 added the derived relation provenance label (modeled on
	// semanticRecord so the streaming filter round-trips it; the provider
	// computes it from type and resolution, both already modeled, so it carries
	// no information this build was missing). Leaving this at 1.1 fired
	// provider_schema_newer_minor on EVERY index against a current
	// entire-graph, telling users facts were dropped when they were not;
	// leaving it at 1.3 against the released graph v0.5.0 pair did the same.
	semanticSchemaVersion               = "1.4"
	semanticSupportedSchemaRange        = "semantic schema >=1.0 <2.0"
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

	// semanticWorktreeModeName is the worktree_mode value recorded for an index
	// built from dirty worktree content rather than committed HEAD. The two are
	// different artifacts at the same commit, so several checks compare against
	// it by name.
	semanticWorktreeModeName = "worktree"
)

// A var so lock-contention tests can fail fast instead of waiting out the
// production retry window, matching the maxDocumentTranscriptBytes seam.
var semanticIndexLockTimeout = 10 * time.Second

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
	GraphRepoRoot    string `json:"graph_repo_root,omitempty"`
	StoreOwner       string `json:"store_owner,omitempty"`
	graphStore       *graphStoreDescriptor
	GeneratedAt      time.Time         `json:"generated_at"`
	Commit           string            `json:"commit,omitempty"`
	Tree             string            `json:"tree,omitempty"`
	Branch           string            `json:"branch,omitempty"`
	DefaultBranch    string            `json:"default_branch,omitempty"`
	DirtyWorktree    bool              `json:"dirty_worktree"`
	Provider         string            `json:"provider,omitempty"`
	ProviderVersion  string            `json:"provider_version,omitempty"`
	IdentityRevision string            `json:"identity_revision,omitempty"`
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
	// Language is schema 1.3's optional partial-failure classification. The
	// provider sets it when a source fails before any file record exists, so
	// it is the ONLY signal of what failed in that case: Path is empty there.
	// Dropping it left those failures unattributable to any language.
	Language string `json:"language,omitempty"`
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
	SchemaVersion    string            `json:"schema_version"`
	Provider         string            `json:"provider"`
	ProviderVersion  string            `json:"provider_version"`
	IdentityRevision string            `json:"identity_revision,omitempty"`
	RepoRoot         string            `json:"repo_root,omitempty"`
	RepoKey          string            `json:"repo_key"`
	Commit           string            `json:"commit"`
	Tree             string            `json:"tree"`
	Languages        []string          `json:"languages"`
	LanguageTiers    map[string]string `json:"language_tiers,omitempty"`
	Capabilities     []string          `json:"capabilities"`
	Warnings         []semanticWarning `json:"warnings"`
	PartialFailures  []semanticWarning `json:"partial_failures"`

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
	Bytes           int                `json:"bytes,omitempty"`            // file record: source size
	ContainerID     string             `json:"container_id,omitempty"`     // symbol record: enclosing symbol
	BodyHash        string             `json:"body_hash,omitempty"`        // symbol record: content hash
	RelationScope   string             `json:"relation_scope,omitempty"`   // relation record: file|external|...
	Resolution      string             `json:"resolution,omitempty"`       // relation record: exact|type_inferred|name_only
	TargetKind      string             `json:"target_kind,omitempty"`      // relation record: symbol|external
	Evidence        []semanticEvidence `json:"evidence,omitempty"`         // relation record: supporting spans
	EvidenceDropped int                `json:"evidence_dropped,omitempty"` // relation record: supporting spans omitted by provider limit

	// Schema 1.4 field. The provider derives it from Type and Resolution at
	// marshal time, so it adds no information — but it must be modeled so the
	// streaming filter round-trips the literal field.
	Provenance string `json:"provenance,omitempty"` // relation record: EXTRACTED|INFERRED|AMBIGUOUS
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
	force       bool
	graphBinary string
	skipGraph   bool
	// localCommit and localTree are brain's OWN `git rev-parse HEAD` and
	// `HEAD^{tree}`, used only to stamp a snapshot header the provider could
	// not stamp itself. entire-graph refuses git metadata subprocesses in a
	// partial clone -- correctly, since a promisor remote can fetch over the
	// network and it runs --no-network -- and returns an empty commit and
	// tree, which made brain discard an index it had already parsed in full.
	//
	// They are withdrawn for a dirty --worktree index: see runSemanticIndex.
	// What is indexed there is the working tree, which does not hash to
	// HEAD^{tree}, and the validator skips the tree check in that mode -- so a
	// stamp would write a wrong content identity that nothing could catch.
	//
	// These are facts rather than options, and they live here only because the
	// stream path already threads this struct end to end.
	localCommit string
	localTree   string
	// Compatibility aliases for callers created before the graph-provider
	// command rename. New code should use graphBinary and skipGraph.
	semBinary      string
	skipSem        bool
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
	// progressCounts reports the running file/symbol/relation tallies while the
	// provider streams, so the caller can render a determinate bar. Optional:
	// nil simply means an indeterminate spinner.
	progressCounts func(files, symbols, relations int)
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

// semanticIndexMissingError is the single wording of "there is no semantic
// index here", used by every surface that discovers one is missing.
//
// It exists because the remedy was wrong in all nine of them: they told the
// reader to run "entire brain index", and there is no "index" verb at the root
// -- the binary answers: unknown command "index". A reader who hit this error
// was told to fix it with a command that could not run, which is worse than
// saying nothing. The remedy is now built from the registered verb and the
// spelling of this binary the reader actually reached, so it is runnable as
// printed. TestEveryCommandPrintedInSourceResolves holds that.
func semanticIndexMissingError() error {
	return fmt.Errorf("code index missing; run `%s`", brainInvocation(os.LookupEnv, semanticIndexVerb))
}

type semanticResetOptions struct {
	force        bool
	semanticOnly bool
}

func newSemanticIndexCommand(opts Options) *cobra.Command {
	indexOpts := semanticIndexOptions{graphBinary: "entire"}
	cmd := &cobra.Command{
		Use:   "index [path]",
		Short: "Build Graph’s shared committed-tree code index",
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
	cmd.Flags().BoolVar(&indexOpts.force, "force", false, "Force Graph to rebuild its committed-tree store")
	cmd.Flags().StringVar(&indexOpts.graphBinary, "graph-binary", "entire", "Entire CLI binary that exposes `graph` provider commands")
	cmd.Flags().BoolVar(&indexOpts.skipGraph, "skip-graph", false, "Record code-index metadata without invoking the graph provider")
	cmd.Flags().BoolVar(&indexOpts.worktree, "worktree", false, "Deprecated: persistent worktree indexing is unsupported; use Graph queries")
	cmd.Flags().DurationVar(&indexOpts.timeout, "graph-timeout", 0, "Overall deadline for Graph indexing (0 uses the default)")
	cmd.Flags().DurationVar(&indexOpts.inactivityTimeout, "graph-inactivity-timeout", 0, "Deprecated: unused by Graph-owned indexing; use --graph-timeout")
	cmd.Flags().StringVar(&indexOpts.profile, "profile", "", "Graph index profile (e.g. full, syntax-only); empty uses full")
	return cmd
}

func newSemanticRepairCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair [path]",
		Short: "Rebuild Graph's committed-tree code index",
		Long:  "Ask Graph to rebuild its shared code store; Brain runtime observations are preserved.",
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
	cmd.Flags().BoolVar(&resetOpts.semanticOnly, "semantic-only", false, "Clear Brain code-index metadata, legacy artifacts and runtime observations")
	cmd.Flags().BoolVar(&resetOpts.force, "force", false, "Confirm removal without an interactive prompt")
	return cmd
}

func runSemanticIndex(ctx context.Context, cmd *cobra.Command, opts Options, indexOpts semanticIndexOptions, target string) error {
	return runGraphOwnedIndex(ctx, cmd, opts, indexOpts, target)
}

func runSemanticRepair(ctx context.Context, cmd *cobra.Command, opts Options, target string) error {
	return runGraphOwnedIndex(ctx, cmd, opts, semanticIndexOptions{force: true}, target)
}

func runSemanticReset(ctx context.Context, cmd *cobra.Command, opts Options, resetOpts semanticResetOptions, target string) error {
	if !resetOpts.force {
		return errors.New("reset requires --force")
	}
	repoDir, local, err := clibase.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
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
	if err := clibase.RejectSymlinkedBrainRoot(storage.BrainDir); err != nil {
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
		if err := clibase.RejectExistingSymlinkPathComponents(storage.BrainDir, semanticDirName); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(storage.BrainDir, semanticDirName)); err != nil {
			return fmt.Errorf("remove code-index artifacts: %w", err)
		}
		if err := writeBrainManifestAndReadme(storage.BrainDir, *manifest); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "reset code index: %s\n", storage.BrainDir)
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
		writeLockOwnerMetadata(filepath.Join(lockDir, brainWriteLockName))
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
	if err := clibase.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	if err := clibase.RejectExistingSymlinkPathComponents(brainDir, semanticLockDir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(brainDir, semanticLockDir, semanticIndexLockName)
	lock, err := acquireFileLock(lockPath, "index_locked", semanticIndexLockTimeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

// semanticDoctorReport is what the brain learns from `graph doctor --json`
// before it spends a snapshot run.
//
// RepoKey is the provider's answer to "what repo_key will you stamp on this
// repository". It is AUTHORITATIVE when present: the provider is the process
// that writes the header, so it cannot be wrong about its own rule, whereas
// providerRepoKeyFromRemotes is a mirror that drifts the moment entire-graph
// changes. It is empty when the installed provider predates the handshake, and
// the mirror is the fallback for exactly that case.
type semanticDoctorReport struct {
	NoEgress bool
	RepoKey  string
}

func runSemanticDoctor(ctx context.Context, runner CommandRunner, repoDir, graphBinary string) (semanticDoctorReport, []semanticWarning) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, semanticDoctorTimeout)
	defer cancel()
	stdout, _, err := runner.Run(runCtx, repoDir, graphBinary, "graph", "doctor", "--json")
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return semanticDoctorReport{}, []semanticWarning{{Code: "provider_doctor_timeout", Severity: "warning", Effect: "provider diagnostics unavailable", Detail: fmt.Sprintf("semantic provider doctor timed out after %s", semanticDoctorTimeout)}}
	}
	if err != nil {
		return semanticDoctorReport{}, []semanticWarning{{Code: "provider_doctor_failed", Severity: "warning", Effect: "provider diagnostics unavailable", Detail: err.Error()}}
	}
	var data map[string]any
	if err := json.Unmarshal(stdout, &data); err != nil {
		return semanticDoctorReport{}, []semanticWarning{{Code: "provider_doctor_malformed", Severity: "warning", Effect: "provider diagnostics unavailable", Detail: err.Error()}}
	}
	report := semanticDoctorReport{RepoKey: strings.TrimSpace(stringValue(data, "repo_key"))}
	if boolValue(data, "network_egress") || boolValue(data, "requires_network") || boolValue(data, "network_required") {
		return report, []semanticWarning{{Code: "provider_network_required", Severity: "error", Effect: "phase 1 local-only guarantee not verified", Detail: "provider doctor reports network access is required"}}
	}
	if boolValue(data, "no_egress") || boolValue(data, "no_egress_verified") || boolValue(data, "local_only") {
		report.NoEgress = true
		return report, nil
	}
	return report, []semanticWarning{{Code: "provider_no_egress_unknown", Severity: "warning", Effect: "phase 1 local-only guarantee not fully verified", Detail: "provider doctor did not report no-egress status"}}
}

func stringValue(data map[string]any, key string) string {
	value, ok := data[key]
	if !ok {
		return ""
	}
	s, _ := value.(string)
	return s
}

// errSemanticProviderAbsent marks a semantic failure whose cause is the graph
// provider not being installed or launchable at all — binary missing, host CLI
// without the graph plugin — as opposed to a provider that ran and
// misbehaved. Refresh degrades this class instead of aborting: the graph
// provider is strongly encouraged but never required, and every other brain
// source builds without it. A provider that is PRESENT and fails stays fatal.
var errSemanticProviderAbsent = errors.New("semantic provider is not installed")

// errSemanticStoreRetired rejects a persisted pre-0.2.0 source: Brain no longer
// reads its own snapshot/generation layout, only Graph's live store handle.
var errSemanticStoreRetired = errors.New("code context store is Graph-owned")

type semanticProviderAbsentError struct{ err error }

func (e semanticProviderAbsentError) Error() string { return e.err.Error() }
func (e semanticProviderAbsentError) Unwrap() error { return e.err }
func (e semanticProviderAbsentError) Is(target error) bool {
	return target == errSemanticProviderAbsent
}

// semanticDoctorFailureError turns a failed no-egress verification into an
// error that names the actual root cause instead of a single generic
// "no-egress status is not verified" claim. runSemanticDoctor already
// distinguishes several unrelated failure modes (binary missing or not
// executable, malformed doctor output, a provider that openly requires
// network access, a doctor that is simply silent about egress) and captures
// the real reason in each warning's Detail; this only got discarded before
// reaching the operator. The code prefix from the first warning is preserved
// so existing callers matching on it keep working.
func semanticDoctorFailureError(graphBinary string, doctorWarnings []semanticWarning) error {
	if (strings.TrimSpace(graphBinary) == entireBinaryName || strings.TrimSpace(graphBinary) == "") && len(doctorWarnings) > 0 && semanticProviderMissing(doctorWarnings[0].Detail) {
		return semanticProviderUnverifiedError(graphBinary, doctorWarnings)
	}
	code := "provider_no_egress_unverified"
	detail := ""
	if len(doctorWarnings) > 0 {
		if doctorWarnings[0].Code != "" {
			code = doctorWarnings[0].Code
		}
		detail = doctorWarnings[0].Detail
	}
	switch code {
	case "provider_doctor_failed":
		var err error
		switch {
		case semanticDetailMeansBinaryNotFound(detail):
			err = fmt.Errorf("%s: graph binary %q was not found: %s; install the provider with `entire plugin install graph`", code, graphBinary, detail)
		case semanticDetailMeansBinaryNotExecutable(detail):
			err = fmt.Errorf("%s: graph binary %q could not be run: %s; check its permissions, or reinstall the provider with `entire plugin install graph`", code, graphBinary, detail)
		default:
			err = fmt.Errorf("%s: semantic provider doctor failed to run graph binary %q: %s; reinstall the provider with `entire plugin install graph`", code, graphBinary, detail)
		}
		if semanticDetailMeansProviderAbsent(detail) {
			return semanticProviderAbsentError{err}
		}
		return err
	case "provider_doctor_timeout":
		return fmt.Errorf("%s: semantic provider doctor on graph binary %q did not respond in time: %s; retry, or reinstall: `entire plugin install graph`", code, graphBinary, detail)
	case "provider_doctor_malformed":
		return fmt.Errorf("%s: semantic provider doctor on graph binary %q returned output that could not be parsed: %s; reinstall: `entire plugin install graph`", code, graphBinary, detail)
	case "provider_network_required":
		return fmt.Errorf("%s: semantic provider graph binary %q requires network access, which local-only indexing forbids: %s; use `entire plugin install graph`", code, graphBinary, detail)
	case "provider_no_egress_unknown":
		return fmt.Errorf("%s: semantic provider graph binary %q did not confirm a no-egress guarantee: %s; upgrade it: `entire plugin install graph`", code, graphBinary, detail)
	default:
		if detail != "" {
			return fmt.Errorf("%s: semantic provider no-egress status is not verified for graph binary %q: %s; reinstall the provider with `entire plugin install graph`", code, graphBinary, detail)
		}
		return fmt.Errorf("%s: semantic provider no-egress status is not verified for graph binary %q; reinstall the provider with `entire plugin install graph`", code, graphBinary)
	}
}

// semanticDetailMeansBinaryNotFound reports whether a runSemanticDoctor
// failure detail indicates the configured --graph-binary does not exist on
// PATH (or a same-named impostor tool shadowed the real one so badly the
// shell itself could not locate an executable), as opposed to some other
// doctor failure.
func semanticDetailMeansBinaryNotFound(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "executable file not found") || strings.Contains(d, "no such file or directory")
}

// semanticDetailMeansBinaryNotExecutable reports whether a runSemanticDoctor
// failure detail indicates the configured --graph-binary exists but could not
// be executed (wrong permissions, wrong architecture, etc.).
func semanticDetailMeansBinaryNotExecutable(detail string) bool {
	d := strings.ToLower(detail)
	return strings.Contains(d, "permission denied") || strings.Contains(d, "exec format error")
}

// semanticProviderUnverifiedError is the one error a reader ever sees when the
// semantic index cannot be built because the provider could not be verified.
//
// runSemanticDoctor already captures WHY in the warning's Detail -- the missing
// plugin's own "unknown command \"graph\"", a timeout, malformed JSON. That
// detail used to be collected and then dropped in favour of the formatted
// string, so the first thing a new user saw was
//
//	provider_doctor_failed: semantic provider no-egress status is not verified
//
// which names neither the cause nor a next step. It is reported here instead:
// code, the human sentence, the underlying detail, and -- for the failure mode
// that a first run actually hits, the entire-graph plugin simply not being
// installed -- the install step by name.
func semanticProviderUnverifiedError(graphBinary string, warnings []semanticWarning) error {
	code := "provider_no_egress_unverified"
	detail := ""
	if len(warnings) > 0 {
		if warnings[0].Code != "" {
			code = warnings[0].Code
		}
		detail = strings.TrimSpace(warnings[0].Detail)
	}
	message := fmt.Sprintf("%s: semantic provider no-egress status is not verified", code)
	if summary := semanticProviderDetailSummary(detail); summary != "" {
		message += ": " + summary
	}
	binary := strings.TrimSpace(graphBinary)
	if binary == "" {
		binary = entireBinaryName
	}
	// One decision, shared with the entity index, about BOTH what the remedy
	// is and whether to say it. The guard this replaces lived here only, so
	// the entity index stacked a duplicate on hosts that name the install
	// command themselves -- and it tested the DETAIL, which is wrong in the
	// other direction: the message is a summary of the detail, and the
	// summary is what cuts the host's remedy line, so a multi-line detail
	// suppressed a sentence the reader never got. See MissingSuffix.
	message += providerdetail.MissingSuffix(message, detail, binary, os.LookupEnv)
	if providerdetail.Missing(detail) && semanticDetailMeansProviderAbsent(detail) {
		return semanticProviderAbsentError{errors.New(message)}
	}
	return errors.New(message)
}

// semanticProviderDetailSummary reduces a subprocess failure to one printable
// line. The rule lives in internal/providerdetail because the entity index
// fails from the same cause and must report it identically; see that package.
func semanticProviderDetailSummary(detail string) string {
	return providerdetail.Summary(detail)
}

// semanticDetailMeansProviderAbsent decides the FATALITY class of a doctor
// failure: an ABSENT provider degrades the refresh's semantic stage, a
// present-and-broken one aborts it. Absence has exactly two shapes. Either
// the doctor process never launched — the command runner stamps "exit status"
// into the detail whenever the binary ran, so no stamp plus a not-found or
// not-executable detail is a launch failure — or the binary RAN and reported
// the graph plugin missing by name. Generic "no such file or directory" text
// from a provider that ran is its own stderr talking about something else
// (e.g. a helper file it could not open) and must stay fatal; only
// semanticProviderMissing, which routes MESSAGES, may match that loosely.
func semanticDetailMeansProviderAbsent(detail string) bool {
	text := strings.ToLower(detail)
	if !strings.Contains(text, "exit status") {
		return semanticDetailMeansBinaryNotFound(detail) || semanticDetailMeansBinaryNotExecutable(detail)
	}
	for _, marker := range []string{
		`unknown command "graph"`,
		"unknown command 'graph'",
		"unknown command graph",
		"plugin is not installed",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// semanticProviderMissing recognises the provider-absent shape of a doctor
// failure. Shared with the entity index for the same reason, and matched
// against the FULL detail so summarising what is shown cannot change what is
// recognised.
func semanticProviderMissing(detail string) bool {
	return providerdetail.Missing(detail)
}

func boolValue(data map[string]any, key string) bool {
	value, ok := data[key]
	if !ok {
		return false
	}
	b, _ := value.(bool)
	return b
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
// semantic relation endpoint ID, or "" when the ID carries no path or the path
// cannot be recovered from the ID alone.
//
// entire-graph builds endpoint IDs by joining unescaped fields with ":":
//
//	symbol:   "<repo-key>:<language>:<path>:<kind>:<qualified-name>"
//	file:     "<repo-key>:file:<path>"
//	external: "external:<kind>:<value>"          (carries no file path)
//
// A file path may legitimately contain ":" ("od:d/mod.py"), a local repo key is
// "local/<directory name>" and so may contain ":" too, and a qualified name
// routinely does (an Express route symbol is named "/users/:id"). Splitting on
// ":" and taking a fixed field therefore reads the wrong segment: it turned
// "od:d/mod.py" into "od" and a colon in the repo key into the language. That
// path is what the ignore set is matched against, so a mis-read segment either
// redacts relations whose file is not ignored or keeps relations whose file is.
//
// A relation record carries no file_path of its own — entire-graph's
// RelationRecord has only from_id/to_id — so the path cannot simply be read off
// the record; it has to come out of the ID. The parse below is therefore
// anchored on the two fields that are known to be free of ":": the repo key
// (known from the snapshot header) and the kind, which is always a bare token.
func semanticEndpointPath(repoKey, id string) string {
	if id == "" || strings.HasPrefix(id, "external:") {
		return ""
	}
	if repoKey == "" {
		// Without the header's repo key the leading field cannot be delimited,
		// so no parse is trustworthy. Endpoints then fall back to the ignored-ID
		// set, which is exact. (A snapshot with no repo_key is rejected by
		// validateSemanticSnapshotHeader anyway.)
		return ""
	}
	rest, ok := strings.CutPrefix(id, repoKey+":")
	if !ok {
		// An endpoint from another repository: it names no file in this one.
		return ""
	}
	// File endpoint: everything after the "file:" marker is the path, colons
	// included, so this shape is always exact.
	if path, ok := strings.CutPrefix(rest, "file:"); ok {
		return path
	}
	// Symbol endpoint: the language never contains ":", so it always ends at the
	// first colon.
	_, rest, ok = strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	return semanticSymbolIDPath(rest)
}

// semanticSymbolIDPath recognizes provider kind markers, not arbitrary bare
// path segments. Unescaped IDs are ambiguous when both the path and name
// contain a kind marker; return no guess then and use snapshot ID metadata.
func semanticSymbolIDPath(rest string) string {
	result := ""
	fallback, candidates := "", 0
	for i := 0; i < len(rest); i++ {
		if rest[i] != ':' || i == 0 {
			continue
		}
		kind, _, ok := strings.Cut(rest[i+1:], ":")
		if !ok {
			continue
		}
		if kind != "" && !strings.ContainsAny(kind, "/.") {
			// A later bare token may be the real (possibly future) kind, or
			// part of the qualified name. Neither interpretation is provable.
			if result != "" {
				return ""
			}
			fallback = rest[:i]
			candidates++
		}
		switch kind {
		case "function", "method", "class", "struct", "interface", "enum", "type", "type_alias", "variable", "constant", "const", "field", "property", "module", "namespace", "package", "constructor", "trait", "impl", "macro", "test", "route", "cli_command", "workflow", "job", "tool":
		default:
			continue
		}
		if result != "" {
			return ""
		}
		result = rest[:i]
	}
	if result == "" && candidates == 1 {
		return fallback
	}
	return result
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
		path := record.semanticPath()
		if path != "" {
			clean, err := validateSemanticProviderPath(path)
			if err != nil {
				return err
			}
			record.setSemanticPath(clean)
		}
	default:
	}
	for i := range record.Evidence {
		path := record.Evidence[i].FilePath
		if path == "" {
			continue
		}
		clean, err := validateSemanticProviderPath(path)
		if err != nil {
			return fmt.Errorf("semantic evidence path: %w", err)
		}
		record.Evidence[i].FilePath = clean
	}
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

// parseSemanticSchemaVersion splits a provider schema_version into its major
// and minor numbers. ADR 0001 fixes the shape as major.minor, so anything that
// is not two non-negative integers is not a version this reader can reason
// about: the old implementation only looked at the text before the first dot,
// which silently accepted "1.abc" and "1." as if they were ordinary 1.x minors
// and let a malformed header ride straight into the store.
func parseSemanticSchemaVersion(version string) (int, int, error) {
	majorText, rest, ok := strings.Cut(version, ".")
	if !ok {
		return 0, 0, fmt.Errorf("semantic schema version %q is not major.minor", version)
	}
	// A trailing patch component is tolerated and ignored: it is additive, and
	// refusing it would violate the tolerant-reader rule.
	minorText, _, _ := strings.Cut(rest, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major < 0 {
		return 0, 0, fmt.Errorf("semantic schema version %q has a non-numeric major", version)
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil || minor < 0 {
		return 0, 0, fmt.Errorf("semantic schema version %q has a non-numeric minor", version)
	}
	return major, minor, nil
}

// validateSemanticSchema applies ADR 0001's compatibility boundary: refuse an
// unknown major, accept every minor within the supported major.
func validateSemanticSchema(version string) error {
	major, _, err := parseSemanticSchemaVersion(version)
	if err != nil {
		return fmt.Errorf("%w (this build reads %s); reinstall the provider with `entire plugin install graph`", err, semanticSupportedSchemaRange)
	}
	if strconv.Itoa(major) != semanticSupportedMajor {
		// Name which side of the pin is behind, so the reader upgrades the
		// right binary instead of reinstalling the one that already works.
		action := "the provider is behind this build; upgrade it with `entire plugin install graph`"
		if supportedMajor, convErr := strconv.Atoi(semanticSupportedMajor); convErr == nil && major > supportedMajor {
			action = "the provider is ahead of this build; upgrade entire-brain"
		}
		return fmt.Errorf("unsupported semantic schema major version %q: this build reads %s — %s", strconv.Itoa(major), semanticSupportedSchemaRange, action)
	}
	return nil
}

// semanticSchemaMinorSkewWarning implements ADR 0001 rule 3: a newer minor
// within the supported major is accepted, but the reader must WARN, because the
// producer may have emitted additive facts this build does not know how to
// ingest. Accepting silently is not compliance — the whole point of the warning
// is that the resulting index is quietly less complete than the snapshot.
//
// This is the single implementation of that rule, applied at every place a
// schema_version crosses into this build alongside validateSemanticSchema:
//
//  1. graphStoreSource — the live Graph-owned store header, recorded on the
//     semantic source manifest it assembles for index, refresh, status and
//     brief;
//  2. semanticFreshnessReport — the "graph" freshness axis reported by brain
//     doctor/status and every semantic read command, degraded rather than ok;
//  3. semanticRefreshNeeded — refresh's non-fatal identity warning;
//  4. validateImportedBundle — an imported bundle manifest, warning appended to
//     the semantic source that is then persisted.
//
// It returns nil for an unparseable or non-supported-major version:
// validateSemanticSchema is the sole authority on rejecting those outright.
// validateImportedBundle runs it directly, first; the three live-path call
// sites above never see a header validateSemanticSchema would reject, because
// fetchGraphStore already ran it as a hard gate before producing the
// descriptor they read schema_version from.
func semanticSchemaMinorSkewWarning(version string) *semanticWarning {
	major, minor, err := parseSemanticSchemaVersion(version)
	if err != nil {
		return nil
	}
	knownMajor, knownMinor, err := parseSemanticSchemaVersion(semanticSchemaVersion)
	if err != nil || strconv.Itoa(major) != semanticSupportedMajor || minor <= knownMinor || major != knownMajor {
		return nil
	}
	return &semanticWarning{
		Code:     "provider_schema_newer_minor",
		Severity: "warning",
		Effect:   "additive semantic facts introduced after " + semanticSchemaVersion + " may not have been ingested",
		// Kept under statusCauseWidth: this Detail is rendered inside the
		// one-line status cause, and the longer wording was cut mid-word
		// ("...so the snapshot is usabl") in the short report.
		Detail: fmt.Sprintf(
			"provider schema_version %q is newer than this build's %s; minors are additive, so the snapshot is usable -- upgrade to ingest everything",
			version, semanticSchemaVersion),
	}
}

// semanticBuildCancelCheckEvery / semanticBuildCancelCheckEveryFile are how
// often the store build asks whether its deadline has passed. Both are small
// enough that a cancelled MCP index stops within a fraction of a second and
// large enough that the check itself is free.
//
// The two differ by two orders of magnitude because the loops do: a snapshot
// record is a JSON unmarshal plus an INSERT (~0.1ms, 205k of them), while a
// parse-cache file is a sha256 of the file's contents plus an artifact write
// (~11ms, ~900 of them). One constant for both is what left the first version of
// this fix still 5 seconds past its deadline: at 1024, a file loop of 900
// entries checks exactly once, on entry.
const (
	semanticBuildCancelCheckEvery     = 1024
	semanticBuildCancelCheckEveryFile = 16
)

func writeSemanticOverlayFile(brainDir, base, head, branch string, now time.Time) (string, error) {
	name := base + ".." + head + ".json"
	rel := filepath.ToSlash(filepath.Join(semanticDirName, "overlays", name))
	payload := map[string]string{
		"base":         base,
		"head":         head,
		"branch":       branch,
		"generated_at": now.Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := clibase.RejectExistingSymlinkPathComponents(brainDir, filepath.FromSlash(rel)); err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(brainDir, filepath.FromSlash(rel)), data, 0o600); err != nil {
		return "", err
	}
	return rel, nil
}

type brainIgnore struct {
	patterns []string
	// repoDir is kept so warning text carrying ABSOLUTE paths can be matched.
	// Provider warnings are built from raw subprocess output (Detail is
	// serr.Error()), so the paths in them are absolute, while every pattern in
	// .brainignore is repo-relative. Without the root to relativise against,
	// Ignored("/abs/repo/secret/x.go") is false for the pattern "secret/" and
	// the warning is never withheld. Empty is valid and simply disables the
	// relativising step.
	repoDir string
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
			return brainIgnore{repoDir: repoDir}, nil
		}
		return brainIgnore{repoDir: repoDir}, fmt.Errorf("read .brainignore: %w", err)
	}
	ignore := brainIgnore{repoDir: repoDir}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ignore.patterns = append(ignore.patterns, filepath.ToSlash(line))
	}
	return ignore, nil
}

// entireAuthoredGatePaths are the files ENTIRE'S OWN installers write into a
// repository. `entire graph setup` creates AGENTS.md, CLAUDE.md and
// .claude/settings.json; brain itself writes .brainignore. So a user who runs
// the documented install sequence arrives at the next command with a tree that
// Entire dirtied, and BOTH components refuse it:
//
//	$ entire graph setup
//	$ entire brain setup
//	seed:     dirty_worktree: refusing to seed uncommitted content without --worktree
//	semantic: dirty_worktree: refusing to index uncommitted content without --worktree
//
// The first person to report this blamed themselves ("could be me"). It was
// not them: the install dirtied the tree the install then refused.
//
// The list is not invented here. The five runtime-state entries are exactly
// this repository's own .brainignore stanza headed "Local agent/editor runtime
// state", which is the maintainers' existing line between agent runtime state
// and source; copying it is what keeps this set principled instead of a guess.
// AGENTS.md, CLAUDE.md and .brainignore are added because they are Entire's
// output too and a fresh repo has no .brainignore for them to be listed in.
//
// The writers live across the whole product, not just this repo, which is why
// "nothing in entire-brain writes it" is the wrong test for membership:
// entire-graph's agent wiring writes AGENTS.md, CLAUDE.md and
// .claude/settings.json; the cli writes .codex/hooks.json for
// `entire enable --agent codex` (cmd/entire/cli/agent/codex/hook_root.go);
// brain writes .brainignore. One install flow, one user, several repos.
//
// This list is GATE-ONLY, and that is the whole design. brainIgnore.Ignored is
// the INDEXING filter — filterSemanticSnapshot, the snapshot stream, the file
// walks and semantic coverage all run through it — so putting AGENTS.md and
// CLAUDE.md in the shared list would DELETE them from the indexed doc corpus.
// That is a regression wearing a fix's clothes: the .brainignore stanza above
// deliberately does NOT list AGENTS.md or CLAUDE.md, because maintainers put
// those two on the indexed side on purpose. They stay indexed. Only the dirty
// GATE learns to ignore them.
//
// The justification for the gate is narrow: the gate exists to stop seeding
// uncommitted SOURCE into an index attributed to a commit. Config that Entire
// wrote is not source. Indexing it is fine; blocking the install on it is not.
// A user's own hand-edits to AGENTS.md/CLAUDE.md stop blocking too, which is
// the same call made once.
//
// .entire/* needs no entry: brainIgnore.Ignored already exempts it by segment.
var entireAuthoredGatePaths = []string{
	"AGENTS.md",
	"CLAUDE.md",
	".brainignore",
	// Verbatim from .brainignore, "Local agent/editor runtime state".
	".claude/settings.json",
	".claude/skills/",
	".codex/config.toml",
	".codex/hooks.json",
	".codex/skills/",
	// One entry per agent `entire enable` installs hooks for, read off the
	// cli's own HookConfigRelPath implementations rather than guessed:
	//
	//	.claude/settings.json             agent/claudecode/hooks.go:692  (above)
	//	.codex/hooks.json                 agent/codex/hooks.go:795       (above)
	//	.cursor/hooks.json                agent/cursor/hooks.go:412
	//	.gemini/settings.json             agent/geminicli/hooks.go:550
	//	.github/hooks/entire.json         agent/copilotcli/hooks.go:363
	//	.factory/settings.json            agent/factoryaidroid/hooks.go:508
	//	.pi/extensions/entire/index.ts    agent/pi/hooks.go:174
	//	.opencode/plugins/entire.ts       agent/opencode/hooks.go:158
	//
	// The list covered two of those eight, so six of the agents Entire
	// supports left their user at the refusal above, blocked by the file
	// Entire had just written for them — the same failure this list was
	// created to end, fixed for Claude Code and Codex only. The membership
	// rule is unchanged and is what admits them: Entire's own installer
	// writes it, so it is config rather than source.
	".cursor/hooks.json",
	".gemini/settings.json",
	".github/hooks/entire.json",
	".factory/settings.json",
	".pi/extensions/entire/index.ts",
	".opencode/plugins/entire.ts",
}

// worktreeGateIgnore is loadBrainIgnore plus entireAuthoredGatePaths, for the
// dirty-worktree GATE only. A user's own .brainignore still applies in full:
// the built-ins are ADDED to the user's patterns, never substituted for them.
func worktreeGateIgnore(repoDir string) (brainIgnore, error) {
	ignore, err := loadBrainIgnore(repoDir)
	if err != nil {
		return brainIgnore{}, err
	}
	return ignore.withGatePaths(entireAuthoredGatePaths), nil
}

// withGatePaths returns a COPY carrying extra patterns. It never appends into
// the receiver's slice: brainIgnore values are passed by value but share the
// backing array, so an in-place append could publish gate-only patterns into an
// indexing ignore that happened to have spare capacity — the exact leak this
// whole seam exists to prevent.
func (i brainIgnore) withGatePaths(paths []string) brainIgnore {
	merged := make([]string, 0, len(i.patterns)+len(paths))
	merged = append(merged, i.patterns...)
	merged = append(merged, paths...)
	return brainIgnore{patterns: merged}
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

// FilterWarnings drops the warnings whose subject is a path the ignore rules
// cover. It is the raw filter; production call sites use
// FilterWarningsReported so the count of withheld warnings is not lost.
func (i brainIgnore) FilterWarnings(warnings []semanticWarning) []semanticWarning {
	filtered, _ := i.filterWarningsCounted(warnings)
	return filtered
}

// warningsHiddenCode is the notice FilterWarningsReported appends when the
// ignore rules withheld anything.
const warningsHiddenCode = "warnings_hidden_by_brainignore"

// FilterWarningsReported filters and then says how much it removed.
//
// Silence here is the actual hazard. The filter runs over the provider's own
// warning and partial-failure lists, so an over-broad ignore rule does not just
// hide a path: it turns a degraded index into a report with an empty warning
// list, which every reader downstream renders as "clean". A count costs one
// record and makes the omission visible.
// filterIgnoredWarningLists filters a header's two warning lists together and
// discloses, in the warnings list only, how much each one lost.
//
// Disclosure has to live in exactly one place and be accurate about what it
// covers. Appending a notice to each list separately put "N provider
// warning(s) were withheld" inside PartialFailures, where it is wrong twice
// over: a withheld-count notice is not a partial failure, and the entries it
// was counting there were failures, not warnings. Partial failures being
// hidden is the more serious of the two, so it is named explicitly rather than
// folded into one total.
func (i brainIgnore) filterIgnoredWarningLists(warnings, partialFailures []semanticWarning) ([]semanticWarning, []semanticWarning) {
	keptWarnings, droppedWarnings := i.filterWarningsCounted(warnings)
	keptFailures, droppedFailures := i.filterWarningsCounted(partialFailures)
	if droppedWarnings+droppedFailures == 0 {
		return keptWarnings, keptFailures
	}

	var detail string
	switch {
	case droppedFailures == 0:
		detail = fmt.Sprintf("%d provider warning(s) named ignored paths and were withheld by brainignore rules", droppedWarnings)
	case droppedWarnings == 0:
		detail = fmt.Sprintf("%d partial indexing failure(s) named ignored paths and were withheld by brainignore rules", droppedFailures)
	default:
		detail = fmt.Sprintf("%d provider warning(s) and %d partial indexing failure(s) named ignored paths and were withheld by brainignore rules",
			droppedWarnings, droppedFailures)
	}
	// The notice carries no path token of its own, so re-filtering an already
	// filtered snapshot cannot swallow it.
	return append(keptWarnings, semanticWarning{
		Code:     warningsHiddenCode,
		Severity: "warning",
		Effect:   "this warning list is incomplete",
		Detail:   detail,
	}), keptFailures
}

func (i brainIgnore) FilterWarningsReported(warnings []semanticWarning) []semanticWarning {
	filtered, dropped := i.filterWarningsCounted(warnings)
	if dropped == 0 {
		return filtered
	}
	// The notice carries no path token of its own, so re-filtering an already
	// filtered snapshot cannot swallow it.
	return append(filtered, semanticWarning{
		Code:     warningsHiddenCode,
		Severity: "warning",
		Effect:   "this warning list is incomplete",
		Detail:   fmt.Sprintf("%d provider warning(s) named ignored paths and were withheld by brainignore rules", dropped),
	})
}

func (i brainIgnore) filterWarningsCounted(warnings []semanticWarning) ([]semanticWarning, int) {
	if len(warnings) == 0 {
		return nil, 0
	}
	filtered := make([]semanticWarning, 0, len(warnings))
	dropped := 0
	for _, warning := range warnings {
		if i.Ignored(warning.Path) || i.MentionsIgnoredPath(warning.Detail) || i.MentionsIgnoredPath(warning.Effect) {
			dropped++
			continue
		}
		filtered = append(filtered, warning)
	}
	return filtered, dropped
}

func sanitizeSemanticWarnings(warnings []semanticWarning, repoDir string) []semanticWarning {
	if len(warnings) == 0 {
		return nil
	}
	sanitized := make([]semanticWarning, 0, len(warnings))
	for _, warning := range warnings {
		warning.Path = sanitizeSemanticWarningPath(warning.Path)
		warning.Language = sanitizeSemanticWarningLanguage(warning.Language)
		warning.Effect = sanitizeSemanticWarningText(warning.Effect, repoDir)
		warning.Detail = sanitizeSemanticWarningText(warning.Detail, repoDir)
		sanitized = append(sanitized, warning)
	}
	return sanitized
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

// sanitizeSemanticWarningLanguage clamps schema 1.3's language tag.
//
// Path and the two text fields are sanitized because they are provider-
// supplied and end up in the manifest, which ships in a bundle. Language
// arrived with 1.3 and went straight through -- the same untrusted string
// from the same untrusted process, reaching the same shared artifact, with
// no check at all.
//
// It is clamped rather than redacted because a language tag is an identifier,
// not prose: there is no legitimate value with a space, a slash or a quote in
// it, so anything outside that shape is not a language tag and the honest
// thing to record is that this failure had no language, which is exactly what
// the field already means when a provider omits it.
func sanitizeSemanticWarningLanguage(language string) string {
	const maxLanguageLen = 32
	if language == "" || len(language) > maxLanguageLen {
		return ""
	}
	for i := 0; i < len(language); i++ {
		ch := language[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '+' || ch == '#' || ch == '-' || ch == '_' || ch == '.':
		default:
			return ""
		}
	}
	return language
}

// redactSemanticPathsInText removes the paths from provider text and keeps
// the sentence they were in.
//
// sanitizeSemanticWarningText is all-or-nothing on purpose: a warning body is
// data nobody reads unless something has already gone wrong, so collapsing
// the whole string to <redacted> costs nothing and cannot leak. A refusal
// REASON is the opposite. It is the one sentence that tells the reader what
// happened and what to do, and <redacted> turns a self-describing refusal
// back into the mystery that cost four wrong hypotheses and an instrumented
// bisect -- the exact failure semanticProviderRefusalContext exists to end.
//
// So the paths go and the words stay: the repo's own directory becomes
// <repo>, every other absolute path becomes <path>. What a path means is
// decided by the same scanner the all-or-nothing sanitizer uses, so the two
// cannot disagree about what counts as one.
func redactSemanticPathsInText(text, repoDir string) string {
	if text == "" {
		return ""
	}
	if repoDir != "" {
		text = strings.ReplaceAll(text, repoDir, "<repo>")
		if slashed := filepath.ToSlash(repoDir); slashed != repoDir {
			text = strings.ReplaceAll(text, slashed, "<repo>")
		}
	}
	text = replaceAbsolutePathSpans(text, unixAbsolutePathSpans)
	return replaceAbsolutePathSpans(text, windowsDrivePathSpans)
}

// replaceAbsolutePathSpans swaps every span the finder reports for <path>.
func replaceAbsolutePathSpans(text string, find func(string) [][2]int) string {
	spans := find(text)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	prev := 0
	for _, span := range spans {
		b.WriteString(text[prev:span[0]])
		b.WriteString("<path>")
		prev = span[1]
	}
	b.WriteString(text[prev:])
	return b.String()
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
	return len(windowsDrivePathSpans(text)) > 0
}

// windowsDrivePathSpans reports every drive-letter path in text as a
// half-open [start, end) byte range.
//
// This is the single definition of what a Windows path IS: the all-or-nothing
// sanitizer asks whether there is one, the refusal redactor asks where they
// are, and neither gets to hold its own opinion.
func windowsDrivePathSpans(text string) [][2]int {
	var spans [][2]int
	for i := 0; i+2 < len(text); i++ {
		if !isASCIIAlpha(text[i]) || text[i+1] != ':' {
			continue
		}
		if i > 0 && !isPathBoundary(text[i-1]) {
			continue
		}
		next := text[i+2]
		if next != '\\' && next != '/' && isPathBoundary(next) {
			continue
		}
		end := i + 2
		for end < len(text) && !isPathBoundary(text[end]) {
			end++
		}
		spans = append(spans, [2]int{i, end})
		i = end - 1
	}
	return spans
}

func containsUnixAbsolutePathText(text string) bool {
	return len(unixAbsolutePathSpans(text)) > 0
}

// unixAbsolutePathSpans reports every absolute POSIX path in text as a
// half-open [start, end) byte range. See windowsDrivePathSpans: one
// definition, two questions.
//
// Trailing "." and ":" are left OUTSIDE the span, so a path at the end of a
// sentence keeps its full stop.
func unixAbsolutePathSpans(text string) [][2]int {
	var spans [][2]int
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
		if !looksLikeUnixAbsolutePath(candidate) {
			continue
		}
		spans = append(spans, [2]int{i, i + len(candidate)})
		i = end - 1
	}
	return spans
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

// MentionsIgnoredPath reports whether free-form warning text NAMES a path the
// ignore rules cover. Every candidate is parsed out as a path token and matched
// with Ignored; the text is never substring-matched against a pattern.
//
// Substring matching was the bug. `.brainignore` holds repository-relative
// patterns, and warning Detail at this point in the pipeline still holds the
// provider's absolute paths (sanitizeSemanticWarnings has not run yet), so a
// pattern like `entire-brain` — the checkout's own directory name, and the
// first line of this repository's own .brainignore — is a substring of EVERY
// absolute path under it. Every index warning matched and was discarded, and
// status then reported a clean index over files it had silently failed to
// parse.
func (i brainIgnore) MentionsIgnoredPath(text string) bool {
	for _, token := range brainIgnorePathTokens(text) {
		if i.Ignored(i.relativiseToRepo(token)) {
			return true
		}
	}
	return false
}

// relativiseToRepo turns an absolute path inside the repo into the
// repo-relative form .brainignore patterns are written in. A path outside the
// repo, or a path already relative, is returned unchanged -- an outside path
// genuinely is not covered by this repo's ignore rules.
//
// The symlink-evaluated root is tried as well: on macOS a temp dir reached as
// /var/... resolves to /private/var/..., and provider output can carry either.
func (i brainIgnore) relativiseToRepo(token string) string {
	if i.repoDir == "" || !absolutePathToken(token) {
		return token
	}
	roots := []string{i.repoDir}
	if resolved, err := filepath.EvalSymlinks(i.repoDir); err == nil && resolved != i.repoDir {
		roots = append(roots, resolved)
	}
	for _, root := range roots {
		prefix := filepath.ToSlash(root)
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if rel, ok := cutPathPrefix(token, prefix); ok && rel != "" {
			return rel
		}
	}
	return token
}

// absolutePathToken recognises an absolute path in the slash-normalised form
// brainIgnorePathTokens produces. A Windows path is absolute as "C:/x" and as
// "//host/share/x", neither of which starts with a single slash -- checking for
// "/" alone silently disabled ignore matching for every absolute provider
// warning on Windows, which CI caught and a Unix-only test could not.
func absolutePathToken(token string) bool {
	if strings.HasPrefix(token, "/") {
		return true
	}
	if len(token) >= 3 && token[1] == ':' && token[2] == '/' {
		c := token[0]
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	return false
}

// cutPathPrefix strips a path prefix, folding case where the filesystem does.
// Windows and macOS both commonly compare paths case-insensitively, so a
// provider that reports "C:/Repo/secret/x.go" against a root recorded as
// "C:/repo" must still match.
func cutPathPrefix(token, prefix string) (string, bool) {
	if rel, ok := strings.CutPrefix(token, prefix); ok {
		return rel, true
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		return "", false
	}
	if len(token) < len(prefix) || !strings.EqualFold(token[:len(prefix)], prefix) {
		return "", false
	}
	return token[len(prefix):], true
}

// brainIgnorePathTokens extracts the path-shaped words from prose.
//
// A token is split on the characters that cannot occur inside a path in a
// sentence, stripped of trailing sentence punctuation, and kept only when it
// still looks like a path (it contains a separator or a dot). Trailing-only
// trimming is deliberate: `.env` is a filename, not a sentence fragment.
func brainIgnorePathTokens(text string) []string {
	if text == "" {
		return nil
	}
	fields := strings.FieldsFunc(filepath.ToSlash(text), func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '"', '\'', '`', '(', ')', '[', ']', '{', '}', '<', '>', '|', ',', ';', '=':
			return true
		default:
			return false
		}
	})
	tokens := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimRight(field, ".,;:!?")
		if field == "" || field == "." || field == ".." || field == "/" {
			continue
		}
		if !strings.ContainsAny(field, "/.") {
			continue
		}
		tokens = append(tokens, field)
		// A compiler or parser error names its location as
		// "<path>:<line>:<col>", and TrimRight above only removes a TRAILING
		// colon -- so ".env:3:5" survives as one token and matches no
		// exact-file pattern: Ignored() compares against ".env", and
		// ".env:3:5" is not equal to it, does not contain "/.env" and does not
		// end in ".env".
		//
		// That is the shape the warnings this filter exists to withhold
		// actually take, and such an error routinely quotes the offending
		// SOURCE LINE -- so a parse failure in an ignored .env, .pem, .key or
		// a user's own secrets pattern was written into the snapshot warnings
		// and printed by `brain status`. Measured before this fix:
		// "/repo/.env" was withheld and "/repo/.env:3:5" was not.
		//
		// The bare path is offered as an ADDITIONAL token rather than
		// replacing the original, so a file whose name genuinely ends in
		// digits after a colon still matches its own pattern.
		if trimmed := trimPathPositionSuffix(field); trimmed != field && trimmed != "" {
			tokens = append(tokens, trimmed)
		}
	}
	return tokens
}

// trimPathPositionSuffix removes trailing ":<digits>" groups, the
// "<path>:<line>:<col>" form every compiler, linter and parser uses.
//
// Only digits are stripped, and only from the end: a path containing a colon
// followed by anything else is left alone, because that colon may be part of
// the name.
func trimPathPositionSuffix(token string) string {
	for {
		idx := strings.LastIndexByte(token, ':')
		// A position suffix needs a non-empty path before its colon. Keep
		// leading-colon tokens intact rather than reducing them to no path.
		if idx <= 0 || idx == len(token)-1 {
			return token
		}
		suffix := token[idx+1:]
		allDigits := true
		for _, r := range suffix {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if !allDigits {
			return token
		}
		token = token[:idx]
	}
}

// worktreeDirty answers the GATE question — "is there uncommitted work here
// that a HEAD-attributed index must not absorb?" — so it reads the gate ignore,
// not the indexing one.
//
// Its other three callers were checked rather than inherited by accident, and
// all three want the same answer:
//
//   - verifySemanticWorktreeStable compares this result against a dirtyBefore
//     captured by the same gate. If the two used different ignore sets, an
//     install that merely rewrote .claude/settings.json mid-index would report
//     "worktree dirtiness changed during semantic indexing" — a failure invented
//     by the mismatch itself. Consistency here is not optional.
//   - semanticStaleReport feeds the `worktree` axis a reader sees in `status`.
//     It must not print "dirty" about a tree the gate calls clean, or the
//     remedy mapping in status_render.go names a command the state does not
//     need.
//   - semanticRefreshNeeded (refresh.go) turns dirty into "rebuild". Left on the
//     indexing ignore it would queue a rebuild for an Entire-authored config
//     edit and then have the rebuild refuse it — which is the reported bug,
//     one layer up.
func worktreeDirty(ctx context.Context, runner CommandRunner, repoDir string) (bool, error) {
	ignore, err := worktreeGateIgnore(repoDir)
	if err != nil {
		return false, err
	}
	return worktreeDirtyWithIgnore(ctx, runner, repoDir, ignore)
}

func worktreeDirtyWithIgnore(ctx context.Context, runner CommandRunner, repoDir string, ignore brainIgnore) (bool, error) {
	dirty, _, err := worktreeDirtyDetail(ctx, runner, repoDir, ignore)
	return dirty, err
}

// worktreeDirtyDetail answers "is it dirty" and "which paths" from ONE git
// status. The refusal has to name the offending files, and re-running status to
// find out would let the answer change between the two calls.
func worktreeDirtyDetail(ctx context.Context, runner CommandRunner, repoDir string, ignore brainIgnore) (bool, []string, error) {
	stdout, err := gitStatusPorcelainAll(ctx, runner, repoDir)
	if err != nil {
		return false, nil, err
	}
	filtered := filterWorktreeStatus(stdout, ignore)
	if strings.TrimSpace(string(filtered)) == "" {
		return false, nil, nil
	}
	return true, worktreeStatusPathList(filtered), nil
}

// worktreeStatusPathList flattens already-filtered porcelain lines into the
// distinct paths behind them, in status order.
func worktreeStatusPathList(status []byte) []string {
	var paths []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, path := range worktreeStatusPaths(line) {
			if path == "" {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
	}
	return paths
}

// dirtyWorktreeRefusal builds the message both gates raise. It used to name a
// flag and nothing else — not which files were in the way, and not what to do
// instead — so a reader could clear one offending file, hit the second gate,
// and still be told only "--worktree".
//
// The dirtyWorktreeErrorCode+":" prefix is a MACHINE CONTRACT and is preserved
// exactly: setupIsDirtyWorktree (setup.go) matches on it to attach the hint,
// and watch's recovery path keys on it to continue instead of dying. The prose
// after the prefix is free to change; the prefix is not.

// dirtyWorktreeRefusalPathLimit bounds the list in a refusal. The point is to
// show the reader WHICH files blocked them, not to reproduce `git status`: a
// tree with 250 dirty files is exactly the case this message exists for, and
// pasting 250 paths into an error is how it stops being read.
const dirtyWorktreeRefusalPathLimit = 5

// worktreeDirtyPaths lists the paths that make the worktree dirty, with
// .brainignore already applied -- the same filter the gate itself uses, so the
// refusal names only files that actually blocked it and never one the reader
// has already excluded.
func worktreeDirtyPaths(ctx context.Context, runner CommandRunner, repoDir string, ignore brainIgnore) ([]string, error) {
	stdout, err := gitStatusPorcelainAll(ctx, runner, repoDir)
	if err != nil {
		return nil, err
	}
	var paths []string
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(filterWorktreeStatus(stdout, ignore)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, path := range worktreeStatusPaths(line) {
			if path == "" {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// dirtyWorktreeRefusal is the one wording of both dirty-tree refusals.
//
// They used to offer a single way out -- "without --worktree" -- and --worktree
// is the wrong hatch for the reader who hits this most often: it INDEXES the
// uncommitted files rather than getting them out of the way. .brainignore is
// the hatch that fits, the gate already loads and honours it (loadBrainIgnore,
// worktreeDirtyWithIgnore), and it appeared in zero error strings and zero
// README lines. A user with ~250 dirty research files was stuck on exactly
// this, with the answer sitting unmentioned in the code that refused them.
//
// The blocking paths are named for the same reason: "the worktree is dirty" is
// not actionable when the reader cannot see which files are meant.
func dirtyWorktreeRefusal(verb string, paths []string) error {
	// Both halves of this message were earned separately and both are kept:
	// naming .brainignore is the only escape hatch that was documented nowhere,
	// and the COUNT tells a reader whether they are looking at two stray files
	// or two hundred -- which decides which of the three ways out they want.
	message := fmt.Sprintf(
		"%s: refusing to %s uncommitted content without --worktree; commit or stash them, list them in .brainignore to keep them out of the brain, or pass --worktree to %s them as they are",
		dirtyWorktreeErrorCode, verb, verb)
	if len(paths) > 0 {
		shown := paths
		extra := 0
		if len(shown) > dirtyWorktreeRefusalPathLimit {
			extra = len(shown) - dirtyWorktreeRefusalPathLimit
			shown = shown[:dirtyWorktreeRefusalPathLimit]
		}
		// Singular on purpose: "1 uncommitted paths" is how a reader learns the
		// tool is not paying attention.
		noun := "paths"
		if len(paths) == 1 {
			noun = "path"
		}
		message += fmt.Sprintf("; %d uncommitted %s: %s", len(paths), noun, strings.Join(shown, ", "))
		if extra > 0 {
			message += fmt.Sprintf(" (and %d more)", extra)
		}
	}
	return errors.New(message)
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
	stdout, _, err := runGitWithScratchIndex(ctx, runner, repoDir, args...)
	if err == nil {
		return stdout, nil
	}
	args = []string{"diff"}
	if cached {
		args = append(args, "--cached")
	}
	args = append(args, "--binary", "HEAD")
	stdout, _, fallbackErr := runGitWithScratchIndex(ctx, runner, repoDir, args...)
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
	// Remedy is the next step, when one exists. Additive, so existing readers
	// are unaffected.
	//
	// The dirty-worktree remedy used to live inside Detail. Moving it out
	// stopped the short status printing it twice, and left the JSON carrying
	// a cause with no way out: the text renderer re-attached it, and an agent
	// or a script reading `status --json` -- which is how the MCP surface and
	// every automated consumer read this -- got only the diagnosis. A field
	// is the fix rather than putting it back in Detail, because a renderer
	// that wants the cause without the remedy can still have it, and the two
	// surfaces now read the same value instead of one of them re-deriving it.
	Remedy string `json:"remedy,omitempty"`
}

type brainBlindSpot struct {
	Path   string `json:"path"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

func brainBlindSpotsForRepo(ctx context.Context, opts Options, target string) ([]brainBlindSpot, error) {
	repoDir, local, err := clibase.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
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
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
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
			Detail: fmt.Sprintf("%d partial failures without a recorded path; run `"+brainInvocation(os.LookupEnv, "refresh")+"` to locate them", unknownPaths),
		})
	}
	return spots, nil
}

func semanticStaleReport(ctx context.Context, opts Options, target string) (staleReport, error) {
	return semanticFreshnessReport(ctx, opts, target, true)
}

// Status checks availability and freshness; doctor and retrieval retain content validation.
func semanticFreshnessReport(ctx context.Context, opts Options, target string, audit bool) (staleReport, error) {
	repoDir, local, err := clibase.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return staleReport{}, err
	}
	if !local {
		return staleReport{}, errors.New("status requires a local repository")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return staleReport{}, err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return staleReport{}, err
	}
	if err := checkLegacyBrainLayout(storage.BrainDir, manifest); err != nil {
		return staleReport{Severity: "unsafe", Axes: map[string]staleAxis{"brain_layout": {State: "unsafe", Detail: err.Error()}}}, nil
	}
	d, err := fetchGraphStore(ctx, opts, repoDir, "", graphStoreProfile(manifest))
	if err != nil {
		return staleReport{Severity: "degraded", Axes: map[string]staleAxis{"graph": {State: "missing", Detail: err.Error()}}}, nil
	}
	axes := map[string]staleAxis{"graph": {State: "ok", Current: d.Commit, Indexed: d.Commit, Detail: "Graph owns the committed-tree store"}}
	var warnings []semanticWarning
	// fetchGraphStore already refused a major mismatch; a newer supported-major
	// minor reaches here and degrades the axis rather than failing the report,
	// matching the warn-not-refuse half of ADR 0001 rule 3.
	if skew := semanticSchemaMinorSkewWarning(d.Header.SchemaVersion); skew != nil {
		warnings = append(warnings, *skew)
		axes["graph"] = staleAxis{State: "degraded", Current: d.Commit, Indexed: d.Commit, Detail: skew.Detail}
	}
	if dirty, err := worktreeDirty(ctx, opts.Runner, repoDir); err != nil {
		return staleReport{}, err
	} else if dirty {
		axes["worktree"] = staleAxis{State: dirtyUnindexedAxisState, Detail: "Graph store describes committed HEAD"}
	}
	return staleReport{Severity: aggregateStaleSeverity(axes), Axes: axes, Warnings: warnings}, nil
}

const (
	// semanticParseErrorCode marks files where the tree-sitter parse produced error
	// nodes but symbols were still extracted (error-tolerant parse). A small number
	// of these is expected for any real codebase — newer language syntax the bundled
	// grammar version predates — and does not make the indexed facts unsafe to use.
	semanticParseErrorCode = "E_PARSE_ERROR"

	// These codes mirror entire-graph's intentional-skip contract. The provider
	// emits a file record and preserves the partial failure for visibility, but
	// excludes the record from its completeness-failure count because parsing was
	// deliberately skipped by policy rather than attempted and failed.
	semanticFileTooLargeCode = "E_FILE_TOO_LARGE"
	semanticMinifiedCode     = "E_MINIFIED"
)

func semanticIntentionalSkipCode(code string) bool {
	switch code {
	case semanticFileTooLargeCode, semanticMinifiedCode:
		return true
	default:
		return false
	}
}

// semanticParseErrorTolerance is the fraction of indexed files that may carry
// only benign parse errors before the brain is reported as degraded. Below this
// threshold the completeness axis stays ok; the failures remain recorded in the
// semantic source so they can still be inspected and acted on. Above it, the
// volume is high enough to suspect a grammar/provider problem worth surfacing.
const semanticParseErrorTolerance = 0.10

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
	limit   int
	offset  int
	json    bool
	details bool
}

type semanticContextOptions struct {
	limit          int
	offset         int
	includeContent bool
	json           bool
	details        bool
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
	limit   int
	depth   int
	json    bool
	details bool
}

type semanticImpactResult struct {
	Roots     []semanticRecord `json:"roots"`
	Symbols   []semanticRecord `json:"symbols"`
	Relations []semanticRecord `json:"relations"`
}

type semanticChangesOptions struct {
	limit   int
	json    bool
	persist bool
}

type semanticChangesReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Clean is true when there are no *semantically-relevant* changed files since
	// the indexed HEAD — i.e. after changedSemanticFiles applies the built-in
	// exclusions and `.brainignore`. It lets a consumer distinguish "nothing to
	// map" from a broken command without inferring it from empty arrays; it is
	// NOT a `git status`-clean signal (changes confined to ignored paths still
	// report clean=true).
	Clean   bool                   `json:"clean"`
	Files   []string               `json:"files"`
	Ranges  []semanticChangedRange `json:"ranges,omitempty"`
	Symbols []semanticRecord       `json:"symbols"`
	// Facts are durable facts whose locus names a changed file/symbol — "what the
	// brain already knows about the code you're touching". Omitted when none.
	Facts []factRecord `json:"facts,omitempty"`
}

type semanticChangedRange struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
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
	limit   int
	json    bool
	details bool
}

type semanticTestSuggestion struct {
	Symbol semanticRecord `json:"symbol"`
	Reason string         `json:"reason"`
	// weak is a bounded reservoir-only fallback. It remains available through
	// freshness filtering, then is omitted from the emitted packet.
	weak bool
	// fallback inherits exact/graph evidence from a task-aligned root. It is
	// emitted only when no candidate-local strong suggestion survives freshness.
	fallback bool
}

type semanticTestsResult struct {
	Roots       []semanticRecord         `json:"roots"`
	Suggestions []semanticTestSuggestion `json:"suggestions"`
	// synthesisSuggestions preserves the former bounded file-evidence stream
	// for likely-file/action synthesis while Suggestions carries the ranked,
	// higher-precision agent-facing stream. It is never serialized.
	synthesisSuggestions []semanticTestSuggestion
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
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
	if err != nil {
		return err
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return semanticIndexMissingError()
	}
	// Kept for the empty-result inventory below: explaining WHICH empty this is
	// needs the store that was actually searched.
	storePath, err := validateSemanticDeclaredStore(storage.BrainDir, manifest.Sources.Semantic)
	if err != nil {
		return err
	}
	storePathForInventory := storePath
	results, err := findSemanticSymbolsInSQLite(storePath, query, queryOpts.limit, queryOpts.offset)
	if err != nil {
		return err
	}
	if queryOpts.json {
		outputResults := any(nonNil(results))
		if !queryOpts.details {
			outputResults = compactSemanticRecords(results)
		}
		data, err := json.MarshalIndent(struct {
			Freshness         staleReport       `json:"freshness"`
			CompletenessLevel string            `json:"completeness_level,omitempty"`
			Trust             string            `json:"trust,omitempty"`
			LanguageTiers     map[string]string `json:"language_tiers,omitempty"`
			Pagination        semanticPage      `json:"pagination"`
			Results           any               `json:"results"`
		}{Freshness: freshness, CompletenessLevel: manifest.Sources.Semantic.CompletenessLevel, Trust: manifest.Sources.Semantic.Trust, LanguageTiers: manifest.Sources.Semantic.LanguageTiers, Pagination: semanticPage{Limit: queryOpts.limit, Offset: queryOpts.offset, Count: len(results)}, Results: outputResults}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "code index freshness: %s\n", freshness.Severity)
	}
	if len(results) == 0 {
		printSemanticNoSymbolMatch(cmd, storePathForInventory, query)
		return nil
	}
	for _, result := range results {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s:%d-%d\n", result.Kind, displaySymbolName(result), result.FilePath, result.StartLine, result.EndLine)
	}
	return nil
}

// printSemanticNoMatch writes the text-mode empty-state line for a semantic
// read that matched nothing.
//
// These commands used to print zero bytes and exit 0 on a no-match query, which
// is the one outcome a caller cannot act on: a silent success is equally
// consistent with "the index has no such symbol", "the index was never built"
// and "this command is broken". Their --json siblings always said so with
// "results": [], and `inspect boundaries` and `inspect changes` already say so
// in prose; this is that same idiom, extended with the query so an empty index
// stays distinguishable from an unlucky search term.
func printSemanticNoMatch(cmd *cobra.Command, kind, query string) {
	fmt.Fprintf(cmd.OutOrStdout(), "no %s found in the code index for %q\n", kind, query)
}

// printSemanticNoSymbolMatch adds the one thing the bare message cannot say:
// WHICH of the indistinguishable cases this is.
//
// The comment above names three — no such symbol, index never built, command
// broken — and there is a fourth it misses: the index holds no symbols OF THAT
// KIND. Go package-level consts and vars are not extracted at all, so
// `search-graph defaultDistillTimeout` returns the same empty line as a typo
// does, and the honest reading ("constants are not in here") is unavailable.
//
// This cost real time: given that line for a symbol I could see in the source,
// I concluded the search was broken and spent an hour on a false root cause.
// An inventory of what the index DOES hold answers it in one line.
func printSemanticNoSymbolMatch(cmd *cobra.Command, storePath, query string) {
	printSemanticNoMatch(cmd, "symbols", query)
	// Snapshot-backed searches have no SQLite inventory to inspect. A missing
	// inventory does not imply that the searched snapshot has no symbols.
	if storePath == "" {
		return
	}
	total, distinctKinds, kinds := semanticIndexKindInventory(storePath)
	if total == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "note: the code index holds no symbols at all — run `"+brainInvocation(os.LookupEnv, semanticIndexVerb)+"`")
		return
	}
	shown := strings.Join(kinds, ", ")
	if distinctKinds > len(kinds) {
		// Say that the list is partial. Six of twenty kinds presented as the
		// whole set is the same misdirection as an under-reported total.
		shown = fmt.Sprintf("%s (%d of %d kinds)", shown, len(kinds), distinctKinds)
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"note: the index holds %d symbols of kinds %s; Go consts and vars are not extracted, so a constant name finds nothing here\n",
		total, shown)
}

// semanticIndexKindInventory reports what the store actually contains. Every
// failure is a zero inventory rather than an error: this runs only to explain
// an empty result, and must never turn one into a command failure.
func semanticIndexKindInventory(storePath string) (total int, distinctKinds int, kinds []string) {
	if storePath == "" {
		return 0, 0, nil
	}
	db, err := openSemanticReadDB(storePath)
	if err != nil {
		return 0, 0, nil
	}
	defer db.Close()
	// The TOTAL comes from its own count, not from summing the displayed rows.
	//
	// Summing the LIMIT 6 groups is a partial sum: with more than six distinct
	// kinds it under-reports, and this note exists precisely to give an agent
	// an accurate picture after an empty result. A too-low total risks the same
	// false root cause the note was written to prevent -- which is the mistake
	// that prompted writing it.
	if err := db.QueryRow(`SELECT count(*) FROM symbols`).Scan(&total); err != nil {
		return 0, 0, nil
	}

	// And the KIND COUNT is its own query too, so a truncated list can say so
	// rather than presenting six of twenty as if that were all of them.
	if err := db.QueryRow(`SELECT count(DISTINCT kind) FROM symbols`).Scan(&distinctKinds); err != nil {
		return total, 0, nil
	}

	rows, err := db.Query(`SELECT kind, count(*) c FROM symbols GROUP BY kind ORDER BY c DESC LIMIT 6`)
	if err != nil {
		return total, distinctKinds, nil
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var c int
		if err := rows.Scan(&k, &c); err != nil {
			return total, distinctKinds, nil
		}
		kinds = append(kinds, k)
	}
	if rows.Err() != nil {
		return total, distinctKinds, nil
	}
	return total, distinctKinds, kinds
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
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return semanticIndexMissingError()
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	if contextOpts.includeContent && freshness.Severity != "ok" {
		return fmt.Errorf("code context content requires fresh index data: %s", freshness.Severity)
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
		outputContext := any(result)
		if !contextOpts.details {
			outputContext = struct {
				Symbols   []compactSemanticRecord `json:"symbols"`
				Relations []compactSemanticRecord `json:"relations"`
				Neighbors []compactSemanticRecord `json:"neighbors,omitempty"`
				Content   []semanticContent       `json:"content,omitempty"`
			}{
				Symbols: compactSemanticRecords(result.Symbols), Relations: compactSemanticRecords(result.Relations),
				Neighbors: compactSemanticRecords(result.Neighbors), Content: result.Content,
			}
		}
		data, err := json.MarshalIndent(struct {
			Freshness         staleReport       `json:"freshness"`
			CompletenessLevel string            `json:"completeness_level,omitempty"`
			Trust             string            `json:"trust,omitempty"`
			LanguageTiers     map[string]string `json:"language_tiers,omitempty"`
			Pagination        semanticPage      `json:"pagination"`
			Context           any               `json:"context"`
		}{Freshness: freshness, CompletenessLevel: manifest.Sources.Semantic.CompletenessLevel, Trust: manifest.Sources.Semantic.Trust, LanguageTiers: manifest.Sources.Semantic.LanguageTiers, Pagination: semanticPage{Limit: contextOpts.limit, Offset: contextOpts.offset, Count: len(symbols)}, Context: outputContext}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "code index freshness: %s\n", freshness.Severity)
	}
	if len(result.Symbols) == 0 && len(result.Relations) == 0 && len(result.Neighbors) == 0 && len(result.Content) == 0 {
		printSemanticNoMatch(cmd, "context", query)
		return nil
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
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return semanticIndexMissingError()
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
		impact := any(result)
		if !impactOpts.details {
			impact = struct {
				Roots     []compactSemanticRecord `json:"roots"`
				Symbols   []compactSemanticRecord `json:"symbols"`
				Relations []compactSemanticRecord `json:"relations"`
			}{
				Roots:     compactSemanticRecords(result.Roots),
				Symbols:   compactSemanticRecords(result.Symbols),
				Relations: compactSemanticRecords(result.Relations),
			}
		}
		data, err := json.MarshalIndent(struct {
			Freshness staleReport `json:"freshness"`
			Impact    any         `json:"impact"`
		}{Freshness: freshness, Impact: impact}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "code index freshness: %s\n", freshness.Severity)
	}
	if len(symbols) == 0 && len(relations) == 0 {
		printSemanticNoMatch(cmd, "impact", query)
		return nil
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
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return semanticIndexMissingError()
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	files, err := changedSemanticFiles(ctx, opts.Runner, repoDir)
	if err != nil {
		return err
	}
	ranges, err := changedSemanticRanges(ctx, opts.Runner, repoDir, manifest.Sources.Semantic.WorktreeMode == "worktree")
	if err != nil {
		return err
	}
	ranges = semanticChangedRangesForFiles(ranges, files)
	symbols, err := semanticSymbolsForChangedRanges(storage.BrainDir, manifest.Sources.Semantic, files, ranges, changesOpts.limit)
	if err != nil {
		return err
	}
	if files == nil {
		files = []string{}
	}
	report := semanticChangesReport{GeneratedAt: opts.Now().UTC(), Clean: len(files) == 0, Files: files, Ranges: ranges, Symbols: nonNil(symbols)}
	// Surface durable facts about the code being touched. Best-effort: a missing
	// facts source or a branch lookup failure simply yields no facts, never an
	// error on the changes command. Skipped on a clean tree — no changed files
	// means there is nothing to match a fact's locus against.
	if !report.Clean {
		if branch, branchErr := clibase.GitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); branchErr == nil && strings.TrimSpace(branch) != "" {
			if facts, factsErr := loadFacts(storage.BrainDir, strings.TrimSpace(branch)); factsErr == nil {
				report.Facts = factsRelevantToChange(files, symbols, facts, changesOpts.limit)
			}
		}
	}
	if changesOpts.persist {
		unlock, err := acquireSemanticIndexLock(storage.BrainDir)
		if err != nil {
			return err
		}
		writeErr := writeSemanticChangesReport(storage.BrainDir, report)
		unlock()
		if writeErr != nil {
			return writeErr
		}
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
		fmt.Fprintf(cmd.OutOrStdout(), "code index freshness: %s\n", freshness.Severity)
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
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return semanticIndexMissingError()
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
		fmt.Fprintf(cmd.OutOrStdout(), "code index freshness: %s\n", freshness.Severity)
	}
	if len(result.Boundaries) == 0 && len(result.Relations) == 0 && len(result.Handlers) == 0 {
		// Distinguish "the index has no such boundaries" from a silent failure:
		// these surfaces only exist if the semantic provider emitted them.
		fmt.Fprintf(cmd.OutOrStdout(), "no %s found in the code index\n", spec.Name)
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
	manifest, err := loadGraphManifest(ctx, opts, repoDir, storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return semanticIndexMissingError()
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
		tests := any(result)
		if !testsOpts.details {
			suggestions := make([]compactSemanticTestSuggestion, len(result.Suggestions))
			for i, suggestion := range result.Suggestions {
				suggestions[i] = compactSemanticTestSuggestion{
					Symbol: compactSemanticRecordFrom(suggestion.Symbol),
					Reason: suggestion.Reason,
				}
			}
			tests = struct {
				Roots       []compactSemanticRecord         `json:"roots"`
				Suggestions []compactSemanticTestSuggestion `json:"suggestions"`
			}{
				Roots:       compactSemanticRecords(result.Roots),
				Suggestions: suggestions,
			}
		}
		data, err := json.MarshalIndent(struct {
			Freshness staleReport `json:"freshness"`
			Tests     any         `json:"tests"`
		}{Freshness: freshness, Tests: tests}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	if freshness.Severity != "ok" {
		fmt.Fprintf(cmd.OutOrStdout(), "code index freshness: %s\n", freshness.Severity)
	}
	if len(result.Suggestions) == 0 {
		printSemanticNoMatch(cmd, "tests", query)
		return nil
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
	db, err := openSemanticReadDB(storePath)
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

func semanticSymbolsForFiles(brainDir string, source *semanticSourceManifest, files []string, limit int) ([]semanticRecord, error) {
	storePath, err := validateSemanticDeclaredStore(brainDir, source)
	if err != nil {
		return nil, err
	}
	return findSemanticSymbolsForFilesSQLite(storePath, files, limit)
}

// semanticSymbolsForChangedRanges narrows a file-level candidate set to the
// symbols whose indexed line spans overlap actual diff hunks. Files without a
// parseable hunk deliberately retain whole-file behavior: pure renames,
// binaries, and untracked files can still be semantically meaningful even
// though `git diff --unified=0` has no usable line range for them.
func semanticSymbolsForChangedRanges(brainDir string, source *semanticSourceManifest, files []string, ranges []semanticChangedRange, limit int) ([]semanticRecord, error) {
	if len(files) == 0 {
		return nil, nil
	}
	candidateLimit := source.Symbols
	if candidateLimit < limit {
		candidateLimit = limit
	}
	if candidateLimit <= 0 {
		candidateLimit = int(^uint(0) >> 1)
	}
	candidates, err := semanticSymbolsForFiles(brainDir, source, files, candidateLimit)
	if err != nil {
		return nil, err
	}
	byFile := make(map[string][]semanticChangedRange, len(files))
	for _, file := range files {
		byFile[file] = nil
	}
	for _, changed := range ranges {
		if _, ok := byFile[changed.File]; ok {
			byFile[changed.File] = append(byFile[changed.File], changed)
		}
	}
	matched := make([]semanticRecord, 0, min(limit, len(candidates)))
	for _, candidate := range candidates {
		fileRanges, ok := byFile[candidate.FilePath]
		if !ok {
			continue
		}
		include := len(fileRanges) == 0
		for _, changed := range fileRanges {
			if candidate.StartLine <= changed.EndLine && candidate.EndLine >= changed.StartLine {
				include = true
				break
			}
		}
		if !include {
			continue
		}
		matched = append(matched, candidate)
		if len(matched) >= limit {
			break
		}
	}
	return matched, nil
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
	return semanticTestsResult{
		Roots:       roots,
		Suggestions: visibleSemanticTestSuggestions(rankSemanticTestSuggestions(symbolsByID, roots, relations, query, limit), limit),
	}, nil
}

func semanticTestFactsReservoir(brainDir string, source *semanticSourceManifest, query string, limit int) (semanticTestsResult, error) {
	roots, _, relations, symbolsByID, err := semanticGraphFacts(brainDir, source, query, 1, limit)
	if err != nil {
		return semanticTestsResult{}, err
	}
	return semanticTestsResult{
		Roots:                roots,
		Suggestions:          rankSemanticTestSuggestions(symbolsByID, roots, relations, query, limit),
		synthesisSuggestions: nonNil(legacySemanticTestSuggestions(symbolsByID, roots, relations, limit)),
	}, nil
}

func semanticGraphFacts(brainDir string, source *semanticSourceManifest, query string, depth, limit int) ([]semanticRecord, []semanticRecord, []semanticRecord, map[string]semanticRecord, error) {
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

func loadSemanticBoundaryRecords(brainDir string, source *semanticSourceManifest, relationTypes []string) (map[string]semanticRecord, []semanticRecord, error) {
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

func validateSemanticDeclaredStore(brainDir string, source *semanticSourceManifest) (string, error) {
	if source.graphStore != nil {
		return source.StorePath, nil
	}
	return "", fmt.Errorf("%w; run `%s`", errSemanticStoreRetired, brainInvocation(os.LookupEnv, semanticIndexVerb))
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
const (
	semanticQueryTokenCandidateLimit = 32
	semanticQueryTokenSearchLimit    = 8
	semanticQueryLeadingTokenReserve = 4
)

func semanticQueryTokens(query string) []string {
	seen := map[string]struct{}{}
	var tokens []string
	add := func(token string) bool {
		if len(token) < 3 || historyQueryStopword(token) {
			return true
		}
		if _, ok := seen[token]; ok {
			return true
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
		return len(tokens) < semanticQueryTokenCandidateLimit
	}
	for _, raw := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		raw = strings.Trim(raw, "_")
		if !add(raw) {
			break
		}
		for _, variant := range semanticQueryMorphologyVariants(raw) {
			if !add(variant) {
				return tokens
			}
		}
	}
	return tokens
}

// semanticQueryMorphologyVariants bridges common task nouns and past-tense
// descriptions to identifier verbs without turning semantic lookup into an
// unrestricted stemmer. Code commonly uses Normalize/Validate/Authenticate
// while issue text says normalization/validation/authentication; these exact
// derivations preserve meaning and give IDF ranking the identifier-shaped term.
func semanticQueryMorphologyVariants(token string) []string {
	switch token {
	case "environment", "environments", "environmental":
		// Environment is almost universally shortened to env in identifiers and
		// filenames (pluginEnv, cmd.Env, plugin_env.go). Preserve the full noun
		// and add its conventional code form so natural-language tasks can find
		// those symbols without requiring callers to guess the abbreviation.
		return []string{"env"}
	}
	for _, rule := range []struct {
		suffix      string
		replacement string
	}{
		{suffix: "ization", replacement: "ize"},
		{suffix: "isation", replacement: "ise"},
		{suffix: "ized", replacement: "ize"},
		{suffix: "ised", replacement: "ise"},
		{suffix: "ation", replacement: "ate"},
	} {
		if strings.HasSuffix(token, rule.suffix) && len(token) > len(rule.suffix)+2 {
			return []string{strings.TrimSuffix(token, rule.suffix) + rule.replacement}
		}
	}
	return nil
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
	type weightedToken struct {
		value  string
		weight int
		order  int
	}
	weighted := make([]weightedToken, 0, len(tokens))
	for i, token := range tokens {
		var df int
		if err := db.QueryRow(`SELECT count(*) FROM symbol_fts WHERE instr(lower(text), ?) > 0`, token).Scan(&df); err != nil {
			return nil, err
		}
		if df == 0 {
			continue
		}
		weighted = append(weighted, weightedToken{value: token, weight: tokenIDFWeight(total, df), order: i})
	}
	// Keep the task's leading identifiers before filling the fixed search budget
	// with rare terms. Pure IDF selection lets incidental rare words at the end
	// of a long agent prompt displace its task ID and primary domain nouns.
	selected := append([]weightedToken(nil), weighted[:min(semanticQueryLeadingTokenReserve, len(weighted))]...)
	sort.SliceStable(weighted, func(i, j int) bool {
		if weighted[i].weight != weighted[j].weight {
			return weighted[i].weight > weighted[j].weight
		}
		return weighted[i].order < weighted[j].order
	})
	for _, candidate := range weighted {
		if len(selected) >= semanticQueryTokenSearchLimit {
			break
		}
		if slices.ContainsFunc(selected, func(existing weightedToken) bool { return existing.order == candidate.order }) {
			continue
		}
		selected = append(selected, candidate)
	}
	if len(selected) == 0 {
		return nil, nil
	}
	tokens = tokens[:0]
	weights := make([]int, 0, len(selected))
	for _, candidate := range selected {
		tokens = append(tokens, candidate.value)
		weights = append(weights, candidate.weight)
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
	if query == "" {
		return false
	}
	// A slash alone is not enough: natural-language concepts, URLs, and prose
	// commonly contain one. Only explicit relative/absolute paths or values with
	// a recognized source extension suppress semantic token fallback.
	if strings.Contains(query, "://") || strings.ContainsAny(query, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(query, "./") || strings.HasPrefix(query, "../") ||
		strings.HasPrefix(query, "/") || strings.HasPrefix(query, `\\`) {
		return true
	}
	switch strings.ToLower(filepath.Ext(query)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".sql", ".java", ".rb", ".mjs", ".cjs":
		return true
	}
	return false
}

func findSemanticSymbolsInSQLite(storePath, query string, limit, offset int) ([]semanticRecord, error) {
	db, err := openSemanticReadDB(storePath)
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

func findSemanticRelationsForSymbolsInSQLite(storePath string, symbols []semanticRecord, limit int) ([]semanticRecord, error) {
	if len(symbols) == 0 {
		return nil, nil
	}
	db, err := openSemanticReadDB(storePath)
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
	db, err := openSemanticReadDB(storePath)
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
	db, err := openSemanticReadDB(storePath)
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
	db, err := openSemanticReadDB(storePath)
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

func findSemanticSymbolsForFilesSQLite(storePath string, files []string, limit int) ([]semanticRecord, error) {
	if len(files) == 0 {
		return nil, nil
	}
	db, err := openSemanticReadDB(storePath)
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
	diffOutput, _, err := runGitWithScratchIndex(ctx, runner, repoDir, "diff", "--name-status", "-M", "-C", "HEAD")
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

func changedSemanticRanges(ctx context.Context, runner CommandRunner, repoDir string, indexedWorktree bool) ([]semanticChangedRange, error) {
	diffOutput, _, err := runGitWithScratchIndex(ctx, runner, repoDir, "diff", "--unified=0", "--no-ext-diff", "--no-color", "--no-prefix", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("inspect changed lines: %w", err)
	}
	return parseSemanticChangedRanges(string(diffOutput), indexedWorktree), nil
}

func parseSemanticChangedRanges(diff string, indexedWorktree bool) []semanticChangedRange {
	var oldFile, newFile string
	var ranges []semanticChangedRange
	readingFileHeaders := true
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			oldFile, newFile = "", ""
			readingFileHeaders = true
		case readingFileHeaders && strings.HasPrefix(line, "--- "):
			oldFile = semanticDiffPath(strings.TrimPrefix(line, "--- "))
		case readingFileHeaders && strings.HasPrefix(line, "+++ "):
			newFile = semanticDiffPath(strings.TrimPrefix(line, "+++ "))
			readingFileHeaders = false
		case strings.HasPrefix(line, "@@ "):
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			oldStart, oldCount, oldOK := parseSemanticDiffSpan(fields[1], '-')
			newStart, newCount, newOK := parseSemanticDiffSpan(fields[2], '+')
			if !oldOK || !newOK {
				continue
			}
			file, start, count := oldFile, oldStart, oldCount
			if indexedWorktree {
				file, start, count = newFile, newStart, newCount
			}
			if file == "" {
				continue
			}
			// A zero-count side is an insertion/deletion boundary. Anchor it to
			// the nearest indexed line so an enclosing symbol can still match.
			if start < 1 {
				start = 1
			}
			end := start
			if count > 0 {
				end = start + count - 1
			}
			ranges = append(ranges, semanticChangedRange{File: file, StartLine: start, EndLine: end})
		}
	}
	return ranges
}

func semanticChangedRangesForFiles(ranges []semanticChangedRange, files []string) []semanticChangedRange {
	allowed := make(map[string]struct{}, len(files))
	for _, file := range files {
		allowed[file] = struct{}{}
	}
	filtered := make([]semanticChangedRange, 0, len(ranges))
	for _, changed := range ranges {
		if _, ok := allowed[changed.File]; ok {
			filtered = append(filtered, changed)
		}
	}
	return filtered
}

func semanticDiffPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/dev/null" {
		return ""
	}
	return decodeGitPorcelainPath(value)
}

func parseSemanticDiffSpan(value string, prefix byte) (int, int, bool) {
	if len(value) < 2 || value[0] != prefix {
		return 0, 0, false
	}
	parts := strings.SplitN(value[1:], ",", 2)
	start, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	count := 1
	if len(parts) == 2 {
		count, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false
		}
	}
	if start < 0 || count < 0 {
		return 0, 0, false
	}
	return start, count, true
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

func writeSemanticChangesReport(brainDir string, report semanticChangesReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	rel := filepath.Join(semanticDirName, "changes", "latest.json")
	if err := clibase.RejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(brainDir, rel), data, 0o600)
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
	return semanticTestContainsFold(symbol.Kind, "test") ||
		semanticTestHasSuffixFold(symbol.FilePath, "_test.go") ||
		semanticTestContainsFold(symbol.FilePath, "/test/") ||
		semanticTestContainsFold(symbol.FilePath, "/tests/") ||
		semanticTestHasPrefixFold(symbol.Name, "test")
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

func newSemanticBundleCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Import or export local brain bundles",
	}
	var output string
	var expectedChecksum string
	var includeSessions bool
	var overwrite bool
	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export this brain's layers as a portable bundle (captured sessions excluded by default)",
		Long: `export writes one local archive containing this brain's layers so another
machine can rebuild the same brain from it.

Included by default: seed, docs, facts and runtime observations.
Code indexes stay with Graph and are rebuilt from the local checkout.

Excluded by default: the session family -- captured transcripts and the two
layers projected from them, history and patterns. Transcripts are verbatim
conversation content, and a history projection is committed against a
fingerprint of the sessions it was built from, so a bundle carrying history
without its sessions lands as immediately stale state the receiving brain
discards on its next refresh. --include-sessions ships all three together.

Never included: privacy policy (session tombstones and their transactions) and
local operational state (locks, worker logs, the bundle audit log). Those are
statements about this machine, not content of the brain, and a bundle that
carried them would apply one machine's exclusions to another.

Every run prints exactly which layers it wrote and which it left out.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output == "" {
				return errors.New("--output is required")
			}
			return runSemanticBundleExport(cmd.Context(), cmd, opts, output, includeSessions)
		},
	}
	exportCmd.Flags().StringVar(&output, "output", "", "Local output archive path")
	exportCmd.Flags().BoolVar(&includeSessions, "include-sessions", false, "Also export the session family: captured transcripts plus the history and patterns projections built from them")
	importCmd := &cobra.Command{
		Use:   "import <archive>",
		Short: "Import a local brain bundle",
		Long: `import verifies the archive's checksum, repo key and schema, then restores the
layers it carries into this brain.

A layer the destination brain already holds is NOT overwritten unless you pass
--overwrite: restoring a bundle over a live brain replaces its facts, docs and
history with the producer's, and that is a decision the operator makes, not a
side effect of an import. The refusal names every conflicting layer and happens
before anything is written.

Privacy policy in the destination brain is never touched by an import.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if expectedChecksum == "" {
				return errors.New("--sha256 is required for bundle import")
			}
			return runSemanticBundleImport(cmd.Context(), cmd, opts, args[0], expectedChecksum, overwrite)
		},
	}
	importCmd.Flags().StringVar(&expectedChecksum, "sha256", "", "Expected SHA-256 checksum for the local bundle archive")
	importCmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite brain layers the destination already has (files the bundle carries are replaced; files it does not carry stay)")
	cmd.AddCommand(exportCmd, importCmd)
	return cmd
}

// brainBundleLayer is one brain layer `bundle export` carries beside the
// semantic index.
//
// The bundle used to be semantic-only, and that was never a decision: the
// command was written on 2026-05-31, the day semantic.go was created and the
// day `semantic` was the ONLY source a brain had. history, facts, docs and
// patterns all landed in the weeks after (2026-06-01 through 2026-06-16) and
// nobody came back to the exporter. The result exported one layer of seven,
// said nothing about the other six, and a round-trip into a fresh store
// silently destroyed every authored fact -- in a command whose entire job is to
// move a brain without losing it. The design doc is explicit about the intended
// contents ("semantic manifests, semantic SQLite generations, raw provider
// artifacts needed for repair, seed summaries ... exclude ... session
// transcripts by default unless explicitly requested", docs/semantic_brain_plan.md),
// which is what this table now implements.
type brainBundleLayer struct {
	name string
	dir  string
	// skip names dir-relative subtrees a bundle must never carry. They are not
	// brain content: `history/tombstones.json` and `history/privacy/` are this
	// machine's privacy POLICY (a bundle carrying them would apply one
	// operator's exclusions to another brain), and `history/work/` is worker
	// lifecycle state and logs.
	skip []string
	// withSessions marks the session family: the captured transcripts
	// themselves and the two layers that are projections OF them. They travel
	// only under --include-sessions, for two reasons that point the same way.
	//
	// Privacy: transcripts are verbatim conversation content, and history and
	// patterns are distilled from it.
	//
	// Coherence: a history projection is committed against a fingerprint of the
	// sessions it was built from (see loadProjectionStateForSource). Ship it
	// without them and the destination is immediately and correctly reported
	// stale -- `memory_reconciliation: error (memory_source_stale)` -- and the
	// first refresh discards the imported projection and rebuilds it from the
	// destination's own sessions. Carrying it by default would be shipping
	// something the receiving brain throws away.
	//
	// seed, docs, facts and semantic have no such tie: they are projections of
	// the repository, or durable authored records, and both ends share a
	// repo_key.
	withSessions bool
}

const (
	bundleLayerSeed     = "seed"
	bundleLayerDocs     = "docs"
	bundleLayerFacts    = "facts"
	bundleLayerHistory  = "history"
	bundleLayerPatterns = "patterns"
	bundleLayerSessions = "sessions"
	bundleLayerSemantic = "semantic"
	// brainPatternsDirName is the pattern layer's directory under the brain
	// root. The pattern code writes the literal in several places; the bundle
	// needs a name it can put in a table.
	brainPatternsDirName = "patterns"
)

var brainBundleLayers = []brainBundleLayer{
	{name: "runtime_traces", dir: "semantic/traces"},
	{name: bundleLayerSeed, dir: seedDirName},
	{name: bundleLayerDocs, dir: docDirName},
	{name: bundleLayerFacts, dir: factsDirName},
	{name: bundleLayerSessions, dir: exportSessionsDirectory, withSessions: true},
	{name: bundleLayerHistory, dir: historyDirName, skip: []string{sessionTombstonesFileName, "privacy", "work"}, withSessions: true},
	{name: bundleLayerPatterns, dir: brainPatternsDirName, withSessions: true},
}

// skipsPath reports whether a brain-relative slash path falls inside one of the
// layer's never-bundled subtrees.
func (l brainBundleLayer) skipsPath(relSlash string) bool {
	for _, skip := range l.skip {
		full := l.dir + "/" + skip
		if relSlash == full || strings.HasPrefix(relSlash, full+"/") {
			return true
		}
	}
	return false
}

// copyBundleLayerSource moves one layer's manifest source between two
// brainSources. Export copies brain -> bundle and import copies bundle ->
// brain, so the mapping lives in exactly one place and the two directions
// cannot drift.
func copyBundleLayerSource(dst, src *brainSources, layer string) {
	if dst == nil || src == nil {
		return
	}
	switch layer {
	case bundleLayerSeed:
		dst.Seed = src.Seed
	case bundleLayerDocs:
		dst.Docs = src.Docs
	case bundleLayerFacts:
		dst.Facts = src.Facts
	case bundleLayerHistory:
		dst.History = src.History
	case bundleLayerPatterns:
		dst.Patterns = src.Patterns
	case bundleLayerSessions:
		dst.Sessions = src.Sessions
	}
}

// collectBrainBundleLayerPaths lists the brain-relative files of one layer, in
// sorted order, skipping the never-bundled subtrees. An absent layer is not an
// error: a brain that has never run `distill` simply has no facts to carry.
//
// It reads the same shape from a brain directory and from an extracted bundle
// root, so export and import walk identical rules.
func collectBrainBundleLayerPaths(root string, layer brainBundleLayer) ([]string, error) {
	layerRoot := filepath.Join(root, layer.dir)
	info, err := os.Lstat(layerRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("bundle refuses symlinked brain layer: %s", layer.dir)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("brain layer must be a directory: %s", layer.dir)
	}
	var paths []string
	err = filepath.WalkDir(layerRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if layer.skipsPath(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if err := rejectSymlinkPathComponents(root, rel); err != nil {
			return err
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

func runSemanticBundleExport(ctx context.Context, cmd *cobra.Command, opts Options, output string, includeSessions bool) error {
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
	if err := checkLegacyBrainLayout(storage.BrainDir, manifest); err != nil {
		return err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	bundleSources := &brainSources{}
	bundleBranches := append([]exportBranch(nil), manifest.Branches...)
	included := []string{}
	var omitted, sessionFamily []string
	layerFiles := map[string][]string{}
	var layerOrder []string
	for _, layer := range brainBundleLayers {
		if layer.name == bundleLayerSessions && checkpointBackedSessions(manifest) {
			if !includeSessions {
				sessionFamily = append(sessionFamily, layer.name)
				continue
			}
			guard, err := loadSessionReadGuard(storage.BrainDir, manifest)
			if err != nil {
				return err
			}
			source := *manifest.Sources.Sessions
			source.TranscriptStorage = ""
			// Excluded sessions remain in the local catalog for later inclusion,
			// but neither their catalog entries nor their bytes belong in a bundle.
			// The transcript store rechecks privacy when reading each kept entry.
			source.Sessions = guardExportSessions(guard, source.Sessions)
			source.OldestSessionAt = oldestSessionAt(source.Sessions)
			source.LatestCheckpointID = latestCheckpointID(source.Sessions)
			branchDirs := make(map[string]string)
			for _, branch := range manifest.Branches {
				branchDirs[branch.Branch] = branch.Directory
			}
			for _, branch := range source.Branches {
				branchDirs[branch.Branch] = branch.Directory
			}
			source.Branches = summarizeBranchExports(source.Sessions, branchDirs, source.DefaultBranch)
			bundleBranches = source.Branches
			bundleSources.Sessions = &source
			for _, session := range source.Sessions {
				layerFiles[layer.name] = append(layerFiles[layer.name], session.TranscriptPath)
			}
			layerOrder = append(layerOrder, layer.name)
			included = append(included, layer.name)
			continue
		}
		paths, err := collectBrainBundleLayerPaths(storage.BrainDir, layer)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			continue
		}
		if layer.withSessions && !includeSessions {
			sessionFamily = append(sessionFamily, layer.name)
			continue
		}
		layerFiles[layer.name] = paths
		layerOrder = append(layerOrder, layer.name)
		copyBundleLayerSource(bundleSources, manifest.Sources, layer.name)
		included = append(included, layer.name)
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
		// GeneratedAt was never set, so every bundle ever produced carried
		// "0001-01-01T00:00:00Z" -- an artifact with no production time at all.
		GeneratedAt: opts.Now().UTC(),
		RepoKey:     storage.Key,
		// BrainVersion attributes the ARTIFACT to the build that wrote it.
		// sources.semantic.provider_version cannot do this job: it is the
		// semantic PROVIDER's version, copied verbatim out of the snapshot
		// header entire-graph wrote, and validateSemanticSourceMatchesSnapshot
		// fails the import if the two disagree -- so overwriting it with this
		// binary's version would be both false and broken.
		BrainVersion: opts.Version,
		// Branches is repo structure (branch name, brain-relative directory,
		// session count) and is what tells a reader which branches a bundle
		// covers. It was dropped along with everything else that was not the
		// semantic source.
		//
		// repo_root stays out, deliberately and against the letter of the
		// report that asked for it: it is the PRODUCER's absolute filesystem
		// path, nothing on the import side ever reads it (the destination
		// resolves its own), and this exporter already spends
		// sanitizeSemanticSourceForExport redacting exactly such paths out of
		// warnings. Putting the host path back in the header would undo that.
		// repo_key, which is already exported, is the portable identity.
		Branches: bundleBranches,
		Sources:  bundleSources,
	}); err != nil {
		_ = f.Close()
		return err
	}
	transcriptStore := newTranscriptStore(opts.Runner, storage.BrainDir)
	for _, name := range layerOrder {
		for _, rel := range layerFiles[name] {
			if name == bundleLayerSessions && checkpointBackedSessions(manifest) {
				if err := addTranscriptBundlePath(ctx, tw, transcriptStore, rel, opts.Now()); err != nil {
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
	// Say what is in the archive and what is not. A bundle that silently
	// carried one layer of seven is the defect; a bundle that carries six and
	// says nothing would be the same defect with a better ratio.
	sort.Strings(included)
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "exported bundle: %s\nsha256: %s\n", output, checksum)
	// --sha256 is REQUIRED on import, and the digest is only printed here, so a
	// reader who keeps the archive and not this output cannot import it without
	// recomputing the hash. Print the whole command rather than the ingredient.
	// The path is quoted because this line is meant to be copy-pasted: an
	// --output with a space in it produced a command that ran against the
	// wrong path. The label above is prose, not a command, so it is not quoted.
	//
	// shellQuotedRepoDir is the package's existing single-quoter; its name says
	// repo dir but it quotes any path, and adding a third one for this would be
	// worse than reusing it. output is a required flag, so its empty-input
	// fallback is not reachable here.
	fmt.Fprintf(out, "import it with: %s bundle import %s --sha256 %s\n",
		setupBrainCommand(setupCommandPrefix(os.LookupEnv)), shellQuotedRepoDir(output), checksum)
	fmt.Fprintf(out, "includes: %s\n", strings.Join(included, ", "))
	if len(sessionFamily) > 0 {
		sort.Strings(sessionFamily)
		omitted = append(omitted, strings.Join(sessionFamily, "/")+" (captured sessions and their projections; pass --include-sessions)")
	}
	omitted = append(omitted, "privacy policy and local operational state (never bundled)")
	fmt.Fprintf(out, "omits: %s\n", strings.Join(omitted, ", "))
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
	data, err := clibase.SafeReadFile(path, clibase.SemanticSnapshotMaxBytes())
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
	// clean == ".." must be listed explicitly: filepath.Clean("..") is "..",
	// which is neither "." nor prefixed with "../", so a bare ".." otherwise
	// falls through this whole test. The per-component walk below then Lstats
	// the PARENT of root — a real directory, not a symlink — and passes it, and
	// the closing containment check returns nil early whenever root does not
	// exist yet. One level up is exactly the escape the guard exists to stop.
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
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
	if !clibase.PathInside(rootResolved, pathResolved) {
		return fmt.Errorf("path escapes root through symlinks: %s", rel)
	}
	return nil
}

func rejectBrainRootPathSymlinks(brainRoot, rel string) error {
	if err := clibase.RejectSymlinkedBrainRoot(brainRoot); err != nil {
		return err
	}
	return clibase.RejectExistingSymlinkPathComponents(brainRoot, rel)
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
	if clibase.PathInside(brainAbs, outputAbs) {
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
	if clibase.PathInside(brainResolved, outputResolved) {
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
	if !clibase.PathInside(rootAbs, repoAbs) {
		return fmt.Errorf("indexed repository %q is outside the bound repository root", repoDir)
	}
	return nil
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

func runSemanticBundleImport(ctx context.Context, cmd *cobra.Command, opts Options, archive, expectedChecksum string, overwrite bool) error {
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
	if err := requireCurrentBrainLayout(storage.BrainDir); err != nil {
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
	if err := checkLegacyBrainLayout(tmpDir, importManifest); err != nil {
		if errors.Is(err, errLegacyBrainLayout) {
			return fmt.Errorf("%w in this bundle; import it with entire-brain 0.2.0 to preserve runtime traces, then export it again before retrying", errLegacyBrainLayout)
		}
		return err
	}
	// The destination may have changed before we acquired the writer lock.
	if err := requireCurrentBrainLayout(storage.BrainDir); err != nil {
		return err
	}
	// Decide the overwrite question BEFORE the first byte is written, so a
	// refusal leaves the destination brain exactly as it was rather than
	// half-imported.
	conflicts, err := brainBundleLayerConflicts(storage.BrainDir, tmpDir)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 && !overwrite {
		// "import into a brain without them" named a state a real receiver is
		// never in: a brain that has run `setup` always holds seed and docs, so
		// the only reachable choice is to accept the replacement or not make it.
		return bundleImportConflictError(conflicts)
	}
	if len(conflicts) > 0 {
		// The loss is silent otherwise: the layers are replaced, and the next
		// empty `recall` blames distillation coverage rather than this import.
		fmt.Fprintf(cmd.OutOrStdout(), "replacing this brain's %s with the bundle's; anything recorded here and absent from the bundle is no longer retrievable\n", strings.Join(conflicts, ", "))
	}
	// Record the import BEFORE replacing anything. Written afterwards, a failed
	// audit write leaves the facts already destroyed with no trace that an
	// import did it, and the empty `recall` that follows goes back to blaming
	// distillation coverage -- the exact defect this entry exists to prevent.
	// An entry for an import that then fails part way is the honest record:
	// layers may already have been touched, and the reader needs to know.
	// Record WHICH layers this import REPLACES -- `conflicts`, the layers
	// present on BOTH sides -- not the layers the bundle happens to carry.
	//
	// This has been raised twice as "it should record what was imported". It
	// must not. The note this feeds says "facts recorded here before then are
	// no longer present", which is only true if this brain HELD facts and the
	// import overwrote them. Recording the bundle's contents instead would fire
	// that note at a brain that never had facts, telling a reader their
	// remembered facts are gone when nothing was lost -- and telling them so in
	// preference to the distillation advice that would actually have helped.
	//
	// Pinned by TestAnImportIntoABrainWithNoFactsExplainsNothing.
	if err := appendSemanticAuditLayers(storage.BrainDir, "bundle_import", archive, sum, conflicts); err != nil {
		return err
	}
	restored, err := restoreBundleBrainLayers(storage.BrainDir, tmpDir)
	if err != nil {
		return err
	}
	if len(restored) > 0 {
		if err := writeBrainBundleLayerSources(storage.BrainDir, storage.Key, restored, importManifest); err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "imported bundle entries: %d\nsha256: %s\n", imported, sum)
	fmt.Fprintf(out, "restored layers: %s\n", strings.Join(restored, ", "))
	// Graph-owned source metadata is not restored; Graph resolves its own store
	// locally. Old bundles with Brain-owned stores were refused above.
	if importManifest.Sources.Semantic != nil && !slices.Contains(restored, bundleLayerSemantic) {
		fmt.Fprintln(out, "bundle's code index was not imported: code indexing is Graph-owned since 0.2.0")
	}
	return nil
}

// brainBundleLayerConflicts names the layers a bundle carries that this brain
// already holds.
//
// It exists because restoring a bundle over a live brain replaces that brain's
// facts, docs and history with the producer's, and the command whose defect
// report is "a round-trip destroyed my facts" must not close that hole by
// opening the mirror image of it on the destination side. An import into a
// brain that has never built those layers -- restore, migration, reproducing a
// CI failure -- is unaffected and needs no flag.
func brainBundleLayerConflicts(brainDir, bundleRoot string) ([]string, error) {
	var conflicts []string
	for _, layer := range brainBundleLayers {
		incoming, err := collectBrainBundleLayerPaths(bundleRoot, layer)
		if err != nil {
			return nil, err
		}
		if len(incoming) == 0 {
			continue
		}
		existing, err := collectBrainBundleLayerPaths(brainDir, layer)
		if err != nil {
			return nil, err
		}
		occupied := len(existing) > 0
		if layer.name == bundleLayerSessions && !occupied {
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return nil, err
			}
			occupied = manifest.Sources != nil && manifest.Sources.Sessions != nil && len(manifest.Sources.Sessions.Sessions) > 0
		}
		if occupied {
			conflicts = append(conflicts, layer.name)
		}
	}
	return conflicts, nil
}

// restoreBundleBrainLayers writes the non-semantic layers a bundle carries into
// the destination brain and reports which ones it restored.
//
// Files are written individually rather than by swapping whole directories.
// That is the same shape the semantic snapshot restore above already uses, and
// it has the property that matters here: it touches only paths the bundle
// actually carries, so the destination's privacy tombstones, privacy
// transactions and worker state -- which no bundle contains -- cannot be
// deleted by an import. Replacing history/ wholesale would have silently
// un-excluded every session the destination had tombstoned.
func restoreBundleBrainLayers(brainDir, bundleRoot string) ([]string, error) {
	var restored []string
	for _, layer := range brainBundleLayers {
		paths, err := collectBrainBundleLayerPaths(bundleRoot, layer)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			continue
		}
		for _, rel := range paths {
			content, err := clibase.SafeReadFile(filepath.Join(bundleRoot, rel), clibase.SemanticSnapshotMaxBytes())
			if err != nil {
				return nil, err
			}
			if err := clibase.RejectExistingSymlinkPathComponents(brainDir, rel); err != nil {
				return nil, err
			}
			target := filepath.Join(brainDir, rel)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return nil, err
			}
			if err := writeFileAtomic(target, content, 0o600); err != nil {
				return nil, err
			}
		}
		restored = append(restored, layer.name)
	}
	return restored, nil
}

// writeBrainBundleLayerSources records the restored layers' manifest sources in
// the destination brain. Without this the files would land on disk and every
// status, doctor and retrieval surface would still report the layers as absent,
// because those read the manifest and not the directory.
func writeBrainBundleLayerSources(brainDir, repoKey string, restored []string, bundle *exportManifest) error {
	if bundle == nil || bundle.Sources == nil {
		return nil
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.SchemaVersion = brainManifestSchemaVersion
	manifest.RepoKey = repoKey
	for _, name := range restored {
		copyBundleLayerSource(manifest.Sources, bundle.Sources, name)
	}
	applySessionSourceAliases(manifest)
	return writeBrainManifestAndReadme(brainDir, *manifest)
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
	// Checked first, ahead of repo_key and every other field: a bundle from a
	// newer format must be refused before this build guesses at fields it has
	// never seen, not after it has already decided they look fine.
	if manifest.SchemaVersion > brainManifestSchemaVersion {
		return nil, fmt.Errorf("bundle schema_version %d is newer than supported version %d; upgrade entire-brain to import this bundle", manifest.SchemaVersion, brainManifestSchemaVersion)
	}
	if manifest.RepoKey == "" {
		return nil, errors.New("bundle manifest missing repo_key")
	}
	if !semanticRepoKeyEqual(manifest.RepoKey, repoKey) {
		return nil, fmt.Errorf("bundle repo_key %q does not match current repo %q", manifest.RepoKey, repoKey)
	}
	if manifest.Sources == nil {
		return nil, errors.New("bundle manifest missing sources")
	}
	// Code-index schemas are not validated here: no code layer is restored.
	// The caller separately checks for old layouts before restoring any data.
	return &manifest, nil
}

// The integrity checks over a generation — that the snapshot still matches the
// manifest, and that the store holds the rows the manifest declares — answer
// the same way for as long as the files do not change. Generations are
// content-keyed and indexing writes a new one rather than mutating an existing
// one, but a file can still be truncated, rewritten or corrupted underneath a
// running process, and that must not go unnoticed.
//
// They were being re-run per query, and they are not cheap: on a 747k-line
// repository the store validation counts rows in a 128 MB SQLite file (436 ms)
// and the snapshot check JSON-parses an 81 MB file (164 ms), together about
// half of a 1.2 s search. A process issuing several searches paid that for each
// one, to re-derive an answer that could not have changed.
//
// So the memo key carries the file's size and modification time, taken from a
// stat that costs microseconds against a check that costs hundreds of
// milliseconds. A file that changes gets re-verified in full; one that has not
// is not re-read. The first search in a process always performs both checks,
// and a one-shot CLI invocation is unchanged.
//
// The residual hole is a corruption that preserves size and mtime exactly. That
// is the ordinary limit of stat-based invalidation, and it is narrower than
// re-reading nothing — but it is a hole, not an absence of one.
var semanticIntegrityMemo sync.Map

// semanticIntegrityMemoCount tracks distinct keys so the memo can be bounded.
// Keys embed file identity (size and mtime), so every reindex mints a fresh set
// and the old ones are never looked up again. In a short CLI invocation that is
// harmless, but the MCP server's serve loop is long-lived and fields calls
// across many repos and reindex cycles, where an unbounded map grows for the
// life of the process.
//
// Dropping the whole memo when it gets too large is safe and deliberately
// simple: every entry is a pure function of a file version, so a miss costs one
// re-validation and never a wrong answer. An LRU would buy a slightly better
// hit rate for materially more machinery.
var semanticIntegrityMemoCount atomic.Int64

const semanticIntegrityMemoMaxEntries = 1024

// semanticIntegrityMemoWrites linearizes insertion against the count and the
// eviction. LoadOrStore and Add are each atomic but not atomic together, so
// without this an insert could land between another goroutine's Clear and its
// Store(0): the entry stays live while its increment is wiped, the count drifts
// below the true size, and the bound stops holding. Reads still go through
// sync.Map's lock-free path; only the write side takes this.
var semanticIntegrityMemoWrites sync.Mutex

// rememberSemanticIntegrity stores a verdict and bounds the memo.
func rememberSemanticIntegrity(key string, verdict any) {
	semanticIntegrityMemoWrites.Lock()
	defer semanticIntegrityMemoWrites.Unlock()
	if _, loaded := semanticIntegrityMemo.LoadOrStore(key, verdict); loaded {
		return
	}
	if semanticIntegrityMemoCount.Add(1) > semanticIntegrityMemoMaxEntries {
		semanticIntegrityMemo.Clear()
		semanticIntegrityMemo.Store(key, verdict)
		semanticIntegrityMemoCount.Store(1)
	}
}

type semanticSnapshotVerdict struct {
	header semanticHeader
	counts semanticCounts
	err    error
}

// fileIdentity returns a stat-derived fingerprint for cache keying. An
// unreadable file yields a distinct marker so it is never confused with a
// previously readable one.
func fileIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "missing:" + err.Error()
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// A wrapper gives successful (nil error) verdicts a concrete type too.
type semanticStoreVerdict struct {
	err error
}

func semanticRepoKeyEqual(a, b string) bool {
	return a == b || strings.EqualFold(a, b)
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
	if !bundleEntryPathAllowed(clean, cleanSlash) {
		return fmt.Errorf("bundle entry outside allowed paths: %s", header.Name)
	}
	return nil
}

// bundleEntryPathAllowed is the import-side allowlist, and it is deliberately
// narrower than "any path under the brain".
//
// The skip check runs BEFORE the layer prefix check, so an archive claiming to
// carry history/tombstones.json, history/privacy/* or history/work/* is
// refused outright rather than silently dropped: those are the destination's
// own privacy policy and operational state, and a bundle has no business
// naming them. semantic/audit.jsonl stays out for the same reason -- only the
// snapshot and generation subtrees of semantic/ are allowed.
func bundleEntryPathAllowed(clean, cleanSlash string) bool {
	if clean == exportManifestFileName {
		return true
	}
	if strings.HasPrefix(cleanSlash, filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir))+"/") ||
		strings.HasPrefix(cleanSlash, filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir))+"/") {
		return true
	}
	for _, layer := range brainBundleLayers {
		if layer.skipsPath(cleanSlash) {
			return false
		}
		if strings.HasPrefix(cleanSlash, layer.dir+"/") {
			return true
		}
	}
	return false
}

// bundleImportConflictError refuses an import that would replace layers.
//
// The old wording ended "or import into a brain without them", naming a state
// a real receiver is never in: `setup` always writes seed and docs, so by the
// time anyone has a brain to import into, the empty-brain path is gone. What
// was missing is the part that matters -- what accepting costs.
func bundleImportConflictError(conflicts []string) error {
	// Kept under statusCauseWidth (140): this error is recorded as a freshness
	// axis detail, and a 402-character cause was clipped mid-sentence in every
	// surface that shows one. The consequence a reader must not miss is that
	// --overwrite DROPS what is here and absent from the bundle; the rest was
	// explanation that the flag's own help carries.
	return fmt.Errorf("bundle import would replace existing brain layers (%s); with --overwrite, anything here the bundle lacks stops being retrievable", strings.Join(conflicts, ", "))
}

// lastBundleImportAt reports when a bundle last replaced layers in this brain.
//
// An import with --overwrite replaces the fact layer wholesale, so facts the
// reader remembered here before it are simply gone. Without this the next empty
// recall blames distillation coverage and sends them to re-distill, which
// cannot bring back what an import replaced.
//
// Best effort by design: the audit log is local operational state and may be
// absent, truncated or partly unreadable. A malformed line is skipped rather
// than failing the caller, because a missing explanation must never turn a
// working recall into an error.
func lastBundleImportAt(brainDir string) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(brainDir, semanticDirName, semanticAuditLogName))
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Action      string   `json:"action"`
			GeneratedAt string   `json:"generated_at"`
			Layers      []string `json:"layers"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.Action != "bundle_import" {
			continue
		}
		// Only an import that replaced the FACT layer explains a missing fact.
		// An entry with no layers predates this field: treat it as unknown and
		// skip, rather than asserting something the record does not say.
		if !slices.Contains(entry.Layers, bundleLayerFacts) {
			continue
		}
		at, err := time.Parse(time.RFC3339, entry.GeneratedAt)
		if err != nil {
			continue
		}
		if at.After(newest) {
			newest = at
		}
	}
	return newest, !newest.IsZero()
}

func appendSemanticAudit(brainDir, action, path, checksum string) error {
	return appendSemanticAuditLayers(brainDir, action, path, checksum, nil)
}

// appendSemanticAuditLayers records which layers an action replaced.
//
// Without this an import of docs alone and an import that destroyed the fact
// layer were indistinguishable in the log, so the empty-recall note read every
// bundle_import as proof the facts had been replaced. A note that explains an
// empty result with something that did not happen is worse than the
// distillation advice it set out to correct.
func appendSemanticAuditLayers(brainDir, action, path, checksum string, layers []string) error {
	entry := map[string]any{
		"action":       action,
		"path":         path,
		"sha256":       checksum,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if len(layers) > 0 {
		entry["layers"] = layers
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
	if err := clibase.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	auditPath := filepath.Join(brainDir, semanticDirName, semanticAuditLogName)
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		return err
	}
	if err := clibase.RejectExistingSymlinkPathComponents(brainDir, filepath.Join(semanticDirName, semanticAuditLogName)); err != nil {
		return err
	}
	return nil
}
