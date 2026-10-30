package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
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
	data, present, err := brainstore.ReadPrivacyArtifact(brainDir, historyShortTermPath, "short-term history", base.DefaultMaxReadBytes)
	if err != nil || !present {
		return empty, err
	}
	var overlay shortTermIndex
	if err := json.Unmarshal(data, &overlay); err != nil || overlay.Files == nil {
		return empty, fmt.Errorf("%s: decode %s", brainstore.StateCorruptCode, historyShortTermPath)
	}
	if overlay.Version != historyShortTermVersion || overlay.ReconcilerVersion != historyShortTermReconcilerVersion {
		return empty, fmt.Errorf("%s: unsupported %s schema/reconciler version", brainstore.UnsupportedVersionCode, historyShortTermPath)
	}
	// A stale base pin is irrelevant to privacy: its records are still on disk
	// and therefore must be inspected rather than silently treated as absent.
	return overlay, nil
}

func loadHistoryScanCacheForPrivacy(brainDir string) (historyScanCache, error) {
	empty := historyScanCache{Version: historyScanCacheVersion, Files: map[string]historyScanCacheEntry{}}
	data, present, err := brainstore.ReadPrivacyArtifact(brainDir, historyScanCachePath, "history scan cache", 256<<20)
	if err != nil || !present {
		return empty, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return empty, fmt.Errorf("%s: open %s: %w", brainstore.StateCorruptCode, historyScanCachePath, err)
	}
	decoded, readErr := base.SafeReadAll(gz, base.SemanticSnapshotMaxBytes(), historyScanCachePath+" (decompressed)")
	closeErr := gz.Close()
	if readErr != nil {
		return empty, fmt.Errorf("%s: read %s: %w", brainstore.StateCorruptCode, historyScanCachePath, readErr)
	}
	if closeErr != nil {
		return empty, fmt.Errorf("%s: close %s: %w", brainstore.StateCorruptCode, historyScanCachePath, closeErr)
	}
	var cache historyScanCache
	if err := json.Unmarshal(decoded, &cache); err != nil || cache.Files == nil {
		return empty, fmt.Errorf("%s: decode %s", brainstore.StateCorruptCode, historyScanCachePath)
	}
	if cache.Version != historyScanCacheVersion {
		return empty, fmt.Errorf("%s: unsupported %s schema version %d", brainstore.UnsupportedVersionCode, historyScanCachePath, cache.Version)
	}
	return cache, nil
}

func loadDistillCacheForPrivacy(brainDir string) (distillCache, error) {
	empty := distillCache{Version: distillCacheVersion, Sessions: map[string]string{}}
	data, present, err := brainstore.ReadPrivacyArtifact(brainDir, distillCachePath, "distill cache", base.MaxManifestBytes)
	if err != nil || !present {
		return empty, err
	}
	var cache distillCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Sessions == nil {
		return empty, fmt.Errorf("%s: decode %s", brainstore.StateCorruptCode, distillCachePath)
	}
	if cache.Version != distillCacheVersion {
		return empty, fmt.Errorf("%s: unsupported %s schema version %d", brainstore.UnsupportedVersionCode, distillCachePath, cache.Version)
	}
	return cache, nil
}

func validatePrivacyJSONLines(brainDir, rel string) error {
	data, present, err := brainstore.ReadPrivacyArtifact(brainDir, rel, "derived JSON store", 256<<20)
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
			return fmt.Errorf("%s: parse %s line %d", brainstore.StateCorruptCode, rel, i+1)
		}
	}
	return nil
}

// A var so the ceiling regression test can exercise the bound without
// scanning two million rows; production never changes it.
var privacyVectorVerifyMaxRows = 2_000_000

func inspectPrivacyVectorIDsSnapshot(snapshot, rel string, wanted map[string]struct{}, findingSessionID string) ([]privacyVerifyFinding, error) {
	db, err := sql.Open(sqlitedriver.Name, brainstore.SQLiteReadOnlyDSN(snapshot))
	if err != nil {
		return nil, fmt.Errorf("%s: open vector store %s: %w", brainstore.StateCorruptCode, rel, err)
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return nil, fmt.Errorf("%s: vector store %s failed integrity verification", brainstore.StateCorruptCode, rel)
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT record_id FROM history_ids LIMIT %d`, privacyVectorVerifyMaxRows+1))
	if err != nil {
		return nil, fmt.Errorf("%s: inspect vector IDs in %s: %w", brainstore.StateCorruptCode, rel, err)
	}
	defer rows.Close()
	var findings []privacyVerifyFinding
	count := 0
	for rows.Next() {
		count++
		if count > privacyVectorVerifyMaxRows {
			return nil, fmt.Errorf("%s: vector store %s exceeds verification ceiling", brainstore.StateUnsafeCode, rel)
		}
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("%s: inspect vector ID in %s: %w", brainstore.StateCorruptCode, rel, err)
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
		return nil, fmt.Errorf("%s: inspect vector IDs in %s: %w", brainstore.StateCorruptCode, rel, err)
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
			ownerByPath[brainstore.NormalizePrivacyTranscriptPath(rel)] = id
		}
	}
	db, err := sql.Open(sqlitedriver.Name, brainstore.SQLiteReadOnlyDSN(snapshotPath))
	if err != nil {
		return nil, fmt.Errorf("%s: open %s: %w", brainstore.StateCorruptCode, patterncore.CorpusPath, err)
	}
	defer db.Close()
	type corpusTable struct {
		name       string
		pathColumn string
		artifact   string
	}
	var findings []privacyVerifyFinding
	if got := patterncore.CorpusMeta(db, "privacy_policy_identity"); got != expectedPolicyIdentity {
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
			return nil, fmt.Errorf("%s: inspect %s %s: %w", brainstore.StateCorruptCode, patterncore.CorpusPath, table.name, err)
		}
		count := 0
		for rows.Next() {
			count++
			if count > privacyCorpusVerifyMaxRows {
				rows.Close()
				return nil, fmt.Errorf("%s: %s %s exceeds %d rows", brainstore.StateCorruptCode, patterncore.CorpusPath, table.name, privacyCorpusVerifyMaxRows)
			}
			var rowID, sessionID, sourcePath string
			if err := rows.Scan(&rowID, &sessionID, &sourcePath); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: scan %s %s: %w", brainstore.StateCorruptCode, patterncore.CorpusPath, table.name, err)
			}
			owner := ""
			if _, excluded := stones.Excluded[strings.TrimSpace(sessionID)]; excluded {
				owner = strings.TrimSpace(sessionID)
			} else {
				owner = ownerByPath[brainstore.NormalizePrivacyTranscriptPath(sourcePath)]
			}
			key := table.artifact + "\x00" + owner
			if owner != "" && !seen[key] {
				seen[key] = true
				findings = append(findings, privacyVerifyFinding{
					SessionID: owner,
					Artifact:  table.artifact,
					Detail:    fmt.Sprintf("%s row %s retains excluded session/path %s", table.name, rowID, brainstore.NormalizePrivacyTranscriptPath(sourcePath)),
				})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%s: iterate %s %s: %w", brainstore.StateCorruptCode, patterncore.CorpusPath, table.name, err)
		}
		rows.Close()
	}
	return findings, nil
}

// privacyDerivedReadGate prevents the tombstone-first cleanup window from
// exposing corpus-derived content. A valid tombstone does not permanently
// disable patterns: once the authoritative full verification is clean, reads
// resume normally.
func privacyDerivedReadGate(brainDir string) (bool, error) {
	return patterncore.DerivedReadGate(brainDir, verifyDerivedClean)
}

func requirePrivacyDerivedRead(brainDir string) error {
	return patterncore.RequireDerivedRead(brainDir, verifyDerivedClean)
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
				if err := cmdutil.WriteJSON(cmd, report); err != nil {
					return err
				}
			} else if err := cmdutil.WriteText(cmd, func(out io.Writer) {
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
				return fmt.Errorf("privacy verify found %d violations; re-run `"+base.BrainInvocation(os.LookupEnv, "privacy purge")+" <session-id>` (idempotent) or `"+base.BrainInvocation(os.LookupEnv, "refresh")+"` to rebuild", len(report.Findings))
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
	stones, tombstoneState, err := brainstore.LoadSessionTombstonesChecked(brainDir)
	if err != nil {
		return report, err
	}
	manifestForStorage, err := bmanifest.Load(brainDir)
	if err != nil {
		return report, err
	}
	if checkpointBackedSessions(manifestForStorage) {
		inventory, err := collectHistorySessionInventoryMode(context.Background(), brainDir, true)
		if err != nil && !errors.Is(err, errHistorySessionsRootMissing) {
			return report, err
		}
		if inventory != nil {
			defer inventory.Close()
			for _, file := range inventory.files {
				report.Findings = append(report.Findings, privacyVerifyFinding{Artifact: "exported_transcript", Detail: "stray materialization in checkpoint-backed brain: " + file.Rel + "; run " + base.BrainInvocation(os.LookupEnv, "refresh") + " sessions"})
			}
		}
		store := newTranscriptStore(ExecRunner{}, brainDir)
		guard, err := store.guardNow()
		if err != nil {
			return report, err
		}
		for id := range stones.Excluded {
			if !guard.BlocksSession(id) {
				report.Findings = append(report.Findings, privacyVerifyFinding{SessionID: id, Artifact: "transcript_read_gate", Detail: "tombstone is not enforced by transcript store"})
			}
		}
		// Exercise the same gate used by actual reads. A random storage error
		// is not evidence of privacy enforcement: require the tombstone refusal.
		for rel, id := range guard.Paths {
			_, finish, err := store.Open(context.Background(), rel)
			if finish != nil {
				_ = finish()
			}
			if err == nil || !strings.Contains(err.Error(), "is tombstoned") {
				report.Findings = append(report.Findings, privacyVerifyFinding{SessionID: id, Artifact: "transcript_read_gate", Detail: "transcript read did not attest tombstone: " + rel})
			}
		}
	}
	if len(stones.Excluded) == 0 {
		_, finalState, err := brainstore.LoadSessionTombstonesChecked(brainDir)
		if err != nil {
			return report, err
		}
		if finalState.Identity != tombstoneState.Identity {
			return report, fmt.Errorf("%s: session tombstones changed during privacy verification", brainstore.PrivacyDirtyCode)
		}
		report.Clean = len(report.Findings) == 0
		return report, nil
	}
	manifest, err := bmanifest.Load(brainDir)
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
		tx, present, txErr := brainstore.LoadPrivacyTransactionChecked(brainDir, id)
		if txErr != nil {
			return report, txErr
		}
		if present {
			txBySession[id] = tx
			for _, ref := range tx.SessionRefs {
				refsBySession[id][ref] = true
			}
			paths, scopeErr := brainstore.PrivacyTransactionTranscriptPaths(tx)
			if scopeErr != nil {
				return report, scopeErr
			}
			for _, rel := range paths {
				pathsBySession[id][rel] = true
			}
		}
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		for _, session := range manifest.Sources.Sessions.Sessions {
			id := strings.TrimSpace(session.SessionID)
			if paths, tracked := pathsBySession[id]; tracked {
				if rel := brainstore.NormalizePrivacyTranscriptPath(session.TranscriptPath); rel != "" {
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
		if refs, tracked := refsBySession[job.SessionID]; tracked && strings.HasPrefix(job.SessionRef, brainstore.ConversationSessionIDPrefix) {
			refs[job.SessionRef] = true
		}
	}
	excludedPath := func(rel string) (string, bool) {
		rel = brainstore.NormalizePrivacyTranscriptPath(rel)
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
	if source != nil {
		index, err := historystore.LoadIndex(brainDir, source)
		if err != nil {
			return report, err
		}
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
	// Entity index session linkage. The derived entity -> commit join records
	// which captured sessions changed each symbol, and `entities history` reads
	// it back verbatim. Cleanup strips the tombstoned ids; this proves it,
	// rather than leaving the one session-derived field in that file
	// uninspected while the report claims every projection is clean.
	entityCache, entityPresent, err := loadPrivacyEntityIndex(brainDir)
	if err != nil {
		return report, err
	}
	if entityPresent {
		for key, occurrences := range entityCache.Entries {
			for _, occurrence := range occurrences {
				for _, id := range occurrence.SessionIDs {
					id = strings.TrimSpace(id)
					if _, tombstoned := stones.Excluded[id]; tombstoned && id != "" {
						add(id, "entity_index", "entity "+key+" at commit "+occurrence.Commit)
					}
				}
			}
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
	tombstoneInfo, tombstonesPresent, statErr := brainstore.PrivacyArtifactInfo(brainDir, brainstore.SessionTombstonesPath, "session tombstones")
	if statErr != nil {
		return report, statErr
	}
	if !tombstonesPresent {
		return report, fmt.Errorf("%s: session tombstones disappeared during privacy verification", brainstore.StateCorruptCode)
	}
	cutoff := tombstoneInfo.ModTime()
	artifacts, inventoryErr := privacyDerivedStoreArtifacts(brainDir)
	if inventoryErr != nil {
		return report, inventoryErr
	}
	presentArtifacts := map[string]bool{}
	currentArtifacts := map[string]bool{}
	for _, rel := range artifacts {
		storeInfo, present, statErr := brainstore.PrivacyArtifactInfo(brainDir, rel, "derived privacy store")
		if statErr != nil {
			return report, statErr
		}
		if !present {
			continue
		}
		presentArtifacts[rel] = true
		switch {
		case rel == patterncore.CorpusPublicationMarkerPath:
			add(newestID, "pattern_corpus_publication", rel+" records an interrupted publication; privacy cleanup must remove the classified marker and candidate")
			continue
		case patterncore.IsCorpusRollbackJournalRel(rel):
			// A rollback journal may retain pre-tombstone pages and cannot be
			// proven clean by the main database's row-level snapshot. Its mtime
			// is therefore never evidence of a successful privacy rebuild.
			add(newestID, "pattern_corpus_rollback_journal", rel+" is opaque content-bearing rollback state; privacy cleanup must remove it")
			continue
		case patterncore.IsCorpusStagingArtifactRel(rel):
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
		case patternsTasksPath, patterncore.ProceduresPath, patterncore.PracticesPath, patterncore.RunsRelPath:
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
		patterncore.CorpusPath,
		historyFTSDBRelPath(),
		filepath.ToSlash(filepath.Join(brainstore.HistoryDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(brainstore.HistoryDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
	} {
		if !currentArtifacts[rel] {
			continue
		}
		snapshot, cleanup, snapshotErr := brainstore.SnapshotPrivacySQLiteStore(brainDir, rel)
		if snapshotErr != nil {
			return report, snapshotErr
		}
		defer cleanup()
		if err := brainstore.ValidatePrivacySQLiteSnapshot(snapshot, rel); err != nil {
			return report, err
		}
		sqliteSnapshots[rel] = snapshot
	}
	if currentArtifacts[patterncore.CorpusPath] {
		corpusFindings, err := inspectPrivacyPatternCorpus(sqliteSnapshots[patterncore.CorpusPath], stones, pathsBySession, tombstoneState.Identity, newestID)
		if err != nil {
			return report, err
		}
		report.Findings = append(report.Findings, corpusFindings...)
	}
	// Semantic retrieval was removed (trail #278): no writer can rebuild or
	// re-validate a vector store, so any surviving legacy store is itself a
	// finding — its rows predate the removal and may retain purged sessions.
	for _, rel := range []string{
		filepath.ToSlash(filepath.Join(brainstore.HistoryDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(brainstore.HistoryDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
	} {
		if !currentArtifacts[rel] {
			continue
		}
		add("", rel, "legacy semantic vector store survives after the semantic-retrieval removal; delete it (its rows cannot be attributed and may retain purged sessions)")
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
					return report, fmt.Errorf("%s: inspect %s: %w", brainstore.StateCorruptCode, historyFTSDBRelPath(), checkErr)
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
		if tx, ok := txBySession[id]; ok && tx.State == brainstore.PrivacyStateError {
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
			_, present, statErr := brainstore.PrivacyArtifactInfo(brainDir, rel, "exported transcript")
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
	_, finalTombstoneState, err := brainstore.LoadSessionTombstonesChecked(brainDir)
	if err != nil {
		return report, err
	}
	if finalTombstoneState.Identity != tombstoneState.Identity {
		return report, fmt.Errorf("%s: session tombstones changed during privacy verification", brainstore.PrivacyDirtyCode)
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
	return rel == historystore.IndexPath || rel == projectionStateRel ||
		strings.HasPrefix(rel, historystore.GenerationsDir+"/") || strings.HasPrefix(rel, historystore.StagingDir+"/")
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
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return nil, err
	}
	stones, _, err := brainstore.LoadSessionTombstonesChecked(brainDir)
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
	err := brainstore.WithBrainPrivacySideEffectLock(brainDir, func() error {
		return brainstore.WithBrainWriteLock(brainDir, func() error {
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
		return cmdutil.WriteJSON(cmd, payload)
	}
	return cmdutil.WriteText(cmd, func(out io.Writer) {
		mode := "applied"
		if dryRun {
			mode = "dry-run: would apply"
		}
		fmt.Fprintf(out, "%s retention (older than %s) to %d sessions\n", mode, cutoff.Format(time.RFC3339), len(plan))
		for _, entry := range plan {
			fmt.Fprintf(out, "  %s %s (%s, %s)\n", entry.Action, entry.SessionID, cmdutil.ValueOrUnset(entry.Branch), cmdutil.ValueOrUnset(entry.CreatedAt))
		}
		for _, caveat := range caveats {
			fmt.Fprintf(out, "  NOT purged: %s\n", caveat)
		}
	})
}
