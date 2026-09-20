package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// history_delta.go is the brain's SHORT-TERM MEMORY: a small, bounded overlay
// index covering only the session transcripts that changed since the last full
// (long-term) index build. The split mirrors the human model the product aims
// for:
//
//   - short-term path: `refresh delta` scans just the changed/new transcripts
//     (seconds even on huge brains; change detection rides the same
//     descriptor-rooted content identity as the scan cache) and writes
//     history/short-term.json.
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
// byte-identical results to the pre-overlay behavior; freshness is additive,
// never a ranking change for cold brains.

const (
	historyShortTermFileName = "short-term.json"
	historyShortTermPath     = historyDirName + "/" + historyShortTermFileName
	// historyShortTermVersion 3 binds per-file reuse to the exact bounded
	// descriptor-rooted content digest. Older size+mtime-only overlays are
	// disposable and rebuild on the next delta.
	historyShortTermVersion = 3
	// historyShortTermReconcilerVersion stamps the record-reconciliation rule
	// the overlay was built to participate in (3 = newest-valid-copy selection
	// scoped by logical conversation session, preserving legacy ID collisions
	// across branches/sessions).
	historyShortTermReconcilerVersion = 3
)

// Typed overlay load states: callers must be able to distinguish "no
// overlay" from "unusable overlay", and doctor must never claim coverage from
// anything but a current, complete overlay.
const (
	shortTermStateAbsent      = "absent"
	shortTermStateCurrent     = "current"
	shortTermStateStale       = "stale"
	shortTermStateCorrupt     = "corrupt"
	shortTermStateUnsupported = "unsupported"
)

// historyShortTermMaxRecords bounds the overlay. Short-term memory is a
// buffer, not an archive: past this, the oldest files are dropped (their
// content is still in the canonical transcripts) and the overlay reports
// itself truncated so doctor can recommend consolidation. A var so tests can
// exercise the bound without 20k-record fixtures.
var historyShortTermMaxRecords = 20000

// beforeHistoryShortTermInventoryRecheck is the deterministic membership-race
// seam immediately before the final bounded inventory comparison and overlay
// publication.
var beforeHistoryShortTermInventoryRecheck = func() {}

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
	// FailedFiles are transcripts the delta scan could not read or parse this
	// pass: the overlay is durably partial, never silently complete.
	FailedFiles []string `json:"failed_files,omitempty"`
	// ScanWarnings are directory-level collection problems; like FailedFiles
	// they void any completeness claim.
	ScanWarnings []string `json:"scan_warnings,omitempty"`
	// ReconcilerVersion records the cross-tier record-reconciliation rule this
	// overlay was built for.
	ReconcilerVersion int `json:"reconciler_version,omitempty"`
}

// complete reports whether the overlay fully represents every changed
// transcript it was asked to cover: nothing failed, nothing dropped.
func (o shortTermIndex) complete() bool {
	return !o.Truncated && len(o.FailedFiles) == 0 && len(o.ScanWarnings) == 0
}

// shortTermIncompleteness names exactly why an overlay is not complete
// coverage, for doctor/stats messaging.
func shortTermIncompleteness(overlay shortTermIndex) string {
	var parts []string
	if overlay.Truncated {
		parts = append(parts, "buffer full, oldest changed transcripts dropped")
	}
	if n := len(overlay.FailedFiles); n > 0 {
		parts = append(parts, fmt.Sprintf("%d transcripts failed to scan", n))
	}
	if n := len(overlay.ScanWarnings); n > 0 {
		parts = append(parts, fmt.Sprintf("%d collection warnings", n))
	}
	if len(parts) == 0 {
		return "incomplete"
	}
	return strings.Join(parts, "; ")
}

type shortTermFile struct {
	Size                int64           `json:"size"`
	ModUnixNano         int64           `json:"mod_unix_nano"`
	ContentDigest       string          `json:"content_digest"`
	SortTime            time.Time       `json:"sort_time"`
	Records             []historyRecord `json:"records"`
	IncompleteExchanges int             `json:"incomplete_exchanges,omitempty"`
}

// loadHistoryShortTermState loads the overlay with a typed state:
// absent, current, stale (base pin mismatch), corrupt (unreadable/oversized/
// invalid JSON), or unsupported (version mismatch). Only a current overlay
// carries records; every other state returns the empty overlay so retrieval
// falls back to exact long-term-only behavior.
func loadHistoryShortTermState(brainDir string, source *historySourceManifest) (shortTermIndex, string) {
	empty := shortTermIndex{Version: historyShortTermVersion, Files: map[string]shortTermFile{}}
	data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)), defaultMaxReadBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, shortTermStateAbsent
		}
		return empty, shortTermStateCorrupt
	}
	var overlay shortTermIndex
	if err := json.Unmarshal(data, &overlay); err != nil || overlay.Files == nil {
		return empty, shortTermStateCorrupt
	}
	if overlay.Version != historyShortTermVersion {
		return empty, shortTermStateUnsupported
	}
	// An overlay built for a different record-reconciliation rule must not
	// participate in ranking or coverage claims: the winner selection it was
	// built to join no longer holds.
	if overlay.ReconcilerVersion != historyShortTermReconcilerVersion {
		return empty, shortTermStateUnsupported
	}
	base := time.Time{}
	if source != nil {
		base = source.GeneratedAt
	}
	if !overlay.BaseGeneratedAt.Equal(base) {
		return empty, shortTermStateStale
	}
	return overlay, shortTermStateCurrent
}

// loadHistoryShortTerm returns the overlay when it is valid for the CURRENT
// long-term build; every non-current state reads as an empty overlay, which
// restores exact long-term-only behavior. Health surfaces use
// loadHistoryShortTermState to tell the failure modes apart.
func loadHistoryShortTerm(brainDir string, source *historySourceManifest) shortTermIndex {
	overlay, _ := loadHistoryShortTermState(brainDir, source)
	return overlay
}

// loadHistoryShortTermRaw parses the overlay file without the base-pin or
// version checks, for the consolidation guard: it runs after the pin has
// already moved and only needs the covered source fingerprint. Never rank
// from this view.
func loadHistoryShortTermRaw(brainDir string) (shortTermIndex, string) {
	empty := shortTermIndex{Version: historyShortTermVersion, Files: map[string]shortTermFile{}}
	data, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)), defaultMaxReadBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return empty, shortTermStateAbsent
		}
		return empty, shortTermStateCorrupt
	}
	var overlay shortTermIndex
	if err := json.Unmarshal(data, &overlay); err != nil || overlay.Files == nil {
		return empty, shortTermStateCorrupt
	}
	return overlay, shortTermStateCurrent
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
// build; consolidation; under the brain write lock). Best-effort: a missing
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
	// Failed counts transcripts this delta could not scan; the overlay
	// records their identities durably.
	Failed int `json:"failed_files,omitempty"`
}

// shortTermCanonicalInputRefusal separates ordinary availability failures,
// which remain durably visible as an incomplete overlay, from failures that
// make the canonical input unsafe or unbounded. Permission-denied preserves
// the established retryable FailedFiles behavior; containment, source races,
// cancellation, and bounds refuse publication and leave the prior generation
// untouched.
func shortTermCanonicalInputRefusal(err error) bool {
	if err == nil || os.IsPermission(err) {
		return false
	}
	return errors.Is(err, errHistorySessionInventoryDegraded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), memoryErrSourceStale) ||
		strings.Contains(err.Error(), memoryErrInputTooLarge)
}

// buildHistoryShortTermLocked rebuilds the overlay: every session transcript
// whose content digest differs from the long-term scan cache is short-term
// material; files unchanged since the previous delta are carried over without
// re-parsing. Caller holds the brain write lock.
func buildHistoryShortTermLocked(outputDir string, now time.Time) (shortTermStats, error) {
	return buildHistoryShortTermLockedContext(context.Background(), outputDir, now)
}

func buildHistoryShortTermLockedContext(ctx context.Context, outputDir string, now time.Time) (shortTermStats, error) {
	var stats shortTermStats
	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		return stats, err
	}
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	stones, _, err := loadSessionTombstonesChecked(outputDir)
	if err != nil {
		return stats, err
	}
	inventory, err := collectHistorySessionInventory(ctx, outputDir)
	if err != nil {
		// Match the explicit sentinel, not fs.ErrNotExist: a transcript
		// directory that vanishes DEEPER in the walk also carries an ENOENT,
		// and treating that as an empty Brain would silently clear the overlay
		// on a real integrity failure. os.IsNotExist never matched either,
		// because it does not unwrap.
		if errors.Is(err, errHistorySessionsRootMissing) {
			// No exported sessions at all: an empty overlay is correct.
			clearHistoryShortTerm(outputDir)
			return stats, nil
		}
		return stats, err
	}
	defer inventory.Close()
	files := inventory.files
	cache := loadHistoryScanCache(outputDir)
	previous := loadHistoryShortTerm(outputDir, source)
	excludedByPath, err := excludedTranscriptPathsChecked(outputDir, manifest, stones)
	if err != nil {
		return stats, err
	}
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
		ReconcilerVersion:   historyShortTermReconcilerVersion,
	}
	if source != nil {
		overlay.BaseGeneratedAt = source.GeneratedAt
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		rel := file.Rel
		if _, excluded := excludedByPath[rel]; excluded {
			continue
		}
		content, contentDigest, readErr := readHistorySessionInventoryFile(ctx, inventory, file)
		if readErr != nil {
			if shortTermCanonicalInputRefusal(readErr) {
				// Canonical containment, bound, and source-stability failures
				// refuse the entire delta. The prior overlay remains active.
				return stats, readErr
			}
			overlay.FailedFiles = append(overlay.FailedFiles, rel)
			stats.Failed++
			continue
		}
		// Long-term already current for this file: not short-term material.
		if cached, ok := cache.Files[rel]; ok && cached.Size == file.Size && cached.ModUnixNano == file.ModUnixNano && cached.ContentDigest != "" && cached.ContentDigest == contentDigest {
			if err := inventory.validateFileMembership(ctx, file); err != nil {
				return stats, err
			}
			continue
		}
		// Unchanged since the previous delta: carry over without re-parsing.
		if prev, ok := previous.Files[rel]; ok && prev.Size == file.Size && prev.ModUnixNano == file.ModUnixNano && prev.ContentDigest != "" && prev.ContentDigest == contentDigest {
			if err := inventory.validateFileMembership(ctx, file); err != nil {
				return stats, err
			}
			overlay.Files[rel] = prev
			stats.Reused++
			continue
		}
		records, incomplete, _, ok := scanSessionFileReaderRecords(ctx, bytes.NewReader(content), file.Path, file.Rel)
		if !ok {
			// Unscannable now: record the identity durably so no surface can
			// claim complete coverage; the next delta or full build
			// retries it.
			overlay.FailedFiles = append(overlay.FailedFiles, rel)
			stats.Failed++
			continue
		}
		records = annotateHistoryRecordBranches(records, rel, branchByPath)
		records = annotateConversationIdentity(records, rel, repoKey, sessionByPath)
		overlay.Files[rel] = shortTermFile{
			Size: file.Size, ModUnixNano: file.ModUnixNano, ContentDigest: contentDigest, SortTime: file.SortTime,
			Records: records, IncompleteExchanges: incomplete,
		}
		stats.Scanned++
	}
	if err := inventory.validateAll(ctx); err != nil {
		return stats, err
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
	sort.Strings(overlay.FailedFiles)
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
	beforeHistoryShortTermInventoryRecheck()
	currentInventory, err := collectHistorySessionInventory(ctx, outputDir)
	if err != nil {
		return stats, err
	}
	membershipUnchanged := inventory.sameMembership(currentInventory)
	_ = currentInventory.Close()
	if !membershipUnchanged {
		return stats, fmt.Errorf("%s: canonical transcript membership changed before short-term publication", memoryErrSourceStale)
	}
	// An overlay that holds no records but DID fail or drop something must
	// stay on disk: deleting it would erase the very state that proves
	// coverage is incomplete.
	if len(overlay.Files) == 0 && overlay.complete() {
		clearHistoryShortTerm(outputDir)
		return stats, nil
	}
	return stats, saveHistoryShortTerm(outputDir, overlay)
}

// --- retrieval integration ---

// freshHistory is the two-tier view retrieval ranks over: the long-term index
// exactly as loaded (FTS freshness identity untouched) plus the short-term
// overlay, with per-file replacement semantics; a file the overlay re-scanned
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
	fresh := loadFreshHistoryOverlay(brainDir, source)
	fresh.index = index
	return fresh, nil
}

func loadFreshHistoryOverlay(brainDir string, source *historySourceManifest) freshHistory {
	fresh := freshHistory{}
	overlay := loadHistoryShortTerm(brainDir, source)
	if len(overlay.Files) == 0 {
		return fresh
	}
	fresh.replaced = make(map[string]bool, len(overlay.Files))
	rels := make([]string, 0, len(overlay.Files))
	for rel := range overlay.Files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		entry := overlay.Files[rel]
		fresh.replaced[rel] = true
		fresh.overlay = append(fresh.overlay, entry.Records...)
	}
	return fresh
}

// rankFreshHistoryLexicalFromSource preserves the direct FTS payload path for
// the long-term tier, then overlays short-term records without forcing a full
// index.json load on the common BM25-only path.
func rankFreshHistoryLexicalFromSource(brainDir string, source *historySourceManifest, kind, query string, limit int) ([]scoredHistoryRecord, string, int, error) {
	fresh := loadFreshHistoryOverlay(brainDir, source)
	if len(fresh.replaced) == 0 && len(fresh.overlay) == 0 {
		return rankHistoryLexicalFromSource(brainDir, source, kind, query, limit)
	}
	// Filter before the payload reader deduplicates or limits candidates. This
	// preserves the direct path even when the full JSON truth is unavailable.
	overlayByKey := make(map[historyRecordReplacementKey]historyRecord)
	for _, r := range fresh.overlay {
		key := recordReplacementKey(r)
		if prev, ok := overlayByKey[key]; ok {
			r = newestHistoryRecord(prev, r)
		}
		overlayByKey[key] = r
	}
	eligible := func(r historyRecord) bool {
		if fresh.replaced[r.Path] {
			return false
		}
		if replacement, ok := overlayByKey[recordReplacementKey(r)]; ok {
			return sameRecordCopy(newestHistoryRecord(r, replacement), r)
		}
		return true
	}
	lex, used, directErr := rankHistoryViaFreshFTSCutoffDetailed(brainDir, source, kind, query, limit, historyFTSRelevanceCutoff, eligible)
	derivedCorrupt := errors.Is(directErr, errHistoryFTSPayloadCorrupt)
	if directErr != nil && !derivedCorrupt {
		return nil, "", 0, directErr
	}
	if used {
		for _, hit := range lex {
			fresh.index.Records = append(fresh.index.Records, hit.Record)
		}
		ranked := rankFreshHistory(fresh, kind, query, limit, nil, func(historyIndex, func(historyRecord) bool) ([]scoredHistoryRecord, bool) { return lex, true })
		return ranked, historyIndexAccessFTSPayload, source.Records + len(fresh.overlay), nil
	}
	index, err := loadBrainHistoryIndex(brainDir, source)
	if err != nil {
		return nil, "", 0, err
	}
	fresh.index = index
	repaired := !derivedCorrupt || rebuildHistoryFTSFromTruth(brainDir, index) == nil
	ranked := rankFreshHistory(fresh, kind, query, limit, nil, func(index historyIndex, eligible func(historyRecord) bool) ([]scoredHistoryRecord, bool) {
		if !repaired {
			return nil, false
		}
		ranked, complete, ok := rankHistoryViaFTSFiltered(brainDir, index, kind, query, limit, historyFTSRelevanceCutoff, eligible)
		return ranked, ok && complete
	})
	return ranked, historyIndexAccessJSON, len(index.Records) + len(fresh.overlay), nil
}

// historyRecordSourceTime derives a record's source recency from its
// transcript path: the exporter embeds a sortable timestamp in the filename,
// and the full build's re-export dedupe iterates files newest-first on
// exactly this key. Zero when the filename carries no timestamp.
func historyRecordSourceTime(r historyRecord) time.Time {
	return historySessionSortTime(r.Path, time.Time{})
}

// newestHistoryRecord picks the winner between two copies of one stable ID:
// newest source time, then lexicographically greatest path (paths embed the
// export timestamp), then greatest source digest. The rule is derived only
// from the records themselves, never from which tier held them, so search,
// fusion, and get cannot disagree on the winner.
func newestHistoryRecord(a, b historyRecord) historyRecord {
	at, bt := historyRecordSourceTime(a), historyRecordSourceTime(b)
	if at.After(bt) {
		return a
	}
	if bt.After(at) {
		return b
	}
	if a.Path != b.Path {
		if a.Path > b.Path {
			return a
		}
		return b
	}
	if a.SourceDigest >= b.SourceDigest {
		return a
	}
	return b
}

// sameRecordCopy reports whether two records are the same physical copy of a
// stable identity: same source path, digest, and anchor line.
func sameRecordCopy(a, b historyRecord) bool {
	return a.Path == b.Path && a.SourceDigest == b.SourceDigest && a.Line == b.Line
}

// historyRecordReplacementKey is the scope in which one physical export may
// replace another. Conversation IDs from older indexes are not globally
// unique: the same legacy ID can legitimately occur in different branches or
// sessions. Keeping that scope in the key preserves the ambiguity for callers
// to resolve instead of silently deleting one conversation. Records without a
// stable ID use their physical identity and are never globally collapsed.
type historyRecordReplacementKey struct {
	ID           string
	Kind         string
	Branch       string
	Session      string
	Path         string
	Line         int
	SourceDigest string
}

func recordReplacementKey(r historyRecord) historyRecordReplacementKey {
	id := strings.TrimSpace(r.ID)
	if id == "" {
		return historyRecordReplacementKey{
			Kind: r.Kind, Path: r.Path, Line: r.Line, SourceDigest: r.SourceDigest,
		}
	}
	key := historyRecordReplacementKey{ID: id, Kind: r.Kind}
	if r.Kind == conversationKind {
		// Per-Brain repository identity is implicit. This matches virtual
		// session identity's branch normalization for annotated records and
		// uses its stable default for legacy records without branch metadata.
		key.Branch = conversationCanonicalBranch(r, nil)
		key.Session = strings.TrimSpace(r.SessionID)
		if key.Session == "" {
			// This is the same fallback used by virtual session identity. It
			// prevents two degraded sessions from replacing one another merely
			// because an old producer emitted the same exchange ID.
			key.Session = strings.TrimSpace(r.SourceDigest)
		}
	}
	return key
}

// duplicateRecordWinners maps every replacement-scoped stable identity that
// appears more than once across the two tiers (or twice inside the overlay,
// which lacks the full build's re-export dedupe) to its newest valid copy.
// Empty overlay means the long-term index's own build-time dedupe already
// holds and there is nothing to reconcile.
func (f freshHistory) duplicateRecordWinners() map[historyRecordReplacementKey]historyRecord {
	if len(f.overlay) == 0 {
		return nil
	}
	seen := map[historyRecordReplacementKey]historyRecord{}
	var winners map[historyRecordReplacementKey]historyRecord
	consider := func(r historyRecord) {
		key := recordReplacementKey(r)
		prev, ok := seen[key]
		if !ok {
			seen[key] = r
			return
		}
		w := newestHistoryRecord(prev, r)
		seen[key] = w
		if winners == nil {
			winners = map[historyRecordReplacementKey]historyRecord{}
		}
		winners[key] = w
	}
	for _, r := range f.index.Records {
		if f.replaced[r.Path] {
			continue
		}
		consider(r)
	}
	for _, r := range f.overlay {
		consider(r)
	}
	return winners
}

// mergedRecords is the raw two-tier union: long-term records minus superseded
// files, plus every overlay record. Duplicate stable IDs are NOT reconciled
// here; readers that resolve records by ID must use reconciledRecords.
func (f freshHistory) mergedRecords() []historyRecord {
	if len(f.overlay) == 0 && len(f.replaced) == 0 {
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

// reconciledRecords is the get/multi-get view: the two-tier union with
// every duplicate replacement-scoped identity collapsed to its newest valid
// copy through the same winner rule ranking uses, so expansion resolves
// exactly the record search ranked while cross-session ID collisions remain.
func (f freshHistory) reconciledRecords() []historyRecord {
	merged := f.mergedRecords()
	winners := f.duplicateRecordWinners()
	if len(winners) == 0 {
		return merged
	}
	out := make([]historyRecord, 0, len(merged))
	emitted := map[historyRecordReplacementKey]bool{}
	for _, r := range merged {
		key := recordReplacementKey(r)
		if w, ok := winners[key]; ok {
			if !sameRecordCopy(w, r) || emitted[key] {
				continue
			}
			emitted[key] = true
		}
		out = append(out, r)
	}
	return out
}

// longTermReconciled is longTermActive minus copies superseded by a newer
// overlay copy. The semantic arms rank over it so a stale long-term
// copy can never be scored while get would expand the newer overlay copy.
func (f freshHistory) longTermReconciled() historyIndex {
	active := f.longTermActive()
	winners := f.duplicateRecordWinners()
	if len(winners) == 0 {
		return active
	}
	kept := make([]historyRecord, 0, len(active.Records))
	for _, r := range active.Records {
		if w, ok := winners[recordReplacementKey(r)]; ok && !sameRecordCopy(w, r) {
			continue
		}
		kept = append(kept, r)
	}
	return historyIndex{GeneratedAt: active.GeneratedAt, Records: kept}
}

// longTermActive returns the long-term records minus superseded files, for
// rankers that never touch the FTS store (semantic arms).
func (f freshHistory) longTermActive() historyIndex {
	if len(f.replaced) == 0 {
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

// rankFreshHistory ranks both tiers with replacement eligibility applied
// before candidate limits. The callback receives the original on-disk index
// and the composed eligibility predicate, preserving its FTS identity. Overlay
// and fallback candidates use the same filters before the lists are RRF-fused.

func rankFreshHistory(
	fresh freshHistory,
	kind, query string,
	limit int,
	pred func(historyRecord) bool,
	longTermRank func(historyIndex, func(historyRecord) bool) ([]scoredHistoryRecord, bool),
) []scoredHistoryRecord {
	winners := fresh.duplicateRecordWinners()
	superseded := func(r historyRecord) bool {
		w, ok := winners[recordReplacementKey(r)]
		return ok && !sameRecordCopy(w, r)
	}
	eligible := pred
	if len(fresh.replaced) > 0 || len(winners) > 0 {
		eligible = func(r historyRecord) bool {
			return !fresh.replaced[r.Path] && !superseded(r) && (pred == nil || pred(r))
		}
	}
	lex, ok := longTermRank(fresh.index, eligible)
	if !ok {
		fallback := fresh.index
		if eligible != nil {
			fallback = historyIndex{GeneratedAt: fresh.index.GeneratedAt, Records: filterHistoryRecords(fresh.index.Records, eligible)}
		}
		lex = rankHistoryRecordsScored(fallback, kind, query, limit, 0)
	}
	if len(fresh.overlay) == 0 && len(fresh.replaced) == 0 {
		return lex
	}
	kept := lex[:0:0]
	for _, scored := range lex {
		if fresh.replaced[scored.Record.Path] || superseded(scored.Record) {
			continue
		}
		kept = append(kept, scored)
	}
	overlayRecords := make([]historyRecord, 0, len(fresh.overlay))
	seenOverlay := map[historyRecordReplacementKey]bool{}
	for _, r := range fresh.overlay {
		if pred != nil && !pred(r) {
			continue
		}
		key := recordReplacementKey(r)
		if superseded(r) || (winners[key].ID != "" && seenOverlay[key]) {
			continue
		}
		seenOverlay[key] = true
		overlayRecords = append(overlayRecords, r)
	}
	overlayRanked := rankHistoryRecordsScored(historyIndex{Records: overlayRecords}, kind, query, limit, 0)
	if len(overlayRanked) == 0 {
		return kept
	}
	return fuseScoredRankLists([][]scoredHistoryRecord{kept, overlayRanked}, limit)
}

// rankFreshHistoryExhaustive is the two-tier counterpart for the multi-concept lexical
// AND contract. It enumerates the effective long-term and short-term candidate
// sets through the same replacement-scoped winner rules as normal
// retrieval, then detects overflow only after those rules and the supplied
// filters have been applied. It intentionally does not change rankFreshHistory:
// ordinary single-concept retrieval remains bounded and fast.
func rankFreshHistoryExhaustive(
	fresh freshHistory,
	kind, query string,
	ceiling int,
	pred func(historyRecord) bool,
	longTermRank func(historyIndex, func(historyRecord) bool) ([]scoredHistoryRecord, historyExhaustiveRankState, bool, int),
) ([]scoredHistoryRecord, historyExhaustiveRankState) {
	if ceiling <= 0 {
		return nil, historyExhaustiveRankComplete
	}
	winners := fresh.duplicateRecordWinners()
	superseded := func(r historyRecord) bool {
		w, ok := winners[recordReplacementKey(r)]
		return ok && !sameRecordCopy(w, r)
	}
	eligibleLongTerm := func(r historyRecord) bool {
		if fresh.replaced[r.Path] || superseded(r) {
			return false
		}
		return pred == nil || pred(r)
	}

	lex, state, ok, rawScanned := longTermRank(fresh.index, eligibleLongTerm)
	if state != historyExhaustiveRankComplete {
		return nil, state
	}
	if !ok {
		// A pure-Go fallback must inspect the complete long-term slice to prove
		// exhaustive coverage. Bound that work before allocating a filtered copy;
		// unlike SQLite, there is no inverted index that can skip non-matches.
		fallbackScanned := len(fresh.index.Records)
		if fallbackScanned > historyFTSExhaustiveRawScanCeiling ||
			rawScanned > historyFTSExhaustiveRawScanCeiling-fallbackScanned {
			return nil, historyExhaustiveRankRawScanOverflow
		}
		rawScanned += fallbackScanned
		fallback := historyIndex{GeneratedAt: fresh.index.GeneratedAt, Records: filterHistoryRecords(fresh.index.Records, eligibleLongTerm)}
		lex = rankHistoryRecordsScoredExhaustive(fallback, kind, query, ceiling+1)
		if len(lex) > ceiling {
			return nil, historyExhaustiveRankCandidateOverflow
		}
	}
	if len(fresh.overlay) == 0 {
		return lex, historyExhaustiveRankComplete
	}
	// The overlay is always ranked in memory. Its inspection budget joins the
	// long-term arm's budget for this concept rather than resetting it, so a
	// large two-tier union cannot evade the raw-scan refusal by splitting rows
	// across the FTS and overlay stores.
	if len(fresh.overlay) > historyFTSExhaustiveRawScanCeiling ||
		rawScanned > historyFTSExhaustiveRawScanCeiling-len(fresh.overlay) {
		return nil, historyExhaustiveRankRawScanOverflow
	}

	overlayRecords := make([]historyRecord, 0, len(fresh.overlay))
	seenOverlay := map[historyRecordReplacementKey]bool{}
	for _, r := range fresh.overlay {
		key := recordReplacementKey(r)
		if superseded(r) || (winners[key].ID != "" && seenOverlay[key]) || (pred != nil && !pred(r)) {
			continue
		}
		seenOverlay[key] = true
		overlayRecords = append(overlayRecords, r)
	}
	overlayRanked := rankHistoryRecordsScoredExhaustive(historyIndex{Records: overlayRecords}, kind, query, ceiling+1)
	if len(overlayRanked) > ceiling {
		return nil, historyExhaustiveRankCandidateOverflow
	}
	fused := fuseScoredRankLists([][]scoredHistoryRecord{lex, overlayRanked}, ceiling+1)
	if len(fused) > ceiling {
		return nil, historyExhaustiveRankCandidateOverflow
	}
	return fused, historyExhaustiveRankComplete
}

// exhaustiveHistoryRecordIdentity is the multi-concept candidate dedup key. Logical session
// scope distinguishes legacy ID collisions across sessions; physical identity
// is the fallback for records that predate stable IDs. Copy-winner
// reconciliation runs before this key is consulted, so re-exported copies
// still contribute once.
func exhaustiveHistoryRecordIdentity(r historyRecord) historyRecordReplacementKey {
	return recordReplacementKey(r)
}

// rankHistoryRecordsScoredExhaustive is the in-memory lexical ranker for multi-concept
// complete-enumeration path. The ordinary ranker intentionally collapses equal
// normalized summaries for concise single-concept results; that policy is not
// valid for session-scoped AND coverage because equal text can be independent
// evidence in two sessions.
func rankHistoryRecordsScoredExhaustive(index historyIndex, kind, query string, limit int) []scoredHistoryRecord {
	if limit <= 0 {
		return nil
	}
	allowed := historyInspectKinds(kind)
	scored := make([]scoredHistoryRecord, 0, min(len(index.Records), limit))
	seen := map[historyRecordReplacementKey]struct{}{}
	for i, record := range index.Records {
		if len(allowed) > 0 {
			if _, ok := allowed[record.Kind]; !ok {
				continue
			}
		} else if historyGeneralRankingHiddenKind(record.Kind) {
			continue
		}
		score := historyRecordQueryScoreMin(record, query, 0)
		if score == 0 {
			continue
		}
		key := exhaustiveHistoryRecordIdentity(record)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		score += historyInspectKindPreference(kind, record.Kind)
		scored = append(scored, scoredHistoryRecord{Record: record, Score: score, Order: i})
	}
	sort.Slice(scored, func(i, j int) bool {
		left, right := scored[i], scored[j]
		if left.Score != right.Score {
			return left.Score > right.Score
		}
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

// filterHistoryRecords returns the records passing pred, for the in-memory
// ranking arms. Never feed a filtered slice to the FTS ranker: the shared
// store's freshness identity must only ever see the on-disk index.
func filterHistoryRecords(records []historyRecord, pred func(historyRecord) bool) []historyRecord {
	kept := make([]historyRecord, 0, len(records))
	for _, r := range records {
		if pred(r) {
			kept = append(kept, r)
		}
	}
	return kept
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
		line += "; buffer full; run `entire brain refresh` to consolidate"
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
		stats, buildErr = buildHistoryShortTermLockedContext(ctx, storage.BrainDir, opts.Now().UTC())
		return buildErr
	})
	return stats, err
}
