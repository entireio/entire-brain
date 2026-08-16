package cli

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// session_privacy_verify.go completes Phase 4: `privacy verify` proves that
// excluded and purged sessions are absent from every inspectable derived
// truth, and `privacy retention` applies an age/branch policy over sessions.
//
// Verification model: the brain's text truths (history index, scan cache,
// short-term overlay, episodes, facts, distill cache, exported transcripts)
// are inspected directly and strictly. The FTS and pattern SQLite stores get
// integrity plus row-level attribution checks; the corpus also carries the
// exact tombstone-policy identity used by its builder. Vector stores are
// deleted wholesale during cleanup; a recreated store must postdate the policy
// and its record-ID table must exactly fit the current tombstone-filtered
// history generation. No unreadable or unclassified state is treated as empty.

type privacyVerifyFinding struct {
	SessionID string `json:"session_id"`
	Artifact  string `json:"artifact"`
	Detail    string `json:"detail"`
}

type privacyVerifyReport struct {
	CheckedSessions int                    `json:"checked_sessions"`
	Findings        []privacyVerifyFinding `json:"findings"`
	Clean           bool                   `json:"clean"`
}

func loadHistoryShortTermForPrivacy(brainDir string) (shortTermIndex, error) {
	empty := shortTermIndex{Version: historyShortTermVersion, Files: map[string]shortTermFile{}}
	data, present, err := readPrivacyArtifact(brainDir, historyShortTermPath, "short-term history", defaultMaxReadBytes)
	if err != nil || !present {
		return empty, err
	}
	var overlay shortTermIndex
	if err := json.Unmarshal(data, &overlay); err != nil || overlay.Files == nil {
		return empty, fmt.Errorf("%s: decode %s", memoryErrStateCorrupt, historyShortTermPath)
	}
	if overlay.Version != historyShortTermVersion || overlay.ReconcilerVersion != historyShortTermReconcilerVersion {
		return empty, fmt.Errorf("%s: unsupported %s schema/reconciler version", memoryErrUnsupportedVersion, historyShortTermPath)
	}
	// A stale base pin is irrelevant to privacy: its records are still on disk
	// and therefore must be inspected rather than silently treated as absent.
	return overlay, nil
}

func loadHistoryScanCacheForPrivacy(brainDir string) (historyScanCache, error) {
	empty := historyScanCache{Version: historyScanCacheVersion, Files: map[string]historyScanCacheEntry{}}
	data, present, err := readPrivacyArtifact(brainDir, historyScanCachePath, "history scan cache", 256<<20)
	if err != nil || !present {
		return empty, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return empty, fmt.Errorf("%s: open %s: %w", memoryErrStateCorrupt, historyScanCachePath, err)
	}
	decoded, readErr := safeReadAll(gz, semanticSnapshotMaxBytes(), historyScanCachePath+" (decompressed)")
	closeErr := gz.Close()
	if readErr != nil {
		return empty, fmt.Errorf("%s: read %s: %w", memoryErrStateCorrupt, historyScanCachePath, readErr)
	}
	if closeErr != nil {
		return empty, fmt.Errorf("%s: close %s: %w", memoryErrStateCorrupt, historyScanCachePath, closeErr)
	}
	var cache historyScanCache
	if err := json.Unmarshal(decoded, &cache); err != nil || cache.Files == nil {
		return empty, fmt.Errorf("%s: decode %s", memoryErrStateCorrupt, historyScanCachePath)
	}
	if cache.Version != historyScanCacheVersion {
		return empty, fmt.Errorf("%s: unsupported %s schema version %d", memoryErrUnsupportedVersion, historyScanCachePath, cache.Version)
	}
	return cache, nil
}

func loadDistillCacheForPrivacy(brainDir string) (distillCache, error) {
	empty := distillCache{Version: distillCacheVersion, Sessions: map[string]string{}}
	data, present, err := readPrivacyArtifact(brainDir, distillCachePath, "distill cache", maxManifestBytes)
	if err != nil || !present {
		return empty, err
	}
	var cache distillCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Sessions == nil {
		return empty, fmt.Errorf("%s: decode %s", memoryErrStateCorrupt, distillCachePath)
	}
	if cache.Version != distillCacheVersion {
		return empty, fmt.Errorf("%s: unsupported %s schema version %d", memoryErrUnsupportedVersion, distillCachePath, cache.Version)
	}
	return cache, nil
}

func validatePrivacyJSONLines(brainDir, rel string) error {
	data, present, err := readPrivacyArtifact(brainDir, rel, "derived JSON store", 256<<20)
	if err != nil || !present {
		return err
	}
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var value map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &value); err != nil || value == nil {
			return fmt.Errorf("%s: parse %s line %d", memoryErrStateCorrupt, rel, i+1)
		}
	}
	return nil
}

func validatePrivacySQLiteSnapshot(path, rel string) error {
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("%s: open derived SQLite store %s: %w", memoryErrStateCorrupt, rel, err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("%s: verify derived SQLite store %s: %w", memoryErrStateCorrupt, rel, err)
	}
	if integrity != "ok" {
		return fmt.Errorf("%s: derived SQLite store %s failed integrity check: %s", memoryErrStateCorrupt, rel, integrity)
	}
	return nil
}

const privacyVectorVerifyMaxRows = 2_000_000
const privacySQLiteSnapshotMaxBytes int64 = 16 << 30

type privacySQLiteSnapshotSource struct {
	rel      string
	path     string
	expected os.FileInfo
	file     *os.File
}

// snapshotPrivacySQLiteStore pins the Brain-relative main database and its WAL
// through descriptor-bound, no-follow opens, then streams a bounded copy into
// a private temporary directory. The private WAL is checkpointed before the
// immutable verification readers open the snapshot. Copying only the main file
// would miss committed rows that have not yet been checkpointed from WAL.
// SQLite never receives a repository-controlled path.
func snapshotPrivacySQLiteStore(brainDir, rel string) (string, func(), error) {
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return "", nil, fmt.Errorf("%s: unsafe SQLite store path: %w", memoryErrStateUnsafe, err)
	}
	rel = filepath.ToSlash(clean)

	openSource := func(sourceRel, label string, required bool) (*privacySQLiteSnapshotSource, bool, error) {
		expected, present, infoErr := privacyArtifactInfo(brainDir, sourceRel, label)
		if infoErr != nil {
			return nil, present, infoErr
		}
		if !present {
			if required {
				return nil, false, fmt.Errorf("%s: SQLite store disappeared before verification: %s", memoryErrStateCorrupt, sourceRel)
			}
			return nil, false, nil
		}
		path := filepath.Join(brainDir, filepath.FromSlash(sourceRel))
		file, openErr := privacyOpen(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
		if openErr != nil {
			return nil, true, fmt.Errorf("%s: open %s without following links: %w", memoryErrStateUnsafe, label, openErr)
		}
		if aliasErr := rejectOpenFileAlias(path, file, label); aliasErr != nil {
			_ = file.Close()
			return nil, true, fmt.Errorf("%s: validate %s identity: %w", memoryErrStateUnsafe, label, aliasErr)
		}
		return &privacySQLiteSnapshotSource{rel: sourceRel, path: path, expected: expected, file: file}, true, nil
	}

	mainSource, _, err := openSource(rel, "SQLite store", true)
	if err != nil {
		return "", nil, err
	}
	sources := []*privacySQLiteSnapshotSource{mainSource}
	defer func() {
		for _, source := range sources {
			_ = source.file.Close()
		}
	}()
	walRel := rel + "-wal"
	walSource, walPresent, err := openSource(walRel, "SQLite WAL", false)
	if err != nil {
		return "", nil, err
	}
	if walPresent {
		sources = append(sources, walSource)
	}

	tempDir, err := os.MkdirTemp("", "entire-brain-privacy-sqlite-")
	if err != nil {
		return "", nil, fmt.Errorf("%s: create private SQLite verification directory: %w", memoryErrStateCorrupt, err)
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }
	snapshotPath := filepath.Join(tempDir, "snapshot.sqlite")
	totalCopied := int64(0)
	for i, source := range sources {
		if source.expected.Size() < 0 || source.expected.Size() > privacySQLiteSnapshotMaxBytes-totalCopied {
			cleanup()
			return "", nil, fmt.Errorf("%s: SQLite store and WAL exceed the bounded snapshot ceiling", memoryErrStateUnsafe)
		}
		destination := snapshotPath
		if i > 0 {
			destination += "-wal"
		}
		snapshot, createErr := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			cleanup()
			return "", nil, fmt.Errorf("%s: create SQLite verification snapshot: %w", memoryErrStateCorrupt, createErr)
		}
		remaining := privacySQLiteSnapshotMaxBytes - totalCopied
		copied, copyErr := io.Copy(snapshot, io.LimitReader(source.file, remaining+1))
		closeErr := snapshot.Close()
		if copyErr != nil || closeErr != nil || copied != source.expected.Size() || copied > remaining {
			cleanup()
			return "", nil, fmt.Errorf("%s: SQLite store or WAL changed or exceeded bounds while snapshotting", memoryErrStateUnsafe)
		}
		totalCopied += copied
	}

	// Revalidate every pinned descriptor and pathname after the complete set was
	// copied. This catches a checkpoint, append, truncation, or path replacement
	// that raced the main/WAL snapshot. Also prove an initially absent WAL did
	// not appear while the main database was being copied.
	for _, source := range sources {
		after, statErr := source.file.Stat()
		if statErr != nil || !os.SameFile(source.expected, after) || after.Size() != source.expected.Size() || !after.ModTime().Equal(source.expected.ModTime()) {
			cleanup()
			return "", nil, fmt.Errorf("%s: SQLite store or WAL changed while snapshotting", memoryErrStateUnsafe)
		}
		if aliasErr := rejectOpenFileAlias(source.path, source.file, "SQLite snapshot source"); aliasErr != nil {
			cleanup()
			return "", nil, fmt.Errorf("%s: SQLite store or WAL path changed while snapshotting: %w", memoryErrStateUnsafe, aliasErr)
		}
		current, present, infoErr := privacyArtifactInfo(brainDir, source.rel, "SQLite snapshot source")
		if infoErr != nil || !present || !os.SameFile(source.expected, current) || current.Size() != source.expected.Size() || !current.ModTime().Equal(source.expected.ModTime()) {
			cleanup()
			return "", nil, fmt.Errorf("%s: SQLite store or WAL identity changed while snapshotting", memoryErrStateUnsafe)
		}
	}
	if !walPresent {
		if _, present, infoErr := privacyArtifactInfo(brainDir, walRel, "SQLite WAL"); infoErr != nil || present {
			cleanup()
			return "", nil, fmt.Errorf("%s: SQLite WAL appeared while snapshotting", memoryErrStateUnsafe)
		}
	}

	// Replay committed WAL frames only inside the trusted temporary directory.
	// The later immutable readers can now inspect one self-contained main file.
	db, err := sql.Open(sqliteDriverName, snapshotPath)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s: open private SQLite snapshot: %w", memoryErrStateCorrupt, err)
	}
	db.SetMaxOpenConns(1)
	var busy, logFrames, checkpointedFrames int
	checkpointErr := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames)
	closeErr := db.Close()
	if checkpointErr != nil || closeErr != nil || busy != 0 || (logFrames > 0 && checkpointedFrames != logFrames) {
		cleanup()
		return "", nil, fmt.Errorf("%s: replay private SQLite WAL before verification", memoryErrStateCorrupt)
	}
	return snapshotPath, cleanup, nil
}

func inspectPrivacyVectorIDsSnapshot(snapshot, rel string, wanted map[string]struct{}, findingSessionID string) ([]privacyVerifyFinding, error) {
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(snapshot))
	if err != nil {
		return nil, fmt.Errorf("%s: open vector store %s: %w", memoryErrStateCorrupt, rel, err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return nil, fmt.Errorf("%s: vector store %s failed integrity verification", memoryErrStateCorrupt, rel)
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT record_id FROM history_ids LIMIT %d`, privacyVectorVerifyMaxRows+1))
	if err != nil {
		return nil, fmt.Errorf("%s: inspect vector IDs in %s: %w", memoryErrStateCorrupt, rel, err)
	}
	defer rows.Close()
	var findings []privacyVerifyFinding
	count := 0
	for rows.Next() {
		count++
		if count > privacyVectorVerifyMaxRows {
			return nil, fmt.Errorf("%s: vector store %s exceeds verification ceiling", memoryErrStateUnsafe, rel)
		}
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: inspect vector ID in %s: %w", memoryErrStateCorrupt, rel, err)
		}
		if _, current := wanted[id]; !current {
			findings = append(findings, privacyVerifyFinding{
				SessionID: findingSessionID,
				Artifact:  "vector_store",
				Detail:    rel + " contains record " + id + " outside the current tombstone-filtered history generation; rebuild vectors",
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: inspect vector IDs in %s: %w", memoryErrStateCorrupt, rel, err)
	}
	return findings, nil
}

const privacyCorpusVerifyMaxRows = 2_000_000

// inspectPrivacyPatternCorpus performs row-level attribution over the corpus,
// including path aliases whose session_id is blank or different. Mtime and
// integrity alone cannot prove a valid SQLite file was rebuilt from clean
// inputs after the tombstone landed.
func inspectPrivacyPatternCorpus(snapshotPath string, stones sessionTombstones, pathsBySession map[string]map[string]bool, expectedPolicyIdentity, findingSessionID string) ([]privacyVerifyFinding, error) {
	ownerByPath := map[string]string{}
	for id, paths := range pathsBySession {
		for rel := range paths {
			ownerByPath[normalizePrivacyTranscriptPath(rel)] = id
		}
	}
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(snapshotPath))
	if err != nil {
		return nil, fmt.Errorf("%s: open %s: %w", memoryErrStateCorrupt, patternCorpusPath, err)
	}
	defer db.Close()
	type corpusTable struct {
		name       string
		pathColumn string
		artifact   string
	}
	var findings []privacyVerifyFinding
	if got := corpusMeta(db, "privacy_policy_identity"); got != expectedPolicyIdentity {
		findings = append(findings, privacyVerifyFinding{
			SessionID: findingSessionID,
			Artifact:  "pattern_corpus_policy",
			Detail:    "corpus was not built under the current tombstone policy; rebuild patterns before serving it",
		})
	}
	seen := map[string]bool{}
	for _, table := range []corpusTable{
		{name: "indexed_sessions", pathColumn: "transcript_path", artifact: "pattern_corpus_session"},
		{name: "episodes", pathColumn: "source_path", artifact: "pattern_corpus_episode"},
	} {
		query := fmt.Sprintf("SELECT id, session_id, %s FROM %s LIMIT %d", table.pathColumn, table.name, privacyCorpusVerifyMaxRows+1)
		rows, err := db.Query(query)
		if err != nil {
			return nil, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateCorrupt, patternCorpusPath, table.name, err)
		}
		count := 0
		for rows.Next() {
			count++
			if count > privacyCorpusVerifyMaxRows {
				rows.Close()
				return nil, fmt.Errorf("%s: %s %s exceeds %d rows", memoryErrStateCorrupt, patternCorpusPath, table.name, privacyCorpusVerifyMaxRows)
			}
			var rowID, sessionID, sourcePath string
			if err := rows.Scan(&rowID, &sessionID, &sourcePath); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: scan %s %s: %w", memoryErrStateCorrupt, patternCorpusPath, table.name, err)
			}
			owner := ""
			if _, excluded := stones.Excluded[strings.TrimSpace(sessionID)]; excluded {
				owner = strings.TrimSpace(sessionID)
			} else {
				owner = ownerByPath[normalizePrivacyTranscriptPath(sourcePath)]
			}
			key := table.artifact + "\x00" + owner
			if owner != "" && !seen[key] {
				seen[key] = true
				findings = append(findings, privacyVerifyFinding{
					SessionID: owner,
					Artifact:  table.artifact,
					Detail:    fmt.Sprintf("%s row %s retains excluded session/path %s", table.name, rowID, normalizePrivacyTranscriptPath(sourcePath)),
				})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%s: iterate %s %s: %w", memoryErrStateCorrupt, patternCorpusPath, table.name, err)
		}
		rows.Close()
	}
	return findings, nil
}

func loadPrivacyTransactionChecked(brainDir, sessionID string) (privacyTransaction, bool, error) {
	rel := privacyTransactionRel(sessionID)
	data, present, err := readPrivacyArtifact(brainDir, rel, "privacy transaction", maxManifestBytes)
	if err != nil || !present {
		return privacyTransaction{}, present, err
	}
	tx, err := decodePrivacyTransaction(data, sessionID)
	if err != nil {
		return privacyTransaction{}, true, err
	}
	return tx, true, nil
}

func decodePrivacyTransaction(data []byte, sessionID string) (privacyTransaction, error) {
	var tx privacyTransaction
	version, err := decodeStrictVersionedJSON(data, &tx, privacyTransactionVersion, "privacy transaction")
	if err != nil {
		return privacyTransaction{}, err
	}
	validState := tx.State == privacyStateRequested || tx.State == privacyStateGuarded || tx.State == privacyStateRebuilding ||
		tx.State == privacyStateVerified || tx.State == privacyStateComplete || tx.State == privacyStateError
	if (version != privacyTransactionLegacyVersion && version != privacyTransactionVersion) || tx.SchemaVersion != version || tx.SessionID != sessionID ||
		(tx.Operation != "exclude" && tx.Operation != "purge") || !validState || tx.StartedAt.IsZero() || tx.UpdatedAt.IsZero() ||
		tx.UpdatedAt.Before(tx.StartedAt) {
		return privacyTransaction{}, fmt.Errorf("%s: invalid privacy transaction %s", memoryErrStateCorrupt, privacyTransactionRel(sessionID))
	}
	for _, artifact := range tx.Artifacts {
		if strings.TrimSpace(artifact.Path) == "" || artifact.Bytes < 0 {
			return privacyTransaction{}, fmt.Errorf("%s: invalid privacy transaction artifact %s", memoryErrStateCorrupt, privacyTransactionRel(sessionID))
		}
	}
	if len(tx.SessionRefs) > privacyTransactionSessionRefMax {
		return privacyTransaction{}, fmt.Errorf("%s: privacy transaction has too many session refs %s", memoryErrStateCorrupt, privacyTransactionRel(sessionID))
	}
	seenRefs := map[string]bool{}
	for _, ref := range tx.SessionRefs {
		if !strings.HasPrefix(ref, conversationSessionIDPrefix) || strings.TrimSpace(strings.TrimPrefix(ref, conversationSessionIDPrefix)) == "" || seenRefs[ref] {
			return privacyTransaction{}, fmt.Errorf("%s: invalid privacy transaction session ref %s", memoryErrStateCorrupt, privacyTransactionRel(sessionID))
		}
		seenRefs[ref] = true
	}
	return tx, nil
}

// privacyDerivedReadGate prevents the tombstone-first cleanup window from
// exposing corpus-derived content. A valid tombstone does not permanently
// disable patterns: once the authoritative full verification is clean, reads
// resume normally.
func privacyDerivedReadGate(brainDir string) (bool, error) {
	// The corpus is optional, but when present its main database and WAL must be
	// ordinary Brain-owned files even when there are no tombstones. Without this
	// unconditional check, best-effort pattern loaders could hide an unsafe alias
	// as an empty result on status/get/skill surfaces.
	if err := validatePatternCorpusReadSafety(brainDir); err != nil {
		return false, err
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return false, err
	}
	if len(stones.Excluded) == 0 {
		return true, nil
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil {
		return false, err
	}
	return report.Clean, nil
}

func requirePrivacyDerivedRead(brainDir string) error {
	allowed, err := privacyDerivedReadGate(brainDir)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("%s: derived pattern state is unavailable until excluded-session cleanup verifies clean; run `entire brain privacy purge <session-id>`", memoryErrPrivacyDirty)
	}
	return nil
}

func newPrivacyVerifyCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Prove excluded and purged sessions are absent from every derived projection",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			report, err := verifySessionPrivacy(storage.BrainDir)
			if err != nil {
				return err
			}
			if jsonOut {
				if err := writeJSON(cmd, report); err != nil {
					return err
				}
			} else if err := writeText(cmd, func(out io.Writer) {
				fmt.Fprintf(out, "verified %d excluded/purged sessions\n", report.CheckedSessions)
				for _, finding := range report.Findings {
					fmt.Fprintf(out, "VIOLATION %s: %s (%s)\n", finding.SessionID, finding.Artifact, finding.Detail)
				}
				if report.Clean {
					fmt.Fprintln(out, "clean: no excluded content in any inspected projection")
				}
			}); err != nil {
				return err
			}
			if !report.Clean {
				return fmt.Errorf("privacy verify found %d violations; re-run `entire brain privacy purge <session-id>` (idempotent) or `entire brain refresh` to rebuild", len(report.Findings))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// verifySessionPrivacy walks every tombstoned session and asserts its absence
// from each inspectable projection. Purged sessions (tombstone reason
// "purged") additionally require the exported transcript to be gone; excluded
// sessions deliberately keep it.
func verifySessionPrivacy(brainDir string) (privacyVerifyReport, error) {
	report := privacyVerifyReport{Findings: []privacyVerifyFinding{}}
	stones, tombstoneState, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return report, err
	}
	if len(stones.Excluded) == 0 {
		report.Clean = true
		return report, nil
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return report, err
	}
	add := func(sessionID, artifact, detail string) {
		report.Findings = append(report.Findings, privacyVerifyFinding{SessionID: sessionID, Artifact: artifact, Detail: detail})
	}

	// Resolve each tombstoned session's durable cleanup scope. The transaction
	// retains canonical refs and transcript artifacts captured before mutation,
	// so refresh cannot make abstract artifacts unverifiable by dropping the manifest
	// entry after a failed tombstone-first cleanup.
	pathsBySession := map[string]map[string]bool{}
	refsBySession := map[string]map[string]bool{}
	txBySession := map[string]privacyTransaction{}
	for id := range stones.Excluded {
		pathsBySession[id] = map[string]bool{}
		refsBySession[id] = sessionRefsForSessionID(brainDir, manifest, id)
		tx, present, txErr := loadPrivacyTransactionChecked(brainDir, id)
		if txErr != nil {
			return report, txErr
		}
		if present {
			txBySession[id] = tx
			for _, ref := range tx.SessionRefs {
				refsBySession[id][ref] = true
			}
			for _, artifact := range tx.Artifacts {
				rel := normalizePrivacyTranscriptPath(artifact.Path)
				if strings.HasPrefix(rel, exportSessionsDirectory+"/") {
					pathsBySession[id][rel] = true
				}
			}
		}
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		for _, session := range manifest.Sources.Sessions.Sessions {
			id := strings.TrimSpace(session.SessionID)
			if paths, tracked := pathsBySession[id]; tracked {
				if rel := normalizePrivacyTranscriptPath(session.TranscriptPath); rel != "" {
					paths[rel] = true
				}
			}
		}
	}
	hints, hintIssues := loadMemoryHintsChecked(brainDir)
	jobs, jobIssues := loadMemoryJobsChecked(brainDir)
	if err := memoryStateError(append(hintIssues, jobIssues...)); err != nil {
		return report, err
	}
	for _, job := range jobs {
		if refs, tracked := refsBySession[job.SessionID]; tracked && strings.HasPrefix(job.SessionRef, conversationSessionIDPrefix) {
			refs[job.SessionRef] = true
		}
	}
	excludedPath := func(rel string) (string, bool) {
		rel = normalizePrivacyTranscriptPath(rel)
		for id, paths := range pathsBySession {
			if paths[rel] {
				return id, true
			}
		}
		return "", false
	}

	// History index records. A declared source is authoritative and must load.
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	var activeIndex historyIndex
	if source != nil {
		index, err := loadBrainHistoryIndex(brainDir, source)
		if err != nil {
			return report, err
		}
		activeIndex = index
		for _, record := range index.Records {
			if _, tombstoned := stones.Excluded[record.SessionID]; tombstoned && record.SessionID != "" {
				add(record.SessionID, "history_index", "record "+record.ID)
			} else if id, hit := excludedPath(record.Path); hit {
				add(id, "history_index", "record "+record.ID+" via path "+record.Path)
			}
		}
	}
	// The overlay is independently discoverable on disk. Inspect it even when
	// the manifest declares no history source; otherwise an orphaned current or
	// corrupt overlay could make verification claim a false clean state.
	overlay, err := loadHistoryShortTermForPrivacy(brainDir)
	if err != nil {
		return report, err
	}
	for rel, entry := range overlay.Files {
		if id, hit := excludedPath(rel); hit {
			add(id, "short_term_memory", rel)
			continue
		}
		// Record-level check mirrors the history-index check: an overlay
		// record can carry a tombstoned session id under a path the manifest
		// no longer maps.
		for _, record := range entry.Records {
			if _, tombstoned := stones.Excluded[record.SessionID]; tombstoned && record.SessionID != "" {
				add(record.SessionID, "short_term_memory", "record "+record.ID+" in "+rel)
			}
		}
	}
	// Scan cache entries.
	cache, err := loadHistoryScanCacheForPrivacy(brainDir)
	if err != nil {
		return report, err
	}
	for rel := range cache.Files {
		if id, hit := excludedPath(rel); hit {
			add(id, "scan_cache", rel)
		}
	}
	// Episodes. Malformed NDJSON is not preservable opaque content: both purge
	// and verify use the same strict parser and fail before claiming cleanliness.
	episodeLines, _, err := loadPrivacyEpisodeLines(brainDir)
	if err != nil {
		return report, err
	}
	for _, line := range episodeLines {
		record := line.record
		if _, tombstoned := stones.Excluded[strings.TrimSpace(record.SessionID)]; tombstoned && record.SessionID != "" {
			add(record.SessionID, "episodes", "episode "+record.ID)
		} else if id, hit := excludedPath(record.Source.Path); hit {
			add(id, "episodes", "episode "+record.ID+" via anchor")
		}
	}
	// Fact provenance anchors.
	byBranch, err := loadAllFactBranchesForPrivacy(brainDir)
	if err != nil {
		return report, err
	}
	for branch, facts := range byBranch {
		for _, fact := range facts {
			for _, anchor := range fact.Provenance {
				if _, tombstoned := stones.Excluded[strings.TrimSpace(anchor.SessionID)]; tombstoned && anchor.SessionID != "" {
					add(anchor.SessionID, "facts", "fact "+fact.ID+" (branch "+branch+")")
				} else if id, hit := excludedPath(anchor.Transcript); hit {
					add(id, "facts", "fact "+fact.ID+" (branch "+branch+") via transcript anchor")
				}
			}
		}
	}
	// Distill cache keys.
	distill, err := loadDistillCacheForPrivacy(brainDir)
	if err != nil {
		return report, err
	}
	for key := range distill.Sessions {
		for id := range stones.Excluded {
			if strings.HasSuffix(key, "/"+url.PathEscape(id)) {
				add(id, "distill_cache", key)
			}
		}
	}
	// Derived binary stores: exclude and purge delete every store in
	// the shared inventory wholesale (then rebuild what regenerates), and
	// later rebuilds honor tombstones at build time. A store file that still
	// predates the last tombstone write is exactly the failed/locked deletion
	// this check exists to catch; a store recreated after cleanup is clean by
	// construction. Both timestamps come from the same filesystem clock (the
	// tombstone file's mtime, not the recorded wall-clock time) so an
	// injected or skewed clock cannot fake either outcome.
	var newestID string
	var newestAt time.Time
	for id, stone := range stones.Excluded {
		if stone.At.After(newestAt) {
			newestID, newestAt = id, stone.At
		}
	}
	tombstoneInfo, tombstonesPresent, statErr := privacyArtifactInfo(brainDir, sessionTombstonesPath, "session tombstones")
	if statErr != nil {
		return report, statErr
	}
	if !tombstonesPresent {
		return report, fmt.Errorf("%s: session tombstones disappeared during privacy verification", memoryErrStateCorrupt)
	}
	cutoff := tombstoneInfo.ModTime()
	artifacts, inventoryErr := privacyDerivedStoreArtifacts(brainDir)
	if inventoryErr != nil {
		return report, inventoryErr
	}
	presentArtifacts := map[string]bool{}
	currentArtifacts := map[string]bool{}
	for _, rel := range artifacts {
		storeInfo, present, statErr := privacyArtifactInfo(brainDir, rel, "derived privacy store")
		if statErr != nil {
			return report, statErr
		}
		if !present {
			continue
		}
		presentArtifacts[rel] = true
		switch {
		case rel == patternCorpusPublicationMarkerPath:
			add(newestID, "pattern_corpus_publication", rel+" records an interrupted publication; privacy cleanup must remove the classified marker and candidate")
			continue
		case isPatternCorpusRollbackJournalRel(rel):
			// A rollback journal may retain pre-tombstone pages and cannot be
			// proven clean by the main database's row-level snapshot. Its mtime
			// is therefore never evidence of a successful privacy rebuild.
			add(newestID, "pattern_corpus_rollback_journal", rel+" is opaque content-bearing rollback state; privacy cleanup must remove it")
			continue
		case isPatternCorpusStagingArtifactRel(rel):
			add(newestID, "pattern_corpus_staging", rel+" is an unpublished content-bearing corpus generation; privacy cleanup must remove it")
			continue
		}
		if inactiveHistoryProjectionArtifact(rel) {
			add(newestID, "derived_store", rel+" is an inactive history projection; privacy cleanup must remove every unreferenced generation")
			continue
		}
		if !storeInfo.ModTime().After(cutoff) {
			add(newestID, "derived_store", rel+" does not postdate the newest tombstone; its deletion failed or was skipped, re-run purge")
			continue
		}
		// A post-cleanup store may legitimately have been rebuilt. Validate the
		// content-bearing formats before treating that mtime as proof: corrupt
		// or opaque state can never make the read gate return clean.
		switch rel {
		case patternsTasksPath, patternsProceduresPath, patternsPracticesPath, patternRunsRelPath:
			if err := validatePrivacyJSONLines(brainDir, rel); err != nil {
				return report, err
			}
		}
		currentArtifacts[rel] = true
	}
	// Every SQLite reader below uses a descriptor-pinned private snapshot.
	// Repository-controlled paths are never handed to SQLite, and each primary
	// store is copied only once for integrity plus row-level inspection.
	sqliteSnapshots := map[string]string{}
	for _, rel := range []string{
		patternCorpusPath,
		historyFTSDBRelPath(),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
	} {
		if !currentArtifacts[rel] {
			continue
		}
		snapshot, cleanup, snapshotErr := snapshotPrivacySQLiteStore(brainDir, rel)
		if snapshotErr != nil {
			return report, snapshotErr
		}
		defer cleanup()
		if err := validatePrivacySQLiteSnapshot(snapshot, rel); err != nil {
			return report, err
		}
		sqliteSnapshots[rel] = snapshot
	}
	if currentArtifacts[patternCorpusPath] {
		corpusFindings, err := inspectPrivacyPatternCorpus(sqliteSnapshots[patternCorpusPath], stones, pathsBySession, tombstoneState.Identity, newestID)
		if err != nil {
			return report, err
		}
		report.Findings = append(report.Findings, corpusFindings...)
	}
	vectorRels := []struct {
		rel        string
		hiddenKind func(string) bool
	}{
		{rel: filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)), hiddenKind: historyGeneralRankingHiddenKind},
		{rel: filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable)), hiddenKind: conversationSemanticHiddenKind},
	}
	for _, vectorStore := range vectorRels {
		if !currentArtifacts[vectorStore.rel] {
			continue
		}
		if source == nil {
			// The store is newer than the newest tombstone, but the manifest
			// declares no history source, so there is no active index to
			// attribute its rows against. Skipping it (the previous behavior)
			// still published Clean = true for a store whose membership was
			// never checked, and requirePrivacyDerivedRead then allowed every
			// retrieval and pattern mutation to proceed against it. An
			// un-attributable store is a finding, not a pass.
			add("", vectorStore.rel, "vector store is newer than the newest tombstone but the manifest declares no history source, so its rows cannot be attributed; rebuild the history projection then re-verify")
			continue
		}
		wanted, _ := vectorEligibleRecordIDs(activeIndex, vectorStore.hiddenKind)
		findings, inspectErr := inspectPrivacyVectorIDsSnapshot(sqliteSnapshots[vectorStore.rel], vectorStore.rel, wanted, newestID)
		if inspectErr != nil {
			return report, inspectErr
		}
		report.Findings = append(report.Findings, findings...)
	}
	// Hosted-provider egress receipts retain content-free session identity and
	// digest metadata. They are still session-linked records and must disappear
	// with an exclusion/purge, with corrupt/unreadable receipt state failing
	// verification closed.
	legacyEgressReceipts, egressErr := loadLegacyAbstractEgressReceiptsChecked(brainDir)
	if egressErr != nil {
		return report, egressErr
	}
	for id := range stones.Excluded {
		refs := refsBySession[id]
		for ref := range refs {
			receipt, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, ref)
			if err != nil {
				return report, err
			}
			if present {
				add(id, "abstract_egress_receipt", receipt.Path+" survives for an excluded session; re-run purge")
			}
		}
		for _, receipt := range legacyEgressReceipts {
			if refs[receipt.Receipt.SessionRef] {
				add(id, "abstract_egress_receipt", receipt.Path+" survives for an excluded session; re-run purge")
			}
		}
	}
	// FTS content inspection: the BM25 store indexes each record's transcript
	// path as text, so a rebuilt-yet-dirty store is detectable by querying for
	// the excluded transcripts' distinctive basenames. This is a real
	// row-level check, independent of timestamps. Vector stores receive their
	// own row-level record-ID membership check above; their embedding payloads
	// are intentionally not treated as searchable text.
	if currentArtifacts[historyFTSDBRelPath()] {
		for id, paths := range pathsBySession {
			for rel := range paths {
				hits, checkErr := historyFTSContainsPathPhraseAt(sqliteSnapshots[historyFTSDBRelPath()], rel)
				if checkErr != nil {
					return report, fmt.Errorf("%s: inspect %s: %w", memoryErrStateCorrupt, historyFTSDBRelPath(), checkErr)
				}
				if hits {
					add(id, "fts_store", "rows for excluded transcript "+rel+" remain in the BM25 store; re-run purge or delete "+historyFTSDBRelPath())
				}
			}
		}
	}
	// Abstracts: an artifact whose session reference belongs to a
	// tombstoned session is a violation (cleanup deletes them; verify proves
	// it). Inventory is bounded and fail-closed: an unsafe or unparseable file
	// cannot be skipped because its session_ref cannot be trusted.
	abstractInventory := loadSessionAbstractInventory(brainDir)
	if err := memoryStateError(abstractInventory.Issues); err != nil {
		return report, err
	}
	abstractRefOwner := map[string]string{}
	for id := range stones.Excluded {
		refs := refsBySession[id]
		for ref := range refs {
			abstractRefOwner[ref] = id
		}
	}
	for digest, entry := range abstractInventory.ByDigest {
		if entry.State != sessionAbstractCurrent {
			continue
		}
		if id := abstractRefOwner[entry.Artifact.SessionRef]; id != "" {
			add(id, "session_abstract", "artifact "+filepath.Base(abstractRel(digest))+" survives for an excluded session; re-run purge")
		}
	}
	// Operational metadata is content-free, but privacy purge promises to
	// remove session-linked hints and jobs immediately. Treat survivors as a
	// verification failure rather than silently retaining lifecycle identity.
	for _, hint := range hints {
		if _, tombstoned := stones.Excluded[hint.SessionID]; tombstoned {
			add(hint.SessionID, "memory_hint", "durable lifecycle hint survives for an excluded session; re-run purge")
		}
	}
	for _, job := range jobs {
		if _, tombstoned := stones.Excluded[job.SessionID]; tombstoned {
			add(job.SessionID, "memory_job", "durable work record "+job.JobID+" survives for an excluded session; re-run purge")
		}
	}
	// Failed cleanup transactions stay visible until a re-run completes them
	//. In-flight states are not findings: this function runs inside
	// the cleanup itself, whose own transaction is mid-transition.
	for id := range stones.Excluded {
		if tx, ok := txBySession[id]; ok && tx.State == privacyStateError {
			add(id, "privacy_transaction", fmt.Sprintf("%s failed (%s); re-run the operation", tx.Operation, tx.Error))
		}
	}
	// Purged sessions must have no exported transcript left; excluded (not
	// purged) sessions deliberately keep theirs.
	for id, stone := range stones.Excluded {
		if stone.Reason != "purged" {
			continue
		}
		for rel := range pathsBySession[id] {
			_, present, statErr := privacyArtifactInfo(brainDir, rel, "exported transcript")
			if statErr != nil {
				return report, statErr
			}
			if present {
				add(id, "exported_transcript", rel+" (re-exported by the capture layer; projections stay excluded; purge again to delete the copy)")
			}
		}
	}
	// Verification is a snapshot proof. If the policy changes under us, retry
	// instead of publishing Clean=true for a mixed set of tombstones/artifacts.
	_, finalTombstoneState, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return report, err
	}
	if finalTombstoneState.Identity != tombstoneState.Identity {
		return report, fmt.Errorf("%s: session tombstones changed during privacy verification", memoryErrPrivacyDirty)
	}
	report.CheckedSessions = len(stones.Excluded)
	report.Clean = len(report.Findings) == 0
	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].SessionID != report.Findings[j].SessionID {
			return report.Findings[i].SessionID < report.Findings[j].SessionID
		}
		return report.Findings[i].Artifact < report.Findings[j].Artifact
	})
	return report, nil
}

func inactiveHistoryProjectionArtifact(rel string) bool {
	rel = filepath.ToSlash(rel)
	return rel == historyIndexPath || rel == projectionStateRel ||
		strings.HasPrefix(rel, historyGenerationsDir+"/") || strings.HasPrefix(rel, historyStagingDir+"/")
}

// --- retention policy ---

type retentionPlanEntry struct {
	SessionID string `json:"session_id"`
	Branch    string `json:"branch,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Action    string `json:"action"` // exclude | purge
}

func newPrivacyRetentionCommand(opts Options) *cobra.Command {
	var maxAge time.Duration
	var branch string
	var purge, dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "retention",
		Short: "Apply a retention policy: exclude or purge sessions older than --max-age (optionally per branch)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if maxAge <= 0 {
				return fmt.Errorf("--max-age is required and must be positive")
			}
			return runPrivacyRetention(cmd, opts, maxAge, strings.TrimSpace(branch), purge, dryRun, jsonOut)
		},
	}
	cmd.Flags().DurationVar(&maxAge, "max-age", 0, "Sessions whose capture time is older than this are selected (required, e.g. 2160h for 90 days)")
	cmd.Flags().StringVar(&branch, "branch", "", "Only sessions captured on this branch")
	cmd.Flags().BoolVar(&purge, "purge", false, "Physically purge selected sessions (default: exclude only, keeping exported transcripts)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report the selection without changing anything")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func privacyRetentionPlan(brainDir string, cutoff time.Time, branch string, purge bool) ([]retentionPlanEntry, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return nil, err
	}
	var selected []retentionPlanEntry
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		for _, session := range manifest.Sources.Sessions.Sessions {
			id := strings.TrimSpace(session.SessionID)
			if id == "" || session.CreatedAt.IsZero() || !session.CreatedAt.Before(cutoff) {
				continue
			}
			if branch != "" && session.Branch != branch {
				continue
			}
			if _, already := stones.Excluded[id]; already && !purge {
				continue // exclude policy is idempotent; purge re-runs to catch re-exports
			}
			action := "exclude"
			if purge {
				action = "purge"
			}
			entry := retentionPlanEntry{SessionID: id, Branch: session.Branch, Action: action}
			if !session.CreatedAt.IsZero() {
				entry.CreatedAt = session.CreatedAt.UTC().Format(time.RFC3339)
			}
			selected = append(selected, entry)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].CreatedAt < selected[j].CreatedAt })
	return selected, nil
}

// applyPrivacyRetention selects and mutates under the same privacy→Brain lock
// boundary. A provider can delay this operation, and projections may advance
// while it waits; selection is therefore recomputed only after both locks are
// held so the applied plan cannot silently omit a newly eligible session.
func applyPrivacyRetention(brainDir string, env EntireEnv, repoKey string, now, cutoff time.Time, branch string, purge bool, maxAge time.Duration) ([]retentionPlanEntry, []string, error) {
	var plan []retentionPlanEntry
	var caveats []string
	err := withBrainPrivacySideEffectLock(brainDir, func() error {
		return withBrainWriteLock(brainDir, func() error {
			var err error
			plan, err = privacyRetentionPlan(brainDir, cutoff, branch, purge)
			if err != nil {
				return err
			}
			for _, entry := range plan {
				// Both actions run the shared cleanup executor: retention
				// exclude removes derived facts/episodes/patterns/caches/
				// stores exactly like the privacy exclude command, and both
				// paths publish success only after verification.
				sessionPlan, planErr := buildSessionPurgePlan(brainDir, entry.SessionID)
				if planErr != nil {
					return planErr
				}
				for _, caveat := range purgeGitmetaSyncCaveats(env, repoKey, sessionPlan) {
					if !slices.Contains(caveats, caveat) {
						caveats = append(caveats, caveat)
					}
				}
				if entry.Action == "purge" {
					if err := executeSessionPurge(brainDir, entry.SessionID, sessionPlan, now); err != nil {
						return err
					}
					continue
				}
				if err := executeSessionCleanup(brainDir, entry.SessionID, sessionPlan, now, fmt.Sprintf("retention max-age %s", maxAge), true); err != nil {
					return err
				}
			}
			return nil
		})
	})
	return plan, caveats, err
}

func runPrivacyRetention(cmd *cobra.Command, opts Options, maxAge time.Duration, branch string, purge, dryRun, jsonOut bool) error {
	ctx := cmd.Context()
	storage, err := resolveSessionsBrain(ctx, opts)
	if err != nil {
		return err
	}
	brainDir := storage.BrainDir
	now := opts.Now().UTC()
	cutoff := now.Add(-maxAge)
	var plan []retentionPlanEntry
	var caveats []string
	if dryRun {
		plan, err = privacyRetentionPlan(brainDir, cutoff, branch, purge)
		if err != nil {
			return err
		}
	} else {
		plan, caveats, err = applyPrivacyRetention(brainDir, opts.Env, storage.Key, now, cutoff, branch, purge, maxAge)
		if err != nil {
			return err
		}
	}
	if jsonOut {
		if plan == nil {
			plan = []retentionPlanEntry{}
		}
		payload := map[string]any{"cutoff": cutoff.Format(time.RFC3339), "dry_run": dryRun, "sessions": plan}
		if len(caveats) > 0 {
			payload["caveats"] = caveats
		}
		return writeJSON(cmd, payload)
	}
	return writeText(cmd, func(out io.Writer) {
		mode := "applied"
		if dryRun {
			mode = "dry-run: would apply"
		}
		fmt.Fprintf(out, "%s retention (older than %s) to %d sessions\n", mode, cutoff.Format(time.RFC3339), len(plan))
		for _, entry := range plan {
			fmt.Fprintf(out, "  %s %s (%s, %s)\n", entry.Action, entry.SessionID, valueOrUnset(entry.Branch), valueOrUnset(entry.CreatedAt))
		}
		for _, caveat := range caveats {
			fmt.Fprintf(out, "  NOT purged: %s\n", caveat)
		}
	})
}
