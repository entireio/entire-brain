package cli

import (
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
// Verification model: the brain's TEXT truths (history index, scan cache,
// short-term overlay, episodes, facts, distill cache, exported transcripts)
// are inspected directly. The binary derived stores (FTS, vector stores,
// pattern corpus) are pure regenerations of those truths and are deleted
// wholesale by purge; a clean truth plus the delete-on-purge policy covers
// them, and they are reported as such rather than pretended to be
// row-inspected.

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
	stones := loadSessionTombstones(brainDir)
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

	// Resolve each tombstoned session's transcript paths from the manifest
	// (a purged session may no longer be listed; then only id-keyed artifacts
	// are checkable, which is exactly what remains).
	pathsBySession := map[string]map[string]bool{}
	for id := range stones.Excluded {
		pathsBySession[id] = map[string]bool{}
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		for _, session := range manifest.Sources.Sessions.Sessions {
			id := strings.TrimSpace(session.SessionID)
			if paths, tracked := pathsBySession[id]; tracked {
				if rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath)); rel != "" {
					paths[rel] = true
				}
			}
		}
	}
	excludedPath := func(rel string) (string, bool) {
		for id, paths := range pathsBySession {
			if paths[rel] {
				return id, true
			}
		}
		return "", false
	}

	// History index + short-term overlay records.
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	if source != nil {
		index, err := loadBrainHistoryIndex(brainDir, source)
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
		overlay := loadHistoryShortTerm(brainDir, source)
		for rel, entry := range overlay.Files {
			if id, hit := excludedPath(rel); hit {
				add(id, "short_term_memory", rel)
				continue
			}
			// Record-level check mirrors the history-index check: an overlay
			// record can carry a tombstoned session id under a path the
			// manifest no longer maps.
			for _, record := range entry.Records {
				if _, tombstoned := stones.Excluded[record.SessionID]; tombstoned && record.SessionID != "" {
					add(record.SessionID, "short_term_memory", "record "+record.ID+" in "+rel)
				}
			}
		}
	}
	// Scan cache entries.
	cache := loadHistoryScanCache(brainDir)
	for rel := range cache.Files {
		if id, hit := excludedPath(rel); hit {
			add(id, "scan_cache", rel)
		}
	}
	// Episodes.
	if data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(patternsEpisodesPath))); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			var record episodeRecord
			if json.Unmarshal([]byte(trimmed), &record) != nil {
				continue
			}
			if _, tombstoned := stones.Excluded[strings.TrimSpace(record.SessionID)]; tombstoned && record.SessionID != "" {
				add(record.SessionID, "episodes", "episode "+record.ID)
			} else if id, hit := excludedPath(filepath.ToSlash(strings.TrimSpace(record.Source.Path))); hit {
				add(id, "episodes", "episode "+record.ID+" via anchor")
			}
		}
	}
	// Fact provenance anchors.
	if byBranch, err := loadAllFactBranches(brainDir); err == nil {
		for branch, facts := range byBranch {
			for _, fact := range facts {
				for _, anchor := range fact.Provenance {
					if _, tombstoned := stones.Excluded[strings.TrimSpace(anchor.SessionID)]; tombstoned && anchor.SessionID != "" {
						add(anchor.SessionID, "facts", "fact "+fact.ID+" (branch "+branch+")")
					}
				}
			}
		}
	}
	// Distill cache keys.
	distill := loadDistillCache(brainDir)
	for key := range distill.Sessions {
		for id := range stones.Excluded {
			if strings.HasSuffix(key, "/"+url.PathEscape(id)) {
				add(id, "distill_cache", key)
			}
		}
	}
	// Derived binary stores (R0-2): exclude and purge delete every store in
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
	if info, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath))); statErr == nil {
		cutoff := info.ModTime()
		for _, rel := range privacyDerivedStoreArtifacts(brainDir) {
			storeInfo, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel)))
			if statErr != nil {
				continue
			}
			if storeInfo.ModTime().Before(cutoff) {
				add(newestID, "derived_store", rel+" predates the newest tombstone; its deletion failed or was skipped, re-run purge")
			}
		}
	}
	// FTS content inspection: the BM25 store indexes each record's transcript
	// path as text, so a rebuilt-yet-dirty store is detectable by querying for
	// the excluded transcripts' distinctive basenames. This is a real
	// row-level check, independent of timestamps; the vector stores stay
	// covered by deletion plus the mtime check above (their rows are
	// embeddings, not queryable text).
	for id, paths := range pathsBySession {
		for rel := range paths {
			hits, checkErr := historyFTSContainsPathPhrase(brainDir, rel)
			if checkErr != nil {
				break // store absent or unreadable; deletion/mtime checks cover it
			}
			if hits {
				add(id, "fts_store", "rows for excluded transcript "+rel+" remain in the BM25 store; re-run purge or delete "+historyFTSDBRelPath())
			}
		}
	}
	// Purged sessions must have no exported transcript left; excluded (not
	// purged) sessions deliberately keep theirs.
	for id, stone := range stones.Excluded {
		if stone.Reason != "purged" {
			continue
		}
		for rel := range pathsBySession[id] {
			if _, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); statErr == nil {
				add(id, "exported_transcript", rel+" (re-exported by the capture layer; projections stay excluded; purge again to delete the copy)")
			}
		}
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

func runPrivacyRetention(cmd *cobra.Command, opts Options, maxAge time.Duration, branch string, purge, dryRun, jsonOut bool) error {
	ctx := cmd.Context()
	storage, err := resolveSessionsBrain(ctx, opts)
	if err != nil {
		return err
	}
	brainDir := storage.BrainDir
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return err
	}
	now := opts.Now().UTC()
	cutoff := now.Add(-maxAge)
	stones := loadSessionTombstones(brainDir)
	var plan []retentionPlanEntry
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
			plan = append(plan, entry)
		}
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].CreatedAt < plan[j].CreatedAt })

	var caveats []string
	if !dryRun && len(plan) > 0 {
		if err := withBrainWriteLock(brainDir, func() error {
			for _, entry := range plan {
				// Both actions run the shared cleanup executor: retention
				// exclude removes derived facts/episodes/patterns/caches/
				// stores exactly like the privacy exclude command, and both
				// paths publish success only after verification (R0-1).
				sessionPlan, planErr := buildSessionPurgePlan(brainDir, entry.SessionID)
				if planErr != nil {
					return planErr
				}
				for _, caveat := range purgeGitmetaSyncCaveats(opts.Env, storage.Key, sessionPlan) {
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
		}); err != nil {
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
