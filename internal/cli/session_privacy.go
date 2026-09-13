package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	privacyLstat   = os.Lstat
	privacyOpen    = os.OpenFile
	privacyReadAll = safeReadAll
	privacyReadDir = func(dir *os.File, n int) ([]os.DirEntry, error) { return dir.ReadDir(n) }
	privacyWalkDir = filepath.WalkDir
)

const privacyInventoryMaxEntries = 10_000

// privacyArtifactInfo is the common fail-closed leaf check used by privacy
// inventory and verification. In particular, it lstat's before any open so a
// FIFO/device/socket can never make a checked privacy read block indefinitely.
func privacyArtifactInfo(brainDir, rel, label string) (os.FileInfo, bool, error) {
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %s has an unsafe path %q: %w", memoryErrStateUnsafe, label, rel, err)
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	info, err := privacyLstat(filepath.Join(brainDir, clean))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s: %s is not a regular file: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	return info, true, nil
}

// readPrivacyArtifact reads a known brain-relative regular file through an
// injectable, bounded reader. Missing is reported separately; every other
// stat/read/type failure is an error rather than an empty privacy state.
func readPrivacyArtifact(brainDir, rel, label string, maxBytes int64) ([]byte, bool, error) {
	_, present, err := privacyArtifactInfo(brainDir, rel, label)
	if err != nil || !present {
		return nil, present, err
	}
	clean, _ := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	path := filepath.Join(brainDir, clean)
	// O_NONBLOCK on Unix prevents an attacker-controlled regular-to-FIFO swap
	// between the lstat above and this open from hanging a privacy gate. It is a
	// no-op on Windows and has no effect on regular-file reads.
	f, err := privacyOpen(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, true, fmt.Errorf("%s: open %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	defer f.Close()
	if err := rejectOpenFileAlias(path, f, label); err != nil {
		return nil, true, fmt.Errorf("%s: validate opened %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	data, err := privacyReadAll(f, maxBytes, path)
	if err != nil {
		return nil, true, fmt.Errorf("%s: read %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	return data, true, nil
}

func readPrivacyDirectory(brainDir, rel, label string) ([]os.DirEntry, bool, error) {
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %s has an unsafe path %q: %w", memoryErrStateUnsafe, label, rel, err)
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	info, err := privacyLstat(filepath.Join(brainDir, clean))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, false, fmt.Errorf("%s: %s is not a directory: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	path := filepath.Join(brainDir, clean)
	dir, err := privacyOpen(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, true, fmt.Errorf("%s: open %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	defer dir.Close()
	pathInfo, err := privacyLstat(path)
	if err != nil {
		return nil, true, fmt.Errorf("%s: validate opened %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	dirInfo, err := dir.Stat()
	if err != nil {
		return nil, true, fmt.Errorf("%s: validate opened %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(pathInfo, dirInfo) || !dirInfo.IsDir() {
		return nil, true, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	entries, err := privacyReadDir(dir, privacyInventoryMaxEntries+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, true, fmt.Errorf("%s: read %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	if len(entries) > privacyInventoryMaxEntries {
		return nil, true, fmt.Errorf("%s: %s %s exceeds %d entries", memoryErrStateCorrupt, label, filepath.ToSlash(clean), privacyInventoryMaxEntries)
	}
	return entries, true, nil
}

// session_privacy.go is Phase 4 (privacy, exclusion, and deletion) of the
// conversational-memory plan: brain-local session tombstones understood before
// derived indexing, plus list/exclude/include/purge administration with
// dry-run.
//
// Authority model: the tombstone is brain-local (the plan's resolved default
// until a capture-layer exclusion contract exists in the Entire CLI). Purge
// physically deletes the brain's LOCAL projections (the exported transcript
// copy under sessions/ and every derived store built from it), and the
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

type sessionTombstoneState string

const (
	sessionTombstoneAbsent      sessionTombstoneState = "absent"
	sessionTombstoneCurrent     sessionTombstoneState = "current"
	sessionTombstoneCorrupt     sessionTombstoneState = "corrupt"
	sessionTombstoneUnsupported sessionTombstoneState = "unsupported"
)

// sessionTombstoneFileState is content-free state for status and projection
// identity checks. Identity hashes the exact bytes parsed by the checked
// loader, avoiding a second-read race during generation preparation.
type sessionTombstoneFileState struct {
	State    sessionTombstoneState
	Version  int
	Identity string
}

// sessionTombstoneLoadError keeps privacy failures machine-classifiable on
// every CLI/MCP serving and derivation boundary. Its Error prefix is also the
// repository's established JSON command-error contract.
type sessionTombstoneLoadError struct {
	Code    string
	State   sessionTombstoneState
	Version int
	Err     error
}

func (e *sessionTombstoneLoadError) Error() string {
	detail := "session tombstone state is invalid"
	if e.Err != nil {
		detail = e.Err.Error()
	}
	return e.Code + ": " + detail
}

func (e *sessionTombstoneLoadError) Unwrap() error { return e.Err }

func emptySessionTombstones() sessionTombstones {
	return sessionTombstones{Version: sessionTombstonesVersion, Excluded: map[string]sessionTombstone{}}
}

// loadSessionTombstonesChecked is the only loader allowed on serving,
// derivation, worker, privacy, and administration boundaries. Missing means a
// valid empty policy. Once the file exists, unreadable/malformed state fails
// closed: treating it as empty could disclose or re-derive excluded content.
func loadSessionTombstonesChecked(brainDir string) (sessionTombstones, sessionTombstoneFileState, error) {
	empty := emptySessionTombstones()
	data, present, err := readPrivacyArtifact(brainDir, sessionTombstonesPath, "session tombstones", maxManifestBytes)
	if err != nil {
		state := sessionTombstoneFileState{State: sessionTombstoneCorrupt}
		return sessionTombstones{}, state, &sessionTombstoneLoadError{
			Code: memoryErrStateCorrupt, State: state.State,
			Err: fmt.Errorf("read session tombstones: %w", err),
		}
	}
	if !present {
		return empty, sessionTombstoneFileState{State: sessionTombstoneAbsent, Version: sessionTombstonesVersion, Identity: "absent"}, nil
	}
	sum := sha256.Sum256(data)
	state := sessionTombstoneFileState{Identity: "sha256:" + hex.EncodeToString(sum[:])}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		state.State = sessionTombstoneCorrupt
		return sessionTombstones{}, state, &sessionTombstoneLoadError{
			Code: memoryErrStateCorrupt, State: state.State,
			Err: errors.New("session tombstones cannot be parsed"),
		}
	}
	state.Version = header.Version
	if header.Version > sessionTombstonesVersion {
		state.State = sessionTombstoneUnsupported
		return sessionTombstones{}, state, &sessionTombstoneLoadError{
			Code: memoryErrUnsupportedVersion, State: state.State, Version: header.Version,
			Err: fmt.Errorf("session tombstone schema %d is unsupported (current %d)", header.Version, sessionTombstonesVersion),
		}
	}
	var stones sessionTombstones
	if _, err := decodeVersionedJSONBody(data, &stones, false); err != nil {
		message := "session tombstones cannot be parsed strictly"
		if errors.Is(err, errTrailingJSONData) {
			message = "session tombstones contain trailing JSON data"
		}
		state.State = sessionTombstoneCorrupt
		return sessionTombstones{}, state, &sessionTombstoneLoadError{
			Code: memoryErrStateCorrupt, State: state.State,
			Err: errors.New(message),
		}
	}
	if stones.Version != sessionTombstonesVersion {
		state.State = sessionTombstoneCorrupt
		return sessionTombstones{}, state, &sessionTombstoneLoadError{
			Code: memoryErrStateCorrupt, State: state.State, Version: stones.Version,
			Err: fmt.Errorf("session tombstone schema %d is invalid (current %d)", stones.Version, sessionTombstonesVersion),
		}
	}
	if stones.Excluded == nil {
		state.State = sessionTombstoneCorrupt
		return sessionTombstones{}, state, &sessionTombstoneLoadError{
			Code: memoryErrStateCorrupt, State: state.State,
			Err: errors.New("session tombstones excluded map is missing"),
		}
	}
	state.State = sessionTombstoneCurrent
	return stones, state, nil
}

// loadSessionTombstones is a convenience for tests and locked code that has
// already established a valid tombstone file. It panics instead of ever
// converting invalid present state into an empty policy. Production boundaries
// must use loadSessionTombstonesChecked and return its typed error.
func loadSessionTombstones(brainDir string) sessionTombstones {
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		panic(err)
	}
	return stones
}

func saveSessionTombstones(brainDir string, stones sessionTombstones) error {
	if _, _, err := loadSessionTombstonesChecked(brainDir); err != nil {
		return err
	}
	data, err := marshalSessionTombstones(stones)
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, sessionTombstonesPath, data, 0o600)
}

func marshalSessionTombstones(stones sessionTombstones) ([]byte, error) {
	stones.Version = sessionTombstonesVersion
	data, err := json.MarshalIndent(stones, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// excludedTranscriptPaths resolves the brain-relative transcript paths that
// belong to excluded sessions, from the trusted manifest only.
func normalizePrivacyTranscriptPath(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	clean := filepath.Clean(filepath.FromSlash(trimmed))
	if clean == "." {
		return ""
	}
	return filepath.ToSlash(clean)
}

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
		rel := normalizePrivacyTranscriptPath(session.TranscriptPath)
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
			stones, _, err := loadSessionTombstonesChecked(brainDir)
			if err != nil {
				return err
			}
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
				// An empty session list is the normal state of a brain that has
				// never captured a session, and printing nothing for it made the
				// one command a privacy-conscious user reaches for indistinguishable
				// from a command that failed to look. Say which it is.
				if len(entries) == 0 {
					fmt.Fprintln(out, "no captured sessions in this brain")
					return
				}
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

// sessionReadGuard is the retrieval-time tombstone view: every
// conversation/history/fact retrieval boundary consults it so an excluded
// session becomes unreadable the moment its tombstone lands, without waiting
// for the derived rebuild. Defense in depth, not a substitute for cleanup.
type sessionReadGuard struct {
	ids            map[string]sessionTombstone
	paths          map[string]string // excluded transcript rel -> session id, from the manifest
	policyIdentity string            // exact checked tombstone bytes used to build the guard
}

// loadSessionReadGuard builds the guard from the tombstone set and, when a
// manifest is supplied, the excluded sessions' transcript paths (records that
// lost their session id still resolve by path). Invalid present state is a
// typed error; serving must stop rather than silently restore excluded data.
func loadSessionReadGuard(brainDir string, manifest *exportManifest) (sessionReadGuard, error) {
	stones, state, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return sessionReadGuard{}, err
	}
	guard := sessionReadGuard{policyIdentity: state.Identity}
	if len(stones.Excluded) == 0 {
		return guard, nil
	}
	guard.ids = stones.Excluded
	if manifest != nil {
		guard.paths = excludedTranscriptPaths(manifest, stones)
	}
	return guard, nil
}

func (g sessionReadGuard) empty() bool { return len(g.ids) == 0 }

func (g sessionReadGuard) blocksSession(sessionID string) bool {
	if g.empty() {
		return false
	}
	_, ok := g.ids[strings.TrimSpace(sessionID)]
	return ok
}

func (g sessionReadGuard) blocksExportSession(session exportSession) bool {
	if g.blocksSession(session.SessionID) {
		return true
	}
	_, blocked := g.paths[normalizePrivacyTranscriptPath(session.TranscriptPath)]
	return blocked
}

func guardExportSessions(guard sessionReadGuard, sessions []exportSession) []exportSession {
	if guard.empty() || len(sessions) == 0 {
		return sessions
	}
	kept := make([]exportSession, 0, len(sessions))
	for _, session := range sessions {
		if !guard.blocksExportSession(session) {
			kept = append(kept, session)
		}
	}
	return kept
}

func (g sessionReadGuard) blocksRecord(r historyRecord) bool {
	if g.blocksSession(r.SessionID) {
		return true
	}
	_, ok := g.paths[normalizePrivacyTranscriptPath(r.Path)]
	return ok
}

func (g sessionReadGuard) blocksFactAnchor(anchor factAnchor) bool {
	if g.blocksSession(anchor.SessionID) {
		return true
	}
	_, blocked := g.paths[normalizePrivacyTranscriptPath(anchor.Transcript)]
	return blocked
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
		if !g.blocksFactAnchor(anchor) {
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
// list: an excluded session must never produce new derived facts or
// records, even though its canonical transcript may still exist.
func filterTombstonedSessions(brainDir string, sessions []exportSession) ([]exportSession, error) {
	manifest := &exportManifest{Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: sessions}}}
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return nil, err
	}
	return guardExportSessions(guard, sessions), nil
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
	if err := withBrainPrivacySideEffectLock(brainDir, func() error {
		return withBrainWriteLock(brainDir, func() error {
			// Exclusion is purge minus the transcript: the tombstone guards
			// reads immediately, then every derived projection the session fed --
			// index records, facts, episodes, pattern outputs, caches, FTS and
			// vector stores -- is removed or rebuilt from the surviving truth.
			// Errors propagate; a partial cleanup is a failure, not a success.
			plan, planErr := buildSessionPurgePlan(brainDir, sessionID)
			if planErr != nil {
				return planErr
			}
			return executeSessionCleanup(brainDir, sessionID, plan, opts.Now().UTC(), strings.TrimSpace(reason), true)
		})
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
			if err := withBrainPrivacySideEffectLock(brainDir, func() error {
				return withBrainWriteLock(brainDir, func() error {
					return includeSessionLocked(brainDir, sessionID, opts.Now().UTC())
				})
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

// rebuildSessionHistoryForIncludeLocked is an injected commit seam for the
// re-include transaction. Production uses the same generation-addressed,
// manifest-last history publisher as refresh; tests fail or pause this exact
// boundary to prove rollback and lock ordering.
var rebuildSessionHistoryForIncludeLocked = func(brainDir string, now time.Time) error {
	_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
	return err
}

// beforeIncludePrivacyTransactionDirSync is injectable so the
// unlink-succeeded/sync-failed transaction outcome is deterministic in tests.
var beforeIncludePrivacyTransactionDirSync = func(string) error { return nil }

type includePrivacyRollback struct {
	tombstonesBefore   []byte
	tombstonesAfter    []byte
	transactionRel     string
	transactionBefore  []byte
	transactionPresent bool
}

// includeSessionLocked removes one exclusion and rebuilds the including history
// generation as one externally atomic operation. The caller holds the privacy
// side-effect lock before the Brain write lock, so no cooperative read, provider
// side effect, or privacy mutation can observe the temporary policy. If any
// pre-commit step fails, the exact validated tombstone and transaction bytes are
// restored before either lock is released, leaving the same command retryable.
func includeSessionLocked(brainDir, sessionID string, now time.Time) error {
	stones, tombstoneState, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return err
	}
	if _, ok := stones.Excluded[sessionID]; !ok {
		return fmt.Errorf("session %s is not excluded", sessionID)
	}
	tombstonesBefore, present, err := readPrivacyArtifact(brainDir, sessionTombstonesPath, "session tombstones", maxManifestBytes)
	if err != nil {
		return err
	}
	if !present || privacyBytesIdentity(tombstonesBefore) != tombstoneState.Identity {
		return fmt.Errorf("%s: session tombstones changed before include", memoryErrStateUnsafe)
	}
	_, transactionPresent, err := loadPrivacyTransactionChecked(brainDir, sessionID)
	if err != nil {
		return err
	}
	var transactionBefore []byte
	transactionRel := privacyTransactionRel(sessionID)
	if transactionPresent {
		transactionBefore, present, err = readPrivacyArtifact(brainDir, transactionRel, "privacy transaction", maxManifestBytes)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("%s: privacy transaction disappeared before include", memoryErrStateUnsafe)
		}
		if _, err := decodePrivacyTransaction(transactionBefore, sessionID); err != nil {
			return err
		}
	}

	delete(stones.Excluded, sessionID)
	tombstonesAfter, err := marshalSessionTombstones(stones)
	if err != nil {
		return err
	}
	rollback := includePrivacyRollback{
		tombstonesBefore: tombstonesBefore, tombstonesAfter: tombstonesAfter,
		transactionRel: transactionRel, transactionBefore: transactionBefore, transactionPresent: transactionPresent,
	}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		// Atomic replacement can report a durability failure after rename made
		// the included policy visible. The result is therefore ambiguous: always
		// inspect and restore the exact prior state before releasing either lock.
		return errors.Join(err, restoreIncludePrivacyState(brainDir, rollback))
	}
	currentTombstones, present, err := readPrivacyArtifact(brainDir, sessionTombstonesPath, "included session tombstones", maxManifestBytes)
	if err != nil || !present || !bytes.Equal(currentTombstones, tombstonesAfter) {
		if err == nil {
			err = fmt.Errorf("%s: session tombstones changed while including %s", memoryErrStateUnsafe, sessionID)
		}
		return errors.Join(err, restoreIncludePrivacyState(brainDir, rollback))
	}
	// Re-include is a clean slate: the old cleanup transaction no longer
	// describes a live exclusion. Removing it before the rebuild also means a
	// successful manifest switch cannot be followed by a fallible cleanup step.
	if err := removePrivacyTransactionForInclude(brainDir, sessionID, transactionPresent); err != nil {
		return errors.Join(err, restoreIncludePrivacyState(brainDir, rollback))
	}
	if err := rebuildSessionHistoryForIncludeLocked(brainDir, now); err != nil {
		return errors.Join(err, restoreIncludePrivacyState(brainDir, rollback))
	}
	return nil
}

func privacyBytesIdentity(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// restoreIncludePrivacyState refuses to overwrite any unexpected concurrent or
// forward-version leaf. Cooperative writers cannot enter while include holds
// the Brain lock, but exact checks keep rollback non-destructive even under an
// out-of-process or adversarial replacement.
func restoreIncludePrivacyState(brainDir string, rollback includePrivacyRollback) error {
	var restoreErr error
	currentTombstones, present, err := readPrivacyArtifact(brainDir, sessionTombstonesPath, "session tombstones during include rollback", maxManifestBytes)
	switch {
	case err != nil:
		restoreErr = errors.Join(restoreErr, err)
	case present && bytes.Equal(currentTombstones, rollback.tombstonesBefore):
		// A failed atomic replacement may already have left the original bytes.
	case !present || len(rollback.tombstonesAfter) == 0 || !bytes.Equal(currentTombstones, rollback.tombstonesAfter):
		restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: session tombstones changed during include rollback", memoryErrStateUnsafe))
	default:
		if err := restoreIncludePrivacyFile(brainDir, sessionTombstonesPath, "session tombstones", rollback.tombstonesBefore); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore session tombstones after failed include: %w", err))
		}
	}

	currentTransaction, transactionPresent, err := readPrivacyArtifact(brainDir, rollback.transactionRel, "privacy transaction during include rollback", maxManifestBytes)
	switch {
	case err != nil:
		restoreErr = errors.Join(restoreErr, err)
	case !rollback.transactionPresent && !transactionPresent:
	case !rollback.transactionPresent:
		restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: privacy transaction appeared during include rollback", memoryErrStateUnsafe))
	case transactionPresent && bytes.Equal(currentTransaction, rollback.transactionBefore):
	case transactionPresent:
		restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: privacy transaction changed during include rollback", memoryErrStateUnsafe))
	default:
		if err := restoreIncludePrivacyFile(brainDir, rollback.transactionRel, "privacy transaction", rollback.transactionBefore); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore privacy transaction after failed include: %w", err))
		}
	}
	return errors.Join(restoreErr, verifyIncludePrivacyRollback(brainDir, rollback))
}

func verifyIncludePrivacyRollback(brainDir string, rollback includePrivacyRollback) error {
	var verifyErr error
	if err := requireExactIncludePrivacyFile(brainDir, sessionTombstonesPath, "session tombstones", rollback.tombstonesBefore); err != nil {
		verifyErr = errors.Join(verifyErr, err)
	}
	current, present, err := readPrivacyArtifact(brainDir, rollback.transactionRel, "privacy transaction after include rollback", maxManifestBytes)
	switch {
	case err != nil:
		verifyErr = errors.Join(verifyErr, err)
	case rollback.transactionPresent && (!present || !bytes.Equal(current, rollback.transactionBefore)):
		verifyErr = errors.Join(verifyErr, fmt.Errorf("%s: exact privacy transaction rollback bytes are not live", memoryErrStateUnsafe))
	case !rollback.transactionPresent && present:
		verifyErr = errors.Join(verifyErr, fmt.Errorf("%s: unexpected privacy transaction is live after rollback", memoryErrStateUnsafe))
	}
	return verifyErr
}

// restoreIncludePrivacyFile resolves the only ambiguous atomic-write failure:
// rename succeeded but syncing the parent directory did not. It proves the
// exact rollback bytes are live, performs the missing durability barrier, and
// proves them again. Every other write failure remains an error; callers never
// infer successful rollback from an unclassified failure.
func restoreIncludePrivacyFile(brainDir, rel, label string, before []byte) error {
	err := writeBrainRelativeFileAtomic(brainDir, rel, before, 0o600)
	if err == nil {
		return nil
	}
	var durabilityErr *atomicReplaceDurabilityError
	if !errors.As(err, &durabilityErr) {
		return err
	}
	if exactErr := requireExactIncludePrivacyFile(brainDir, rel, label, before); exactErr != nil {
		return errors.Join(err, exactErr)
	}
	if rootErr := rejectSymlinkedBrainRoot(brainDir); rootErr != nil {
		return errors.Join(err, rootErr)
	}
	clean, cleanErr := cleanBrainRelativePath(rel)
	if cleanErr != nil {
		return errors.Join(err, cleanErr)
	}
	if syncErr := syncParentDir(filepath.Join(brainDir, clean)); syncErr != nil {
		return errors.Join(err, fmt.Errorf("sync restored %s directory: %w", label, syncErr))
	}
	if exactErr := requireExactIncludePrivacyFile(brainDir, rel, label, before); exactErr != nil {
		return errors.Join(err, exactErr)
	}
	return nil
}

func requireExactIncludePrivacyFile(brainDir, rel, label string, want []byte) error {
	current, present, err := readPrivacyArtifact(brainDir, rel, label+" after include rollback", maxManifestBytes)
	if err != nil {
		return err
	}
	if !present || !bytes.Equal(current, want) {
		return fmt.Errorf("%s: exact %s rollback bytes are not live", memoryErrStateUnsafe, label)
	}
	return nil
}

// sessionPurgePlan is the dry-run contract: exactly the artifacts (and their
// current bytes) a purge would remove or rebuild.
type sessionPurgePlan struct {
	SessionID string `json:"session_id"`
	// SessionRefs are content-free canonical conversation identities captured
	// before the tombstone lands. They remain durable in the privacy transaction
	// so a retry can remove abstracts/egress even if refresh later drops or
	// renames the manifest session entry.
	SessionRefs []string `json:"session_refs,omitempty"`
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
	// WorkMetadata is content-free lifecycle/job state tied to this raw
	// session. Privacy cleanup removes it immediately rather than retaining
	// operational evidence about a session the user excluded or purged.
	WorkMetadata []purgeArtifact `json:"work_metadata,omitempty"`
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
	apply := func() error {
		return withBrainWriteLock(brainDir, func() error {
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
		})
	}
	if !dryRun {
		operation := apply
		apply = func() error { return withBrainPrivacySideEffectLock(brainDir, operation) }
	}
	if err := apply(); err != nil {
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
		for _, artifact := range plan.WorkMetadata {
			fmt.Fprintf(out, "  memory work metadata %s (%d bytes)\n", artifact.Path, artifact.Bytes)
		}
		for _, caveat := range plan.Caveats {
			fmt.Fprintf(out, "  NOT purged: %s\n", caveat)
		}
	})
}

// buildSessionPurgePlan computes exactly what a purge touches, from the
// trusted manifest and the current index. Purging an unknown or already-purged
// session yields an empty plan (idempotent), not an error.
func loadAllFactBranchesForPrivacy(brainDir string) (map[string][]factRecord, error) {
	entries, present, err := readPrivacyDirectory(brainDir, factsDirName, "facts directory")
	if err != nil || !present {
		if err != nil {
			return nil, err
		}
		return map[string][]factRecord{}, nil
	}
	byBranch := map[string][]factRecord{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: facts inventory contains a symlink: %s", memoryErrStateUnsafe, entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("%s: inspect facts entry %s: %w", memoryErrStateCorrupt, entry.Name(), err)
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: facts inventory contains a nonregular entry: %s", memoryErrStateUnsafe, entry.Name())
			}
			continue // taxonomy and other root-level metadata are not fact stores.
		}
		rel := filepath.ToSlash(filepath.Join(factsDirName, entry.Name(), factsFileName))
		data, exists, err := readPrivacyArtifact(brainDir, rel, "fact store", 256<<20)
		if err != nil {
			return nil, err
		}
		if exists {
			for lineNumber, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				var record factRecord
				if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
					return nil, fmt.Errorf("%s: parse %s line %d: %w", memoryErrStateCorrupt, rel, lineNumber+1, err)
				}
				byBranch[record.Branch] = append(byBranch[record.Branch], record)
			}
		}
		proposalRel := filepath.ToSlash(filepath.Join(factsDirName, entry.Name(), factsProposalsFileName))
		proposalData, proposalExists, err := readPrivacyArtifact(brainDir, proposalRel, "fact proposal store", 64<<20)
		if err != nil {
			return nil, err
		}
		if proposalExists {
			for lineNumber, line := range strings.Split(string(proposalData), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				var proposal factProposal
				if err := json.Unmarshal([]byte(trimmed), &proposal); err != nil {
					return nil, fmt.Errorf("%s: parse %s line %d: %w", memoryErrStateCorrupt, proposalRel, lineNumber+1, err)
				}
			}
		}
	}
	return byBranch, nil
}

func factAnchorMatchesSession(anchor factAnchor, sessionID string, transcriptRels map[string]bool) bool {
	if strings.TrimSpace(anchor.SessionID) == strings.TrimSpace(sessionID) {
		return true
	}
	rel := normalizePrivacyTranscriptPath(anchor.Transcript)
	return rel != "" && transcriptRels[rel]
}

func buildSessionPurgePlan(brainDir, sessionID string) (sessionPurgePlan, error) {
	plan := sessionPurgePlan{SessionID: sessionID}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return plan, err
	}
	refSet := sessionRefsForSessionID(brainDir, manifest, sessionID)
	priorTx, priorPresent, err := loadPrivacyTransactionChecked(brainDir, sessionID)
	if err != nil {
		return plan, err
	}
	if priorPresent {
		for _, ref := range priorTx.SessionRefs {
			refSet[ref] = true
		}
	}
	transcriptRels := map[string]bool{}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		for _, session := range manifest.Sources.Sessions.Sessions {
			if strings.TrimSpace(session.SessionID) != sessionID {
				continue
			}
			rel := normalizePrivacyTranscriptPath(session.TranscriptPath)
			if rel == "" || transcriptRels[rel] {
				continue
			}
			transcriptRels[rel] = true
			artifact := purgeArtifact{Path: rel}
			info, present, statErr := privacyArtifactInfo(brainDir, rel, "session transcript")
			if statErr != nil {
				return plan, statErr
			}
			if present {
				artifact.Bytes = info.Size()
			}
			plan.Transcripts = append(plan.Transcripts, artifact)
		}
	}
	sort.Slice(plan.Transcripts, func(i, j int) bool { return plan.Transcripts[i].Path < plan.Transcripts[j].Path })
	if manifest.Sources != nil && manifest.Sources.History != nil {
		// A declared-but-unreadable index is a real storage problem: failing
		// here beats an under-reported plan that execution would then trust
		// (dry-run and execution inventories must match).
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
	byBranch, ferr := loadAllFactBranchesForPrivacy(brainDir)
	if ferr != nil {
		return plan, fmt.Errorf("load facts: %w", ferr)
	}
	for _, facts := range byBranch {
		for _, fact := range facts {
			matched, remaining := 0, 0
			for _, anchor := range fact.Provenance {
				if factAnchorMatchesSession(anchor, sessionID, transcriptRels) {
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
	plan.Episodes, err = countSessionEpisodes(brainDir, sessionID, transcriptRels)
	if err != nil {
		return plan, err
	}
	// Rebuildable derived stores removed wholesale: their row-level content is
	// keyed by record ids/text derived from the purged transcripts, and every
	// one regenerates from the surviving truth (index rebuild, lazy FTS build,
	// next vector sync, next patterns build). runs.ndjson is a build log and
	// logs are inside the plan's deletion inventory.
	derivedArtifacts, inventoryErr := privacyDerivedStoreArtifacts(brainDir)
	if inventoryErr != nil {
		return plan, inventoryErr
	}
	for _, rel := range derivedArtifacts {
		info, present, statErr := privacyArtifactInfo(brainDir, rel, "derived privacy store")
		if statErr != nil {
			return plan, statErr
		}
		if present {
			plan.DerivedStores = append(plan.DerivedStores, purgeArtifact{Path: rel, Bytes: info.Size()})
		}
	}
	// Durable memory hints/jobs are intentionally content-free, but they still
	// retain the identity and lifecycle of this session. Include their exact
	// files in dry-run and execution inventories so privacy cleanup is complete
	// and auditable.
	hints, hintIssues := loadMemoryHintsChecked(brainDir)
	jobInventory := loadMemoryJobInventory(brainDir)
	if err := memoryStateError(append(hintIssues, jobInventory.Issues...)); err != nil {
		return plan, err
	}
	addWork := func(rel string) error {
		artifact := purgeArtifact{Path: rel}
		info, present, statErr := privacyArtifactInfo(brainDir, rel, "memory work metadata")
		if statErr != nil {
			return statErr
		}
		if present {
			artifact.Bytes = info.Size()
		}
		plan.WorkMetadata = append(plan.WorkMetadata, artifact)
		return nil
	}
	for _, hint := range hints {
		if hint.SessionID == sessionID {
			if err := addWork(memoryHintRel(hint.RepoKey, hint.SessionID, hint.Branch)); err != nil {
				return plan, err
			}
		}
	}
	for _, job := range jobInventory.Jobs {
		if job.SessionID != sessionID {
			continue
		}
		if ref := strings.TrimSpace(job.SessionRef); ref != "" {
			if !strings.HasPrefix(ref, conversationSessionIDPrefix) {
				return plan, fmt.Errorf("%s: abstract job has invalid session ref", memoryErrStateCorrupt)
			}
			refSet[ref] = true
		}
		for _, rel := range memoryJobSourcePaths(job) {
			if err := addWork(rel); err != nil {
				return plan, err
			}
		}
		cancellationRel := memoryCancellationRel(job.JobID)
		_, present, statErr := privacyArtifactInfo(brainDir, cancellationRel, "memory cancellation marker")
		if statErr != nil {
			return plan, statErr
		}
		if present {
			if err := addWork(cancellationRel); err != nil {
				return plan, err
			}
		}
	}
	if len(refSet) > privacyTransactionSessionRefMax {
		return plan, fmt.Errorf("%s: privacy scope exceeds %d canonical session refs", memoryErrQueryTooBroad, privacyTransactionSessionRefMax)
	}
	for ref := range refSet {
		plan.SessionRefs = append(plan.SessionRefs, ref)
	}
	sort.Strings(plan.SessionRefs)
	egressReceipts, egressErr := abstractEgressReceiptsForSessionRefsChecked(brainDir, refSet)
	if egressErr != nil {
		return plan, egressErr
	}
	for _, receipt := range egressReceipts {
		if err := addWork(receipt.Path); err != nil {
			return plan, err
		}
	}
	sort.Slice(plan.WorkMetadata, func(i, j int) bool { return plan.WorkMetadata[i].Path < plan.WorkMetadata[j].Path })
	return plan, nil
}

// privacyDerivedStoreRels is the static inventory of rebuildable derived
// stores that exclude/purge delete wholesale and `privacy verify` re-checks;
// dry-run, execution, and verification must never disagree on this list.
func privacyDerivedStoreRels() []string {
	rels := []string{
		historyFTSDBRelPath(),
		filepath.ToSlash(filepath.Join(historyDirName, historyScanCacheFileName)),
		historyShortTermPath,
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
		patternsTasksPath,
		patternsProceduresPath,
		patternsPracticesPath,
		patternRunsRelPath,
	}
	rels = append(rels, patternCorpusPrivacyArtifactRels()...)
	rels = append(rels, patternCorpusPublicationMarkerPath)
	return rels
}

// privacyDerivedStoreArtifacts expands the static inventory with the
// per-branch fact embedding caches (embeddings of distilled fact text; a
// purged session's facts may be partially recoverable from them) and the
// SQLite WAL/SHM siblings of every ordinary store and the publication layer's
// canonical WAL/SHM/rollback-journal set for the pattern corpus, so dry-run
// reports and cleanup delete exactly the same artifact set, orphans included.
func privacyDerivedStoreArtifacts(brainDir string) ([]string, error) {
	rels := privacyDerivedStoreRels()
	publication, _, publicationPresent, err := loadPatternCorpusPublicationForPrivacy(brainDir)
	if err != nil {
		return nil, err
	}
	if publicationPresent {
		// Inventory the marker-authorized candidate even if it disappears before
		// the directory scan. A current marker is valid operational state, but
		// both leaves are transient/content-bearing and privacy cleanup owns them.
		rels = append(rels, filepath.ToSlash(filepath.Join("patterns", publication.Candidate)))
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	activeIndex, activeReceipt := "", ""
	if manifest.Sources != nil && manifest.Sources.History != nil {
		activeIndex = filepath.ToSlash(strings.TrimSpace(manifest.Sources.History.IndexPath))
		activeReceipt = filepath.ToSlash(strings.TrimSpace(manifest.Sources.History.ProjectionStatePath))
		if activeReceipt == "" && manifest.Sources.History.ProjectionStateDigest != "" {
			activeReceipt = projectionStateRel
		}
	}
	for _, legacy := range []struct {
		path   string
		active string
	}{{historyIndexPath, activeIndex}, {projectionStateRel, activeReceipt}} {
		if legacy.path == legacy.active {
			continue
		}
		info, statErr := privacyLstat(filepath.Join(brainDir, filepath.FromSlash(legacy.path)))
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, fmt.Errorf("memory_state_corrupt: inventory legacy projection %s: %w", legacy.path, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("memory_state_corrupt: inactive legacy projection is not a regular file: %s", legacy.path)
		}
		rels = append(rels, legacy.path)
	}
	// History generations are immutable publication artifacts. Every old
	// generation may still contain the excluded session even though only one is
	// manifest-referenced, so privacy cleanup inventories and deletes them all.
	for _, rootRel := range []string{historyGenerationsDir, historyStagingDir} {
		root := filepath.Join(brainDir, filepath.FromSlash(rootRel))
		err := privacyWalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("memory_state_corrupt: history projection inventory contains a symlink: %s", path)
			}
			if d.IsDir() {
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("memory_state_corrupt: history projection inventory contains a nonregular artifact: %s", path)
			}
			if rel, relErr := filepath.Rel(brainDir, path); relErr == nil {
				rel = filepath.ToSlash(rel)
				if validHistoryGenerationArtifactPath(rel, historyIndexFileName) || validHistoryGenerationArtifactPath(rel, projectionStateFileName) ||
					validHistoryStagingArtifactPath(rel, historyIndexFileName) || validHistoryStagingArtifactPath(rel, projectionStateFileName) {
					if rel == activeIndex || rel == activeReceipt {
						return nil
					}
					rels = append(rels, rel)
					return nil
				}
			}
			return fmt.Errorf("memory_state_corrupt: unknown history projection artifact: %s", path)
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("memory_state_corrupt: inventory %s: %w", rootRel, err)
		}
	}
	// A pattern rebuild is assembled beside the corpus before one transactional
	// publication. A crash can leave its randomly named main/WAL/SHM/journal leaves
	// behind, and an in-flight build may still contain an excluded transcript.
	// Inventory the bounded directory explicitly so purge removes every such
	// staging generation and the later publisher fails closed.
	patternEntries, patternsPresent, err := readPrivacyDirectory(brainDir, "patterns", "pattern directory")
	if err != nil {
		return nil, err
	}
	if patternsPresent {
		for _, entry := range patternEntries {
			if !strings.HasPrefix(entry.Name(), patternCorpusStagingPrefix) {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s: pattern staging inventory contains a symlink: %s", memoryErrStateUnsafe, entry.Name())
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return nil, fmt.Errorf("%s: inspect pattern staging artifact %s: %w", memoryErrStateCorrupt, entry.Name(), infoErr)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: pattern staging inventory contains a nonregular artifact: %s", memoryErrStateUnsafe, entry.Name())
			}
			rels = append(rels, filepath.ToSlash(filepath.Join("patterns", entry.Name())))
		}
	}
	// Discover fact-vector caches by directory entries, never by Glob: branch
	// names may contain glob metacharacters, and Glob cannot report unreadable
	// directories or unsafe entry types. Only the two exact store basenames and
	// their SQLite sidecars are allowed in an embeddings directory.
	factEntries, factsPresent, err := readPrivacyDirectory(brainDir, factsDirName, "facts directory")
	if err != nil {
		return nil, err
	}
	if factsPresent {
		for _, branchEntry := range factEntries {
			if branchEntry.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s: facts inventory contains a symlink: %s", memoryErrStateUnsafe, branchEntry.Name())
			}
			branchInfo, infoErr := branchEntry.Info()
			if infoErr != nil {
				return nil, fmt.Errorf("%s: inspect facts entry %s: %w", memoryErrStateCorrupt, branchEntry.Name(), infoErr)
			}
			if !branchInfo.IsDir() {
				if !branchInfo.Mode().IsRegular() {
					return nil, fmt.Errorf("%s: facts inventory contains a nonregular entry: %s", memoryErrStateUnsafe, branchEntry.Name())
				}
				continue
			}
			embedRel := filepath.ToSlash(filepath.Join(factsDirName, branchEntry.Name(), embedStoreDirName))
			embedEntries, embedPresent, readErr := readPrivacyDirectory(brainDir, embedRel, "fact embedding directory")
			if readErr != nil {
				return nil, readErr
			}
			if !embedPresent {
				continue
			}
			for _, entry := range embedEntries {
				if entry.Type()&os.ModeSymlink != 0 {
					return nil, fmt.Errorf("%s: fact embedding inventory contains a symlink: %s/%s", memoryErrStateUnsafe, embedRel, entry.Name())
				}
				info, infoErr := entry.Info()
				if infoErr != nil {
					return nil, fmt.Errorf("%s: inspect fact embedding %s/%s: %w", memoryErrStateCorrupt, embedRel, entry.Name(), infoErr)
				}
				if !info.Mode().IsRegular() {
					return nil, fmt.Errorf("%s: fact embedding inventory contains a nonregular artifact: %s/%s", memoryErrStateUnsafe, embedRel, entry.Name())
				}
				base := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), "-wal"), "-shm")
				if base != embedStoreFileName && base != historyVecStoreFileNamePortable {
					return nil, fmt.Errorf("%s: unknown fact embedding artifact: %s/%s", memoryErrStateCorrupt, embedRel, entry.Name())
				}
				rels = append(rels, filepath.ToSlash(filepath.Join(embedRel, base)))
			}
		}
	}
	withSiblings := make([]string, 0, len(rels)*3)
	seen := make(map[string]bool, len(rels)*3)
	for _, rel := range rels {
		artifacts := []string{rel, rel + "-wal", rel + "-shm"}
		if rel == patternCorpusPublicationMarkerPath || isPatternCorpusPrivacyArtifactRel(rel) || isPatternCorpusStagingArtifactRel(rel) {
			// Pattern publication owns an explicit shared sidecar inventory, and
			// staging leaves were enumerated exactly. Do not synthesize unrelated
			// suffixes for its marker, sidecars, or candidate leaves.
			artifacts = []string{rel}
		}
		for _, artifact := range artifacts {
			if !seen[artifact] {
				seen[artifact] = true
				withSiblings = append(withSiblings, artifact)
			}
		}
	}
	sort.Strings(withSiblings)
	return withSiblings, nil
}

type privacyEpisodeLine struct {
	raw    string
	record episodeRecord
}

func loadPrivacyEpisodeLines(brainDir string) ([]privacyEpisodeLine, bool, error) {
	data, present, err := readPrivacyArtifact(brainDir, patternsEpisodesPath, "pattern episodes", 256<<20)
	if err != nil || !present {
		return nil, present, err
	}
	lines := make([]privacyEpisodeLine, 0)
	for lineNumber, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var record episodeRecord
		if err := json.Unmarshal([]byte(trimmed), &record); err != nil {
			return nil, true, fmt.Errorf("%s: parse %s line %d: %w", memoryErrStateCorrupt, patternsEpisodesPath, lineNumber+1, err)
		}
		lines = append(lines, privacyEpisodeLine{raw: line, record: record})
	}
	return lines, true, nil
}

// countSessionEpisodes counts episode records that would be filtered out of
// patterns/episodes.ndjson by a purge.
func countSessionEpisodes(brainDir, sessionID string, transcriptRels map[string]bool) (int, error) {
	_, removed, err := filterEpisodesFile(brainDir, sessionID, transcriptRels, false)
	return removed, err
}

// filterEpisodesFile removes episode records belonging to the purged session
// (by session id or transcript anchor). With write=false it only counts.
// A malformed line is an opaque attribution boundary and fails closed before
// any rewrite; preserving it could retain excluded content that verify cannot
// associate with a session.
func filterEpisodesFile(brainDir, sessionID string, transcriptRels map[string]bool, write bool) (kept int, removed int, err error) {
	lines, present, readErr := loadPrivacyEpisodeLines(brainDir)
	if readErr != nil {
		return 0, 0, readErr
	}
	if !present {
		return 0, 0, nil
	}
	var out strings.Builder
	for _, line := range lines {
		record := line.record
		if strings.TrimSpace(record.SessionID) == sessionID || transcriptRels[normalizePrivacyTranscriptPath(record.Source.Path)] {
			removed++
			continue
		}
		out.WriteString(line.raw)
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
func purgeSessionFacts(brainDir, sessionID string, transcriptRels map[string]bool) (int, int, error) {
	byBranch, err := loadAllFactBranchesForPrivacy(brainDir)
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
				if factAnchorMatchesSession(anchor, sessionID, transcriptRels) {
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
		proposals, perr := loadFactProposals(brainDir, branch)
		if perr != nil {
			return deletedTotal, strippedTotal, perr
		}
		if len(proposals) > 0 {
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
// A failed write propagates.
func purgeDistillCacheEntries(brainDir, sessionID string) error {
	// Read with the SAME checked loader post-cleanup verification uses. The
	// legacy loadDistillCache swallows every read/parse/version failure and
	// returns an empty cache, so a version-mismatched or unreadable file made
	// this purge silently do nothing and then fail moments later inside
	// verifySessionPrivacy, after the tombstone and all deletions were already
	// committed. Because verify enumerates every tombstoned session, that left
	// the brain in a state where all later exclude/purge/retention runs failed
	// too, blaming verification rather than the stale cache. Every other loader
	// in this cleanup path was converted to the checked form; this one was
	// missed.
	cache, err := loadDistillCacheForPrivacy(brainDir)
	if err != nil {
		return err
	}
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

// --- durable privacy transaction record ---
//
// Every exclude/purge/retention cleanup writes a content-free, versioned
// transaction file with per-stage durable states, so an interrupted or
// failed operation is visible afterward instead of inferred from partial
// artifacts. States: requested (plan computed), guarded (tombstone active),
// rebuilding (deletions and rebuild running), verified (post-cleanup
// verification passed), complete, or error (bounded redacted message; the
// operation is idempotent and re-runnable).

const (
	privacyTransactionVersion       = 2
	privacyTransactionLegacyVersion = 1
	privacyTransactionSessionRefMax = 256

	privacyStateRequested  = "requested"
	privacyStateGuarded    = "guarded"
	privacyStateRebuilding = "rebuilding"
	privacyStateVerified   = "verified"
	privacyStateComplete   = "complete"
	privacyStateError      = "error"

	privacyTransactionErrorMaxBytes = 512
)

type privacyTransaction struct {
	SchemaVersion int       `json:"schema_version"`
	SessionID     string    `json:"session_id"`
	Operation     string    `json:"operation"` // exclude | purge
	State         string    `json:"state"`
	StartedAt     time.Time `json:"started_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Artifacts carries identities and sizes only, never content.
	Artifacts   []purgeArtifact `json:"artifacts,omitempty"`
	SessionRefs []string        `json:"session_refs,omitempty"`
	Error       string          `json:"error,omitempty"`
}

func privacyTransactionRel(sessionID string) string {
	return filepath.ToSlash(filepath.Join(historyDirName, "privacy", url.PathEscape(sessionID)+".json"))
}

func writePrivacyTransaction(brainDir string, tx privacyTransaction, now time.Time) error {
	if _, _, err := loadPrivacyTransactionChecked(brainDir, tx.SessionID); err != nil {
		return err
	}
	tx.SchemaVersion = privacyTransactionVersion
	tx.UpdatedAt = now
	data, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	if _, err := decodePrivacyTransaction(data, tx.SessionID); err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, privacyTransactionRel(tx.SessionID), append(data, '\n'), 0o600)
}

func loadPrivacyTransaction(brainDir, sessionID string) (privacyTransaction, bool) {
	tx, present, err := loadPrivacyTransactionChecked(brainDir, sessionID)
	if err != nil {
		return privacyTransaction{}, false
	}
	return tx, present
}

func removePrivacyTransaction(brainDir, sessionID string) error {
	return removeCheckedMemoryStateFile(brainDir, privacyTransactionRel(sessionID), "privacy transaction", maxManifestBytes, func(data []byte) error {
		_, err := decodePrivacyTransaction(data, sessionID)
		return err
	})
}

// removePrivacyTransactionForInclude adds the directory durability barrier
// required by include's two-file rollback protocol. If syncing fails after the
// unlink, include treats the outcome as ambiguous and restores both exact files.
func removePrivacyTransactionForInclude(brainDir, sessionID string, present bool) error {
	if !present {
		return removePrivacyTransaction(brainDir, sessionID)
	}
	clean, err := cleanBrainRelativePath(privacyTransactionRel(sessionID))
	if err != nil {
		return err
	}
	path := filepath.Join(brainDir, clean)
	pinnedParent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("pin privacy transaction directory: %w", err)
	}
	defer pinnedParent.Close()
	if err := removePrivacyTransaction(brainDir, sessionID); err != nil {
		return err
	}
	if err := beforeIncludePrivacyTransactionDirSync(path); err != nil {
		return fmt.Errorf("sync removed privacy transaction directory: %w", err)
	}
	if err := syncPinnedDirectory(pinnedParent); err != nil {
		return fmt.Errorf("sync removed privacy transaction directory: %w", err)
	}
	return nil
}

// executeSessionPurge applies the plan under the already-held write lock:
// tombstone first (so a crash mid-purge can only leave the session excluded,
// never resurrected), then physical deletion, then a clean rebuild of the
// history projection from the surviving truth.
func executeSessionPurge(brainDir, sessionID string, plan sessionPurgePlan, now time.Time) error {
	return executeSessionCleanup(brainDir, sessionID, plan, now, "purged", false)
}

// beforeSessionAbstractPrivacyCleanup is a deterministic crash/failure seam
// after the tombstone and durable scope exist but before abstract artifacts are
// removed. Production leaves it as a no-op.
var beforeSessionAbstractPrivacyCleanup = func() error { return nil }

// executeSessionCleanup is the shared exclude/purge executor: tombstone,
// transcript deletion (purge only), derived-store deletion, fact/episode/
// cache filtering, then the rebuild. Every deletion error propagates;
// the tombstone-first order keeps a failed run resumable and the session
// unreadable in the meantime. Progress is a durable transaction record:
// each stage transition is persisted before the stage runs, and a
// failure leaves an error state naming what stopped it.
func executeSessionCleanup(brainDir, sessionID string, plan sessionPurgePlan, now time.Time, reason string, keepTranscripts bool) (err error) {
	// Operational publication state is classified before the transaction or
	// tombstone changes. A newer/corrupt/unsafe marker is owned by a producer we
	// cannot safely supersede, so cleanup must preserve both it and its candidate.
	if _, _, _, markerErr := loadPatternCorpusPublicationForPrivacy(brainDir); markerErr != nil {
		return markerErr
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return err
	}
	operation := "purge"
	if keepTranscripts {
		operation = "exclude"
	}
	tx := privacyTransaction{
		SessionID: sessionID, Operation: operation,
		State: privacyStateRequested, StartedAt: now,
		Artifacts:   append(append(append([]purgeArtifact(nil), plan.Transcripts...), plan.DerivedStores...), plan.WorkMetadata...),
		SessionRefs: append([]string(nil), plan.SessionRefs...),
	}
	if txErr := writePrivacyTransaction(brainDir, tx, now); txErr != nil {
		return txErr
	}
	defer func() {
		if err == nil {
			return
		}
		// Best-effort durable failure state; the command error is authoritative.
		tx.State = privacyStateError
		message := err.Error()
		if len(message) > privacyTransactionErrorMaxBytes {
			message = message[:privacyTransactionErrorMaxBytes]
		}
		tx.Error = message
		_ = writePrivacyTransaction(brainDir, tx, now)
	}()
	advance := func(state string) error {
		tx.State = state
		tx.Error = ""
		return writePrivacyTransaction(brainDir, tx, now)
	}
	stones.Excluded[sessionID] = sessionTombstone{At: now, Reason: reason}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		return err
	}
	if err := advance(privacyStateGuarded); err != nil {
		return err
	}
	if err := advance(privacyStateRebuilding); err != nil {
		return err
	}
	// Remove a supported marker first, with a fresh classification and exact
	// identity check. This prevents a stale plan from deleting its candidate
	// before discovering that a forward-version marker took ownership.
	if err := removePatternCorpusPublicationForPrivacy(brainDir); err != nil {
		return fmt.Errorf("delete pattern corpus publication marker: %w", err)
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
		if artifact.Path == patternCorpusPublicationMarkerPath {
			continue // classified and identity-checked above
		}
		if err := removePrivacyDerivedStoreArtifact(brainDir, artifact.Path); err != nil {
			return fmt.Errorf("delete derived store %s: %w", artifact.Path, err)
		}
	}
	transcriptRels := map[string]bool{}
	for _, artifact := range plan.Transcripts {
		transcriptRels[normalizePrivacyTranscriptPath(artifact.Path)] = true
	}
	if _, _, err := purgeSessionFacts(brainDir, sessionID, transcriptRels); err != nil {
		return err
	}
	if _, _, err := filterEpisodesFile(brainDir, sessionID, transcriptRels, true); err != nil {
		return err
	}
	if err := purgeDistillCacheEntries(brainDir, sessionID); err != nil {
		return err
	}
	cleanupManifest, manifestErr := loadBrainManifest(brainDir)
	if manifestErr != nil {
		return manifestErr
	}
	refs := sessionRefsForSessionID(brainDir, cleanupManifest, sessionID)
	for _, ref := range plan.SessionRefs {
		refs[ref] = true
	}
	if err := beforeSessionAbstractPrivacyCleanup(); err != nil {
		return err
	}
	if err := purgeSessionAbstractsForRefs(brainDir, refs); err != nil {
		return err
	}
	if err := purgeAbstractEgressReceiptsForSessionRefs(brainDir, refs); err != nil {
		return err
	}
	for _, artifact := range plan.WorkMetadata {
		if isAbstractEgressReceiptPath(artifact.Path) {
			continue // re-derived, validated, and removed above under this lock
		}
		if err := removeBrainRelativeFile(brainDir, artifact.Path); err != nil {
			return fmt.Errorf("delete memory work metadata %s: %w", artifact.Path, err)
		}
	}
	if err := purgeMemoryWorkForSessionLocked(brainDir, sessionID); err != nil {
		return err
	}
	if _, err := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil); err != nil {
		return err
	}
	// Success is published only after verification passes: the same
	// checks `privacy verify` runs must find nothing for ANY tombstoned
	// session, so a partially cleaned earlier failure also blocks this one.
	report, verifyErr := verifySessionPrivacy(brainDir)
	if verifyErr != nil {
		return fmt.Errorf("post-cleanup verification: %w", verifyErr)
	}
	if !report.Clean {
		return fmt.Errorf("cleanup incomplete: verification found %d violations (first: %s %s); the operation is idempotent, re-run it after resolving the artifact", len(report.Findings), report.Findings[0].Artifact, report.Findings[0].Detail)
	}
	if err := advance(privacyStateVerified); err != nil {
		return err
	}
	return advance(privacyStateComplete)
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

// removePrivacyDerivedStoreArtifact deletes only Brain-relative, classified
// leaves. Ordinary derived stores retain the historical WAL/SHM expansion;
// the pattern corpus instead uses the publication layer's one canonical
// sidecar set, including its rollback journal. Missing files are fine, while
// unsafe path components and every other failure propagate.
func removePrivacyDerivedStoreArtifact(brainDir, rel string) error {
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return fmt.Errorf("%s: derived privacy store has an unsafe path %q: %w", memoryErrStateUnsafe, rel, err)
	}
	rel = filepath.ToSlash(clean)
	if rel == patternCorpusPublicationMarkerPath {
		return removePatternCorpusPublicationForPrivacy(brainDir)
	}
	var candidates []string
	switch {
	case rel == patternCorpusPath:
		candidates = patternCorpusPrivacyArtifactRels()
	case isPatternCorpusPrivacyArtifactRel(rel), isPatternCorpusStagingArtifactRel(rel):
		candidates = []string{rel}
	default:
		candidates = []string{rel, rel + "-wal", rel + "-shm"}
	}
	for _, candidate := range candidates {
		info, present, err := privacyArtifactInfo(brainDir, candidate, "derived privacy store")
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if err := removeBrainRelativeFileExpected(brainDir, candidate, info); err != nil {
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
