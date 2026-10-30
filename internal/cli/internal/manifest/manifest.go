// Package manifest holds the brain-manifest schema -- the types that
// serialize to the brain's manifest.json -- and its checked read layer:
// tolerant reads for consumers, strict reads for writers about to rewrite
// the file. Sunk from package cli (COR-2268) so command packages can
// consume the manifest without importing cli. Writers (the manifest
// replacement path and readme renderers) stay in cli.
package manifest

import (
	"encoding/json"
	"time"

	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
)

const FileName = "manifest.json"

type CheckpointTokenUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

type CheckpointSummary struct {
	Intent  string `json:"intent,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

type Manifest struct {
	TranscriptStorage string    `json:"transcript_storage,omitempty"`
	SchemaVersion     int       `json:"schema_version"`
	GeneratedAt       time.Time `json:"generated_at"`
	// BrainVersion is the entire-brain build that PRODUCED the document. It is
	// set only on exported bundles -- artifacts that leave this machine and so
	// have to be attributable to a release.
	//
	// It is not a duplicate of sources.semantic.provider_version, which is the
	// semantic PROVIDER's version (entire-graph's), copied verbatim out of the
	// snapshot header the provider wrote. That field reading "dev" on a machine
	// whose installed Entire CLI is itself a dev build is CORRECT, and
	// overwriting it with this binary's version would both lie and break
	// validateSemanticSourceMatchesSnapshot, which fails an import when the
	// manifest and the snapshot header disagree. The producing build's own
	// version was simply never recorded anywhere; this is that field.
	//
	// omitempty, and never set on the on-disk brain manifest, so no stored
	// manifest gains a field an older build would then refuse to rewrite.
	BrainVersion string `json:"brain_version,omitempty"`
	RepoRoot     string `json:"repo_root,omitempty"`
	// Stable local checkpoint location, independent of the exporting worktree.
	CheckpointGitDir   string    `json:"checkpoint_git_dir,omitempty"`
	CheckpointPrimary  string    `json:"checkpoint_primary,omitempty"`
	RepoKey            string    `json:"repo_key,omitempty"`
	EntireCLIVersion   string    `json:"entire_cli_version,omitempty"`
	DefaultBranch      string    `json:"default_branch,omitempty"`
	TranscriptMode     string    `json:"transcript_mode"`
	Scope              string    `json:"scope"`
	CheckpointLimit    int       `json:"checkpoint_limit"`
	CheckpointsScanned int       `json:"checkpoints_scanned"`
	Branches           []Branch  `json:"branches,omitempty"`
	Sessions           []Session `json:"sessions"`
	Warnings           []string  `json:"warnings,omitempty"`
	// SuppressedWarnings holds the RAW text of every warning usefulExportWarnings kept out of
	// Warnings. It exists so a diagnostic warning -- "checkpoint snapshot unavailable:" above all,
	// which means a transcript was not readable and the export is therefore incomplete -- is
	// recoverable from the manifest alone, without re-running the export under --debug.
	SuppressedWarnings []string `json:"suppressed_warnings,omitempty"`
	Sources            *Sources `json:"sources,omitempty"`
}

type Branch struct {
	Branch       string `json:"branch"`
	Directory    string `json:"directory"`
	SessionCount int    `json:"session_count"`
	Default      bool   `json:"default,omitempty"`
}

type Session struct {
	SessionID            string                `json:"session_id"`
	Branch               string                `json:"branch,omitempty"`
	SourceBranch         string                `json:"source_branch,omitempty"`
	Authors              []Author              `json:"authors,omitempty"`
	Agent                string                `json:"agent,omitempty"`
	Model                string                `json:"model,omitempty"`
	Kind                 string                `json:"kind,omitempty"`
	ReviewSkills         []string              `json:"review_skills,omitempty"`
	LatestCheckpoint     string                `json:"latest_checkpoint_id"`
	SessionIndex         int                   `json:"session_index"`
	CreatedAt            time.Time             `json:"created_at"`
	TurnID               string                `json:"turn_id,omitempty"`
	IsTask               bool                  `json:"is_task,omitempty"`
	ToolUseID            string                `json:"tool_use_id,omitempty"`
	FilesTouched         []string              `json:"files_touched,omitempty"`
	TokenUsage           *CheckpointTokenUsage `json:"token_usage,omitempty"`
	Summary              *CheckpointSummary    `json:"summary,omitempty"`
	CheckpointsCount     int                   `json:"checkpoints_count,omitempty"`
	TranscriptPath       string                `json:"transcript_path"`
	SourceTranscriptPath string                `json:"-"`
	SourceKey            string                `json:"-"`
}

type Author struct {
	Name         string     `json:"name,omitempty"`
	Email        string     `json:"email,omitempty"`
	Commits      int        `json:"commits,omitempty"`
	LastCommitAt *time.Time `json:"last_commit_at,omitempty"`
}

const SchemaVersion = 4

// MinSchemaVersion is the oldest manifest this build reads.
//
// It is 1 rather than 0 because 0 is not a version anything ever wrote: the
// commit that introduced manifest.json already set schema_version 1, and the
// field carries no `omitempty`, so no build of this tool can emit a manifest
// without one. A manifest reporting 0 is therefore an ABSENT field -- `{}`, a
// hand-edited file, a truncated write -- and not a pre-versioning document to
// adapt. Reading it as "current" was how an empty object passed for a healthy
// brain.
const MinSchemaVersion = 1

type Sources struct {
	Seed     *SeedSource     `json:"seed,omitempty"`
	Sessions *SessionSource  `json:"sessions,omitempty"`
	Semantic *SemanticSource `json:"semantic,omitempty"`
	History  *HistorySource  `json:"history,omitempty"`
	Facts    *FactSource     `json:"facts,omitempty"`
	Docs     *DocSource      `json:"docs,omitempty"`
	Patterns *PatternSource  `json:"patterns,omitempty"`
}

type SessionSource struct {
	TranscriptStorage  string     `json:"transcript_storage,omitempty"`
	GeneratedAt        time.Time  `json:"generated_at"`
	EntireCLIVersion   string     `json:"entire_cli_version,omitempty"`
	DefaultBranch      string     `json:"default_branch,omitempty"`
	TranscriptMode     string     `json:"transcript_mode"`
	Scope              string     `json:"scope"`
	CheckpointLimit    int        `json:"checkpoint_limit"`
	CheckpointsScanned int        `json:"checkpoints_scanned"`
	OldestSessionAt    *time.Time `json:"oldest_session_at,omitempty"`
	LatestCheckpointID string     `json:"latest_checkpoint_id,omitempty"`
	Branches           []Branch   `json:"branches,omitempty"`
	Sessions           []Session  `json:"sessions"`
	Warnings           []string   `json:"warnings,omitempty"`
}

// FactSource is recorded under sources.facts in the brain manifest,
// parallel to HistorySource and the semantic source metadata.
type FactSource struct {
	LastDistilledAt   time.Time      `json:"last_distilled_at,omitzero"`
	GeneratedAt       time.Time      `json:"generated_at"`
	TaxonomyPath      string         `json:"taxonomy_path"`
	Branches          []string       `json:"branches,omitempty"`
	Facts             int            `json:"facts"`
	Distilled         int            `json:"distilled"`
	Authored          int            `json:"authored"`
	Imported          int            `json:"imported,omitempty"`
	Superseded        int            `json:"superseded"`
	Proposals         int            `json:"proposals"`
	Verified          int            `json:"verified"`
	Unsigned          int            `json:"unsigned"`
	ByKind            map[string]int `json:"by_kind,omitempty"`
	ChunksScanned     int            `json:"chunks_scanned"`
	ChunksDistilled   int            `json:"chunks_distilled"`
	CacheHits         int            `json:"cache_hits,omitempty"`
	FailedChunks      int            `json:"failed_chunks,omitempty"`
	PreprocessedBytes int64          `json:"preprocessed_bytes,omitempty"`
	Agent             string         `json:"agent,omitempty"`
	Model             string         `json:"model,omitempty"`
	Effort            string         `json:"effort,omitempty"`
	Branch            string         `json:"branch,omitempty"`
	Force             bool           `json:"force,omitempty"`
	Jobs              int            `json:"jobs,omitempty"`
	ExtractionJobsCap int            `json:"extraction_jobs_cap,omitempty"`
	MaxChunkBytes     int            `json:"max_chunk_bytes,omitempty"`
	Confidence        float64        `json:"confidence_threshold,omitempty"`
	ExtractionCalls   int            `json:"extraction_agent_calls,omitempty"`
	ReconcileCalls    int            `json:"reconcile_agent_calls,omitempty"`
	// The staged-reconcile pass (COR-2184): how many extracted candidates the
	// end-of-run pass adjudicated, how many promoted with no agent call, and
	// how many similarity clusters went to the agent. Disclosed so the staging
	// flow is visible rather than looking like missing facts.
	CandidatesReconciled int `json:"candidates_reconciled,omitempty"`
	PromotedWithoutAgent int `json:"promoted_without_agent,omitempty"`
	ReconcileClusters    int `json:"reconcile_clusters,omitempty"`
	// PendingReconcile counts staged candidates still on disk at summary time —
	// including interrupted extraction, failed decisions, and queued review.
	// They are excluded from Facts (not published). Distill retries failed
	// decisions; queued candidates wait for explicit review.
	PendingReconcile int                       `json:"pending_reconcile,omitempty"`
	TotalAgentCalls  int                       `json:"total_agent_calls,omitempty"`
	TokenUsage       *DistillTokenUsageSummary `json:"token_usage,omitempty"`
	// ExtractionWaitSeconds is the consumer's WAIT on prefetched results, not
	// agent compute: with concurrency > 1, calls completing in the background
	// register ~0s here. Compare total_seconds across --jobs settings for
	// scheduling claims.
	ExtractionWaitSeconds float64  `json:"extraction_wait_seconds,omitempty"`
	ReconcileSeconds      float64  `json:"reconcile_seconds,omitempty"`
	WriteSeconds          float64  `json:"write_seconds,omitempty"`
	TotalSeconds          float64  `json:"total_seconds,omitempty"`
	Warnings              []string `json:"warnings,omitempty"`
}

type HistorySource struct {
	GeneratedAt time.Time `json:"generated_at"`
	IndexPath   string    `json:"index_path"`
	// ExtractionVersion identifies the history record extraction logic used to
	// build the persisted index. A changed extractor must rebuild even when the
	// transcript files themselves are unchanged.
	ExtractionVersion int `json:"extraction_version,omitempty"`
	// IndexBytes/IndexSHA256/RecordsFingerprint let a reader rank straight from
	// the FTS payload table without loading index.json, and give the FTS
	// freshness check an O(1) key.
	IndexBytes             int64  `json:"index_bytes,omitempty"`
	IndexSHA256            string `json:"index_sha256,omitempty"`
	RecordsFingerprint     string `json:"records_fingerprint,omitempty"`
	TranscriptsFingerprint string `json:"transcripts_fingerprint,omitempty"`
	// IndexDigest commits the exact bounded index bytes selected by IndexPath.
	// It is optional only for manifests written before generation integrity was
	// introduced; every new projection publishes it with the manifest-last
	// commit so readers can distinguish legacy from corrupt state.
	IndexDigest string `json:"index_digest,omitempty"`
	// PrivacyIdentity binds this generation to the exact tombstone bytes used
	// while building it. Vector backfill refuses to consume an index whose
	// privacy epoch is not the current one.
	PrivacyIdentity     string `json:"privacy_identity,omitempty"`
	SessionsFingerprint string `json:"sessions_fingerprint,omitempty"`
	Records             int    `json:"records"`
	Decisions           int    `json:"decisions"`
	Learnings           int    `json:"learnings"`
	Validations         int    `json:"validations"`
	ToolCalls           int    `json:"tool_calls"`
	CodeFacts           int    `json:"code_facts"`
	Exchanges           int    `json:"exchanges,omitempty"`
	IncompleteExchanges int    `json:"incomplete_exchanges,omitempty"`
	// ExcludedSessions counts tombstoned sessions skipped before derived
	// indexing (their content is never retained; see session_privacy.go).
	ExcludedSessions int      `json:"excluded_sessions,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
	// ProjectionStatePath/Digest publish the projection receipt file; the
	// manifest's atomic replacement is the commit point, so an interruption
	// before it leaves readers on the previous receipt set.
	ProjectionStatePath   string `json:"projection_state_path,omitempty"`
	ProjectionStateDigest string `json:"projection_state_digest,omitempty"`
}

type SemanticSource struct {
	GraphRepoRoot string `json:"graph_repo_root,omitempty"`
	StoreOwner    string `json:"store_owner,omitempty"`
	// Never serialized: resolved per operation (see GraphStoreDescriptor).
	GraphStore       *GraphStoreDescriptor `json:"-"`
	GeneratedAt      time.Time             `json:"generated_at"`
	Commit           string                `json:"commit,omitempty"`
	Tree             string                `json:"tree,omitempty"`
	Branch           string                `json:"branch,omitempty"`
	DefaultBranch    string                `json:"default_branch,omitempty"`
	DirtyWorktree    bool                  `json:"dirty_worktree"`
	Provider         string                `json:"provider,omitempty"`
	ProviderVersion  string                `json:"provider_version,omitempty"`
	IdentityRevision string                `json:"identity_revision,omitempty"`
	SchemaVersion    string                `json:"schema_version,omitempty"`
	SnapshotPath     string                `json:"snapshot_path,omitempty"`
	StorePath        string                `json:"store_path,omitempty"`
	GenerationPath   string                `json:"generation_path,omitempty"`
	MetricsPath      string                `json:"metrics_path,omitempty"`
	ParseCachePath   string                `json:"parse_cache_path,omitempty"`
	OverlayPath      string                `json:"overlay_path,omitempty"`
	Symbols          int                   `json:"symbols"`
	Relations        int                   `json:"relations"`
	Files            int                   `json:"files,omitempty"`
	Externals        int                   `json:"externals,omitempty"`
	Warnings         []SemanticWarning     `json:"warnings,omitempty"`
	PartialFailures  []SemanticWarning     `json:"partial_failures,omitempty"`
	Capabilities     []string              `json:"capabilities,omitempty"`
	NoEgressVerified bool                  `json:"no_egress_verified"`
	WorktreeMode     string                `json:"worktree_mode,omitempty"`
	WorktreeHash     string                `json:"worktree_hash,omitempty"`

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

type SemanticWarning struct {
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
func (w *SemanticWarning) UnmarshalJSON(data []byte) error {
	type alias SemanticWarning
	var raw struct {
		alias
		ProviderPath   string `json:"file_path"`
		ProviderEffect string `json:"effect_on_semantic_completeness"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*w = SemanticWarning(raw.alias)
	if w.Path == "" {
		w.Path = raw.ProviderPath
	}
	if w.Effect == "" {
		w.Effect = raw.ProviderEffect
	}
	return nil
}

type SemanticHeader struct {
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
	Warnings         []SemanticWarning `json:"warnings"`
	PartialFailures  []SemanticWarning `json:"partial_failures"`

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

type DocSource struct {
	GeneratedAt time.Time `json:"generated_at"`
	IndexPath   string    `json:"index_path"`
	Records     int       `json:"records"`
	Files       int       `json:"files"`
	Warnings    []string  `json:"warnings,omitempty"`
}

// PatternSource is the brain-manifest entry for the patterns layer. It
// parallels HistorySource/FactSource. Freshness is derived from
// the same sessions fingerprint the history source uses, so `patterns status` can
// report current/stale without re-running extraction. The procedure/practice/
// pattern/skill-memory counters are reserved for later phases and stay zero until
// then.
type PatternSource struct {
	GeneratedAt  time.Time `json:"generated_at"`
	EpisodesPath string    `json:"episodes_path"`
	// PrivacyIdentity binds this corpus to the exact tombstone bytes the build
	// filtered against, exactly as HistorySource does. The sessions
	// fingerprint alone cannot see a privacy change: exclude/include leave the
	// canonical session set untouched, so without this an exclusion sweep
	// (which rewrites episodes.ndjson in place, behind this manifest's back)
	// left `patterns status` reporting the pre-sweep episode count as
	// "current", and a later `privacy include` never rebuilt the corpus at all
	// because nothing had changed as far as the fingerprint could tell.
	PrivacyIdentity     string                          `json:"privacy_identity,omitempty"`
	SessionsFingerprint string                          `json:"sessions_fingerprint,omitempty"`
	Episodes            int                             `json:"episodes"`
	Reinforcement       patterncore.ReinforcementCounts `json:"reinforcement"`
	Tasks               int                             `json:"tasks,omitempty"`
	Procedures          int                             `json:"procedures,omitempty"`
	Practices           int                             `json:"practices,omitempty"`
	Patterns            int                             `json:"patterns,omitempty"`
	SkillMemory         int                             `json:"skill_memory,omitempty"`
	Warnings            []string                        `json:"warnings,omitempty"`
}

type SeedSource struct {
	GeneratedAt       time.Time            `json:"generated_at"`
	Commit            string               `json:"commit,omitempty"`
	WorktreeMode      string               `json:"worktree_mode"`
	WorktreeHash      string               `json:"worktree_hash,omitempty"`
	FileFingerprint   string               `json:"file_fingerprint"`
	SummaryPath       string               `json:"summary_path"`
	Documents         []SeedDocument       `json:"documents,omitempty"`
	Entrypoints       []string             `json:"entrypoints,omitempty"`
	Commands          []SeedCommand        `json:"commands,omitempty"`
	HistoryBaseline   *SeedHistoryBaseline `json:"history_baseline,omitempty"`
	HistoryCoverage   *SeedHistoryCoverage `json:"history_coverage,omitempty"`
	Agent             *SeedAgent           `json:"agent,omitempty"`
	Warnings          []string             `json:"warnings,omitempty"`
	DeterministicPath []string             `json:"deterministic_paths,omitempty"`
}

type SeedDocument struct {
	Path      string `json:"path"`
	SeedPath  string `json:"seed_path,omitempty"`
	Bytes     int64  `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Extracted marks a document whose text was read out of a binary container
	// (PDF, Office) rather than copied. The seed file is then derived, not a
	// copy, which matters to anyone reading the brain directory.
	Extracted bool `json:"extracted,omitempty"`
}

type SeedCommand struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Source  string `json:"source"`
}

type SeedHistoryBaseline struct {
	OldestCommitAt  *time.Time `json:"oldest_commit_at,omitempty"`
	OldestSessionAt *time.Time `json:"oldest_session_at,omitempty"`
	SeedRequired    bool       `json:"seed_required"`
	Reason          string     `json:"reason"`
	Confidence      string     `json:"confidence"`
}

type SeedHistoryCoverage struct {
	Path                    string              `json:"path"`
	TotalCommits            int                 `json:"total_commits"`
	PreSessionCommits       int                 `json:"pre_session_commits"`
	CoveredCommits          int                 `json:"covered_commits"`
	MissingSessionCommits   int                 `json:"missing_session_commits"`
	NoSessionHistoryCommits int                 `json:"no_session_history_commits"`
	MergeCommits            int                 `json:"merge_commits"`
	OldestSessionAt         *time.Time          `json:"oldest_session_at,omitempty"`
	ExportedCheckpoints     int                 `json:"exported_checkpoints"`
	UncoveredCommits        []SeedCoveredCommit `json:"uncovered_commits,omitempty"`
	// UncoveredCommitsTotal / UncoveredCommitsTruncated are how the manifest
	// copy of this record admits it is a sample. They are omitempty, so a
	// coverage record that carries every uncovered commit is byte-identical to
	// what previous builds wrote; they appear only when rows were dropped, and
	// then they say how many there were. See manifestSeedHistoryCoverage.
	UncoveredCommitsTotal     int    `json:"uncovered_commits_total,omitempty"`
	UncoveredCommitsTruncated bool   `json:"uncovered_commits_truncated,omitempty"`
	GeneratedFrom             string `json:"generated_from"`
}

type SeedCoveredCommit struct {
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

type SeedAgent struct {
	Mode  string         `json:"mode"`
	Quick SeedAgentPhase `json:"quick"`
	Deep  SeedAgentPhase `json:"deep"`
}

type SeedAgentPhase struct {
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

// This descriptor is resolved for each operation, never persisted. Graph owns
// the filename, schema, cache identity and lifecycle. Brain opens only its
// versioned views, read-only; no graph rows are copied into Brain storage.
type GraphStoreDescriptor struct {
	BrainDir         string         `json:"brain_dir,omitempty"`
	ContractVersion  int            `json:"contract_version"`
	Path             string         `json:"path"`
	EntryDigest      string         `json:"entry_digest"`
	Commit           string         `json:"commit"`
	Tree             string         `json:"tree"`
	Header           SemanticHeader `json:"header"`
	Files            int            `json:"files"`
	Symbols          int            `json:"symbols"`
	Relations        int            `json:"relations"`
	Externals        int            `json:"externals"`
	StoreUnavailable bool           `json:"store_unavailable"`
	Detail           string         `json:"detail"`
}

// DistillTokenUsageSummary is persisted with the fact-source manifest. Calls
// includes successful and failed provider attempts, matching TotalAgentCalls.
// Input and output totals are provider-reported; cached/reasoning fields remain
// separate because providers disagree on whether they are subsets.
type DistillTokenUsageSummary struct {
	Source                   string `json:"source,omitempty"`
	Calls                    int    `json:"calls"`
	CallsReported            int    `json:"calls_reported"` // both input and output present
	CallsPartial             int    `json:"calls_partial"`  // at least one, but not both
	CallsMissing             int    `json:"calls_missing"`  // no provider token fields
	Complete                 bool   `json:"complete"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens,omitempty"`
	CachedInputTokens        int64  `json:"cached_input_tokens,omitempty"`
	ReasoningOutputTokens    int64  `json:"reasoning_output_tokens,omitempty"`
	// Total excludes CachedInputTokens and ReasoningOutputTokens because they
	// are subsets of input/output. Claude's separately billed cache creation/read
	// categories are included alongside uncached input and output.
	TotalTokens int64 `json:"total_tokens"`
}
