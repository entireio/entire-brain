package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// session_privacy.go is Phase 4 (privacy, exclusion, and deletion) of the
// conversational-memory plan: brain-local session tombstones understood before
// derived indexing, plus list/exclude/include/purge administration with
// dry-run.
//
// Authority model: the tombstone is brain-local (the plan's resolved default
// until a capture-layer exclusion contract exists in the Entire CLI). Purge
// physically deletes the brain's LOCAL projections — the exported transcript
// copy under sessions/ and every derived store built from it — and the
// tombstone prevents re-indexing even if a later `refresh sessions` re-exports
// the still-canonical captured material. Deleting the canonical capture itself
// is the capture layer's job, not the brain's.

const (
	sessionTombstonesFileName = "tombstones.json"
	sessionTombstonesPath     = historyDirName + "/" + sessionTombstonesFileName
	sessionTombstonesVersion  = 1
)

type sessionTombstones struct {
	Version int `json:"version"`
	// Excluded maps session_id -> tombstone. Content-free by design: a
	// tombstone must never retain what it excludes.
	Excluded map[string]sessionTombstone `json:"excluded"`
}

type sessionTombstone struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// loadSessionTombstones always returns a usable value; a missing or corrupt
// file reads as "nothing excluded" (exclusion is an explicit user action, so
// failing open here only ever restores default indexing, never deletes data).
func loadSessionTombstones(brainDir string) sessionTombstones {
	empty := sessionTombstones{Version: sessionTombstonesVersion, Excluded: map[string]sessionTombstone{}}
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath)))
	if err != nil {
		return empty
	}
	var stones sessionTombstones
	if err := json.Unmarshal(data, &stones); err != nil || stones.Excluded == nil {
		return empty
	}
	return stones
}

func saveSessionTombstones(brainDir string, stones sessionTombstones) error {
	stones.Version = sessionTombstonesVersion
	data, err := json.MarshalIndent(stones, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, sessionTombstonesPath, append(data, '\n'), 0o600)
}

// excludedTranscriptPaths resolves the brain-relative transcript paths that
// belong to excluded sessions, from the trusted manifest only.
func excludedTranscriptPaths(manifest *exportManifest, stones sessionTombstones) map[string]string {
	out := map[string]string{}
	if len(stones.Excluded) == 0 || manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return out
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		id := strings.TrimSpace(session.SessionID)
		if id == "" {
			continue
		}
		if _, excluded := stones.Excluded[id]; !excluded {
			continue
		}
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel != "" {
			out[rel] = id
		}
	}
	return out
}

// --- administration commands ---

// newPrivacyCommand is the session-privacy administration surface. Named
// `privacy` (not `sessions`) deliberately: `sessions` is the refresh build
// stage, and the root help contract keeps that name off the top level.
func newPrivacyCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "privacy",
		Short: "List, exclude, include, and purge captured sessions from the brain's projections",
	}
	cmd.AddCommand(newSessionsListCommand(opts))
	cmd.AddCommand(newSessionsExcludeCommand(opts))
	cmd.AddCommand(newSessionsIncludeCommand(opts))
	cmd.AddCommand(newSessionsPurgeCommand(opts))
	return cmd
}

func resolveSessionsBrain(ctx context.Context, opts Options) (string, error) {
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return "", err
	}
	if !local {
		return "", fmt.Errorf("sessions administration requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return "", err
	}
	return storage.BrainDir, nil
}

type sessionListEntry struct {
	SessionID      string `json:"session_id"`
	Branch         string `json:"branch,omitempty"`
	Agent          string `json:"agent,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	TranscriptPath string `json:"transcript_path"`
	Excluded       bool   `json:"excluded,omitempty"`
	ExcludedAt     string `json:"excluded_at,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

func newSessionsListCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List captured sessions and their exclusion state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			brainDir, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			stones := loadSessionTombstones(brainDir)
			var entries []sessionListEntry
			if manifest.Sources != nil && manifest.Sources.Sessions != nil {
				for _, session := range manifest.Sources.Sessions.Sessions {
					entry := sessionListEntry{
						SessionID: session.SessionID, Branch: session.Branch, Agent: session.Agent,
						TranscriptPath: session.TranscriptPath,
					}
					if !session.CreatedAt.IsZero() {
						entry.CreatedAt = session.CreatedAt.UTC().Format(time.RFC3339)
					}
					if stone, excluded := stones.Excluded[session.SessionID]; excluded {
						entry.Excluded = true
						entry.ExcludedAt = stone.At.UTC().Format(time.RFC3339)
						entry.Reason = stone.Reason
					}
					entries = append(entries, entry)
				}
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt > entries[j].CreatedAt })
			// Tombstones for sessions no longer in the manifest still matter
			// (they keep a re-export from re-indexing); list them too.
			known := map[string]bool{}
			for _, entry := range entries {
				known[entry.SessionID] = true
			}
			for id, stone := range stones.Excluded {
				if !known[id] {
					entries = append(entries, sessionListEntry{SessionID: id, Excluded: true, ExcludedAt: stone.At.UTC().Format(time.RFC3339), Reason: stone.Reason})
				}
			}
			if jsonOut {
				if entries == nil {
					entries = []sessionListEntry{}
				}
				return writeJSON(cmd, map[string]any{"sessions": entries})
			}
			return writeText(cmd, func(out io.Writer) {
				for _, entry := range entries {
					state := "included"
					if entry.Excluded {
						state = "EXCLUDED"
					}
					fmt.Fprintf(out, "%-10s %s  %s  %s  %s\n", state, valueOrUnset(entry.SessionID), valueOrUnset(entry.CreatedAt), valueOrUnset(entry.Agent), entry.TranscriptPath)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newSessionsExcludeCommand(opts Options) *cobra.Command {
	var reason string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "exclude <session-id>",
		Short: "Exclude a session from all derived projections (tombstone + rebuild; keeps the exported transcript)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionsExclude(cmd.Context(), cmd, opts, args[0], reason, jsonOut)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional note stored with the tombstone (do not put excluded content here)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runSessionsExclude(ctx context.Context, cmd *cobra.Command, opts Options, sessionID, reason string, jsonOut bool) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("session id is required")
	}
	brainDir, err := resolveSessionsBrain(ctx, opts)
	if err != nil {
		return err
	}
	if err := withBrainWriteLock(brainDir, func() error {
		stones := loadSessionTombstones(brainDir)
		stones.Excluded[sessionID] = sessionTombstone{At: opts.Now().UTC(), Reason: strings.TrimSpace(reason)}
		if err := saveSessionTombstones(brainDir, stones); err != nil {
			return err
		}
		_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, opts.Now().UTC(), nil)
		return err
	}); err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(cmd, map[string]any{"excluded": sessionID})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "excluded %s and rebuilt the history projection\n", sessionID)
	return nil
}

func newSessionsIncludeCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "include <session-id>",
		Short: "Re-include a previously excluded session (explicit action; clean rebuild)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := strings.TrimSpace(args[0])
			brainDir, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if err := withBrainWriteLock(brainDir, func() error {
				stones := loadSessionTombstones(brainDir)
				if _, ok := stones.Excluded[sessionID]; !ok {
					return fmt.Errorf("session %s is not excluded", sessionID)
				}
				delete(stones.Excluded, sessionID)
				if err := saveSessionTombstones(brainDir, stones); err != nil {
					return err
				}
				_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, opts.Now().UTC(), nil)
				return err
			}); err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"included": sessionID})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "re-included %s and rebuilt the history projection\n", sessionID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// sessionPurgePlan is the dry-run contract: exactly the artifacts (and their
// current bytes) a purge would remove or rebuild.
type sessionPurgePlan struct {
	SessionID string `json:"session_id"`
	// Transcripts are the exported local copies that will be deleted.
	Transcripts []purgeArtifact `json:"transcripts"`
	// Records is the number of index records (all kinds) derived from those
	// transcripts that the rebuild will drop.
	Records int `json:"records"`
	// DerivedStores are rebuildable stores deleted wholesale (they regenerate
	// from the surviving truth on the next query/refresh).
	DerivedStores []purgeArtifact `json:"derived_stores"`
	DryRun        bool            `json:"dry_run"`
}

type purgeArtifact struct {
	Path  string `json:"path"` // brain-relative
	Bytes int64  `json:"bytes"`
}

func newSessionsPurgeCommand(opts Options) *cobra.Command {
	var dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "purge <session-id>",
		Short: "Tombstone a session and physically delete its local projections (exported transcript + derived records/stores)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionsPurge(cmd.Context(), cmd, opts, args[0], dryRun, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report exactly what would be removed without changing anything")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runSessionsPurge(ctx context.Context, cmd *cobra.Command, opts Options, sessionID string, dryRun, jsonOut bool) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("session id is required")
	}
	brainDir, err := resolveSessionsBrain(ctx, opts)
	if err != nil {
		return err
	}
	var plan sessionPurgePlan
	if err := withBrainWriteLock(brainDir, func() error {
		var planErr error
		plan, planErr = buildSessionPurgePlan(brainDir, sessionID)
		if planErr != nil {
			return planErr
		}
		plan.DryRun = dryRun
		if dryRun {
			return nil
		}
		return executeSessionPurge(brainDir, sessionID, plan, opts.Now().UTC())
	}); err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(cmd, plan)
	}
	return writeText(cmd, func(out io.Writer) {
		mode := "purged"
		if plan.DryRun {
			mode = "purge (dry-run) would remove"
		}
		fmt.Fprintf(out, "%s session %s:\n", mode, plan.SessionID)
		for _, artifact := range plan.Transcripts {
			fmt.Fprintf(out, "  transcript %s (%d bytes)\n", artifact.Path, artifact.Bytes)
		}
		fmt.Fprintf(out, "  %d derived index records\n", plan.Records)
		for _, artifact := range plan.DerivedStores {
			fmt.Fprintf(out, "  derived store %s (%d bytes, rebuilds from surviving truth)\n", artifact.Path, artifact.Bytes)
		}
	})
}

// buildSessionPurgePlan computes exactly what a purge touches, from the
// trusted manifest and the current index. Purging an unknown or already-purged
// session yields an empty plan (idempotent), not an error.
func buildSessionPurgePlan(brainDir, sessionID string) (sessionPurgePlan, error) {
	plan := sessionPurgePlan{SessionID: sessionID}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return plan, err
	}
	transcriptRels := map[string]bool{}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		for _, session := range manifest.Sources.Sessions.Sessions {
			if strings.TrimSpace(session.SessionID) != sessionID {
				continue
			}
			rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
			if rel == "" || transcriptRels[rel] {
				continue
			}
			transcriptRels[rel] = true
			artifact := purgeArtifact{Path: rel}
			if info, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); statErr == nil {
				artifact.Bytes = info.Size()
			}
			plan.Transcripts = append(plan.Transcripts, artifact)
		}
	}
	sort.Slice(plan.Transcripts, func(i, j int) bool { return plan.Transcripts[i].Path < plan.Transcripts[j].Path })
	if manifest.Sources != nil && manifest.Sources.History != nil {
		if index, ierr := loadBrainHistoryIndex(brainDir, manifest.Sources.History); ierr == nil {
			for _, record := range index.Records {
				if transcriptRels[record.Path] {
					plan.Records++
				}
			}
		}
	}
	// Rebuildable derived stores removed wholesale: their row-level content is
	// keyed by record ids/text derived from the purged transcripts, and every
	// one regenerates from the surviving truth (index rebuild, lazy FTS build,
	// next vector sync).
	for _, rel := range []string{
		historyFTSDBRelPath(),
		filepath.ToSlash(filepath.Join(historyDirName, historyScanCacheFileName)),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
	} {
		if info, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); statErr == nil {
			plan.DerivedStores = append(plan.DerivedStores, purgeArtifact{Path: rel, Bytes: info.Size()})
		}
	}
	return plan, nil
}

// executeSessionPurge applies the plan under the already-held write lock:
// tombstone first (so a crash mid-purge can only leave the session excluded,
// never resurrected), then physical deletion, then a clean rebuild of the
// history projection from the surviving truth.
func executeSessionPurge(brainDir, sessionID string, plan sessionPurgePlan, now time.Time) error {
	stones := loadSessionTombstones(brainDir)
	stones.Excluded[sessionID] = sessionTombstone{At: now, Reason: "purged"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		return err
	}
	for _, artifact := range plan.Transcripts {
		path := filepath.Join(brainDir, filepath.FromSlash(artifact.Path))
		if err := rejectSymlinkPathComponents(brainDir, filepath.FromSlash(artifact.Path)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, artifact := range plan.DerivedStores {
		removeSQLiteStoreFiles(filepath.Join(brainDir, filepath.FromSlash(artifact.Path)))
	}
	_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
	return err
}

// removeSQLiteStoreFiles deletes a store file plus SQLite WAL/SHM siblings;
// every target is a regenerable derived artifact, so missing files are fine.
func removeSQLiteStoreFiles(path string) {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(candidate)
	}
}

// Portable mirrors of the cgo-only vector-store filenames so the purge
// inventory covers both stores on every build (the purego build has no store
// constructors, but the files may exist from a cgo binary's refresh).
const (
	historyVecStoreFileNamePortable      = "vectors.sqlite"
	conversationVecStoreFileNamePortable = "conversation-vectors.sqlite"
)
