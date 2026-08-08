package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"
)

// history_delta.go is the brain's SHORT-TERM MEMORY: a small, bounded overlay
// index covering only the session transcripts that changed since the last full
// (long-term) index build. The split mirrors the human model the product aims
// for:
//
//   - short-term path: `refresh delta` scans just the changed/new transcripts
//     (seconds even on huge brains — change detection rides the same
//     size+mtime signal as the scan cache) and writes history/short-term.json.
//     Retrieval searches it alongside the long-term index, so an in-flight
//     session's earlier turns and a parallel terminal's work are recallable
//     near-real-time.
//   - long-term memory: the existing full index + FTS + vectors, expensive and
//     complete. A finished full build IS consolidation: it re-absorbs
//     everything the overlay covered and clears it (the hook lives in
//     writeBrainHistoryIndexAndSourceLocked).
//
// The overlay is disposable and always rebuildable; it never becomes a second
// source of truth. When it is empty or absent, every retrieval path returns
// byte-identical results to the pre-overlay behavior — freshness is additive,
// never a ranking change for cold brains.

const (
	historyShortTermFileName = "short-term.json"
	historyShortTermPath     = historyDirName + "/" + historyShortTermFileName
	historyShortTermVersion  = 1
)

// historyShortTermMaxRecords bounds the overlay. Short-term memory is a
// buffer, not an archive: past this, the oldest files are dropped (their
// content is still in the canonical transcripts) and the overlay reports
// itself truncated so doctor can recommend consolidation. A var so tests can
// exercise the bound without 20k-record fixtures.
var historyShortTermMaxRecords = 20000

type shortTermIndex struct {
	Version int `json:"version"`
	// BaseGeneratedAt pins the long-term build this overlay extends. After a
	// consolidation (full build) the overlay is cleared under the same lock;
	// this pin is the belt-and-braces guard that voids a stale overlay anyway.
	BaseGeneratedAt time.Time `json:"base_generated_at"`
	// SessionsFingerprint is the exported-session set the overlay was built
	// against, so doctor can report "long-term stale but short-term covers it".
	SessionsFingerprint string                   `json:"sessions_fingerprint,omitempty"`
	GeneratedAt         time.Time                `json:"generated_at"`
	Truncated           bool                     `json:"truncated,omitempty"`
	Files               map[string]shortTermFile `json:"files"`
}

type shortTermFile struct {
	Size                int64           `json:"size"`
	ModUnixNano         int64           `json:"mod_unix_nano"`
	SortTime            time.Time       `json:"sort_time"`
	Records             []historyRecord `json:"records"`
	IncompleteExchanges int             `json:"incomplete_exchanges,omitempty"`
}

// loadHistoryShortTerm returns the overlay when it is valid for the CURRENT
// long-term build (version + base pin match); anything else — missing,
// corrupt, or stale — reads as an empty overlay, which restores exact
// long-term-only behavior.
func loadHistoryShortTerm(brainDir string, source *historySourceManifest) shortTermIndex {
	empty := shortTermIndex{Version: historyShortTermVersion, Files: map[string]shortTermFile{}}
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)))
	if err != nil {
		return empty
	}
	var overlay shortTermIndex
	if err := json.Unmarshal(data, &overlay); err != nil || overlay.Files == nil || overlay.Version != historyShortTermVersion {
		return empty
	}
	base := time.Time{}
	if source != nil {
		base = source.GeneratedAt
	}
	if !overlay.BaseGeneratedAt.Equal(base) {
		return empty
	}
	return overlay
}

func saveHistoryShortTerm(brainDir string, overlay shortTermIndex) error {
	overlay.Version = historyShortTermVersion
	data, err := json.Marshal(overlay)
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, historyShortTermPath, append(data, '\n'), 0o600)
}

// clearHistoryShortTerm removes the overlay (called after a completed full
// build — consolidation — under the brain write lock). Best-effort: a missing
// file is the desired state, and a leftover stale overlay is voided by the
// BaseGeneratedAt pin anyway.
func clearHistoryShortTerm(brainDir string) {
	_ = os.Remove(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)))
}

type shortTermStats struct {
	Files               int  `json:"files"`
	Records             int  `json:"records"`
	Exchanges           int  `json:"exchanges"`
	IncompleteExchanges int  `json:"incomplete_exchanges"`
	Reused              int  `json:"reused_files"`
	Scanned             int  `json:"scanned_files"`
	Truncated           bool `json:"truncated,omitempty"`
}

// buildHistoryShortTermLocked rebuilds the overlay: every session transcript
// whose (size, mtime) differs from the long-term scan cache is short-term
// material; files unchanged since the previous delta are carried over without
// re-scanning. Caller holds the brain write lock.
func buildHistoryShortTermLocked(outputDir string, now time.Time) (shortTermStats, error) {
	var stats shortTermStats
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return stats, err
	}
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	sessionsRoot := filepath.Join(outputDir, exportSessionsDirectory)
	if _, err := os.Stat(sessionsRoot); err != nil {
		if os.IsNotExist(err) {
			// No exported sessions at all: an empty overlay is correct.
			clearHistoryShortTerm(outputDir)
			return stats, nil
		}
		return stats, err
	}
	var warnings []string
	files, err := collectHistorySessionFiles(sessionsRoot, &warnings)
	if err != nil {
		return stats, err
	}
	cache := loadHistoryScanCache(outputDir)
	previous := loadHistoryShortTerm(outputDir, source)
	excludedByPath := excludedTranscriptPaths(manifest, loadSessionTombstones(outputDir))
	branchByPath := historyBranchByTranscriptPath(manifest)
	sessionByPath := historySessionByTranscriptPath(manifest)
	repoKey := ""
	if manifest != nil {
		repoKey = manifest.RepoKey
	}
	overlay := shortTermIndex{
		Version:             historyShortTermVersion,
		GeneratedAt:         now,
		SessionsFingerprint: brainSessionsFingerprint(outputDir),
		Files:               map[string]shortTermFile{},
	}
	if source != nil {
		overlay.BaseGeneratedAt = source.GeneratedAt
	}
	for _, file := range files {
		rel, relErr := filepath.Rel(outputDir, file.Path)
		if relErr != nil {
			rel = file.Path
		}
		rel = filepath.ToSlash(rel)
		if _, excluded := excludedByPath[rel]; excluded {
			continue
		}
		// Long-term already current for this file: not short-term material.
		if cached, ok := cache.Files[rel]; ok && cached.Size == file.Size && cached.ModUnixNano == file.ModUnixNano {
			continue
		}
		// Unchanged since the previous delta: carry over without re-scanning.
		if prev, ok := previous.Files[rel]; ok && prev.Size == file.Size && prev.ModUnixNano == file.ModUnixNano {
			overlay.Files[rel] = prev
			stats.Reused++
			continue
		}
		records, incomplete, _, ok := scanSessionFileRecords(outputDir, file.Path, rel)
		if !ok {
			continue // unscannable now; the next delta or full build retries
		}
		records = annotateHistoryRecordBranches(records, rel, branchByPath)
		records = annotateConversationIdentity(records, rel, repoKey, sessionByPath)
		overlay.Files[rel] = shortTermFile{
			Size: file.Size, ModUnixNano: file.ModUnixNano, SortTime: file.SortTime,
			Records: records, IncompleteExchanges: incomplete,
		}
		stats.Scanned++
	}
	// Bound the buffer: drop oldest files first (canonical transcripts retain
	// everything; consolidation restores completeness).
	total := 0
	for _, entry := range overlay.Files {
		total += len(entry.Records)
	}
	if total > historyShortTermMaxRecords {
		type aged struct {
			rel  string
			at   time.Time
			size int
		}
		byAge := make([]aged, 0, len(overlay.Files))
		for rel, entry := range overlay.Files {
			byAge = append(byAge, aged{rel: rel, at: entry.SortTime, size: len(entry.Records)})
		}
		sort.Slice(byAge, func(i, j int) bool { return byAge[i].at.Before(byAge[j].at) })
		for _, candidate := range byAge {
			if total <= historyShortTermMaxRecords {
				break
			}
			delete(overlay.Files, candidate.rel)
			total -= candidate.size
			overlay.Truncated = true
		}
	}
	stats.Truncated = overlay.Truncated
	stats.Files = len(overlay.Files)
	for _, entry := range overlay.Files {
		stats.Records += len(entry.Records)
		stats.IncompleteExchanges += entry.IncompleteExchanges
		for _, record := range entry.Records {
			if record.Kind == conversationKind {
				stats.Exchanges++
			}
		}
	}
	if len(overlay.Files) == 0 {
		clearHistoryShortTerm(outputDir)
		return stats, nil
	}
	return stats, saveHistoryShortTerm(outputDir, overlay)
}

// --- retrieval integration ---

// freshHistory is the two-tier view retrieval ranks over: the long-term index
// exactly as loaded (FTS freshness identity untouched) plus the short-term
// overlay, with per-file replacement semantics — a file the overlay re-scanned
// supersedes that file's long-term records, exactly as a full rebuild would.
type freshHistory struct {
	index    historyIndex // long-term, as on disk
	overlay  []historyRecord
	replaced map[string]bool // rel paths superseded by the overlay
}

func loadFreshHistory(brainDir string, source *historySourceManifest) (freshHistory, error) {
	index, err := loadBrainHistoryIndex(brainDir, source)
	if err != nil {
		return freshHistory{}, err
	}
	fresh := freshHistory{index: index}
	overlay := loadHistoryShortTerm(brainDir, source)
	if len(overlay.Files) == 0 {
		return fresh, nil
	}
	fresh.replaced = make(map[string]bool, len(overlay.Files))
	for rel, entry := range overlay.Files {
		fresh.replaced[rel] = true
		fresh.overlay = append(fresh.overlay, entry.Records...)
	}
	return fresh, nil
}

// mergedRecords is the get/multi-get view: long-term records minus superseded
// files, plus every overlay record (overlay wins for duplicate IDs by order —
// callers index into maps last-write-wins).
func (f freshHistory) mergedRecords() []historyRecord {
	if len(f.overlay) == 0 {
		return f.index.Records
	}
	out := make([]historyRecord, 0, len(f.index.Records)+len(f.overlay))
	for _, record := range f.index.Records {
		if f.replaced[record.Path] {
			continue
		}
		out = append(out, record)
	}
	return append(out, f.overlay...)
}

// longTermActive returns the long-term records minus superseded files, for
// rankers that never touch the FTS store (semantic arms).
func (f freshHistory) longTermActive() historyIndex {
	if len(f.overlay) == 0 {
		return f.index
	}
	kept := make([]historyRecord, 0, len(f.index.Records))
	for _, record := range f.index.Records {
		if f.replaced[record.Path] {
			continue
		}
		kept = append(kept, record)
	}
	return historyIndex{GeneratedAt: f.index.GeneratedAt, Records: kept}
}

// rankFreshHistory ranks the two tiers: longTermRank runs against the on-disk
// long-term index (so the FTS freshness identity is untouched), superseded
// files' hits are dropped, the overlay is ranked in-memory, and the two lists
// RRF-fuse. With an empty overlay the long-term ranking is returned unchanged
// — bit-for-bit default preservation.
func rankFreshHistory(
	fresh freshHistory,
	kind, query string,
	limit int,
	longTermRank func(historyIndex) ([]scoredHistoryRecord, bool),
) []scoredHistoryRecord {
	lex, ok := longTermRank(fresh.index)
	if !ok {
		lex = rankHistoryRecordsScored(fresh.index, kind, query, limit, 0)
	}
	if len(fresh.overlay) == 0 {
		return lex
	}
	kept := lex[:0:0]
	for _, scored := range lex {
		if fresh.replaced[scored.Record.Path] {
			continue
		}
		kept = append(kept, scored)
	}
	overlayRanked := rankHistoryRecordsScored(historyIndex{Records: fresh.overlay}, kind, query, limit, 0)
	if len(overlayRanked) == 0 {
		return kept
	}
	return fuseScoredRankLists([][]scoredHistoryRecord{kept, overlayRanked}, limit)
}

// --- refresh delta verb ---

func newDeltaCommand(opts Options) *cobra.Command {
	var noExport bool
	cmd := &cobra.Command{
		Use:   "delta",
		Short: "Cheap short-term memory update: export new checkpoints and index only changed transcripts",
		Long: `delta is the short-term memory path: it incrementally exports session
transcripts from checkpoints (reusing the export cursor), then scans ONLY the
transcripts that changed since the last full index build into a small overlay
(history/short-term.json) that retrieval searches alongside the long-term
index. Seconds even on very large brains.

The next full 'refresh' (or 'refresh history') is consolidation: it absorbs
everything the overlay covers into long-term memory and clears the overlay.
'watch' runs delta on every tick, so an in-flight session's earlier turns are
recallable near-real-time.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefreshDelta(cmd, opts, !noExport)
		},
	}
	cmd.Flags().BoolVar(&noExport, "no-export", false, "Skip the incremental session export; only re-index already-exported transcripts")
	return cmd
}

func runRefreshDelta(cmd *cobra.Command, opts Options, export bool) error {
	stats, err := runRefreshDeltaStats(cmd, opts, export)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("short-term memory: %d records (%d exchanges) from %d changed transcripts (%d re-scanned, %d carried over)",
		stats.Records, stats.Exchanges, stats.Files, stats.Scanned, stats.Reused)
	if stats.Truncated {
		line += "; buffer full — run `entire brain refresh` to consolidate"
	}
	fmt.Fprintln(cmd.OutOrStdout(), line)
	return nil
}

// runRefreshDeltaStats is the delta core: incremental export (optional) plus
// the short-term overlay build, returning the stats so watch can escalate to
// consolidation on a full buffer.
func runRefreshDeltaStats(cmd *cobra.Command, opts Options, export bool) (shortTermStats, error) {
	ctx := cmd.Context()
	if export {
		exportOpts := exportCommandOptions{
			outputDir:       defaultExportDir,
			checkpointLimit: defaultCheckpointLimit,
			entireBinary:    "entire",
			scope:           exportScopeAll,
		}
		if err := runExport(ctx, cmd, opts, exportOpts); err != nil {
			return shortTermStats{}, fmt.Errorf("delta session export: %w", err)
		}
	}
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return shortTermStats{}, err
	}
	if !local {
		return shortTermStats{}, fmt.Errorf("refresh delta requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return shortTermStats{}, err
	}
	var stats shortTermStats
	err = withBrainWriteLock(storage.BrainDir, func() error {
		var buildErr error
		stats, buildErr = buildHistoryShortTermLocked(storage.BrainDir, opts.Now().UTC())
		return buildErr
	})
	return stats, err
}
