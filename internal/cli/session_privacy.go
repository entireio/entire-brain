package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
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
// physically deletes the brain's LOCAL projections; the exported transcript
// copy under sessions/ and every derived store built from it; and the
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
	cmd.AddCommand(newPrivacyVerifyCommand(opts))
	cmd.AddCommand(newPrivacyRetentionCommand(opts))
	return cmd
}

func resolveSessionsBrain(ctx context.Context, opts Options) (repoStorage, error) {
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return repoStorage{}, err
	}
	if !local {
		return repoStorage{}, fmt.Errorf("sessions administration requires a local repository path: %s", target)
	}
	return repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
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
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			brainDir := storage.BrainDir
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

// sessionReadGuard is the retrieval-time tombstone view (R0-1): every
// conversation/history/fact retrieval boundary consults it so an excluded
// session becomes unreadable the moment its tombstone lands, without waiting
// for the derived rebuild. Defense in depth, not a substitute for cleanup.
type sessionReadGuard struct {
	ids   map[string]sessionTombstone
	paths map[string]string // excluded transcript rel -> session id, from the manifest
}

// loadSessionReadGuard builds the guard from the tombstone set and, when a
// manifest is supplied, the excluded sessions' transcript paths (records that
// lost their session id still resolve by path). Same fail-open contract as
// loadSessionTombstones: corruption restores indexing, never deletes data.
func loadSessionReadGuard(brainDir string, manifest *exportManifest) sessionReadGuard {
	stones := loadSessionTombstones(brainDir)
	if len(stones.Excluded) == 0 {
		return sessionReadGuard{}
	}
	guard := sessionReadGuard{ids: stones.Excluded}
	if manifest != nil {
		guard.paths = excludedTranscriptPaths(manifest, stones)
	}
	return guard
}

func (g sessionReadGuard) empty() bool { return len(g.ids) == 0 }

func (g sessionReadGuard) blocksSession(sessionID string) bool {
	if g.empty() {
		return false
	}
	_, ok := g.ids[strings.TrimSpace(sessionID)]
	return ok
}

func (g sessionReadGuard) blocksRecord(r historyRecord) bool {
	if g.blocksSession(r.SessionID) {
		return true
	}
	_, ok := g.paths[r.Path]
	return ok
}

// blocksFact mirrors the purge rule at read time: a fact whose every
// provenance anchor points at excluded sessions is unreadable; a fact still
// corroborated by a non-excluded session (or with no session provenance at
// all, e.g. seed-derived) stays visible.
func (g sessionReadGuard) blocksFact(f factRecord) bool {
	if g.empty() || len(f.Provenance) == 0 {
		return false
	}
	for _, anchor := range f.Provenance {
		if !g.blocksSession(anchor.SessionID) {
			return false
		}
	}
	return true
}

// guardFactRecords filters a fact slice through the exclusion guard,
// returning the input unchanged when nothing is excluded.
func guardFactRecords(guard sessionReadGuard, facts []factRecord) []factRecord {
	if guard.empty() || len(facts) == 0 {
		return facts
	}
	kept := make([]factRecord, 0, len(facts))
	for _, f := range facts {
		if guard.blocksFact(f) {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// filterTombstonedSessions drops excluded sessions from a derivation work
// list (R0-1): an excluded session must never produce new derived facts or
// records, even though its canonical transcript may still exist.
func filterTombstonedSessions(brainDir string, sessions []exportSession) []exportSession {
	stones := loadSessionTombstones(brainDir)
	if len(stones.Excluded) == 0 {
		return sessions
	}
	kept := sessions[:0:0]
	for _, session := range sessions {
		if _, excluded := stones.Excluded[strings.TrimSpace(session.SessionID)]; excluded {
			continue
		}
		kept = append(kept, session)
	}
	return kept
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
	storage, err := resolveSessionsBrain(ctx, opts)
	if err != nil {
		return err
	}
	brainDir := storage.BrainDir
	if err := withBrainWriteLock(brainDir, func() error {
		// Exclusion is purge minus the transcript (R0-1): the tombstone guards
		// reads immediately, then every derived projection the session fed --
		// index records, facts, episodes, pattern outputs, caches, FTS and
		// vector stores -- is removed or rebuilt from the surviving truth.
		// Errors propagate; a partial cleanup is a failure, not a success.
		plan, planErr := buildSessionPurgePlan(brainDir, sessionID)
		if planErr != nil {
			return planErr
		}
		return executeSessionCleanup(brainDir, sessionID, plan, opts.Now().UTC(), strings.TrimSpace(reason), true)
	}); err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(cmd, map[string]any{"excluded": sessionID})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "excluded %s and removed or rebuilt its derived projections\n", sessionID)
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
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			brainDir := storage.BrainDir
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
	// FactsDeleted counts durable facts whose ONLY provenance is the purged
	// session (physically removed); FactAnchorsStripped counts purged-session
	// anchors removed from facts that remain corroborated by other sessions.
	FactsDeleted        int `json:"facts_deleted"`
	FactAnchorsStripped int `json:"fact_anchors_stripped"`
	// Episodes counts pattern-layer episode records derived from the session.
	Episodes int `json:"episodes"`
	// DerivedStores are rebuildable stores deleted wholesale (they regenerate
	// from the surviving truth on the next query/refresh/patterns build).
	// skill-memory.ndjson is deliberately NOT here: it holds user curation
	// decisions (ids/status/counters, no transcript content), not derivation.
	DerivedStores []purgeArtifact `json:"derived_stores"`
	// Caveats name locations a purge does NOT clean, so incomplete deletion is
	// explicit rather than silent (e.g. the `facts sync` git-meta store, whose
	// keep-both merge model has no deletion semantics yet).
	Caveats []string `json:"caveats,omitempty"`
	DryRun  bool     `json:"dry_run"`
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
	storage, err := resolveSessionsBrain(ctx, opts)
	if err != nil {
		return err
	}
	brainDir := storage.BrainDir
	var plan sessionPurgePlan
	if err := withBrainWriteLock(brainDir, func() error {
		var planErr error
		plan, planErr = buildSessionPurgePlan(brainDir, sessionID)
		if planErr != nil {
			return planErr
		}
		plan.Caveats = append(plan.Caveats, purgeGitmetaSyncCaveats(opts.Env, storage.Key, plan)...)
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
		if plan.FactsDeleted > 0 || plan.FactAnchorsStripped > 0 {
			fmt.Fprintf(out, "  %d single-source facts deleted, %d anchors stripped from corroborated facts\n", plan.FactsDeleted, plan.FactAnchorsStripped)
		}
		if plan.Episodes > 0 {
			fmt.Fprintf(out, "  %d pattern episodes\n", plan.Episodes)
		}
		for _, artifact := range plan.DerivedStores {
			fmt.Fprintf(out, "  derived store %s (%d bytes, rebuilds from surviving truth)\n", artifact.Path, artifact.Bytes)
		}
		for _, caveat := range plan.Caveats {
			fmt.Fprintf(out, "  NOT purged: %s\n", caveat)
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
		// A declared-but-unreadable index is a real storage problem: failing
		// here beats an under-reported plan that execution would then trust
		// (R0-2: dry-run and execution inventories must match).
		index, ierr := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
		if ierr != nil {
			return plan, fmt.Errorf("load history index: %w", ierr)
		}
		for _, record := range index.Records {
			if transcriptRels[record.Path] {
				plan.Records++
			}
		}
	}
	// Facts derived from the purged session (single-source facts are deleted;
	// multi-source facts only lose the purged anchor). A corrupt fact store is
	// an error, not an empty count.
	byBranch, ferr := loadAllFactBranches(brainDir)
	if ferr != nil {
		return plan, fmt.Errorf("load facts: %w", ferr)
	}
	for _, facts := range byBranch {
		for _, fact := range facts {
			matched, remaining := 0, 0
			for _, anchor := range fact.Provenance {
				if strings.TrimSpace(anchor.SessionID) == sessionID {
					matched++
				} else {
					remaining++
				}
			}
			switch {
			case matched > 0 && remaining == 0:
				plan.FactsDeleted++
			case matched > 0:
				plan.FactAnchorsStripped += matched
			}
		}
	}
	// Pattern-layer episodes derived from the session.
	plan.Episodes = countSessionEpisodes(brainDir, sessionID, transcriptRels)
	// Rebuildable derived stores removed wholesale: their row-level content is
	// keyed by record ids/text derived from the purged transcripts, and every
	// one regenerates from the surviving truth (index rebuild, lazy FTS build,
	// next vector sync, next patterns build). runs.ndjson is a build log and
	// logs are inside the plan's deletion inventory.
	for _, rel := range privacyDerivedStoreRels() {
		if info, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); statErr == nil {
			plan.DerivedStores = append(plan.DerivedStores, purgeArtifact{Path: rel, Bytes: info.Size()})
		}
	}
	return plan, nil
}

// privacyDerivedStoreRels is the one inventory of rebuildable derived stores
// that exclude/purge delete wholesale and `privacy verify` re-checks; dry-run,
// execution, and verification must never disagree on this list (R0-2).
func privacyDerivedStoreRels() []string {
	return []string{
		historyFTSDBRelPath(),
		filepath.ToSlash(filepath.Join(historyDirName, historyScanCacheFileName)),
		historyShortTermPath,
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
		patternCorpusPath,
		patternsTasksPath,
		patternsProceduresPath,
		patternsPracticesPath,
		patternRunsRelPath,
	}
}

// countSessionEpisodes counts episode records that would be filtered out of
// patterns/episodes.ndjson by a purge.
func countSessionEpisodes(brainDir, sessionID string, transcriptRels map[string]bool) int {
	_, removed, _ := filterEpisodesFile(brainDir, sessionID, transcriptRels, false)
	return removed
}

// filterEpisodesFile removes episode records belonging to the purged session
// (by session id or transcript anchor). With write=false it only counts.
// Unparseable lines are preserved verbatim; filtering must never corrupt what
// it does not understand.
func filterEpisodesFile(brainDir, sessionID string, transcriptRels map[string]bool, write bool) (kept int, removed int, err error) {
	data, readErr := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(patternsEpisodesPath)))
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return 0, 0, nil
		}
		return 0, 0, readErr
	}
	var out strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var record episodeRecord
		if json.Unmarshal([]byte(trimmed), &record) == nil {
			if strings.TrimSpace(record.SessionID) == sessionID || transcriptRels[filepath.ToSlash(strings.TrimSpace(record.Source.Path))] {
				removed++
				continue
			}
		}
		out.WriteString(line)
		out.WriteByte('\n')
		kept++
	}
	if !write || removed == 0 {
		return kept, removed, nil
	}
	return kept, removed, writeBrainRelativeFileAtomic(brainDir, patternsEpisodesPath, []byte(out.String()), 0o600)
}

// purgeSessionFacts applies the fact rules: strip purged-session anchors,
// physically delete facts left with no provenance, and prune proposals that
// reference a deleted fact. Returns (deleted, strippedAnchors).
func purgeSessionFacts(brainDir, sessionID string) (int, int, error) {
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return 0, 0, err
	}
	deletedTotal, strippedTotal := 0, 0
	for branch, facts := range byBranch {
		changed := false
		deletedIDs := map[string]bool{}
		kept := make([]factRecord, 0, len(facts))
		for _, fact := range facts {
			remaining := fact.Provenance[:0:0]
			stripped := 0
			for _, anchor := range fact.Provenance {
				if strings.TrimSpace(anchor.SessionID) == sessionID {
					stripped++
					continue
				}
				remaining = append(remaining, anchor)
			}
			if stripped > 0 && len(remaining) == 0 {
				deletedIDs[fact.ID] = true
				deletedTotal++
				changed = true
				continue
			}
			if stripped > 0 {
				fact.Provenance = remaining
				strippedTotal += stripped
				changed = true
			}
			kept = append(kept, fact)
		}
		if !changed {
			continue
		}
		if err := writeFacts(brainDir, branch, kept); err != nil {
			return deletedTotal, strippedTotal, err
		}
		if proposals, perr := loadFactProposals(brainDir, branch); perr == nil && len(proposals) > 0 {
			keptProposals := proposals[:0:0]
			for _, proposal := range proposals {
				if deletedIDs[proposal.CandidateID] || deletedIDs[proposal.TargetID] {
					continue
				}
				keptProposals = append(keptProposals, proposal)
			}
			if len(keptProposals) != len(proposals) {
				if err := writeFactProposals(brainDir, branch, keptProposals); err != nil {
					return deletedTotal, strippedTotal, err
				}
			}
		}
	}
	return deletedTotal, strippedTotal, nil
}

// purgeDistillCacheEntries drops the purged session's distill-cache entries
// (fingerprint hashes keyed by branch/session; no content, but a purged
// session must not look "already distilled" if it is ever re-included).
// A failed write propagates (R0-2).
func purgeDistillCacheEntries(brainDir, sessionID string) error {
	cache := loadDistillCache(brainDir)
	if len(cache.Sessions) == 0 {
		return nil
	}
	suffix := "/" + url.PathEscape(sessionID)
	changed := false
	for key := range cache.Sessions {
		if strings.HasSuffix(key, suffix) {
			delete(cache.Sessions, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return saveDistillCache(brainDir, cache)
}

// executeSessionPurge applies the plan under the already-held write lock:
// tombstone first (so a crash mid-purge can only leave the session excluded,
// never resurrected), then physical deletion, then a clean rebuild of the
// history projection from the surviving truth.
func executeSessionPurge(brainDir, sessionID string, plan sessionPurgePlan, now time.Time) error {
	return executeSessionCleanup(brainDir, sessionID, plan, now, "purged", false)
}

// executeSessionCleanup is the shared exclude/purge executor: tombstone,
// transcript deletion (purge only), derived-store deletion, fact/episode/
// cache filtering, then the rebuild. Every deletion error propagates (R0-2);
// the tombstone-first order keeps a failed run resumable and the session
// unreadable in the meantime.
func executeSessionCleanup(brainDir, sessionID string, plan sessionPurgePlan, now time.Time, reason string, keepTranscripts bool) error {
	stones := loadSessionTombstones(brainDir)
	stones.Excluded[sessionID] = sessionTombstone{At: now, Reason: reason}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		return err
	}
	if !keepTranscripts {
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
	}
	for _, artifact := range plan.DerivedStores {
		if err := removeSQLiteStoreFiles(filepath.Join(brainDir, filepath.FromSlash(artifact.Path))); err != nil {
			return fmt.Errorf("delete derived store %s: %w", artifact.Path, err)
		}
	}
	if _, _, err := purgeSessionFacts(brainDir, sessionID); err != nil {
		return err
	}
	transcriptRels := map[string]bool{}
	for _, artifact := range plan.Transcripts {
		transcriptRels[artifact.Path] = true
	}
	if _, _, err := filterEpisodesFile(brainDir, sessionID, transcriptRels, true); err != nil {
		return err
	}
	if err := purgeDistillCacheEntries(brainDir, sessionID); err != nil {
		return err
	}
	_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
	return err
}

// purgeGitmetaSyncCaveats reports fact copies a purge can NOT clean: the
// `facts sync` git-meta store (plugin cache, shared fact-set head). Its
// keep-both merge model has no deletion semantics yet, so purged facts synced
// there survive; and a later `facts sync` can merge them back into the local
// store. Surfacing this explicitly beats silently claiming complete deletion;
// real deletion semantics for the shared store are parked (see the plan repo's
// parking lot).
func purgeGitmetaSyncCaveats(env EntireEnv, repoKey string, plan sessionPurgePlan) []string {
	if plan.FactsDeleted == 0 && plan.FactAnchorsStripped == 0 {
		return nil
	}
	gitDir, err := gitmetaDirForKey(env, repoKey)
	if err != nil {
		return nil
	}
	if _, statErr := os.Stat(gitDir); statErr != nil {
		return nil
	}
	return []string{fmt.Sprintf(
		"a `facts sync` git-meta store exists at %s; previously synced copies of purged facts remain there and a future `facts sync` may merge them back; the shared fact-set store has no deletion semantics yet",
		gitDir,
	)}
}

// removeSQLiteStoreFiles deletes a store file plus SQLite WAL/SHM siblings;
// every target is a regenerable derived artifact, so missing files are fine.
// Any other failure (locked store, permission) propagates: a store that
// survives a privacy operation must fail that operation, never be reported
// as removed (R0-2).
func removeSQLiteStoreFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Portable mirrors of the cgo-only vector-store filenames so the purge
// inventory covers both stores on every build (the purego build has no store
// constructors, but the files may exist from a cgo binary's refresh).
const (
	historyVecStoreFileNamePortable      = "vectors.sqlite"
	conversationVecStoreFileNamePortable = "conversation-vectors.sqlite"
)
