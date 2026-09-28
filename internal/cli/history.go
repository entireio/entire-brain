package cli

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
)

const (
	historyDirName           = "history"
	historyIndexFileName     = "index.json"
	historyIndexPath         = historyDirName + "/" + historyIndexFileName
	historyGenerationsDir    = historyDirName + "/generations"
	historyStagingDir        = historyDirName + "/staging"
	historyScanCacheFileName = "scan-cache.json.gz"
	historyScanCachePath     = historyDirName + "/" + historyScanCacheFileName
	historyMaxLineBytes      = 1024 * 1024
	// historyScanCacheVersion gates reuse of cached per-file scan results.
	// Bump it whenever the history record extraction/classification logic
	// changes so stale cached records are discarded on the next refresh.
	// v2: index user prompts as "request" records.
	// v3: stop EXTRACTING request records during scan (the v2 indexing) — they were
	// excluded from general ranking (measured noise) and the only surfaces that
	// could query them (inspect requests / brain_history kind=requests) were
	// removed. The request-kind gates/filtering are kept as defensive support for
	// stale indexes; bumping discards per-file scan caches that still hold request
	// records so they stop reappearing on refresh.
	// v4: RE-introduce request extraction (wrapper-filtered, via
	// transcriptUserText). Two Phase 2 consumers reopened the v3 closed
	// question with new evidence: `brief --handoff` needs each session's
	// opening request and the history eval's midtask stratum mines follow-up
	// questions as queries. The v3 noise concern is unchanged and unaffected:
	// both rankers still exclude kind=request from general ranking.
	// v5: extract experimental conversation "exchange" records (conversation.go)
	// during the scan; older caches do not contain them.
	// v6: conversation-scoped wrapper filtering (hook-injected pseudo-requests)
	// and API-error narrative exclusion changed exchange extraction; cached v5
	// exchanges would keep the noise.
	// v7: bind cache reuse to the transcript content digest; size+mtime alone
	// misses atomic same-metadata leaf replacement.
	// v8: TWO independent v7 formats were developed in parallel (one keyed on
	// content_digest from the descriptor-rooted inventory, one on content_sha256
	// with pre-annotation raw records). They are deliberately invalidated rather
	// than guessed apart, exactly as the two v6 formats were.
	// v9: two independent v8s merged. One lineage's v8 switched code-fact
	// extraction from a fixed phrase list to the structural signal
	// (declarations, documented flag defaults, literal assignments, file:line
	// anchors) with a matching pre-decode line filter; the other's v8 was the
	// parallel-format invalidation above. A warm v8 cache from either lineage
	// would silently keep that lineage's behavior, so both are invalidated.
	// v10: admit all transcript JSON for authoritative extraction and include
	// wrapper-filtered user requests from document transcripts.
	historyScanCacheVersion = 10
)

type historySourceManifest struct {
	GeneratedAt time.Time `json:"generated_at"`
	IndexPath   string    `json:"index_path"`
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

type historyIndex struct {
	GeneratedAt time.Time       `json:"generated_at"`
	Records     []historyRecord `json:"records"`
	Warnings    []string        `json:"warnings,omitempty"`
	// storageIdentity is the manifest-addressed immutable generation path when
	// this index was loaded from one. It is not serialized into the index; local
	// accelerators use it to distinguish equal-time/equal-count generations.
	storageIdentity string
	// contentIdentity hashes the bounded bytes read from disk. It detects local
	// corruption/tampering within an otherwise immutable generation path.
	contentIdentity string
	// recordsFingerprint is populated after build/load validation so the FTS
	// freshness check stays O(1). Any caller that replaces Records must clear it.
	recordsFingerprint string
}

type historyRecord struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Branch  string   `json:"branch,omitempty"`
	Path    string   `json:"path"`
	Line    int      `json:"line"`
	Summary string   `json:"summary"`
	Terms   []string `json:"terms,omitempty"`

	// Experimental conversation-exchange extension (kind "exchange", see
	// conversation.go). All fields are additive and omitted for classic
	// records, so the established history JSON is byte-identical for them.
	// Summary holds the bounded deterministic search projection; the only
	// conversation body stored in the index; full text lives only in the
	// exported transcript and is re-parsed by get.
	SessionID   string `json:"session_id,omitempty"`
	Agent       string `json:"agent,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"` // RFC3339 session time
	EndLine     int    `json:"end_line,omitempty"`   // inclusive 1-based range end
	TurnOrdinal int    `json:"turn_ordinal,omitempty"`
	// RangeIncomplete is set when the exact source range could not be proven
	// (range_complete=false in the contract; inverted so omitempty works).
	RangeIncomplete     bool     `json:"range_incomplete,omitempty"`
	ContentRole         string   `json:"content_role,omitempty"` // "historical_evidence"
	ProjectionTruncated bool     `json:"projection_truncated,omitempty"`
	SourceDigest        string   `json:"source_digest,omitempty"` // transcript digest at extraction
	RequestDigest       string   `json:"request_digest,omitempty"`
	IdentityDegraded    bool     `json:"identity_degraded,omitempty"`
	ToolNames           []string `json:"tool_names,omitempty"`
}

type scoredHistoryRecord struct {
	Record historyRecord
	Score  int
	Order  int
}

type historyFragment struct {
	Text   string
	Source string
}

type historySessionFile struct {
	Path        string
	Rel         string
	SortTime    time.Time
	Size        int64
	ModUnixNano int64
	// ContentDigest is filled in once the descriptor-rooted read has verified
	// the file, so the transcripts fingerprint reuses that single read instead
	// of hashing the corpus a second time.
	ContentDigest string
	info          os.FileInfo
}

// historyProjectionIdentity retains both the serializable state identities and
// the exact descriptor-rooted Brain/session-tree membership observed during
// capture. os.SameFile comparisons catch same-size/same-mtime inode replacement
// of the root, directories, supported transcripts, and irrelevant entries.
type historyProjectionIdentity struct {
	SessionsFingerprint string
	SessionFiles        string
	SessionContents     string
	Tombstones          string
	Overlay             string
	brainRoot           os.FileInfo
	sessionEntries      []historySessionTreeEntry
}

func (i historyProjectionIdentity) same(other historyProjectionIdentity) bool {
	return i.SessionContents == other.SessionContents && i.sameMetadata(other)
}

func (i historyProjectionIdentity) sameMetadata(other historyProjectionIdentity) bool {
	if i.SessionsFingerprint != other.SessionsFingerprint || i.SessionFiles != other.SessionFiles || i.Tombstones != other.Tombstones || i.Overlay != other.Overlay {
		return false
	}
	return sameHistorySessionMembership(i.brainRoot, i.sessionEntries, other.brainRoot, other.sessionEntries)
}

// preparedHistoryProjection is an immutable build-beside result. Its history
// index and receipt set are written to generation-addressed, unreferenced paths
// during preparation. Publishing only revalidates identity and atomically
// switches manifest.json to those paths while holding the Brain write lock.
type preparedHistoryProjection struct {
	identity    historyProjectionIdentity
	index       historyIndex
	source      historySourceManifest
	cacheData   []byte
	indexPath   string
	receiptPath string
	stagingDir  string
	generation  string
}

type historyProjectionPublishGuard func() error

var historyProjectionWriteFile = writeBrainRelativeFileAtomic

// beforeHistoryProjectionContentDigest is a deterministic audit seam used to
// prove the commit boundary never performs O(corpus bytes) work.
var beforeHistoryProjectionContentDigest = func(string) {}

// historyScanCache memoizes parsed records for each session transcript. Reuse
// requires size, mtime, and the digest of the exact descriptor-rooted bytes;
// the digest prevents atomic same-metadata replacement from serving stale
// searchable records. Unchanged files still skip parsing, which is the costly
// part of refresh even though the bytes are streamed once for identity.
type historyScanCache struct {
	Version         int                              `json:"version"`
	Files           map[string]historyScanCacheEntry `json:"files"`
	contentIdentity string
}

type historyScanCacheEntry struct {
	Size          int64           `json:"size"`
	ModUnixNano   int64           `json:"mod_unix_nano"`
	ContentDigest string          `json:"content_digest"`
	Records       []historyRecord `json:"records"`
	// IncompleteExchanges preserves the per-file diagnostic (exchanges opened
	// with no visible assistant narrative) across cache reuse.
	IncompleteExchanges int `json:"incomplete_exchanges,omitempty"`
}

func newHistoryIndexCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history [path]",
		Short: "Rebuild the decision/rationale history index from exported session transcripts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runHistoryIndex(cmd.Context(), cmd, opts, target)
		},
	}
	return cmd
}

func runHistoryIndex(ctx context.Context, cmd *cobra.Command, opts Options, target string) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("refresh history requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	source, err := writeBrainHistoryIndexAndSourceContext(ctx, storage.BrainDir, opts.Now().UTC(), nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "indexed %d history records: %s\n", source.Records, filepath.Join(storage.BrainDir, filepath.FromSlash(source.IndexPath)))
	// History vectors ride the history stage (standalone here, and the same
	// sequence inside the full `refresh`): only under the ENTIRE_BRAIN_EMBEDDER
	// opt-in, see history_vec.go for the gate and sizing rationale.
	if strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_EMBEDDER")) != "" {
		e := historySemanticEmbedder(defaultEmbedder())
		store, storeOK := historyVectorStoreFor(storage.BrainDir, e)
		switch {
		case e == nil:
			fmt.Fprintln(cmd.OutOrStdout(), "history vectors: skipped (embed server unavailable)")
		case !storeOK:
			fmt.Fprintln(cmd.OutOrStdout(), "history vectors: skipped (requires the brain_cgo build)")
		default:
			_ = store // availability was checked above; the guarded writer reopens it.
			vectors, serr := syncMemoryProjectionVectorsFully(ctx, storage.BrainDir, opts.Now().UTC(), e, func(progress memoryVectorSyncPass) {
				fmt.Fprintf(cmd.ErrOrStderr(), "history vectors: %d new %s embedded\n", progress.HistoryAdded, pluralUnit("record", progress.HistoryAdded))
			})
			if serr != nil {
				return serr
			}
			fmt.Fprintf(cmd.OutOrStdout(), "history vectors: %d embedded, %d pruned (%d total)\n", vectors.HistoryAdded, vectors.HistoryDropped, vectors.HistoryTotal)
			fmt.Fprintf(cmd.OutOrStdout(), "conversation vectors: %d embedded, %d pruned (%d total)\n", vectors.ConversationAdded, vectors.ConversationDrop, vectors.ConversationTotal)
		}
	}
	return nil
}

// historyIndexProgress reports incremental progress while the history index is
// rebuilt. done counts session files processed so far out of total. It is
// optional; pass nil when no progress reporting is needed.
type historyIndexProgress func(done, total int)

func writeBrainHistoryIndexAndSource(outputDir string, now time.Time, progress historyIndexProgress) (*historySourceManifest, error) {
	return writeBrainHistoryIndexAndSourceContext(context.Background(), outputDir, now, progress)
}

func writeBrainHistoryIndexAndSourceContext(ctx context.Context, outputDir string, now time.Time, progress historyIndexProgress) (*historySourceManifest, error) {
	prepared, err := prepareBrainHistoryProjectionContext(ctx, outputDir, now, progress)
	if err != nil {
		return nil, err
	}
	var source *historySourceManifest
	err = withBrainWriteLock(outputDir, func() error {
		var runErr error
		source, runErr = publishBrainHistoryProjectionLocked(outputDir, prepared, nil)
		return runErr
	})
	if err == nil {
		finalizePreparedHistoryProjection(outputDir, prepared)
	}
	return source, err
}

// writeBrainHistoryIndexAndSourceLocked is the compatibility path for callers
// that already hold the Brain lock. New worker/maintenance paths must use the
// explicit prepare/publish API so transcript scanning never monopolizes the
// lock. This wrapper cannot provide that property because its caller chose the
// lock boundary before entering it.
func writeBrainHistoryIndexAndSourceLocked(outputDir string, now time.Time, progress historyIndexProgress) (*historySourceManifest, error) {
	prepared, err := prepareBrainHistoryProjection(outputDir, now, progress)
	if err != nil {
		return nil, err
	}
	source, err := publishBrainHistoryProjectionLocked(outputDir, prepared, nil)
	if err == nil {
		finalizePreparedHistoryProjectionLocked(outputDir, prepared)
	}
	return source, err
}

// prepareBrainHistoryProjection performs every source scan and all JSON/gzip
// encoding without the Brain write lock. The generated files are immutable and
// unreferenced until manifest.json commits them, so a crash or cancellation in
// this phase leaves the previously published projection readable.
func prepareBrainHistoryProjection(outputDir string, now time.Time, progress historyIndexProgress) (*preparedHistoryProjection, error) {
	return prepareBrainHistoryProjectionContext(context.Background(), outputDir, now, progress)
}

func prepareBrainHistoryProjectionContext(ctx context.Context, outputDir string, now time.Time, progress historyIndexProgress) (*preparedHistoryProjection, error) {
	// The build itself reads and hashes every non-excluded transcript. The
	// opening capture therefore needs only the exact metadata membership; the
	// strong post-capture below binds the build digest without a redundant full
	// corpus read.
	identity, manifest, stones, err := captureHistoryProjectionIdentityMode(ctx, outputDir, false)
	if err != nil {
		return nil, err
	}
	index, source, cache, err := buildBrainHistoryIndexSnapshotContext(ctx, outputDir, now, progress, manifest, stones)
	if err != nil {
		return nil, err
	}
	identity.SessionContents = cache.contentIdentity
	source.PrivacyIdentity = identity.Tombstones
	receipts := buildProjectionReceiptsFromSnapshot(outputDir, manifest, stones, index, now)
	after, _, _, err := captureHistoryProjectionIdentityContext(ctx, outputDir)
	if err != nil {
		return nil, err
	}
	if !identity.same(after) {
		return nil, errors.New("memory_source_stale: sessions, privacy state, or short-term overlay changed while preparing history")
	}
	// Retain the exact post-scan descriptor/root/tree snapshot for the short
	// commit revalidation. The content aggregate already equals the immutable
	// bytes parsed by the build.
	identity = after
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, err
	}
	indexData = append(indexData, '\n')
	indexSum := sha256.Sum256(indexData)
	indexDigest := "sha256:" + hex.EncodeToString(indexSum[:])
	receiptData, receiptDigest, err := encodeProjectionState(receipts)
	if err != nil {
		return nil, err
	}
	generationHash := sha256.New()
	_, _ = generationHash.Write(indexData)
	_, _ = generationHash.Write(receiptData)
	generation := hex.EncodeToString(generationHash.Sum(nil))[:40]
	indexPath := historyGenerationArtifactPath(generation, historyIndexFileName)
	receiptPath := historyGenerationArtifactPath(generation, projectionStateFileName)
	stagingDir := filepath.ToSlash(filepath.Join(historyStagingDir, generation))
	stagedIndexPath := filepath.ToSlash(filepath.Join(stagingDir, historyIndexFileName))
	stagedReceiptPath := filepath.ToSlash(filepath.Join(stagingDir, projectionStateFileName))
	if err := historyProjectionWriteFile(outputDir, stagedIndexPath, indexData, 0o600); err != nil {
		return nil, fmt.Errorf("stage history index: %w", err)
	}
	if err := historyProjectionWriteFile(outputDir, stagedReceiptPath, receiptData, 0o600); err != nil {
		_ = removeHistoryProjectionDirectory(outputDir, stagingDir)
		return nil, fmt.Errorf("stage projection receipts: %w", err)
	}
	cacheData, _ := encodeHistoryScanCache(cache)
	source.IndexPath = indexPath
	source.IndexDigest = indexDigest
	// Publish the byte/record identities alongside the generation digest. They
	// are what lets a reader rank straight from the FTS payload table without
	// loading index.json, what keys the O(1) FTS freshness check, and what
	// historyIndexCurrent uses to skip an unnecessary rebuild. Omitting them on
	// the generation-addressed path silently degraded every one of those to the
	// slow path.
	source.IndexBytes = int64(len(indexData))
	source.IndexSHA256 = historyIndexBytesFingerprint(indexData)
	source.RecordsFingerprint = historyRecordsFingerprint(index.Records)
	index.recordsFingerprint = source.RecordsFingerprint
	source.ProjectionStatePath = receiptPath
	source.ProjectionStateDigest = receiptDigest
	return &preparedHistoryProjection{
		identity: identity, index: index, source: *source, cacheData: cacheData,
		indexPath: indexPath, receiptPath: receiptPath, stagingDir: stagingDir, generation: generation,
	}, nil
}

// publishBrainHistoryProjectionLocked is the short commit boundary. Caller
// holds the Brain write lock. guard is checked both before validation and at
// the final linearization point; the coordinator uses it for cancellation.
func publishBrainHistoryProjectionLocked(outputDir string, prepared *preparedHistoryProjection, guard historyProjectionPublishGuard) (*historySourceManifest, error) {
	if prepared == nil {
		return nil, errors.New("history projection preparation is missing")
	}
	committed := false
	defer func() {
		if !committed {
			_ = discardPreparedHistoryProjection(outputDir, prepared)
		}
	}()
	if guard != nil {
		if err := guard(); err != nil {
			return nil, err
		}
	}
	current, _, _, err := captureHistoryProjectionMetadataIdentity(outputDir)
	if err != nil {
		return nil, err
	}
	if !current.sameMetadata(prepared.identity) {
		return nil, errors.New("memory_source_stale: sessions, privacy state, or short-term overlay changed before history commit")
	}
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		_, receiptState, receiptErr := loadProjectionStateChecked(outputDir, manifest.Sources.History)
		if receiptState == projectionStateUnsupported || receiptState == projectionStateUnsafe {
			if receiptErr != nil {
				return nil, receiptErr
			}
			return nil, fmt.Errorf("active history projection receipts are %s and read-only", receiptState)
		}
	}
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	source := prepared.source
	manifest.Sources.History = &source
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = source.GeneratedAt
	}
	if guard != nil {
		if err := guard(); err != nil {
			return nil, err
		}
	}
	current, _, _, err = captureHistoryProjectionMetadataIdentity(outputDir)
	if err != nil {
		return nil, err
	}
	if !current.sameMetadata(prepared.identity) {
		return nil, errors.New("memory_source_stale: sessions, privacy state, or short-term overlay changed at history commit")
	}
	normalizeBrainManifest(manifest)
	// Normalization may fill default fields on the canonical sessions source.
	// Publish the fingerprint of the exact normalized source written below so
	// checked receipt readers do not mistake our own commit for source drift.
	if manifest.Sources != nil && manifest.Sources.History != nil {
		manifest.Sources.History.SessionsFingerprint = sessionSourceFingerprint(manifest.Sources.Sessions)
		source = *manifest.Sources.History
	}
	manifest.SchemaVersion = brainManifestSchemaVersion
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", exportManifestFileName, err)
	}
	if err := promotePreparedHistoryProjection(outputDir, prepared); err != nil {
		return nil, err
	}
	if err := withBrainManifestWriteLock(outputDir, func() error {
		// Reclassify the exact manifest leaf while every cooperative manifest
		// writer is excluded. The projection's outer Brain lock supplies its
		// source/identity serialization; this leaf lock prevents a different
		// writer family from down-converting vNext state at the commit point.
		// The strict reader is what makes that refusal happen: the bytes below
		// are marshalled from a struct, so a field it could not name would be
		// erased here.
		if _, err := loadBrainManifestForReplace(outputDir); err != nil {
			return err
		}
		return historyProjectionWriteFile(outputDir, exportManifestFileName, append(manifestData, '\n'), 0o600)
	}); err != nil {
		return nil, fmt.Errorf("write %s: %w", exportManifestFileName, err)
	}
	committed = true
	// README is descriptive, not a projection pointer. Refresh it best-effort
	// after the manifest linearization point; a crash here can leave stale prose
	// but never a torn readable projection or a false operation failure.
	_ = historyProjectionWriteFile(outputDir, exportReadmeFileName, []byte(renderBrainReadme(*manifest)), 0o600)
	// The overlay is no longer readable after the manifest switch because its
	// base generation pin differs. Clear it when it represented the source this
	// build absorbed; a crash here only leaves a harmless stale overlay.
	if overlay, state := loadHistoryShortTermRaw(outputDir); state == shortTermStateAbsent || state == shortTermStateCorrupt ||
		overlay.SessionsFingerprint == "" || overlay.SessionsFingerprint == source.SessionsFingerprint {
		clearHistoryShortTerm(outputDir)
	}
	return &source, nil
}

// finalizePreparedHistoryProjection writes disposable accelerators after the
// manifest commit. Pruning briefly reacquires the write lock so a later writer
// cannot make a generation active between the manifest check and quarantine.
// FTS remains lazy: its fingerprint makes the old store unreadable for the new
// index until a query rebuilds it.
func finalizePreparedHistoryProjection(outputDir string, prepared *preparedHistoryProjection) {
	if prepared == nil {
		return
	}
	if len(prepared.cacheData) > 0 {
		_ = writeBrainRelativeFileAtomic(outputDir, historyScanCachePath, prepared.cacheData, 0o600)
	}
	_ = withBrainWriteLock(outputDir, func() error {
		pruneInactiveHistoryGenerations(outputDir)
		return nil
	})
}

// finalizePreparedHistoryProjectionLocked is for the compatibility callers
// that already own the Brain write lock.
func finalizePreparedHistoryProjectionLocked(outputDir string, prepared *preparedHistoryProjection) {
	if prepared == nil {
		return
	}
	if len(prepared.cacheData) > 0 {
		_ = writeBrainRelativeFileAtomic(outputDir, historyScanCachePath, prepared.cacheData, 0o600)
	}
	pruneInactiveHistoryGenerations(outputDir)
}

func promotePreparedHistoryProjection(outputDir string, prepared *preparedHistoryProjection) error {
	if prepared == nil || prepared.stagingDir == "" || prepared.generation == "" {
		return errors.New("history projection staging identity is missing")
	}
	if !validHistoryGenerationArtifactPath(prepared.indexPath, historyIndexFileName) ||
		!validHistoryGenerationArtifactPath(prepared.receiptPath, projectionStateFileName) {
		return errors.New("history projection generation path is invalid")
	}
	if err := recoverHistoryProjectionQuarantines(outputDir); err != nil {
		return fmt.Errorf("recover interrupted history projection cleanup: %w", err)
	}
	finalRel := filepath.ToSlash(filepath.Join(historyGenerationsDir, prepared.generation))
	wantStagingRel := filepath.ToSlash(filepath.Join(historyStagingDir, prepared.generation))
	if prepared.stagingDir != wantStagingRel || !validHistoryProjectionDirectoryPath(prepared.stagingDir) || !validHistoryProjectionDirectoryPath(finalRel) {
		return errors.New("history projection directory path is invalid")
	}
	staging := filepath.Join(outputDir, filepath.FromSlash(prepared.stagingDir))
	final := filepath.Join(outputDir, filepath.FromSlash(finalRel))
	if err := rejectExistingSymlinkPathComponents(outputDir, historyStagingDir); err != nil {
		return err
	}
	if err := rejectExistingSymlinkPathComponents(outputDir, historyGenerationsDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	if err := validateHistoryProjectionDirectory(outputDir, prepared.stagingDir, true); err != nil {
		return fmt.Errorf("validate staged history generation: %w", err)
	}
	if info, err := os.Lstat(final); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("history projection generation must be an exact directory, not a symlink or special file: %s", finalRel)
		}
		manifest, manifestErr := loadBrainManifest(outputDir)
		if manifestErr != nil {
			return manifestErr
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			currentSource := manifest.Sources.History
			referencesFinal := filepath.ToSlash(currentSource.IndexPath) == prepared.indexPath || filepath.ToSlash(currentSource.ProjectionStatePath) == prepared.receiptPath
			if referencesFinal {
				if _, indexErr := loadBrainHistoryIndex(outputDir, currentSource); indexErr != nil {
					return fmt.Errorf("current history generation is unreadable and was left untouched: %w", indexErr)
				}
				if _, receiptState, receiptErr := loadProjectionStateChecked(outputDir, currentSource); receiptState != projectionStateCurrent {
					if receiptErr != nil {
						return receiptErr
					}
					return fmt.Errorf("current history generation has %s projection receipts and was left untouched", receiptState)
				}
				if err := removeHistoryProjectionDirectory(outputDir, prepared.stagingDir); err != nil {
					return fmt.Errorf("discard duplicate staged history generation: %w", err)
				}
				return nil
			}
		}
		// An unreferenced directory is an orphan from an interrupted publish.
		// Quarantine and remove it only after proving that it contains no
		// symlinks, special files, or unknown content.
		if err := removeHistoryProjectionDirectory(outputDir, finalRel); err != nil {
			return fmt.Errorf("replace orphaned history generation: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Re-check immediately before the rename. If an untrusted actor replaced
	// staging after preparation, the manifest must remain on the prior
	// generation and no path below the replacement may be touched.
	if err := validateHistoryProjectionDirectory(outputDir, prepared.stagingDir, true); err != nil {
		return fmt.Errorf("validate staged history generation at promotion: %w", err)
	}
	if err := os.Rename(staging, final); err != nil {
		return fmt.Errorf("promote history generation: %w", err)
	}
	if err := validateHistoryProjectionDirectory(outputDir, finalRel, true); err != nil {
		return fmt.Errorf("validate promoted history generation: %w", err)
	}
	return nil
}

func discardPreparedHistoryProjection(outputDir string, prepared *preparedHistoryProjection) error {
	if prepared == nil || prepared.stagingDir == "" {
		return nil
	}
	var cleanupErr error
	if err := removeHistoryProjectionDirectory(outputDir, prepared.stagingDir); err != nil {
		cleanupErr = err
	}
	manifest, err := loadBrainManifest(outputDir)
	if err == nil && manifest.Sources != nil && manifest.Sources.History != nil &&
		filepath.ToSlash(manifest.Sources.History.IndexPath) == prepared.indexPath {
		return cleanupErr
	}
	finalRel := filepath.ToSlash(filepath.Join(historyGenerationsDir, prepared.generation))
	if err := removeHistoryProjectionDirectory(outputDir, finalRel); err != nil && cleanupErr == nil {
		cleanupErr = err
	}
	return cleanupErr
}

func pruneInactiveHistoryGenerations(outputDir string) {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		return
	}
	// Never enumerate or remove below an aliased history directory. This check
	// covers both legacy fixed-path cleanup and the generations root.
	if err := rejectExistingSymlinkPathComponents(outputDir, historyDirName); err != nil {
		return
	}
	if err := recoverHistoryProjectionQuarantines(outputDir); err != nil {
		return
	}
	active := filepath.ToSlash(strings.TrimSpace(manifest.Sources.History.IndexPath))
	activeReceipt := filepath.ToSlash(strings.TrimSpace(manifest.Sources.History.ProjectionStatePath))
	if active != historyIndexPath {
		_ = os.Remove(filepath.Join(outputDir, filepath.FromSlash(historyIndexPath)))
	}
	if activeReceipt != projectionStateRel {
		legacyReceipt := filepath.Join(outputDir, filepath.FromSlash(projectionStateRel))
		newer, schemaErr := projectionFileHasNewerSchema(legacyReceipt)
		if schemaErr == nil && !newer {
			_ = os.Remove(legacyReceipt)
		}
	}
	activeParts := strings.Split(active, "/")
	if len(activeParts) != 4 || !validHistoryGenerationArtifactPath(active, historyIndexFileName) {
		return
	}
	if err := rejectSymlinkPathComponents(outputDir, historyGenerationsDir); err != nil {
		return
	}
	directory, err := readMemoryStateDirectory(outputDir, historyGenerationsDir, "history generation directory", memoryStateInventoryMaxEntries)
	if err != nil || directory.Truncated {
		return
	}
	entries := directory.Entries
	for _, entry := range entries {
		generation := entry.Name()
		candidateIndex := historyGenerationArtifactPath(generation, historyIndexFileName)
		if generation == activeParts[2] || !validHistoryGenerationArtifactPath(candidateIndex, historyIndexFileName) {
			continue
		}
		dirRel := filepath.ToSlash(filepath.Join(historyGenerationsDir, generation))
		dir := filepath.Join(outputDir, filepath.FromSlash(dirRel))
		if err := validateHistoryProjectionDirectory(outputDir, dirRel, false); err != nil {
			continue
		}
		receiptPath := filepath.Join(dir, projectionStateFileName)
		newer, schemaErr := projectionFileHasNewerSchema(receiptPath)
		if schemaErr != nil || newer {
			continue // unknown-newer generations are never removed by this binary
		}
		_ = removeHistoryProjectionDirectory(outputDir, dirRel)
	}
}

func projectionFileHasNewerSchema(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("projection receipt must be a regular file and must not be a symlink: %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := rejectOpenFileAlias(path, f, "projection receipt"); err != nil {
		return false, err
	}
	data, err := safeReadAll(f, maxManifestBytes, path)
	if err != nil {
		return false, err
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.SchemaVersion < 1 {
		return false, fmt.Errorf("projection receipt has an unreadable schema header: %s", path)
	}
	return header.SchemaVersion > projectionSchemaVersion, nil
}

// validHistoryProjectionDirectoryPath accepts only an exact immutable
// generation or staging directory. Cleanup never operates on an arbitrary
// caller-supplied subtree.
func validHistoryProjectionDirectoryPath(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != historyDirName || (parts[1] != "generations" && parts[1] != "staging") || len(parts[2]) != 40 {
		return false
	}
	_, err := hex.DecodeString(parts[2])
	return err == nil
}

// validateHistoryProjectionDirectory uses Lstat for the directory and each
// child. Only the two immutable projection artifacts are recognized; a
// symlink, special file, or unknown name makes the directory untouchable.
func validateHistoryProjectionDirectory(outputDir, rel string, requireComplete bool) error {
	if !validHistoryProjectionDirectoryPath(rel) {
		return fmt.Errorf("invalid history projection directory: %s", rel)
	}
	parentRel := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	if err := rejectExistingSymlinkPathComponents(outputDir, parentRel); err != nil {
		return err
	}
	abs := filepath.Join(outputDir, filepath.FromSlash(rel))
	_, _, err := inspectHistoryProjectionDirectory(abs, rel, requireComplete)
	return err
}

func inspectHistoryProjectionDirectory(abs, display string, requireComplete bool) (os.FileInfo, []string, error) {
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil, fmt.Errorf("history projection path must be a directory and must not be a symlink: %s", display)
	}
	dir, err := os.OpenFile(abs, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		return nil, nil, fmt.Errorf("history projection directory changed while opening: %s", display)
	}
	entries, err := dir.ReadDir(3)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	found := map[string]bool{}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name != historyIndexFileName && name != projectionStateFileName {
			return nil, nil, fmt.Errorf("history projection directory contains unknown entry %q: %s", name, display)
		}
		childInfo, err := entry.Info()
		if err != nil {
			return nil, nil, err
		}
		if childInfo.Mode()&os.ModeSymlink != 0 || !childInfo.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("history projection artifact must be a regular file and must not be a symlink: %s", filepath.ToSlash(filepath.Join(display, name)))
		}
		found[name] = true
		names = append(names, name)
	}
	if requireComplete && (!found[historyIndexFileName] || !found[projectionStateFileName]) {
		return nil, nil, fmt.Errorf("history projection directory is incomplete: %s", display)
	}
	current, err := os.Lstat(abs)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, current) {
		return nil, nil, fmt.Errorf("history projection directory changed during inspection: %s", display)
	}
	return info, names, nil
}

// removeHistoryProjectionDirectory first renames the exact directory to an
// inert sibling. Renaming a hostile symlink moves the link itself rather than
// traversing its target; the subsequent Lstat validation therefore cannot
// delete an external canary. Unknown content is restored and left untouched.
func removeHistoryProjectionDirectory(outputDir, rel string) error {
	if !validHistoryProjectionDirectoryPath(rel) {
		return fmt.Errorf("refuse cleanup of invalid history projection directory: %s", rel)
	}
	parentRel := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel)))
	if err := rejectExistingSymlinkPathComponents(outputDir, parentRel); err != nil {
		return err
	}
	abs := filepath.Join(outputDir, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("refuse cleanup of symlink or non-directory history projection: %s", rel)
	}
	// Prove the SHAPE while the directory is still at its published path: a
	// projection generation holds exactly index.json and the receipt, both
	// regular files. This is the check that decides whether the directory is
	// safe to delete, and it runs before anything is moved.
	if _, _, err := inspectHistoryProjectionDirectory(abs, rel, false); err != nil {
		return err
	}
	quarantine := abs + ".removing"
	if _, err := os.Lstat(quarantine); err == nil {
		return fmt.Errorf("history projection cleanup quarantine already exists: %s", filepath.ToSlash(rel)+".removing")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(abs, quarantine); err != nil {
		return err
	}
	restore := func(cause error) error {
		if _, currentErr := os.Lstat(abs); os.IsNotExist(currentErr) {
			if renameErr := os.Rename(quarantine, abs); renameErr != nil {
				return fmt.Errorf("%v (also could not restore quarantined projection: %w)", cause, renameErr)
			}
		}
		return cause
	}
	// Re-prove the shape at the quarantine path. Identity is deliberately NOT
	// compared across the rename: os.Lstat resolves file identity lazily by
	// PATH on Windows, so the pre-rename FileInfo re-resolves against a path
	// that no longer exists and os.SameFile is unconditionally false there.
	// That made this cleanup a permanent no-op on Windows, which is what left
	// unreferenced generations behind for privacy verification to find. The
	// rename itself is the atomic step that carries identity: it moved exactly
	// the directory that was at abs, under the Brain write lock, or it failed.
	quarantineInfo, names, err := inspectHistoryProjectionDirectory(quarantine, filepath.ToSlash(rel)+".removing", false)
	if err != nil {
		return restore(err)
	}
	if err := removeKnownHistoryProjectionContents(quarantine, filepath.ToSlash(rel)+".removing", quarantineInfo, names); err != nil {
		return restore(err)
	}
	return nil
}

// removeKnownHistoryProjectionContents unlinks only the closed two-file set
// after revalidating the quarantined directory and each regular child. A raced
// unknown entry makes the final rmdir fail and remains available for diagnosis.
func removeKnownHistoryProjectionContents(abs, display string, expected os.FileInfo, names []string) error {
	for _, name := range names {
		current, _, err := inspectHistoryProjectionDirectory(abs, display, false)
		if err != nil {
			return err
		}
		if !os.SameFile(expected, current) {
			return fmt.Errorf("history projection changed during cleanup: %s", display)
		}
		child := filepath.Join(abs, name)
		childInfo, err := os.Lstat(child)
		if err != nil {
			return err
		}
		if childInfo.Mode()&os.ModeSymlink != 0 || !childInfo.Mode().IsRegular() {
			return fmt.Errorf("history projection artifact changed during cleanup: %s", filepath.ToSlash(filepath.Join(display, name)))
		}
		if err := os.Remove(child); err != nil {
			return err
		}
	}
	current, remaining, err := inspectHistoryProjectionDirectory(abs, display, false)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("history projection changed during cleanup: %s", display)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("history projection gained content during cleanup: %s", display)
	}
	return os.Remove(abs)
}

// recoverHistoryProjectionQuarantines completes or rolls back cleanup that
// stopped after the atomic rename. A manifest-referenced generation is
// restored; every other valid quarantine is deleted through the same closed
// two-file remover. Invalid or unknown contents fail closed.
func recoverHistoryProjectionQuarantines(outputDir string) error {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return err
	}
	active := map[string]bool{}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		source := manifest.Sources.History
		for _, artifact := range []struct {
			path string
			name string
		}{{source.IndexPath, historyIndexFileName}, {source.ProjectionStatePath, projectionStateFileName}} {
			path := filepath.ToSlash(strings.TrimSpace(artifact.path))
			if validHistoryGenerationArtifactPath(path, artifact.name) {
				active[filepath.ToSlash(filepath.Dir(path))] = true
			}
		}
	}
	for _, rootRel := range []string{historyGenerationsDir, historyStagingDir} {
		directory, err := readMemoryStateDirectory(outputDir, rootRel, "history projection root", memoryStateInventoryMaxEntries)
		if err != nil {
			return err
		}
		if !directory.Present {
			continue
		}
		if directory.Truncated {
			return fmt.Errorf("%s: history projection root %s exceeds the bounded inventory", memoryErrStateUnsafe, rootRel)
		}
		root := filepath.Join(outputDir, filepath.FromSlash(rootRel))
		entries := directory.Entries
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".removing") {
				continue
			}
			generation := strings.TrimSuffix(entry.Name(), ".removing")
			originalRel := filepath.ToSlash(filepath.Join(rootRel, generation))
			if !validHistoryProjectionDirectoryPath(originalRel) {
				return fmt.Errorf("invalid history projection cleanup quarantine: %s", filepath.ToSlash(filepath.Join(rootRel, entry.Name())))
			}
			quarantine := filepath.Join(root, entry.Name())
			requireComplete := active[originalRel]
			quarantineInfo, names, err := inspectHistoryProjectionDirectory(quarantine, filepath.ToSlash(filepath.Join(rootRel, entry.Name())), requireComplete)
			if err != nil {
				return err
			}
			original := filepath.Join(outputDir, filepath.FromSlash(originalRel))
			_, originalErr := os.Lstat(original)
			switch {
			case active[originalRel] && os.IsNotExist(originalErr):
				if err := os.Rename(quarantine, original); err != nil {
					return fmt.Errorf("restore active history projection %s: %w", originalRel, err)
				}
				if err := validateHistoryProjectionDirectory(outputDir, originalRel, true); err != nil {
					return fmt.Errorf("validate restored history projection %s: %w", originalRel, err)
				}
			case originalErr != nil && !os.IsNotExist(originalErr):
				return originalErr
			default:
				newer, schemaErr := projectionFileHasNewerSchema(filepath.Join(quarantine, projectionStateFileName))
				if schemaErr != nil || newer {
					// Downgrades and unreadable state are diagnostic, not cleanup
					// authority. Leave the quarantine intact for a compatible binary
					// or explicit repair.
					continue
				}
				if err := removeKnownHistoryProjectionContents(quarantine, filepath.ToSlash(filepath.Join(rootRel, entry.Name())), quarantineInfo, names); err != nil {
					return fmt.Errorf("finish interrupted history projection cleanup %s: %w", originalRel, err)
				}
			}
		}
	}
	return nil
}

func historyGenerationArtifactPath(generation, name string) string {
	return filepath.ToSlash(filepath.Join(historyGenerationsDir, generation, name))
}

func captureHistoryProjectionIdentity(outputDir string) (historyProjectionIdentity, *exportManifest, sessionTombstones, error) {
	return captureHistoryProjectionIdentityContext(context.Background(), outputDir)
}

func captureHistoryProjectionIdentityContext(ctx context.Context, outputDir string) (historyProjectionIdentity, *exportManifest, sessionTombstones, error) {
	return captureHistoryProjectionIdentityMode(ctx, outputDir, true)
}

func captureHistoryProjectionMetadataIdentity(outputDir string) (historyProjectionIdentity, *exportManifest, sessionTombstones, error) {
	return captureHistoryProjectionIdentityMode(context.Background(), outputDir, false)
}

// captureHistoryProjectionIdentityMode performs the complete bounded session
// inventory in both modes. Preparation additionally hashes non-excluded
// transcript bytes. Commit runs metadata-only under the Brain lock: supported
// exporters take that lock and publish transcripts by atomic replacement, so
// retained SameFile root/tree membership plus manifest/privacy/overlay identity
// binds the post-scan content snapshot without O(corpus bytes) lock hold time.
// A hostile local process that mutates an inode in place while forging its size
// and timestamps is local store compromise, outside the cooperative-store
// guarantee; canonical input is not a cross-process filesystem transaction.
func captureHistoryProjectionIdentityMode(ctx context.Context, outputDir string, includeContents bool) (historyProjectionIdentity, *exportManifest, sessionTombstones, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return historyProjectionIdentity{}, nil, sessionTombstones{}, err
	}
	inventory, err := collectHistorySessionInventory(ctx, outputDir)
	if err != nil {
		if errors.Is(err, errHistorySessionsRootMissing) {
			// This runs ahead of buildBrainHistoryIndexSnapshotContext in the
			// prepare path, so without the same empty-Brain branch it would
			// swallow that function's actionable guidance.
			return historyProjectionIdentity{}, nil, sessionTombstones{}, errors.New("session history missing; run `entire brain refresh sessions` first")
		}
		return historyProjectionIdentity{}, nil, sessionTombstones{}, err
	}
	defer inventory.Close()
	stones, tombstonesIdentity, err := historyProjectionTombstoneSnapshot(outputDir)
	if err != nil {
		return historyProjectionIdentity{}, nil, sessionTombstones{}, err
	}
	fileHash := sha256.New()
	for _, entry := range inventory.entries {
		if err := ctx.Err(); err != nil {
			return historyProjectionIdentity{}, nil, sessionTombstones{}, err
		}
		fmt.Fprintf(fileHash, "%s\x00%d\x00%d\x00%d\n", entry.Rel, uint32(entry.Mode), entry.Size, entry.ModUnixNano)
	}
	contentIdentity := ""
	if includeContents {
		excludedByPath, err := excludedTranscriptPathsChecked(outputDir, manifest, stones)
		if err != nil {
			return historyProjectionIdentity{}, nil, sessionTombstones{}, err
		}
		contentCache := historyScanCache{Files: make(map[string]historyScanCacheEntry, len(inventory.files))}
		for _, file := range inventory.files {
			if _, excluded := excludedByPath[file.Rel]; excluded {
				continue
			}
			digest, digestErr := digestHistorySessionInventoryFile(ctx, inventory, file)
			if digestErr != nil {
				return historyProjectionIdentity{}, nil, sessionTombstones{}, digestErr
			}
			contentCache.Files[file.Rel] = historyScanCacheEntry{ContentDigest: digest}
		}
		contentIdentity = historyScanCacheContentIdentity(contentCache)
	}
	overlayIdentity, err := historyProjectionFileIdentity(outputDir, historyShortTermPath, defaultMaxReadBytes)
	if err != nil {
		return historyProjectionIdentity{}, nil, sessionTombstones{}, err
	}
	return historyProjectionIdentity{
		SessionsFingerprint: func() string {
			if manifest.Sources == nil {
				return sessionSourceFingerprintAbsent
			}
			return sessionSourceFingerprint(manifest.Sources.Sessions)
		}(),
		SessionFiles:    "sha256:" + hex.EncodeToString(fileHash.Sum(nil)),
		SessionContents: contentIdentity,
		Tombstones:      tombstonesIdentity,
		Overlay:         overlayIdentity,
		brainRoot:       inventory.rootInfo,
		sessionEntries:  append([]historySessionTreeEntry(nil), inventory.entries...),
	}, manifest, stones, nil
}

func historyProjectionTombstoneSnapshot(outputDir string) (sessionTombstones, string, error) {
	stones, state, err := loadSessionTombstonesChecked(outputDir)
	if err != nil {
		return sessionTombstones{}, state.Identity, err
	}
	return stones, state.Identity, nil
}

func historyProjectionFileIdentity(outputDir, rel string, limit int64) (string, error) {
	data, present, err := readMemoryStateFile(outputDir, filepath.ToSlash(rel), "history projection input", limit)
	if err != nil {
		return "", err
	}
	if !present {
		return "absent", nil
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// historyRecordsFingerprint is the content identity shared by history/index.json
// and its derived indexes. It covers every record field in stable record order,
// while deliberately excluding generation time and warnings: neither changes
// retrieval content. Empty and nil term slices have the same JSON representation
// in index.json (omitempty), so canonicalize them to the same identity here too.
func historyRecordsFingerprint(records []historyRecord) string {
	h := sha256.New()
	_, _ = io.WriteString(h, "entire-brain/history-records/v1\x00")
	_, _ = io.WriteString(h, strconv.Itoa(len(records))+"\x00")
	for _, record := range records {
		if len(record.Terms) == 0 {
			record.Terms = nil
		}
		data, err := json.Marshal(record)
		if err != nil {
			return ""
		}
		_, _ = io.WriteString(h, strconv.Itoa(len(data))+":")
		_, _ = h.Write(data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func historyIndexBytesFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func buildBrainHistoryIndex(outputDir string, now time.Time, progress historyIndexProgress) (historyIndex, *historySourceManifest, error) {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return historyIndex{}, nil, err
	}
	stones, _, err := loadSessionTombstonesChecked(outputDir)
	if err != nil {
		return historyIndex{}, nil, err
	}
	index, source, cache, err := buildBrainHistoryIndexSnapshot(outputDir, now, progress, manifest, stones)
	if err == nil {
		saveHistoryScanCache(outputDir, cache)
	}
	return index, source, err
}

func buildBrainHistoryIndexSnapshot(outputDir string, now time.Time, progress historyIndexProgress, manifest *exportManifest, stones sessionTombstones) (historyIndex, *historySourceManifest, historyScanCache, error) {
	return buildBrainHistoryIndexSnapshotContext(context.Background(), outputDir, now, progress, manifest, stones)
}

func buildBrainHistoryIndexSnapshotContext(ctx context.Context, outputDir string, now time.Time, progress historyIndexProgress, manifest *exportManifest, stones sessionTombstones) (historyIndex, *historySourceManifest, historyScanCache, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	index := historyIndex{GeneratedAt: now}
	inventory, err := collectHistorySessionInventory(ctx, outputDir)
	if err != nil {
		if errors.Is(err, errHistorySessionsRootMissing) {
			return index, nil, historyScanCache{}, errors.New("session history missing; run `entire brain refresh sessions` first")
		}
		return index, nil, historyScanCache{}, err
	}
	defer inventory.Close()
	files := inventory.files

	prevCache := loadHistoryScanCache(outputDir)
	newCache := historyScanCache{Version: historyScanCacheVersion, Files: make(map[string]historyScanCacheEntry, len(files))}
	contentIdentities := historyScanCache{Files: make(map[string]historyScanCacheEntry, len(files))}
	branchByPath := historyBranchByTranscriptPath(manifest)
	sessionByPath := historySessionByTranscriptPath(manifest)
	// Session tombstones (Phase 4): excluded sessions are understood BEFORE
	// derived indexing; their transcripts are skipped entirely (no records,
	// no exchanges, no cache entry), counted without retaining content.
	excludedByPath, err := excludedTranscriptPathsChecked(outputDir, manifest, stones)
	if err != nil {
		return index, nil, historyScanCache{}, err
	}
	repoKey := ""
	if manifest != nil {
		repoKey = manifest.RepoKey
	}
	// The descriptor-rooted inventory above already enumerated every eligible
	// transcript once, so there is no separate candidate-collection pass.
	// total is that inventory.
	// Every candidate advances progress exactly once even when a later open or
	// identity check fails. Only complete identity-stable reads are fingerprinted;
	// only clean parses are cached and indexed. Walk warnings retain walk order,
	// while per-file failures follow this recency-sorted scan order.
	fingerprintedFiles := make([]historySessionFile, 0, len(files))

	total := len(files)
	if progress != nil {
		progress(0, total)
	}
	seenDecisions := map[string]struct{}{}
	// Re-exported sessions (the same session written under multiple transcript
	// paths) produce byte-identical exchanges with identical IDs; measured at
	// 17–31% of exchange records on real brains. Files iterate newest-first, so
	// keeping the first occurrence of each exchange ID retains the newest
	// export's copy (which also carries any appended assistant output).
	seenExchanges := map[historyRecordReplacementKey]struct{}{}
	incompleteExchanges := 0
	excludedSessions := map[string]struct{}{}
	for _, sessionID := range excludedByPath {
		excludedSessions[sessionID] = struct{}{}
	}
	for i, file := range files {
		if err := ctx.Err(); err != nil {
			return index, nil, historyScanCache{}, err
		}
		rel := file.Rel
		if sessionID, excluded := excludedByPath[rel]; excluded {
			excludedSessions[sessionID] = struct{}{}
			if progress != nil {
				progress(i+1, total)
			}
			continue
		}

		var records []historyRecord
		var incomplete int
		content, contentDigest, readErr := readHistorySessionInventoryFile(ctx, inventory, file)
		if readErr != nil {
			return index, nil, historyScanCache{}, readErr
		}
		contentIdentities.Files[rel] = historyScanCacheEntry{ContentDigest: contentDigest}
		// The transcripts fingerprint commits the SET that was successfully
		// read, independent of whether parsing then succeeded: a file whose
		// content is stable but whose lines are unparseable must not change the
		// fingerprint, or a permanent parse failure would force an endless
		// rebuild loop.
		file.ContentDigest = contentDigest
		fingerprintedFiles = append(fingerprintedFiles, file)
		// The content digest is the sole reuse authority: a restored mtime or a
		// same-size rewrite cannot resurrect stale records, and a touch that
		// leaves the bytes alone still reuses.
		if cached, ok := prevCache.Files[rel]; ok && cached.ContentDigest != "" && cached.ContentDigest == contentDigest {
			if err := inventory.validateFileMembership(ctx, file); err != nil {
				return index, nil, historyScanCache{}, err
			}
			// The cache is raw by construction (records are stored before
			// annotation, and the v8 bump invalidates every older format that
			// stored the annotated shape), so reuse must NOT strip: a Branch the
			// scanner itself read out of the transcript is real record data, not
			// manifest inference, and stripping it would let the manifest
			// overwrite transcript truth.
			records = cached.Records
			incomplete = cached.IncompleteExchanges
		} else {
			scanned, scannedIncomplete, warnings, ok := scanSessionFileReaderRecords(ctx, bytes.NewReader(content), file.Path, file.Rel)
			index.Warnings = append(index.Warnings, warnings...)
			if !ok {
				if progress != nil {
					progress(i+1, total)
				}
				continue
			}
			records = scanned
			incomplete = scannedIncomplete
		}
		// Only files that scanned cleanly (or were reused) are cached; a file
		// that errored is left out so the next refresh retries it. Branch and
		// conversation identity are build context derived from the MANIFEST, so
		// the cache stores raw scanner records and the annotations are applied
		// to a copy below. Caching the annotated shape would let a manifest
		// change hide behind a cache hit; both annotators copy, so the cached
		// slice stays raw. Reuse additionally strips annotations, so a cache
		// written by an older build cannot smuggle them back in either.
		newCache.Files[rel] = historyScanCacheEntry{
			ContentDigest:       contentDigest,
			Size:                file.Size,
			ModUnixNano:         file.ModUnixNano,
			Records:             records,
			IncompleteExchanges: incomplete,
		}
		records = annotateHistoryRecordBranches(records, rel, branchByPath)
		records = annotateConversationIdentity(records, rel, repoKey, sessionByPath)
		incompleteExchanges += incomplete

		for _, record := range records {
			if record.Kind == "decision" {
				dedupeKey := normalizeHistorySearchText(record.Summary)
				if _, ok := seenDecisions[dedupeKey]; ok {
					continue
				}
				seenDecisions[dedupeKey] = struct{}{}
			}
			if record.Kind == conversationKind {
				replacementKey := recordReplacementKey(record)
				if _, ok := seenExchanges[replacementKey]; ok {
					continue
				}
				seenExchanges[replacementKey] = struct{}{}
			}
			index.Records = append(index.Records, record)
		}
		if progress != nil {
			progress(i+1, total)
		}
	}
	if err := inventory.validateAll(ctx); err != nil {
		return index, nil, historyScanCache{}, err
	}
	newCache.contentIdentity = historyScanCacheContentIdentity(contentIdentities)
	sort.Slice(index.Records, func(i, j int) bool {
		if index.Records[i].Kind != index.Records[j].Kind {
			return index.Records[i].Kind < index.Records[j].Kind
		}
		if index.Records[i].Path != index.Records[j].Path {
			return index.Records[i].Path < index.Records[j].Path
		}
		return index.Records[i].Line < index.Records[j].Line
	})
	transcriptsFingerprint := historyTranscriptFilesFingerprint(fingerprintedFiles)
	source := &historySourceManifest{
		GeneratedAt:            now,
		IndexPath:              historyIndexPath,
		TranscriptsFingerprint: transcriptsFingerprint,
		SessionsFingerprint: func() string {
			if manifest == nil {
				return ""
			}
			if manifest.Sources == nil {
				return sessionSourceFingerprintAbsent
			}
			return sessionSourceFingerprint(manifest.Sources.Sessions)
		}(),
		Records:             len(index.Records),
		IncompleteExchanges: incompleteExchanges,
		ExcludedSessions:    len(excludedSessions),
		Warnings:            append([]string(nil), index.Warnings...),
	}
	for _, record := range index.Records {
		switch record.Kind {
		case "decision":
			source.Decisions++
		case "learning":
			source.Learnings++
		case "validation":
			source.Validations++
		case "tool_call":
			source.ToolCalls++
		case "code_fact":
			source.CodeFacts++
		case conversationKind:
			source.Exchanges++
		}
	}
	return index, source, newCache, nil
}

// digestHistorySessionInventoryFile binds cache reuse to the complete bounded
// contents of the exact descriptor-rooted leaf captured by the inventory. It
// streams into the hash so large line-oriented transcripts do not require a
// second in-memory copy.
func digestHistorySessionInventoryFile(ctx context.Context, inventory *historySessionInventory, file historySessionFile) (string, error) {
	beforeHistoryProjectionContentDigest(file.Rel)
	if file.Size > maxDocumentTranscriptBytes {
		return "", fmt.Errorf("%s: canonical transcript %s exceeds maximum size of %d bytes", memoryErrInputTooLarge, file.Rel, maxDocumentTranscriptBytes)
	}
	f, err := inventory.open(ctx, file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	limit := maxDocumentTranscriptBytes + 1
	written, readErr := io.Copy(h, io.LimitReader(contextCheckingReader{ctx: ctx, r: f}, limit))
	if written > maxDocumentTranscriptBytes {
		readErr = errors.Join(readErr, fmt.Errorf("%s: canonical transcript %s exceeds maximum size of %d bytes", memoryErrInputTooLarge, file.Rel, maxDocumentTranscriptBytes))
	}
	finishErr := inventory.validateAfterRead(file, f)
	if readErr != nil || finishErr != nil {
		return "", errors.Join(readErr, finishErr)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// collectHistorySessionDigests re-reads the canonical transcript set through
// the descriptor-rooted inventory and returns each included file with its
// verified content digest. It is the freshness-check counterpart of the index
// build, which collects the same digests from the single read it already
// performs.
func collectHistorySessionDigests(ctx context.Context, brainDir string, excludedByPath map[string]string) ([]historySessionFile, error) {
	inventory, err := collectHistorySessionInventory(ctx, brainDir)
	if err != nil {
		if errors.Is(err, errHistorySessionsRootMissing) {
			return nil, nil
		}
		return nil, err
	}
	defer inventory.Close()
	out := make([]historySessionFile, 0, len(inventory.files))
	for _, file := range inventory.files {
		if _, excluded := excludedByPath[file.Rel]; excluded {
			continue
		}
		_, digest, readErr := readHistorySessionInventoryFile(ctx, inventory, file)
		if readErr != nil {
			return nil, readErr
		}
		file.ContentDigest = digest
		out = append(out, file)
	}
	return out, nil
}

func readHistorySessionInventoryFile(ctx context.Context, inventory *historySessionInventory, file historySessionFile) ([]byte, string, error) {
	if file.Size > maxDocumentTranscriptBytes {
		return nil, "", fmt.Errorf("%s: canonical transcript %s exceeds maximum size of %d bytes", memoryErrInputTooLarge, file.Rel, maxDocumentTranscriptBytes)
	}
	f, err := inventory.open(ctx, file)
	if err != nil {
		return nil, "", err
	}
	data, readErr := safeReadAll(contextCheckingReader{ctx: ctx, r: f}, maxDocumentTranscriptBytes, "canonical transcript "+file.Rel)
	finishErr := inventory.validateAfterRead(file, f)
	closeErr := f.Close()
	if readErr != nil || finishErr != nil || closeErr != nil {
		combined := errors.Join(readErr, finishErr, closeErr)
		if readErr != nil && strings.Contains(readErr.Error(), "exceeds maximum size") {
			return nil, "", fmt.Errorf("%s: %w", memoryErrInputTooLarge, combined)
		}
		return nil, "", combined
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func historyScanCacheContentIdentity(cache historyScanCache) string {
	paths := make([]string, 0, len(cache.Files))
	for rel := range cache.Files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		fmt.Fprintf(h, "%s\x00%s\n", rel, cache.Files[rel].ContentDigest)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func historyScanFingerprintError(path string, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("history transcript scan returned no content fingerprint: %s", path)
}

// scanSessionFileRecords scans one exported transcript into classic history
// records plus conversation exchanges. ok=false means the whole file failed to
// scan (retried on the next build); a conversation-extraction failure only
// downgrades to classic records with a warning. Shared by the full (long-term)
// index build and the short-term delta path so both always extract
// identically.
func scanSessionInventoryFileRecords(ctx context.Context, inventory *historySessionInventory, file historySessionFile) (records []historyRecord, incomplete int, warnings []string, ok bool, containmentErr error) {
	data, _, err := readHistorySessionInventoryFile(ctx, inventory, file)
	if err != nil {
		if errors.Is(err, errHistorySessionInventoryDegraded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), memoryErrSourceStale) {
			return nil, 0, nil, false, err
		}
		// A size-bound failure means this projection cannot completely consume the
		// canonical input; it is a refusal, not a skippable parse warning.
		if strings.Contains(err.Error(), "exceeds maximum size") {
			return nil, 0, nil, false, err
		}
		return nil, 0, []string{err.Error()}, false, nil
	}
	records, incomplete, warnings, ok = scanSessionFileReaderRecords(ctx, bytes.NewReader(data), file.Path, file.Rel)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, nil, false, ctxErr
	}
	return records, incomplete, warnings, ok, nil
}

func scanSessionFileDescriptorRecords(ctx context.Context, f *os.File, path, rel string) (records []historyRecord, incomplete int, warnings []string, ok bool) {
	return scanSessionFileReaderRecords(ctx, f, path, rel)
}

func scanSessionFileReaderRecords(ctx context.Context, f io.ReadSeeker, path, rel string) (records []historyRecord, incomplete int, warnings []string, ok bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, []string{fmt.Sprintf("history records skipped for %s: %v", rel, err)}, false
	}
	scanned, scanErr := scanHistoryFileReader(ctx, f, rel, filepath.Ext(path))
	if scanErr != nil {
		// Name the transcript. A whole session drops out of the projection
		// here — one line over historyMaxLineBytes is enough — and the bare
		// error ("bufio.Scanner: token too long") named no file, so the only
		// evidence a reader had was a session count that silently disagreed
		// with the manifest. The conversation branch below already attributes
		// its failures; this one is the same class of loss, and larger.
		return nil, 0, []string{fmt.Sprintf("history records skipped for %s: %v", rel, scanErr)}, false
	}
	records = scanned
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, []string{fmt.Sprintf("history records skipped for %s: %v", rel, err)}, false
	}
	conversationScan, convErr := scanConversationTranscriptFileContext(ctx, f, path)
	if convErr != nil {
		warnings = append(warnings, fmt.Sprintf("conversation exchanges skipped for %s: %v", rel, convErr))
	} else {
		records = append(records, conversationExchangeRecords(rel, conversationScan)...)
		incomplete = conversationScan.Incomplete
	}
	return records, incomplete, warnings, true
}

// historySessionByTranscriptPath maps each exported transcript path to its
// session manifest entry so exchange records can carry session identity.
func historySessionByTranscriptPath(manifest *exportManifest) map[string]exportSession {
	out := map[string]exportSession{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return out
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		out[rel] = session
	}
	return out
}

// annotateConversationIdentity fills the manifest-derived identity of exchange
// records: session id, agent, session time, and the stable conversation: ID.
// Like branch annotation it only fills empty fields. The scan cache deliberately
// stores raw exchange records so identity is re-applied from the current manifest.
func annotateConversationIdentity(records []historyRecord, rel, repoKey string, sessionByPath map[string]exportSession) []historyRecord {
	needsAnnotation := false
	for i := range records {
		if records[i].Kind == conversationKind && records[i].ID == "" {
			needsAnnotation = true
			break
		}
	}
	if !needsAnnotation {
		return records
	}
	session, hasSession := sessionByPath[rel]
	out := append([]historyRecord(nil), records...)
	for i := range out {
		if out[i].Kind != conversationKind || out[i].ID != "" {
			continue
		}
		sessionID := ""
		if hasSession {
			sessionID = strings.TrimSpace(session.SessionID)
			if out[i].Agent == "" {
				out[i].Agent = session.Agent
			}
			if out[i].CreatedAt == "" && !session.CreatedAt.IsZero() {
				out[i].CreatedAt = session.CreatedAt.UTC().Format(time.RFC3339)
			}
		}
		out[i].SessionID = sessionID
		id, degraded := conversationExchangeID(repoKey, sessionID, out[i].TurnOrdinal, out[i].RequestDigest, out[i].SourceDigest)
		out[i].ID = id
		out[i].IdentityDegraded = degraded
	}
	return out
}

func historyBranchByTranscriptPath(manifest *exportManifest) map[string]string {
	out := map[string]string{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return out
	}
	defaultBranch := strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = strings.TrimSpace(manifest.DefaultBranch)
	}
	if defaultBranch == "" {
		defaultBranch = distillDefaultBranch
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		branch := strings.TrimSpace(session.Branch)
		if branch == "" {
			branch = defaultBranch
		}
		out[rel] = branch
	}
	return out
}

func annotateHistoryRecordBranches(records []historyRecord, rel string, branchByPath map[string]string) []historyRecord {
	branch := historyRecordBranchForPath(rel, branchByPath)
	if branch == "" {
		return records
	}
	out := append([]historyRecord(nil), records...)
	for i := range out {
		if out[i].Branch == "" {
			out[i].Branch = branch
		}
	}
	return out
}

func historyRecordBranchForPath(path string, branchByPath map[string]string) string {
	rel := filepath.ToSlash(strings.TrimSpace(path))
	if rel == "" {
		return ""
	}
	if branch := strings.TrimSpace(branchByPath[rel]); branch != "" {
		return branch
	}
	if strings.HasPrefix(rel, "sessions/branches/") {
		rest := strings.TrimPrefix(rel, "sessions/branches/")
		if idx := strings.Index(rest, "/"); idx > 0 {
			return rest[:idx]
		}
	}
	if strings.HasPrefix(rel, "sessions/") {
		rest := strings.TrimPrefix(rel, "sessions/")
		if idx := strings.Index(rest, "/"); idx > 0 {
			return rest[:idx]
		}
	}
	return ""
}

// loadHistoryScanCache reads the per-file scan cache. It always returns a
// usable (non-nil map) value: any read/parse error or a version mismatch
// yields an empty cache so the index simply rebuilds from scratch.
// The cache stores every indexed record for every session, which at scale is
// hundreds of megabytes of JSON. It is gzip-compressed on disk to bound the
// footprint (the record text compresses heavily).
func loadHistoryScanCache(outputDir string) historyScanCache {
	empty := historyScanCache{Version: historyScanCacheVersion, Files: map[string]historyScanCacheEntry{}}
	data, err := safeReadFile(filepath.Join(outputDir, filepath.FromSlash(historyScanCachePath)), semanticSnapshotMaxBytes())
	if err != nil {
		return empty
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return empty
	}
	defer gz.Close()
	// Bound the decompressed stream so a gzip bomb cannot exhaust memory; an
	// over-cap or truncated stream simply rebuilds the cache from scratch.
	limited := io.LimitReader(gz, semanticSnapshotMaxBytes())
	var cache historyScanCache
	if err := json.NewDecoder(limited).Decode(&cache); err != nil {
		return empty
	}
	if cache.Version != historyScanCacheVersion || cache.Files == nil {
		return empty
	}
	return cache
}

// saveHistoryScanCache persists the per-file scan cache. Failures are
// non-fatal: a missing or unwritable cache only costs a full rebuild next time.
func saveHistoryScanCache(outputDir string, cache historyScanCache) {
	data, err := encodeHistoryScanCache(cache)
	if err != nil {
		return
	}
	_ = writeBrainRelativeFileAtomic(outputDir, historyScanCachePath, data, 0o600)
}

func encodeHistoryScanCache(cache historyScanCache) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(cache); err != nil {
		_ = gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// sessionSourceFingerprintAbsent is the fingerprint of a brain that provably
// has NO canonical sessions source: the session export never ran, or ran and
// failed (no host CLI, no readable checkpoint catalog). It is a real, stable,
// comparable value and deliberately not "" — "" is reserved for "the recorder
// could not name the source at all", which is never evidence of freshness.
//
// Conflating the two is what made a sessionless brain refuse its own receipt:
// the publisher wrote "" into sources.history.sessions_fingerprint, the checked
// receipt reader treats "" as unproven, and `memory repair` then republished
// the same "" it had just rejected — a prescribed remedy that could never
// succeed. "absent" matches what the same manifest already writes for
// privacy_identity when there are no tombstones.
const sessionSourceFingerprintAbsent = "absent"

func brainSessionsFingerprint(outputDir string) string {
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		// The manifest itself is unreadable, so the current source cannot be
		// named at all. Unknown, which is not the same as absent.
		return ""
	}
	if manifest.Sources == nil {
		return sessionSourceFingerprintAbsent
	}
	return sessionSourceFingerprint(manifest.Sources.Sessions)
}

// sessionSourceFingerprintCurrent reports whether a recorded fingerprint still
// describes the brain's canonical sessions source.
//
// An empty CURRENT value means the manifest could not be read, so nothing can
// be proven current against it. An empty RECORDED value means the producer did
// not name its source — an index built before fingerprinting — which stays
// stale, EXCEPT against a brain that has no sessions source at all, where there
// is no session set that could have drifted and the two emptinesses describe
// the same state. That exception is what lets a brain whose session export
// failed read back its own receipt instead of reporting memory_source_stale
// forever.
func sessionSourceFingerprintCurrent(recorded, current string) bool {
	if current == "" {
		return false
	}
	if recorded == "" {
		return current == sessionSourceFingerprintAbsent
	}
	return recorded == current
}

func sessionSourceFingerprint(source *sessionSourceManifest) string {
	if source == nil {
		return sessionSourceFingerprintAbsent
	}
	type fingerprintSession struct {
		SessionID        string `json:"session_id"`
		LatestCheckpoint string `json:"latest_checkpoint_id"`
		Branch           string `json:"branch,omitempty"`
		TranscriptPath   string `json:"transcript_path"`
	}
	payload := struct {
		DefaultBranch      string               `json:"default_branch,omitempty"`
		TranscriptMode     string               `json:"transcript_mode"`
		Scope              string               `json:"scope"`
		LatestCheckpointID string               `json:"latest_checkpoint_id,omitempty"`
		Sessions           []fingerprintSession `json:"sessions"`
	}{
		DefaultBranch:      source.DefaultBranch,
		TranscriptMode:     source.TranscriptMode,
		Scope:              source.Scope,
		LatestCheckpointID: source.LatestCheckpointID,
		Sessions:           make([]fingerprintSession, 0, len(source.Sessions)),
	}
	for _, session := range source.Sessions {
		payload.Sessions = append(payload.Sessions, fingerprintSession{
			SessionID:        session.SessionID,
			LatestCheckpoint: session.LatestCheckpoint,
			Branch:           session.Branch,
			TranscriptPath:   session.TranscriptPath,
		})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func historySessionSortTime(path string, fallback time.Time) time.Time {
	base := filepath.Base(path)
	if len(base) >= len("20060102T150405Z") {
		if parsed, err := time.Parse("20060102T150405Z", base[:len("20060102T150405Z")]); err == nil {
			return parsed
		}
	}
	return fallback
}

func scanHistoryFile(outputDir, path string) ([]historyRecord, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("history transcript is not a regular file: %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, fmt.Errorf("history transcript changed or became unsafe while opening: %s", path)
	}
	rel, _ := filepath.Rel(outputDir, path)
	rel = filepath.ToSlash(rel)
	records, scanErr := scanHistoryFileReader(context.Background(), f, rel, filepath.Ext(path))
	after, statErr := os.Lstat(path)
	if statErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("history transcript changed or became unsafe while reading: %s", path)
	}
	return records, scanErr
}

func scanHistoryFileReader(ctx context.Context, f io.ReadSeeker, rel, ext string) ([]historyRecord, error) {
	// Document-form transcripts (e.g. opencode: one pretty-printed JSON
	// document) have no individually parseable lines, so the line scanner
	// below indexes nothing from them. Probe the first line the same way
	// distill does and route them through the shared document parser instead.
	if records, isDocument, err := scanDocumentHistoryFileContext(ctx, f, rel); err != nil {
		return nil, err
	} else if isDocument {
		return records, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil { // rewind the probe read
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), historyMaxLineBytes)
	var records []historyRecord
	lineNumber := 0
	allowRawText := ext == ".md" || ext == ".txt"
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lineNumber++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		// Apply the cheap keyword fast-path BEFORE the JSON decode: most transcript
		// lines are irrelevant to indexing, so on a large history this skips the
		// per-line Unmarshal for the majority of records.
		if !historyLineMayContainIndexedContent(text) {
			continue
		}
		// Decode at most once for the narrative pass. Only transcript records are
		// JSON objects, so skip the decode for raw .md/.txt lines that can't be one
		// (cheap leading-'{' check); those fall through to the raw-text path.
		var fragments []historyFragment
		parsedOK := false
		if strings.HasPrefix(text, "{") {
			var obj map[string]any
			if json.Unmarshal([]byte(text), &obj) == nil {
				fragments = extractHistoryJSONFragments(obj)
				parsedOK = true
			}
		}
		if !parsedOK && allowRawText {
			fragments = []historyFragment{{Text: text, Source: "text"}}
		}
		for _, fragment := range fragments {
			fragment.Text = strings.TrimSpace(fragment.Text)
			if fragment.Text == "" {
				continue
			}
			for _, kind := range classifyHistoryFragment(fragment) {
				records = append(records, historyRecord{
					ID:      historyRecordID(rel, lineNumber, kind, fragment.Text),
					Kind:    kind,
					Path:    rel,
					Line:    lineNumber,
					Summary: truncateString(cleanHistorySummary(fragment.Text), 700),
					Terms:   historyTerms(fragment.Text),
				})
			}
		}
	}
	return records, scanner.Err()
}

func scanHistoryLines(ctx context.Context, rel, ext string, reader io.Reader) ([]historyRecord, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), historyMaxLineBytes)
	var records []historyRecord
	lineNumber := 0
	allowRawText := ext == ".md" || ext == ".txt"
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lineNumber++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		// Apply the cheap keyword fast-path BEFORE the JSON decode: most transcript
		// lines are irrelevant to indexing, so on a large history this skips the
		// per-line Unmarshal for the majority of records.
		if !historyLineMayContainIndexedContent(text) {
			continue
		}
		// Decode at most once for the narrative pass. Only transcript records are
		// JSON objects, so skip the decode for raw .md/.txt lines that can't be one
		// (cheap leading-'{' check); those fall through to the raw-text path.
		var fragments []historyFragment
		parsedOK := false
		if strings.HasPrefix(text, "{") {
			var obj map[string]any
			if json.Unmarshal([]byte(text), &obj) == nil {
				fragments = extractHistoryJSONFragments(obj)
				parsedOK = true
			}
		}
		if !parsedOK && allowRawText {
			fragments = []historyFragment{{Text: text, Source: "text"}}
		}
		for _, fragment := range fragments {
			fragment.Text = strings.TrimSpace(fragment.Text)
			if fragment.Text == "" {
				continue
			}
			for _, kind := range classifyHistoryFragment(fragment) {
				records = append(records, historyRecord{
					ID:      historyRecordID(rel, lineNumber, kind, fragment.Text),
					Kind:    kind,
					Path:    rel,
					Line:    lineNumber,
					Summary: truncateString(cleanHistorySummary(fragment.Text), 700),
					Terms:   historyTerms(fragment.Text),
				})
			}
		}
	}
	return records, scanner.Err()
}

// scanDocumentHistoryFile detects and indexes a document-form transcript (see
// parseDocumentConversation). isDocument=false means the file is line-oriented
// (or not a transcript at all) and the caller's line scanner should handle it
// after rewinding past the probe read. Mirrors the JSONL indexing policy:
// assistant text is narrative and user turns are requests; record lines anchor to
// the document line each message object opens on.
func scanDocumentHistoryFile(f *os.File, rel string) (records []historyRecord, isDocument bool, err error) {
	return scanDocumentHistoryFileContext(context.Background(), f, rel)
}

func scanDocumentHistoryFileContext(ctx context.Context, f io.Reader, rel string) (records []historyRecord, isDocument bool, err error) {
	probe := make([]byte, 4096)
	n, readErr := contextCheckingReader{ctx: ctx, r: f}.Read(probe)
	if readErr != nil && readErr != io.EOF {
		return nil, false, readErr
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(probe[:n])), "\n")
	if !strings.HasPrefix(firstLine, "{") || json.Valid([]byte(firstLine)) {
		return nil, false, nil // JSONL or non-JSON: the line scanner's job
	}
	data, err := safeReadAll(contextCheckingReader{ctx: ctx, r: io.MultiReader(bytes.NewReader(probe[:n]), f)}, maxDocumentTranscriptBytes, "document transcript "+rel)
	if err != nil {
		return nil, false, err
	}
	messages, ok := parseDocumentConversation(string(data))
	if !ok {
		return nil, false, nil
	}
	return historyDocumentRecords(rel, messages), true, nil
}

// historyDocumentRecords turns the parsed messages of a document-form
// transcript into indexable records. It is factored out of the scanner so the
// hash-then-scan parity reference can share exactly this projection.
func historyDocumentRecords(rel string, messages []documentMessage) []historyRecord {
	var records []historyRecord
	appendFragment := func(fragment historyFragment, line int) {
		for _, kind := range classifyHistoryFragment(fragment) {
			records = append(records, historyRecord{
				ID:      historyRecordID(rel, line, kind, fragment.Text),
				Kind:    kind,
				Path:    rel,
				Line:    line,
				Summary: truncateString(cleanHistorySummary(fragment.Text), 700),
				Terms:   historyTerms(fragment.Text),
			})
		}
	}
	for _, message := range messages {
		// Tool outputs are mined through the same fact extractor as JSONL
		// tool_result blocks: only snippets with a structural code-fact signal
		// survive, so bulk tool output still never enters the index.
		for _, output := range message.ToolOutputs {
			for _, fragment := range extractToolResultFacts(output) {
				appendFragment(fragment, message.Line)
			}
		}
		if (message.Role != "assistant" && message.Role != "user") || message.Text == "" {
			continue
		}
		source := "assistant_message"
		if message.Role == "user" {
			if isWrapperRequest(message.Text) {
				continue
			}
			source = "user_prompt"
		}
		appendFragment(historyFragment{Text: message.Text, Source: source}, message.Line)
	}
	return records
}

// extractHistoryJSONFragments returns the indexable fragments of one
// transcript record: the kind-specific extraction (decisions, validations,
// tool calls, …) plus — when the record is a real user turn — a
// "user_prompt" fragment that classifies as a request record.
//
// Request records were extracted in scan-cache v2, removed in v3 ("measured
// noise in general ranking, and no surface queried them"), and re-introduced
// in v4: two Phase 2 consumers now need them — `brief --handoff` (a session's
// opening request) and the history eval's midtask stratum (follow-up
// questions as queries). The v3 noise concern remains addressed the way it
// always was: both rankers exclude kind=request from general ranking; the
// records exist for trajectory surfaces, not retrieval.
func extractHistoryJSONFragments(obj map[string]any) []historyFragment {
	fragments := extractHistoryRecordFragments(obj)
	// transcriptUserText handles all four transcript dialects (codex
	// response_item/event_msg, Claude user, pi message); wrapper injections
	// (environment context, caveats) are filtered with the same predicate
	// every other request consumer uses.
	if text := strings.TrimSpace(transcriptUserText(obj)); text != "" && !isWrapperRequest(text) {
		fragments = append(fragments, historyFragment{Text: text, Source: "user_prompt"})
	}
	return fragments
}

func extractHistoryRecordFragments(obj map[string]any) []historyFragment {
	recordType := jsonString(obj["type"])
	payload := jsonMap(obj["payload"])
	switch recordType {
	case "session_meta", "turn_context", "permission-mode":
		return nil
	case "agent_message":
		return stringFragment(obj["message"], "assistant_message")
	case "event_msg":
		return extractCodexEventFragments(payload)
	case "response_item":
		return extractCodexResponseFragments(payload)
	case "assistant":
		return extractClaudeMessageFragments(obj, "assistant")
	case "user":
		return extractClaudeToolResultFragments(obj)
	case "message":
		// pi wraps every turn as {"type":"message","message":{role,content}}.
		// Without this case the default branch's jsonString(obj["message"]) is
		// "" for a map, so pi sessions indexed to nothing (the same blind spot
		// distillConversationText covers for distill). Mirror the Claude
		// routing: assistant text is narrative, toolResult content can carry
		// code facts, user turns are skipped (as for codex user_message).
		message := jsonMap(obj["message"])
		switch jsonString(message["role"]) {
		case "assistant":
			return extractContentFragments(message["content"], "assistant_message")
		case "toolResult":
			return extractToolResultFacts(distillTextBlocks(message["content"]))
		default:
			return nil
		}
	case "progress":
		return nil
	default:
		if message := jsonString(obj["message"]); message != "" {
			return []historyFragment{{Text: message, Source: "message"}}
		}
		return nil
	}
}

func extractCodexEventFragments(payload map[string]any) []historyFragment {
	switch jsonString(payload["type"]) {
	case "agent_message":
		return stringFragment(payload["message"], "assistant_message")
	case "user_message":
		return nil
	case "task_complete":
		return stringFragment(payload["last_agent_message"], "final_answer")
	case "exec_command_end":
		command := compactJSONValue(payload["command"])
		return []historyFragment{{Text: command, Source: "tool_call:exec_command"}}
	case "patch_apply_end":
		parts := []string{"apply_patch", jsonString(payload["stdout"]), jsonString(payload["stderr"]), compactJSONValue(payload["changes"])}
		return []historyFragment{{Text: strings.Join(parts, " "), Source: "tool_call:apply_patch"}}
	default:
		return nil
	}
}

func extractCodexResponseFragments(payload map[string]any) []historyFragment {
	switch jsonString(payload["type"]) {
	case "message":
		role := jsonString(payload["role"])
		if role != "assistant" {
			return nil
		}
		return extractContentFragments(payload["content"], role+"_message")
	case "function_call":
		name := jsonString(payload["name"])
		return []historyFragment{{Text: strings.TrimSpace(name + " " + compactJSONValue(payload["arguments"])), Source: historyToolCallSource(name)}}
	case "custom_tool_call":
		name := jsonString(payload["name"])
		return []historyFragment{{Text: strings.TrimSpace(name + " " + compactJSONValue(payload["input"])), Source: historyToolCallSource(name)}}
	case "function_call_output":
		return extractToolResultFacts(jsonString(payload["output"]))
	default:
		return nil
	}
}

func extractClaudeToolResultFragments(obj map[string]any) []historyFragment {
	message := jsonMap(obj["message"])
	if len(message) == 0 {
		return nil
	}
	content, ok := message["content"].([]any)
	if !ok {
		return nil
	}
	var fragments []historyFragment
	for _, item := range content {
		block := jsonMap(item)
		if jsonString(block["type"]) != "tool_result" {
			continue
		}
		fragments = append(fragments, extractToolResultFacts(firstNonEmptyString(block["content"], block["text"]))...)
	}
	return fragments
}

func extractClaudeMessageFragments(obj map[string]any, role string) []historyFragment {
	message := jsonMap(obj["message"])
	if len(message) == 0 {
		return nil
	}
	return extractContentFragments(message["content"], role+"_message")
}

func extractContentFragments(content any, source string) []historyFragment {
	switch value := content.(type) {
	case string:
		return []historyFragment{{Text: value, Source: source}}
	case []any:
		var fragments []historyFragment
		for _, item := range value {
			block := jsonMap(item)
			if len(block) == 0 {
				if text := fmt.Sprint(item); strings.TrimSpace(text) != "" {
					fragments = append(fragments, historyFragment{Text: text, Source: source})
				}
				continue
			}
			blockType := jsonString(block["type"])
			switch blockType {
			case "tool_use":
				name := jsonString(block["name"])
				text := strings.TrimSpace(name + " " + compactJSONValue(block["input"]))
				fragments = append(fragments, historyFragment{Text: text, Source: historyToolCallSource(name)})
				fragments = append(fragments, extractToolResultFacts(text)...)
			case "tool_result":
				fragments = append(fragments, extractToolResultFacts(firstNonEmptyString(block["content"], block["text"]))...)
			default:
				if text := firstNonEmptyString(block["text"], block["input_text"], block["output_text"], block["content"]); text != "" {
					fragments = append(fragments, historyFragment{Text: text, Source: source})
				}
			}
		}
		return fragments
	default:
		return nil
	}
}

func extractToolResultFacts(text string) []historyFragment {
	text = strings.TrimSpace(text)
	if text == "" || !historyTextHasCodeFactSignal(text) {
		return nil
	}
	lines := strings.Split(text, "\n")
	seen := map[string]struct{}{}
	var fragments []historyFragment
	lastEnd := -1
	for i, line := range lines {
		if i < lastEnd {
			continue
		}
		if !historyLineHasCodeFactSignal(line) {
			continue
		}
		start := max(0, i-2)
		end := min(len(lines), i+4)
		snippet := cleanHistoryCodeFactSnippet(cleanHistorySummary(strings.Join(lines[start:end], " ")))
		if snippet == "" {
			continue
		}
		if _, ok := seen[snippet]; ok {
			continue
		}
		seen[snippet] = struct{}{}
		fragments = append(fragments, historyFragment{Text: snippet, Source: "tool_result_fact"})
		lastEnd = end
		if len(fragments) >= 8 {
			break
		}
	}
	return fragments
}

func cleanHistoryCodeFactSnippet(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "content:"))
	if value == "" {
		return ""
	}
	return truncateString(value, 4000)
}

// historyFactSignal matches output that states a durable fact about the code:
// a declaration, a flag's documented default, an assignment to a literal, or a
// file:line anchor. Those are the shapes a command prints when it reveals a
// decided value, which is precisely what a later session needs to recover.
//
// This replaces a hardcoded list of phrases ("bare auth", "brainignore",
// "schema contract", "attributionbasecommit", ...) that had been lifted from
// individual benchmark task names. Tool output was indexed only when it
// happened to contain one of them, so on any real repository essentially no
// command output entered the history index: a default printed by --help, a
// constant echoed by a build, or a value dumped by a config command was
// unsearchable. Matching structure instead of vocabulary keeps the index
// repository-agnostic.
// The quote alternative accepts an optional backslash so the signal also fires
// on raw transcript lines, where an embedded quote is JSON-escaped as \".
var historyFactSignal = regexp.MustCompile(
	`(?:^|[\s(])(?:const|func|type|class|def|var|let|interface|enum|struct)\s+[A-Za-z_]` +
		`|--[A-Za-z][\w-]*[^\n]{0,200}?\(default\b` +
		`|[A-Za-z_][A-Za-z0-9_.]*\s*(?::=|=)\s*(?:-?\d|\\?["'` + "`" + `]|true\b|false\b)` +
		`|[\w./-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|rb|c|cc|cpp|h|hpp|kt|swift|sh|sql):\d+`,
)

// historyFactSignalRelaxed is the pre-decode variant of historyFactSignal. Raw
// transcript lines JSON-escape structure (a declaration can sit directly after
// \" or \n), so the strict leading boundary misses content the extractor would
// keep after decoding. The prefilter is an admission gate, not a classifier: a
// false positive costs one JSON decode, a false negative loses the record, so
// declarations here need only a word boundary. Keep the alternatives in sync
// with historyFactSignal.
var historyFactSignalRelaxed = regexp.MustCompile(
	`\b(?:const|func|type|class|def|var|let|interface|enum|struct)\s+[A-Za-z_]` +
		`|--[A-Za-z][\w-]*[^\n]{0,200}?\(default\b` +
		`|[A-Za-z_][A-Za-z0-9_.]*\s*(?::=|=)\s*(?:-?\d|\\?["'` + "`" + `]|true\b|false\b)` +
		`|[\w./-]+\.(?:go|ts|tsx|js|jsx|py|rs|java|rb|c|cc|cpp|h|hpp|kt|swift|sh|sql):\d+`,
)

func historyTextHasCodeFactSignal(text string) bool {
	return historyFactSignal.MatchString(text)
}

func historyLineHasCodeFactSignal(line string) bool {
	return historyTextHasCodeFactSignal(line)
}

func classifyHistoryFragment(fragment historyFragment) []string {
	lower := strings.ToLower(fragment.Text)
	narrative := isHistoryNarrativeSource(fragment.Source)
	tool := strings.HasPrefix(fragment.Source, "tool_call")
	validationTool := tool && !strings.Contains(fragment.Source, "apply_patch")
	kinds := map[string]struct{}{}
	if fragment.Source == "user_prompt" {
		// Request records: excluded from general ranking (measured noise);
		// consumed by trajectory surfaces (handoff, midtask eval mining).
		return []string{"request"}
	}
	if fragment.Source == "tool_result_fact" {
		kinds["code_fact"] = struct{}{}
	}
	if narrative && isDecisionFragment(fragment.Source, lower) {
		kinds["decision"] = struct{}{}
	}
	if narrative && containsAny(lower, "learned", "root cause", "turns out", "lesson", "failure mode", "the issue", "the bug", "found that") {
		kinds["learning"] = struct{}{}
	}
	if (narrative || validationTool) && containsAny(lower, "go test", "pytest", "npm test", "mise run", "validation", "regression test", "hidden validation", "tests pass", "validated with", "ran tests") {
		kinds["validation"] = struct{}{}
	}
	if narrative && isArchitectureFragment(lower) {
		kinds["architecture"] = struct{}{}
	}
	if tool || (fragment.Source == "text" && containsAny(lower, "tool_use", "function_call", "apply_patch", "exec_command", "\"cmd\"", "entire brain", "git grep", "bash ", "read ", "write ")) {
		kinds["tool_call"] = struct{}{}
	}
	out := make([]string, 0, len(kinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func isHistoryNarrativeSource(source string) bool {
	switch source {
	case "assistant_message", "final_answer", "message", "text":
		return true
	default:
		return false
	}
}

func historyToolCallSource(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "tool_call"
	}
	return "tool_call:" + name
}

func historyLineMayContainIndexedContent(text string) bool {
	lower := strings.ToLower(text)
	if containsAny(lower, `"type":"session_meta"`, `"type": "session_meta"`, `"type":"turn_context"`, `"type": "turn_context"`, `"type":"permission-mode"`, `"type": "permission-mode"`) {
		return false
	}
	// JSON message shapes carry user requests and narratives whose vocabulary
	// is broader than any keyword allowlist. Let the extractor classify them.
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		return true
	}
	return len(classifyHistoryFragment(historyFragment{Text: text, Source: "text"})) > 0 || historyFactSignalRelaxed.MatchString(text)
}

func isDecisionFragment(source, lower string) bool {
	if isDecisionProgressFragment(lower) {
		return false
	}
	if containsAny(lower,
		"decision:", "decision -", "decision was", "decision is", "decided",
		"we chose", "we choose", "chose to", "choose to", "rationale",
		"tradeoff", "trade-off", "preferred", "prefer ",
	) {
		return true
	}
	if source != "assistant_message" && source != "final_answer" {
		return false
	}
	return containsAny(lower,
		"must preserve", "must keep", "must not", "should preserve", "should keep",
		"contract", "invariant", "source of truth", "compatibility contract",
		"stable contract", "api contract", "behavioral contract",
	)
}

func isDecisionProgressFragment(lower string) bool {
	return hasAnyPrefix(lower,
		"i’m reading", "i'm reading",
		"i’m checking", "i'm checking",
		"i’m adding", "i'm adding",
		"i’ll treat", "i'll treat",
		"i’ll make", "i'll make",
		"i have enough context",
		"let me ",
		"here is the final plan",
		"server is up",
		"net ",
	)
}

func isArchitectureFragment(lower string) bool {
	return containsAny(lower,
		"architecture", "boundary", "boundaries", "data flow", "contract", "invariant",
		"module", "package", "command surface", "dispatch", "integration", "workflow",
	)
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func historyRecordID(path string, line int, kind, text string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", path, line, kind, text)))
	return "history:" + hex.EncodeToString(sum[:12])
}

func cleanHistorySummary(text string) string {
	text = strings.ReplaceAll(text, "\\n", " ")
	text = strings.Join(strings.Fields(text), " ")
	return text
}

func historyTerms(text string) []string {
	lower := strings.ToLower(text)
	// Generic domain vocabulary only. The former tail of this list
	// (attributionbasecommit, resolvetranscriptpath, brainignore, ...) was
	// lifted from benchmark task names, which biased FTS text toward benchmark
	// queries on every repository.
	candidates := []string{
		"decision", "rationale", "validation", "go test", "apply_patch", "exec_command",
		"semantic", "brief", "stale", "checkpoint", "transcript", "schema", "env",
		"provenance", "bundle", "sha256", "review", "hook", "plugin", "workspace",
		"architecture", "contract", "invariant", "tool", "test", "fallback",
	}
	var terms []string
	for _, candidate := range candidates {
		if strings.Contains(lower, candidate) || strings.Contains(normalizeHistorySearchText(lower), normalizeHistorySearchText(candidate)) {
			terms = append(terms, candidate)
		}
	}
	return terms
}

func jsonMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func jsonString(value any) string {
	s, _ := value.(string)
	return s
}

func stringFragment(value any, source string) []historyFragment {
	text := jsonString(value)
	if text == "" {
		return nil
	}
	return []historyFragment{{Text: text, Source: source}}
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if text := jsonString(value); strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func compactJSONValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(data)
	}
}

func normalizeHistorySearchText(value string) string {
	var b strings.Builder
	runes := []rune(value)
	lastSpace := true
	for i, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if !lastSpace && historyCamelBoundary(runes, i) {
				b.WriteByte(' ')
			}
			b.WriteRune(unicode.ToLower(r))
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func historyCamelBoundary(runes []rune, index int) bool {
	if index <= 0 || index >= len(runes) {
		return false
	}
	current := runes[index]
	previous := runes[index-1]
	if !unicode.IsLetter(current) || !(unicode.IsLetter(previous) || unicode.IsDigit(previous)) || !unicode.IsUpper(current) {
		return false
	}
	if unicode.IsLower(previous) || unicode.IsDigit(previous) {
		return true
	}
	if unicode.IsUpper(previous) && index+1 < len(runes) && unicode.IsLower(runes[index+1]) {
		return true
	}
	return false
}

func historyTextMatchesQuery(text, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return false
	}
	text = strings.ToLower(text)
	if strings.Contains(text, query) {
		return true
	}
	normalizedText := normalizeHistorySearchText(text)
	normalizedQuery := normalizeHistorySearchText(query)
	if normalizedQuery == "" {
		return false
	}
	if strings.Contains(normalizedText, normalizedQuery) {
		return true
	}
	return false
}

func historyRecordMatchesQuery(record historyRecord, query string) bool {
	return historyTextMatchesQuery(record.Summary+" "+strings.Join(record.Terms, " ")+" "+record.Path, query)
}

// historyRecordTimestamp recovers the session capture time encoded in the
// exported transcript path (sessions/<branch>/YYYYMMDDThhmmssZ_...jsonl) so
// callers can order records by recency and report when a record was produced.
// Records sourced from non-session paths (seed/manifest) have no timestamp.
func historyRecordTimestamp(path string) (time.Time, bool) {
	base := filepath.Base(filepath.FromSlash(path))
	if len(base) < 16 {
		return time.Time{}, false
	}
	stamp := base[:16]
	parsed, err := time.Parse("20060102T150405Z", stamp)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// historyRecordMatchedTerms reports which query tokens and identifiers actually
// appear in a record. Returning these makes a match explainable to an agent and
// turns a zero/low-signal result into an actionable "these terms hit, these did
// not" diagnostic instead of an opaque miss.
func historyRecordMatchedTerms(record historyRecord, query string) []string {
	recordText := normalizeHistorySearchText(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	recordRaw := strings.ToUpper(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	seen := map[string]struct{}{}
	var matched []string
	for _, term := range historyQueryTerms(query) {
		if strings.Contains(recordText, term) {
			if _, ok := seen[term]; !ok {
				seen[term] = struct{}{}
				matched = append(matched, term)
			}
		}
	}
	for _, identifier := range historyIdentifierQueryTerms(query) {
		if strings.Contains(recordRaw, identifier) {
			key := strings.ToLower(identifier)
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				matched = append(matched, identifier)
			}
		}
	}
	return matched
}

func rankHistoryRecords(index historyIndex, kind, query string, limit int) []historyRecord {
	scored := rankHistoryRecordsScored(index, kind, query, limit, 0)
	records := make([]historyRecord, 0, len(scored))
	for _, item := range scored {
		records = append(records, item.Record)
	}
	return records
}

// rankHistoryRecordsScored ranks records and returns the scores so callers can
// surface why a record matched. minMatches relaxes the term-coverage threshold
// (0 = strict default, 1 = best-effort partial matching).
func rankHistoryRecordsScored(index historyIndex, kind, query string, limit, minMatches int) []scoredHistoryRecord {
	if limit <= 0 {
		return nil
	}
	allowed := historyInspectKinds(kind)
	scored := make([]scoredHistoryRecord, 0, min(len(index.Records), limit))
	seen := map[string]struct{}{}
	for i, record := range index.Records {
		if len(allowed) > 0 {
			if _, ok := allowed[record.Kind]; !ok {
				continue
			}
		} else if historyGeneralRankingHiddenKind(record.Kind) {
			// Keep user-prompt and conversation-exchange records out of general
			// ranking (requests are measured noise; exchanges are opt-in via the
			// explicit conversation source only).
			continue
		}
		score := historyRecordQueryScoreMin(record, query, minMatches)
		if score == 0 {
			continue
		}
		score += historyInspectKindPreference(kind, record.Kind)
		matchKey := normalizeHistorySearchText(record.Summary)
		if _, ok := seen[matchKey]; ok {
			continue
		}
		seen[matchKey] = struct{}{}
		scored = append(scored, scoredHistoryRecord{Record: record, Score: score, Order: i})
	}
	sort.Slice(scored, func(i, j int) bool {
		left, right := scored[i], scored[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		// Among equally-scored records, prefer the most recent session so
		// "what is the current state of X" surfaces the latest decision first
		// instead of an arbitrary older one.
		leftTime, leftOK := historyRecordTimestamp(left.Record.Path)
		rightTime, rightOK := historyRecordTimestamp(right.Record.Path)
		if leftOK && rightOK && !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		if leftOK != rightOK {
			return leftOK
		}
		if historyKindRank(left.Record.Kind) != historyKindRank(right.Record.Kind) {
			return historyKindRank(left.Record.Kind) > historyKindRank(right.Record.Kind)
		}
		if left.Record.Path != right.Record.Path {
			return left.Record.Path > right.Record.Path
		}
		if left.Record.Line != right.Record.Line {
			return left.Record.Line < right.Record.Line
		}
		return left.Order < right.Order
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored
}

func historyRecordQueryScore(record historyRecord, query string) int {
	return historyRecordQueryScoreMin(record, query, 0)
}

// historyRecordQueryScoreMin scores a record against a query. minMatches
// overrides the default term-coverage threshold: pass 0 for the strict default
// (precise specialist lookups) or 1 to accept best-effort partial matches for
// natural-language questions that would otherwise return nothing.
func historyRecordQueryScoreMin(record historyRecord, query string, minMatches int) int {
	score := 0
	if historyRecordMatchesQuery(record, query) {
		score += 200
	}
	terms := historyQueryTerms(query)
	identifiers := historyIdentifierQueryTerms(query)
	if len(terms) == 0 && len(identifiers) == 0 {
		return score
	}
	recordText := normalizeHistorySearchText(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	recordRaw := strings.ToUpper(record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path)
	recordTerms := normalizeHistorySearchText(strings.Join(record.Terms, " "))
	matches := 0
	for _, term := range terms {
		if strings.Contains(recordText, term) {
			matches++
			score += 10
			if strings.Contains(recordTerms, term) {
				score += 5
			}
		}
	}
	identifierMatches := 0
	for _, identifier := range identifiers {
		if strings.Contains(recordRaw, identifier) {
			identifierMatches++
			score += 90
		}
	}
	effectiveMatches := matches + identifierMatches
	requiredTermCount := len(terms)
	if requiredTermCount == 0 {
		requiredTermCount = len(identifiers)
	}
	required := historyRequiredQueryMatches(requiredTermCount)
	if minMatches > 0 && minMatches < required {
		required = minMatches
	}
	if effectiveMatches < required {
		if score < 200 {
			return 0
		}
		return score + historyKindRank(record.Kind)
	}
	score += effectiveMatches * 2
	if len(terms) > 0 && matches >= len(terms) {
		score += 30
	}
	score += historyRecordEvidenceScore(record, matches, identifierMatches)
	score += historyKindRank(record.Kind)
	return score
}

// historyRecordEvidenceScore rewards evidence shape, not a particular product
// or topic. Query relevance is accounted for by historyRecordQueryScoreMin.
func historyRecordEvidenceScore(record historyRecord, termMatches, identifierMatches int) int {
	text := record.Summary + " " + strings.Join(record.Terms, " ") + " " + record.Path
	lower := strings.ToLower(text)
	score := 0
	pathLower := strings.ToLower(record.Path)
	if pathLower == "manifest.json" || strings.HasPrefix(pathLower, "seed/") {
		score -= 200
	}
	if record.Kind == "code_fact" {
		score += 35
	}
	if containsAny(lower, "diff --git", "+++ b/", "--- a/", "@@", "apply_patch", "unified_diff") {
		score += 40
	}
	if containsAny(lower, "packages/", "internal/", "cmd/", "src/", "apps/", ".go", ".ts", ".tsx", ".js", ".py", ".rs") {
		score += 20
	}
	if identifierMatches > 0 {
		score += 20 * identifierMatches
		if containsAny(lower, "diff --git", "+++ b/", "apply_patch", "unified_diff") {
			score += 25
		}
	}
	if termMatches > 1 && containsAny(lower, "test", "validation", "regression", "contract", "invariant") {
		score += 12
	}
	if containsAny(lower, "start by running:", "entire doctor", "entire status --detailed", "entire session current", "entire checkpoint list") {
		score -= 120
	}
	if record.Kind == "tool_call" && !containsAny(lower, "apply_patch", "diff --git", "unified_diff") {
		score -= 20
	}
	if len(record.Summary) > 2500 && !containsAny(lower, "diff --git", "+++ b/", "apply_patch", "unified_diff") {
		score -= 25
	}
	return score
}

func historyInspectKindPreference(queryKind, recordKind string) int {
	switch queryKind {
	case "architecture":
		switch recordKind {
		case "architecture":
			return 120
		case "decision":
			return 80
		case "learning":
			return 40
		default:
			return 0
		}
	default:
		return 0
	}
}

func historyRequiredQueryMatches(termCount int) int {
	switch {
	case termCount <= 1:
		return 1
	case termCount <= 3:
		return 2
	default:
		return 3
	}
}

// historyGeneralRankingHiddenKind reports the record kinds every general
// (non-kind-scoped) history surface must skip: user-prompt requests (measured
// ranking noise) and conversation exchanges (returned only when the caller
// explicitly selects the conversation source; excluded from history vectors in
// Phase 1; no semantic exchange arm exists yet).
func historyGeneralRankingHiddenKind(kind string) bool {
	return kind == "request" || kind == conversationKind
}

func historyKindRank(kind string) int {
	switch kind {
	case "decision":
		return 40
	case "request":
		return 34
	case "architecture":
		return 32
	case "code_fact":
		return 30
	case "learning":
		return 26
	case "validation":
		return 18
	case "tool_call":
		return 4
	default:
		return 1
	}
}

func historyQueryTerms(query string) []string {
	normalized := normalizeHistorySearchText(query)
	seen := map[string]struct{}{}
	var terms []string
	for _, term := range strings.Fields(normalized) {
		if historyQueryStopword(term) {
			continue
		}
		if len(term) < 3 && !historyShortQueryTerm(term) {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
	}
	return terms
}

// historyQueryAllTerms returns the union of word tokens and identifier tokens a
// query reduces to after stopword removal. Surfacing these lets an agent see
// exactly what the brain searched for, so a thin or empty result is debuggable
// (e.g. an over-specific phrase reduced to one weak term) instead of opaque.
func historyQueryAllTerms(query string) []string {
	seen := map[string]struct{}{}
	var all []string
	for _, term := range historyQueryTerms(query) {
		if _, ok := seen[term]; !ok {
			seen[term] = struct{}{}
			all = append(all, term)
		}
	}
	for _, identifier := range historyIdentifierQueryTerms(query) {
		key := strings.ToLower(identifier)
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			all = append(all, identifier)
		}
	}
	return all
}

func historyIdentifierQueryTerms(query string) []string {
	seen := map[string]struct{}{}
	var terms []string
	for _, candidate := range strings.FieldsFunc(query, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
	}) {
		candidate = strings.Trim(candidate, "_")
		if candidate == "" {
			continue
		}
		upper := strings.ToUpper(candidate)
		if !strings.Contains(upper, "_") && !historyIdentifierLike(candidate) {
			continue
		}
		if historyQueryStopword(strings.ToLower(upper)) {
			continue
		}
		if _, ok := seen[upper]; ok {
			continue
		}
		seen[upper] = struct{}{}
		terms = append(terms, upper)
	}
	return terms
}

func historyIdentifierLike(candidate string) bool {
	if len(candidate) < 3 {
		return false
	}
	hasUpper := false
	hasLower := false
	hasDigit := false
	hasInternalUpper := false
	for i, r := range candidate {
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
			if i > 0 {
				hasInternalUpper = true
			}
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if hasUpper && !hasLower {
		return true
	}
	if hasInternalUpper && hasLower {
		return true
	}
	return hasDigit && (hasUpper || hasLower)
}

func historyShortQueryTerm(term string) bool {
	switch term {
	case "go", "ci", "pr", "ui", "id", "lc":
		return true
	default:
		return false
	}
}

// genericQueryStopwords are generic English fillers and generic task verbs with
// no retrieval signal. It is the SINGLE source of truth shared by every query
// tokenizer in the package — history/semantic specialist search
// (historyQueryStopword) and brain-brief filename matching
// (brainBriefFileMatchTerms) — so the common list never drifts between the two.
// Each call site layers only its own domain-specific noise words on top; those
// legitimately differ (e.g. "regression"/"preserve" are noise for free-text
// history search but valid filename-match terms like regression.go for brief).
var genericQueryStopwords = map[string]bool{
	"and": true, "are": true, "but": true, "can": true,
	"did": true, "does": true, "fix": true, "for": true, "from": true,
	"has": true, "have": true, "how": true, "into": true, "its": true,
	"make": true, "must": true, "need": true, "new": true, "not": true,
	"run": true, "runs": true, "should": true, "than": true, "that": true,
	"the": true, "then": true, "this": true, "use": true, "via": true,
	"want": true, "what": true, "when": true, "where": true, "which": true,
	"why": true, "with": true, "would": true,
}

func historyQueryStopword(term string) bool {
	if genericQueryStopwords[term] {
		return true
	}
	// History/semantic-search-specific noise words layered on the generic set:
	// natural-language filler plus domain verbs/nouns ("regression", "preserve",
	// "restore", ...) that carry no signal for a free-text record search.
	switch term {
	case "a", "an", "as", "at", "be", "been", "by", "current", "do",
		"during", "existing", "in", "is", "it", "keep", "local", "of", "on",
		"only", "or", "other", "previous", "prior", "preserve", "regression",
		"restore", "same", "task", "to", "without":
		return true
	case "behavior", "entire", "fresh", "parent", "setting", "value", "values":
		return true
	case "done", "over", "instead", "we", "our", "could", "about":
		return true
	default:
		return false
	}
}

func loadBrainHistoryIndex(brainDir string, source *historySourceManifest) (historyIndex, error) {
	index, err := loadBrainHistoryIndexOnce(brainDir, source)
	if err == nil || source == nil || !validHistoryGenerationArtifactPath(source.IndexPath, historyIndexFileName) {
		return index, err
	}

	// A reader may have loaded the previous manifest immediately before a
	// writer committed and pruned that generation. Resolve the manifest again
	// and retry once only when it names a different index. A corrupt current
	// generation is never hidden by this race recovery path.
	manifest, manifestErr := loadBrainManifest(brainDir)
	if manifestErr != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		return historyIndex{}, err
	}
	current := manifest.Sources.History
	if current.IndexPath == source.IndexPath {
		return historyIndex{}, err
	}
	retried, retryErr := loadBrainHistoryIndexOnce(brainDir, current)
	if retryErr != nil {
		return historyIndex{}, fmt.Errorf("history generation changed while reading; current generation is also unreadable: %w", retryErr)
	}
	return retried, nil
}

// readBrainHistoryIndexBytes performs the hardened read: canonical path, no
// symlinked components, regular file, no open-alias, bounded read, and (for a
// generation-addressed source) the manifest content digest. It returns the
// exact bytes that were verified so hashing and parsing can never straddle two
// generations.
func readBrainHistoryIndexBytes(brainDir string, source *historySourceManifest) ([]byte, string, bool, error) {
	if source == nil || source.IndexPath == "" {
		return nil, "", false, errors.New("history index missing; run `entire brain refresh`")
	}
	generationAddressed := validHistoryGenerationArtifactPath(source.IndexPath, historyIndexFileName)
	if generationAddressed && source.IndexDigest == "" {
		return nil, "", false, fmt.Errorf("%s: generation-addressed history index is missing its manifest content digest; run `entire brain refresh`", memoryErrMigrationRequired)
	}
	clean, err := validateHistoryIndexPath(source.IndexPath)
	if err != nil {
		return nil, "", false, err
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return nil, "", false, historyIndexReadError(source, err)
	}
	path := filepath.Join(brainDir, clean)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", false, historyIndexReadError(source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, "", false, fmt.Errorf("%s: history index must be a regular file and must not be a symlink: %s", memoryErrStateCorrupt, source.IndexPath)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, "", false, err
	}
	defer f.Close()
	if err := rejectOpenFileAlias(path, f, "history index"); err != nil {
		return nil, "", false, fmt.Errorf("%s: %w", memoryErrStateCorrupt, err)
	}
	data, err := safeReadAll(f, semanticSnapshotMaxBytes(), path)
	if err != nil {
		return nil, "", false, err
	}
	contentSum := sha256.Sum256(data)
	contentDigest := "sha256:" + hex.EncodeToString(contentSum[:])
	if source.IndexDigest != "" {
		if !validSHA256Identity(source.IndexDigest) {
			return nil, "", false, fmt.Errorf("%s: history index digest in manifest is invalid", memoryErrStateCorrupt)
		}
		if source.IndexDigest != contentDigest {
			return nil, "", false, fmt.Errorf("%s: history index digest does not match the manifest commit", memoryErrStateCorrupt)
		}
	}
	return data, contentDigest, generationAddressed, nil
}

// historyIndexReadError names the one condition the hardened read path could
// otherwise only express as a raw filesystem error.
//
// The guard lstats every path component, so deleting the brain's history/
// directory surfaced as `load history index: lstat <brainDir>/history: no such
// file or directory` — an internal path, no condition, no remedy. The nil-source
// case three lines above already says "history index missing; run `entire brain
// refresh`"; a DECLARED index that is not on disk is the same sentence with the
// artifact named. Every other failure keeps its own words.
func historyIndexReadError(source *historySourceManifest, err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return declaredIndexDefect{Source: declaredIndexHistory, Path: source.IndexPath, Absent: true, Err: err}
}

func loadBrainHistoryIndexOnce(brainDir string, source *historySourceManifest) (historyIndex, error) {
	data, contentDigest, generationAddressed, err := readBrainHistoryIndexBytes(brainDir, source)
	if err != nil {
		return historyIndex{}, err
	}
	index, err := decodeBrainHistoryIndex(data, source)
	if err != nil {
		return historyIndex{}, err
	}
	index.contentIdentity = contentDigest
	if generationAddressed {
		index.storageIdentity = source.IndexPath
	}
	return index, nil
}

// loadBrainHistoryIndexWithLegacyIdentity returns the same verified index as
// loadBrainHistoryIndex plus an optional identity upgrade derived from the
// exact byte slice that was parsed. Older manifests predate the strong source
// identity fields required by direct FTS hydration. Keeping hashing and parsing
// on one immutable slice prevents a path replacement from producing a hash of
// generation A and parsed records from generation B.
func loadBrainHistoryIndexWithLegacyIdentity(brainDir string, source *historySourceManifest) (historyIndex, *historyLegacyIdentity, error) {
	data, contentDigest, generationAddressed, err := readBrainHistoryIndexBytes(brainDir, source)
	if err != nil {
		return historyIndex{}, nil, err
	}
	index, err := decodeBrainHistoryIndex(data, source)
	if err != nil {
		return historyIndex{}, nil, err
	}
	index.contentIdentity = contentDigest
	if generationAddressed {
		index.storageIdentity = source.IndexPath
	}
	legacyIdentity := deriveHistoryLegacyIdentity(source, data, index)
	if legacyIdentity != nil {
		// The direct/index-backed FTS freshness check consumes this private
		// identity. It is derived from the parsed truth, not trusted from a
		// legacy manifest, and avoids recomputing it later in this same request.
		index.recordsFingerprint = legacyIdentity.RecordsFingerprint
	}
	return index, legacyIdentity, nil
}

func decodeBrainHistoryIndex(data []byte, source *historySourceManifest) (historyIndex, error) {
	if source.IndexBytes > 0 && int64(len(data)) != source.IndexBytes {
		return historyIndex{}, fmt.Errorf("history index size mismatch: got %d bytes, manifest declares %d; run `entire brain refresh history` to rebuild it", len(data), source.IndexBytes)
	}
	if source.IndexSHA256 != "" && historyIndexBytesFingerprint(data) != source.IndexSHA256 {
		return historyIndex{}, errors.New("history index checksum does not match manifest")
	}
	var index historyIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return historyIndex{}, fmt.Errorf("%s: decode history index: %w", memoryErrStateCorrupt, err)
	}
	if source.RecordsFingerprint != "" {
		if !validHistorySHA256(source.RecordsFingerprint) {
			return historyIndex{}, errors.New("history records fingerprint is invalid")
		}
		if len(index.Records) != source.Records {
			return historyIndex{}, fmt.Errorf("history record count mismatch: got %d, manifest declares %d", len(index.Records), source.Records)
		}
		if !source.GeneratedAt.IsZero() && !index.GeneratedAt.Equal(source.GeneratedAt) {
			return historyIndex{}, errors.New("history index generation does not match manifest")
		}
		// IndexSHA256 binds the exact serialized record set to this manifest.
		// Recomputing the canonical per-record hash here would marshal hundreds
		// of thousands of records on every semantic/get path; the exact byte
		// checksum above already provides the integrity check we need.
		index.recordsFingerprint = source.RecordsFingerprint
	}
	return index, nil
}

// historyTranscriptFilesFingerprint commits the exact transcript set an index
// was built from. It consumes the content digest each file already carries from
// its single verified read, so recomputing it never re-hashes the corpus.
func historyTranscriptFilesFingerprint(files []historySessionFile) string {
	type entry struct{ path, digest string }
	entries := make([]entry, 0, len(files))
	for _, file := range files {
		if file.Rel == "" || file.ContentDigest == "" {
			return ""
		}
		entries = append(entries, entry{path: file.Rel, digest: file.ContentDigest})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	_, _ = io.WriteString(h, "entire-brain/history-transcripts/v1\x00")
	_, _ = io.WriteString(h, strconv.Itoa(len(entries))+"\x00")
	for _, entry := range entries {
		_, _ = io.WriteString(h, strconv.Itoa(len(entry.path))+":")
		_, _ = io.WriteString(h, entry.path)
		_, _ = io.WriteString(h, strconv.Itoa(len(entry.digest))+":")
		_, _ = io.WriteString(h, entry.digest)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func validateHistoryIndexPath(indexPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(indexPath))
	cleanSlash := filepath.ToSlash(clean)
	if cleanSlash != indexPath {
		return "", fmt.Errorf("history index_path must be canonical: %s", indexPath)
	}
	if strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) ||
		(cleanSlash != historyIndexPath && !validHistoryGenerationArtifactPath(cleanSlash, historyIndexFileName)) {
		return "", fmt.Errorf("history index_path is unsafe: %s", indexPath)
	}
	return clean, nil
}

func validHistoryGenerationArtifactPath(rel, fileName string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != historyDirName || parts[1] != "generations" || parts[3] != fileName || len(parts[2]) != 40 {
		return false
	}
	_, err := hex.DecodeString(parts[2])
	return err == nil
}

func validHistoryStagingArtifactPath(rel, fileName string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != historyDirName || parts[1] != "staging" || parts[3] != fileName || len(parts[2]) != 40 {
		return false
	}
	_, err := hex.DecodeString(parts[2])
	return err == nil
}
