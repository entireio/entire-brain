package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/entities"
	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/spf13/cobra"
)

var (
	privacyWalkDir = filepath.WalkDir
)

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

// Session tombstone state lives in brainstore; these aliases keep the
// package-local names for the many existing readers and tests.
type (
	sessionTombstones         = brainstore.SessionTombstones
	sessionTombstone          = brainstore.SessionTombstone
	sessionTombstoneState     = brainstore.SessionTombstoneState
	sessionTombstoneFileState = brainstore.SessionTombstoneFileState
	sessionTombstoneLoadError = brainstore.SessionTombstoneLoadError
)

// --- administration commands ---

// newPrivacyCommand is the session-privacy administration surface. Named
// `privacy` (not `sessions`) deliberately: `sessions` is the refresh build
// stage, and the root help contract keeps that name off the top level.
func newPrivacyCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "privacy",
		Short: "Control which captured sessions the brain retains",
		Long:  "List, exclude, include, and purge captured sessions from the brain's projections",
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
	repoDir, local, err := clibase.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
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
	// Unmatched marks a tombstone that no captured session in this brain
	// answers to -- either armed pre-emptively before capture, or left behind
	// by a session a later refresh dropped. It is a real, active exclusion
	// either way, so it is still listed; it is just not evidence that anything
	// was ever removed, and rendering it identically to a matched exclusion is
	// what let a typo look like a completed cleanup forever.
	Unmatched bool `json:"unmatched,omitempty"`
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
			manifest, err := bmanifest.Load(brainDir)
			if err != nil {
				return err
			}
			stones, _, err := brainstore.LoadSessionTombstonesChecked(brainDir)
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
					entries = append(entries, sessionListEntry{SessionID: id, Excluded: true, Unmatched: true, ExcludedAt: stone.At.UTC().Format(time.RFC3339), Reason: stone.Reason})
				}
			}
			if jsonOut {
				if entries == nil {
					entries = []sessionListEntry{}
				}
				return cmdutil.WriteJSON(cmd, map[string]any{"sessions": entries})
			}
			return cmdutil.WriteText(cmd, func(out io.Writer) {
				// An empty session list is the normal state of a brain that has
				// never captured a session, and printing nothing for it made the
				// one command a privacy-conscious user reaches for indistinguishable
				// from a command that failed to look. Say which it is.
				if len(entries) == 0 {
					fmt.Fprintln(out, "no captured sessions in this brain")
					return
				}
				unmatched := 0
				for _, entry := range entries {
					state := "included"
					switch {
					case entry.Excluded && entry.Unmatched:
						// Distinct from EXCLUDED on purpose. The row is the
						// only place a user can notice that the id they typed
						// matched nothing -- every other column is <unset>,
						// which reads as missing metadata rather than as a
						// tombstone waiting for a session that may never exist.
						state = "UNMATCHED"
						unmatched++
					case entry.Excluded:
						state = "EXCLUDED"
					}
					fmt.Fprintf(out, "%-10s %s  %s  %s  %s\n", state, cmdutil.ValueOrUnset(entry.SessionID), cmdutil.ValueOrUnset(entry.CreatedAt), cmdutil.ValueOrUnset(entry.Agent), entry.TranscriptPath)
				}
				if unmatched > 0 {
					fmt.Fprintf(out, "UNMATCHED: %d tombstone(s) match no captured session in this brain; each stays recorded and applies if such a session is captured later\n", unmatched)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// excludedTranscriptPathsChecked extends the manifest with durable transaction
// paths so refresh cannot reopen or reindex an excluded export it no longer names.
func excludedTranscriptPathsChecked(brainDir string, manifest *exportManifest, stones sessionTombstones) (map[string]string, error) {
	return brainstore.DurableExcludedTranscriptPaths(brainDir, stones, bmanifest.ExcludedTranscriptPaths(manifest, stones))
}

// The session read guard lives in brainstore; the alias keeps the
// package-local name for the many existing readers and tests.
type sessionReadGuard = brainstore.SessionReadGuard

func filterTombstonedSessions(brainDir string, sessions []exportSession) ([]exportSession, error) {
	manifest := &exportManifest{Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: sessions}}}
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return nil, err
	}
	return bmanifest.GuardSessions(guard, sessions), nil
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
	var plan sessionPurgePlan
	matched := false
	if err := brainstore.WithBrainPrivacySideEffectLock(brainDir, func() error {
		return brainstore.WithBrainWriteLock(brainDir, func() error {
			// Exclusion is purge minus the transcript: the tombstone guards
			// reads immediately, then every derived projection the session fed --
			// index records, facts, episodes, pattern outputs, caches, FTS and
			// vector stores -- is removed or rebuilt from the surviving truth.
			// Errors propagate; a partial cleanup is a failure, not a success.
			var planErr error
			plan, planErr = buildSessionPurgePlan(brainDir, sessionID)
			if planErr != nil {
				return planErr
			}
			manifest, manifestErr := bmanifest.Load(brainDir)
			if manifestErr != nil {
				return manifestErr
			}
			matched = sessionExclusionMatchedSomething(manifest, plan, sessionID)
			return executeSessionCleanup(brainDir, sessionID, plan, opts.Now().UTC(), strings.TrimSpace(reason), true)
		})
	}); err != nil {
		return err
	}
	if jsonOut {
		return cmdutil.WriteJSON(cmd, map[string]any{
			"excluded": sessionID,
			// matched says whether anything in this brain answered to the id.
			// A false here is not a failure: the tombstone is recorded and
			// will apply to a session captured later. It is the difference
			// between "cleaned up" and "armed", which the caller cannot infer
			// from the exit code.
			"matched":               matched,
			"records":               plan.Records,
			"facts_deleted":         plan.FactsDeleted,
			"fact_anchors_stripped": plan.FactAnchorsStripped,
			"episodes":              plan.Episodes,
			"transcripts":           len(plan.Transcripts),
		})
	}
	return cmdutil.WriteText(cmd, func(out io.Writer) {
		// Report what happened, not what the command is for. Excluding an id no
		// session in this brain answers to is a legitimate PRE-EMPTIVE privacy
		// operation -- arming the tombstone before capture is strictly safer
		// than refusing it and losing the race -- so the operation stays
		// permitted and keeps exiting 0. What was wrong was the sentence: it
		// claimed to have "removed or rebuilt its derived projections" for a
		// session that had produced none, which is the same class of lie as
		// under-reporting a real cleanup. `purge` already reports its real
		// inventory on the same plan; exclude now does too.
		if !matched {
			fmt.Fprintf(out, "excluded %s: tombstone recorded, nothing to remove or rebuild\n", sessionID)
			fmt.Fprintln(out, "  no captured session in this brain matches this id")
			fmt.Fprintln(out, "  the tombstone stays recorded and applies if such a session is captured later")
			return
		}
		fmt.Fprintf(out, "excluded %s and removed or rebuilt its derived projections:\n", sessionID)
		fmt.Fprintf(out, "  %d derived index records\n", plan.Records)
		if plan.FactsDeleted > 0 || plan.FactAnchorsStripped > 0 {
			fmt.Fprintf(out, "  %d single-source memories deleted, %d anchors stripped from corroborated memories\n", plan.FactsDeleted, plan.FactAnchorsStripped)
		}
		if plan.Episodes > 0 {
			fmt.Fprintf(out, "  %d pattern episodes\n", plan.Episodes)
		}
		for _, artifact := range plan.DerivedStores {
			fmt.Fprintf(out, "  derived store %s (%d bytes, rebuilds from surviving truth)\n", artifact.Path, artifact.Bytes)
		}
	})
}

// sessionExclusionMatchedSomething reports whether anything in this brain
// actually answers to the excluded or purged id.
//
// The evidence is deliberately drawn from the manifest and from the plan's
// manifest-derived counts only -- never from the privacy transaction record or
// the wholesale derived-store inventory. Both of those are non-empty for an id
// this brain has never seen: the transaction is written by the exclusion
// itself, and the rebuildable stores (scan caches, FTS, vectors) are dropped
// unconditionally. Counting either would make a repeated exclusion of the same
// phantom report itself as a real cleanup on the second run.
func sessionExclusionMatchedSomething(manifest *exportManifest, plan sessionPurgePlan, sessionID string) bool {
	if len(plan.Transcripts) > 0 || plan.Records > 0 || plan.FactsDeleted > 0 || plan.FactAnchorsStripped > 0 || plan.Episodes > 0 {
		return true
	}
	return manifestHasCapturedSession(manifest, sessionID)
}

// manifestHasCapturedSession reports whether the brain manifest lists this
// session id as captured. A session with no transcript path and no derived
// records still counts: it was captured, so excluding it is not pre-emptive.
func manifestHasCapturedSession(manifest *exportManifest, sessionID string) bool {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return false
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		if strings.TrimSpace(session.SessionID) == sessionID {
			return true
		}
	}
	return false
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
			if err := brainstore.WithBrainPrivacySideEffectLock(brainDir, func() error {
				return brainstore.WithBrainWriteLock(brainDir, func() error {
					return includeSessionLocked(brainDir, sessionID, opts.Now().UTC())
				})
			}); err != nil {
				return err
			}
			if jsonOut {
				return cmdutil.WriteJSON(cmd, map[string]any{"included": sessionID})
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
	stones, tombstoneState, err := brainstore.LoadSessionTombstonesChecked(brainDir)
	if err != nil {
		return err
	}
	if _, ok := stones.Excluded[sessionID]; !ok {
		return fmt.Errorf("session %s is not excluded", sessionID)
	}
	tombstonesBefore, present, err := brainstore.ReadPrivacyArtifact(brainDir, brainstore.SessionTombstonesPath, "session tombstones", clibase.MaxManifestBytes)
	if err != nil {
		return err
	}
	if !present || privacyBytesIdentity(tombstonesBefore) != tombstoneState.Identity {
		return fmt.Errorf("%s: session tombstones changed before include", brainstore.StateUnsafeCode)
	}
	_, transactionPresent, err := brainstore.LoadPrivacyTransactionChecked(brainDir, sessionID)
	if err != nil {
		return err
	}
	var transactionBefore []byte
	transactionRel := brainstore.PrivacyTransactionRel(sessionID)
	if transactionPresent {
		transactionBefore, present, err = brainstore.ReadPrivacyArtifact(brainDir, transactionRel, "privacy transaction", clibase.MaxManifestBytes)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("%s: privacy transaction disappeared before include", brainstore.StateUnsafeCode)
		}
		if _, err := brainstore.DecodePrivacyTransaction(transactionBefore, sessionID); err != nil {
			return err
		}
	}

	delete(stones.Excluded, sessionID)
	tombstonesAfter, err := brainstore.MarshalSessionTombstones(stones)
	if err != nil {
		return err
	}
	rollback := includePrivacyRollback{
		tombstonesBefore: tombstonesBefore, tombstonesAfter: tombstonesAfter,
		transactionRel: transactionRel, transactionBefore: transactionBefore, transactionPresent: transactionPresent,
	}
	if err := brainstore.SaveSessionTombstones(brainDir, stones); err != nil {
		// Atomic replacement can report a durability failure after rename made
		// the included policy visible. The result is therefore ambiguous: always
		// inspect and restore the exact prior state before releasing either lock.
		return errors.Join(err, restoreIncludePrivacyState(brainDir, rollback))
	}
	currentTombstones, present, err := brainstore.ReadPrivacyArtifact(brainDir, brainstore.SessionTombstonesPath, "included session tombstones", clibase.MaxManifestBytes)
	if err != nil || !present || !bytes.Equal(currentTombstones, tombstonesAfter) {
		if err == nil {
			err = fmt.Errorf("%s: session tombstones changed while including %s", brainstore.StateUnsafeCode, sessionID)
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
	// Cleanup stripped this session's id out of the derived entity join. The
	// join is recomputed from the durable git-meta store, which the exclusion
	// never touched, so the linkage comes back the moment the join is rebuilt —
	// but the cache is pinned to the git-meta tip, which an include does not
	// move, so without this the re-included session would stay missing from
	// `entities history` indefinitely. Unpinning it is best-effort on purpose:
	// a failed rebuild falls back to serving this same (still privacy-clean)
	// cache, so nothing is lost and include must not fail over an accelerator.
	invalidateEntityIndexJoin(brainDir)
	return nil
}

// invalidateEntityIndexJoin unpins the derived entity index from the git-meta
// tip so the next read recomputes it. It deliberately clears the pin rather
// than deleting the file: the rebuild needs the Graph provider, and on a
// machine without it a deleted cache could never be recreated, while an
// unpinned one is still served.
func invalidateEntityIndexJoin(brainDir string) {
	cache, present, err := loadPrivacyEntityIndex(brainDir)
	if err != nil || !present || cache.MetaTip == "" {
		return
	}
	cache.MetaTip = ""
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	_ = brainstore.WriteBrainRelativeFileAtomic(brainDir, entities.CachePath, append(data, '\n'), 0o600)
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
	currentTombstones, present, err := brainstore.ReadPrivacyArtifact(brainDir, brainstore.SessionTombstonesPath, "session tombstones during include rollback", clibase.MaxManifestBytes)
	switch {
	case err != nil:
		restoreErr = errors.Join(restoreErr, err)
	case present && bytes.Equal(currentTombstones, rollback.tombstonesBefore):
		// A failed atomic replacement may already have left the original bytes.
	case !present || len(rollback.tombstonesAfter) == 0 || !bytes.Equal(currentTombstones, rollback.tombstonesAfter):
		restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: session tombstones changed during include rollback", brainstore.StateUnsafeCode))
	default:
		if err := restoreIncludePrivacyFile(brainDir, brainstore.SessionTombstonesPath, "session tombstones", rollback.tombstonesBefore); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore session tombstones after failed include: %w", err))
		}
	}

	currentTransaction, transactionPresent, err := brainstore.ReadPrivacyArtifact(brainDir, rollback.transactionRel, "privacy transaction during include rollback", clibase.MaxManifestBytes)
	switch {
	case err != nil:
		restoreErr = errors.Join(restoreErr, err)
	case !rollback.transactionPresent && !transactionPresent:
	case !rollback.transactionPresent:
		restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: privacy transaction appeared during include rollback", brainstore.StateUnsafeCode))
	case transactionPresent && bytes.Equal(currentTransaction, rollback.transactionBefore):
	case transactionPresent:
		restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: privacy transaction changed during include rollback", brainstore.StateUnsafeCode))
	default:
		if err := restoreIncludePrivacyFile(brainDir, rollback.transactionRel, "privacy transaction", rollback.transactionBefore); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore privacy transaction after failed include: %w", err))
		}
	}
	return errors.Join(restoreErr, verifyIncludePrivacyRollback(brainDir, rollback))
}

func verifyIncludePrivacyRollback(brainDir string, rollback includePrivacyRollback) error {
	var verifyErr error
	if err := requireExactIncludePrivacyFile(brainDir, brainstore.SessionTombstonesPath, "session tombstones", rollback.tombstonesBefore); err != nil {
		verifyErr = errors.Join(verifyErr, err)
	}
	current, present, err := brainstore.ReadPrivacyArtifact(brainDir, rollback.transactionRel, "privacy transaction after include rollback", clibase.MaxManifestBytes)
	switch {
	case err != nil:
		verifyErr = errors.Join(verifyErr, err)
	case rollback.transactionPresent && (!present || !bytes.Equal(current, rollback.transactionBefore)):
		verifyErr = errors.Join(verifyErr, fmt.Errorf("%s: exact privacy transaction rollback bytes are not live", brainstore.StateUnsafeCode))
	case !rollback.transactionPresent && present:
		verifyErr = errors.Join(verifyErr, fmt.Errorf("%s: unexpected privacy transaction is live after rollback", brainstore.StateUnsafeCode))
	}
	return verifyErr
}

// restoreIncludePrivacyFile resolves the only ambiguous atomic-write failure:
// rename succeeded but syncing the parent directory did not. It proves the
// exact rollback bytes are live, performs the missing durability barrier, and
// proves them again. Every other write failure remains an error; callers never
// infer successful rollback from an unclassified failure.
func restoreIncludePrivacyFile(brainDir, rel, label string, before []byte) error {
	err := brainstore.WriteBrainRelativeFileAtomic(brainDir, rel, before, 0o600)
	if err == nil {
		return nil
	}
	var durabilityErr *brainstore.AtomicReplaceDurabilityError
	if !errors.As(err, &durabilityErr) {
		return err
	}
	if exactErr := requireExactIncludePrivacyFile(brainDir, rel, label, before); exactErr != nil {
		return errors.Join(err, exactErr)
	}
	if rootErr := clibase.RejectSymlinkedBrainRoot(brainDir); rootErr != nil {
		return errors.Join(err, rootErr)
	}
	clean, cleanErr := clibase.CleanBrainRelativePath(rel)
	if cleanErr != nil {
		return errors.Join(err, cleanErr)
	}
	if syncErr := brainstore.SyncParentDir(filepath.Join(brainDir, clean)); syncErr != nil {
		return errors.Join(err, fmt.Errorf("sync restored %s directory: %w", label, syncErr))
	}
	if exactErr := requireExactIncludePrivacyFile(brainDir, rel, label, before); exactErr != nil {
		return errors.Join(err, exactErr)
	}
	return nil
}

func requireExactIncludePrivacyFile(brainDir, rel, label string, want []byte) error {
	current, present, err := brainstore.ReadPrivacyArtifact(brainDir, rel, label+" after include rollback", clibase.MaxManifestBytes)
	if err != nil {
		return err
	}
	if !present || !bytes.Equal(current, want) {
		return fmt.Errorf("%s: exact %s rollback bytes are not live", brainstore.StateUnsafeCode, label)
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
	// Matched says whether anything in this brain answered to the id, exactly
	// as exclude's JSON reports it. A false is not a failure: the tombstone is
	// still recorded and applies to a session captured later. The caller cannot
	// infer this from the counts alone -- a captured session can legitimately
	// have zero remaining artifacts.
	Matched bool `json:"matched"`
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
		return brainstore.WithBrainWriteLock(brainDir, func() error {
			var planErr error
			plan, planErr = buildSessionPurgePlan(brainDir, sessionID)
			if planErr != nil {
				return planErr
			}
			manifest, manifestErr := bmanifest.Load(brainDir)
			if manifestErr != nil {
				return manifestErr
			}
			plan.Matched = sessionExclusionMatchedSomething(manifest, plan, sessionID)
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
		apply = func() error { return brainstore.WithBrainPrivacySideEffectLock(brainDir, operation) }
	}
	if err := apply(); err != nil {
		return err
	}
	if jsonOut {
		return cmdutil.WriteJSON(cmd, plan)
	}
	return cmdutil.WriteText(cmd, func(out io.Writer) {
		// Same honesty rule as exclude: an id no captured session answers to
		// must not report as a cleanup. The tombstone still lands (pre-emptive
		// arming is strictly safer than refusing and losing the race), so the
		// operation stays permitted and keeps exiting 0, like exclude.
		if !plan.Matched {
			if plan.DryRun {
				fmt.Fprintf(out, "purge (dry-run) %s: nothing to remove or rebuild\n", plan.SessionID)
				fmt.Fprintln(out, "  no captured session in this brain matches this id")
				return
			}
			fmt.Fprintf(out, "purged %s: tombstone recorded, nothing to remove or rebuild\n", plan.SessionID)
			fmt.Fprintln(out, "  no captured session in this brain matches this id")
			fmt.Fprintln(out, "  the tombstone stays recorded and applies if such a session is captured later")
			return
		}
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
			fmt.Fprintf(out, "  %d single-source memories deleted, %d anchors stripped from corroborated memories\n", plan.FactsDeleted, plan.FactAnchorsStripped)
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
	entries, present, err := brainstore.ReadPrivacyDirectory(brainDir, factstore.DirName, "facts directory")
	if err != nil || !present {
		if err != nil {
			return nil, err
		}
		return map[string][]factRecord{}, nil
	}
	byBranch := map[string][]factRecord{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: facts inventory contains a symlink: %s", brainstore.StateUnsafeCode, entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("%s: inspect facts entry %s: %w", brainstore.StateCorruptCode, entry.Name(), err)
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: facts inventory contains a nonregular entry: %s", brainstore.StateUnsafeCode, entry.Name())
			}
			continue // taxonomy and other root-level metadata are not fact stores.
		}
		rel := filepath.ToSlash(filepath.Join(factstore.DirName, entry.Name(), factstore.FileName))
		data, exists, err := brainstore.ReadPrivacyArtifact(brainDir, rel, "fact store", 256<<20)
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
					return nil, fmt.Errorf("%s: parse %s line %d: %w", brainstore.StateCorruptCode, rel, lineNumber+1, err)
				}
				byBranch[record.Branch] = append(byBranch[record.Branch], record)
			}
		}
		proposalRel := filepath.ToSlash(filepath.Join(factstore.DirName, entry.Name(), factsProposalsFileName))
		proposalData, proposalExists, err := brainstore.ReadPrivacyArtifact(brainDir, proposalRel, "fact proposal store", 64<<20)
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
					return nil, fmt.Errorf("%s: parse %s line %d: %w", brainstore.StateCorruptCode, proposalRel, lineNumber+1, err)
				}
			}
		}
	}
	// Inspect physical snapshot bodies, not the reconciled view. Old clients
	// can delete a legacy record while leaving its snapshot body behind.
	stored, err := factstore.StoredRepositoryRecords(brainDir)
	if err != nil {
		return nil, err
	}

	if len(stored) != 0 {
		seen := map[string]bool{}
		for _, records := range byBranch {
			for _, rec := range records {
				data, err := json.Marshal(rec)
				if err != nil {
					return nil, err
				}
				seen[string(data)] = true
			}
		}
		for _, rec := range stored {
			data, err := json.Marshal(rec)
			if err != nil {
				return nil, err
			}
			if !seen[string(data)] {
				byBranch[rec.Branch] = append(byBranch[rec.Branch], rec)
				seen[string(data)] = true
			}
		}
	}

	return byBranch, nil
}

func factAnchorMatchesSession(anchor factAnchor, sessionID string, transcriptRels map[string]bool) bool {
	if strings.TrimSpace(anchor.SessionID) == strings.TrimSpace(sessionID) {
		return true
	}
	rel := brainstore.NormalizePrivacyTranscriptPath(anchor.Transcript)
	return rel != "" && transcriptRels[rel]
}

func buildSessionPurgePlan(brainDir, sessionID string) (sessionPurgePlan, error) {
	plan := sessionPurgePlan{SessionID: sessionID}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		return plan, err
	}
	refSet := sessionRefsForSessionID(brainDir, manifest, sessionID)
	priorTx, priorPresent, err := brainstore.LoadPrivacyTransactionChecked(brainDir, sessionID)
	if err != nil {
		return plan, err
	}
	if priorPresent {
		for _, ref := range priorTx.SessionRefs {
			refSet[ref] = true
		}
	}
	transcriptRels := map[string]bool{}
	priorPaths, err := brainstore.PrivacyTransactionTranscriptPaths(priorTx)
	if err != nil {
		return plan, err
	}
	for _, rel := range priorPaths {
		transcriptRels[rel] = true
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		for _, session := range manifest.Sources.Sessions.Sessions {
			if strings.TrimSpace(session.SessionID) == sessionID {
				if rel := brainstore.NormalizePrivacyTranscriptPath(session.TranscriptPath); rel != "" {
					transcriptRels[rel] = true
				}
			}
		}
	}
	// Keep missing paths in the durable scope too: a later re-export must
	// remain detectable even after the manifest has forgotten this session.
	for rel := range transcriptRels {
		artifact := purgeArtifact{Path: rel}
		info, present, statErr := brainstore.PrivacyArtifactInfo(brainDir, rel, "session transcript")
		if statErr != nil {
			return plan, statErr
		}
		if present {
			artifact.Bytes = info.Size()
		}
		plan.Transcripts = append(plan.Transcripts, artifact)
	}
	sort.Slice(plan.Transcripts, func(i, j int) bool { return plan.Transcripts[i].Path < plan.Transcripts[j].Path })
	if manifest.Sources != nil && manifest.Sources.History != nil {
		// A declared-but-unreadable index is a real storage problem: failing
		// here beats an under-reported plan that execution would then trust
		// (dry-run and execution inventories must match).
		index, ierr := historystore.LoadIndex(brainDir, manifest.Sources.History)
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
		info, present, statErr := brainstore.PrivacyArtifactInfo(brainDir, rel, "derived privacy store")
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
		info, present, statErr := brainstore.PrivacyArtifactInfo(brainDir, rel, "memory work metadata")
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
			if !strings.HasPrefix(ref, brainstore.ConversationSessionIDPrefix) {
				return plan, fmt.Errorf("%s: abstract job has invalid session ref", brainstore.StateCorruptCode)
			}
			refSet[ref] = true
		}
		for _, rel := range memoryJobSourcePaths(job) {
			if err := addWork(rel); err != nil {
				return plan, err
			}
		}
		cancellationRel := memoryCancellationRel(job.JobID)
		_, present, statErr := brainstore.PrivacyArtifactInfo(brainDir, cancellationRel, "memory cancellation marker")
		if statErr != nil {
			return plan, statErr
		}
		if present {
			if err := addWork(cancellationRel); err != nil {
				return plan, err
			}
		}
	}
	if len(refSet) > brainstore.PrivacyTransactionSessionRefMax {
		return plan, fmt.Errorf("%s: privacy scope exceeds %d canonical session refs", brainstore.QueryTooBroadCode, brainstore.PrivacyTransactionSessionRefMax)
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
		filepath.ToSlash(filepath.Join(brainstore.HistoryDirName, historyScanCacheFileName)),
		historyShortTermPath,
		filepath.ToSlash(filepath.Join(brainstore.HistoryDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(brainstore.HistoryDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
		patternsTasksPath,
		patterncore.ProceduresPath,
		patterncore.PracticesPath,
		patterncore.RunsRelPath,
	}
	rels = append(rels, patterncore.CorpusPrivacyArtifactRels()...)
	rels = append(rels, patterncore.CorpusPublicationMarkerPath)
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
	publication, _, publicationPresent, err := patterncore.LoadCorpusPublicationForPrivacy(brainDir)
	if err != nil {
		return nil, err
	}
	if publicationPresent {
		// Inventory the marker-authorized candidate even if it disappears before
		// the directory scan. A current marker is valid operational state, but
		// both leaves are transient/content-bearing and privacy cleanup owns them.
		rels = append(rels, filepath.ToSlash(filepath.Join("patterns", publication.Candidate)))
	}
	manifest, err := bmanifest.Load(brainDir)
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
	}{{historystore.IndexPath, activeIndex}, {projectionStateRel, activeReceipt}} {
		if legacy.path == legacy.active {
			continue
		}
		info, statErr := brainstore.PrivacyLstat(filepath.Join(brainDir, filepath.FromSlash(legacy.path)))
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
	for _, rootRel := range []string{historystore.GenerationsDir, historystore.StagingDir} {
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
				if historystore.ValidGenerationArtifactPath(rel, historystore.IndexFileName) || historystore.ValidGenerationArtifactPath(rel, projectionStateFileName) ||
					historystore.ValidStagingArtifactPath(rel, historystore.IndexFileName) || historystore.ValidStagingArtifactPath(rel, projectionStateFileName) {
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
	patternEntries, patternsPresent, err := brainstore.ReadPrivacyDirectory(brainDir, "patterns", "pattern directory")
	if err != nil {
		return nil, err
	}
	if patternsPresent {
		for _, entry := range patternEntries {
			if !strings.HasPrefix(entry.Name(), patterncore.CorpusStagingPrefix) {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s: pattern staging inventory contains a symlink: %s", brainstore.StateUnsafeCode, entry.Name())
			}
			info, infoErr := entry.Info()
			if infoErr != nil {
				return nil, fmt.Errorf("%s: inspect pattern staging artifact %s: %w", brainstore.StateCorruptCode, entry.Name(), infoErr)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: pattern staging inventory contains a nonregular artifact: %s", brainstore.StateUnsafeCode, entry.Name())
			}
			rels = append(rels, filepath.ToSlash(filepath.Join("patterns", entry.Name())))
		}
	}
	// Discover fact-vector caches by directory entries, never by Glob: branch
	// names may contain glob metacharacters, and Glob cannot report unreadable
	// directories or unsafe entry types. Only the two exact store basenames and
	// their SQLite sidecars are allowed in an embeddings directory.
	factEntries, factsPresent, err := brainstore.ReadPrivacyDirectory(brainDir, factstore.DirName, "facts directory")
	if err != nil {
		return nil, err
	}
	if factsPresent {
		for _, branchEntry := range factEntries {
			if branchEntry.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s: facts inventory contains a symlink: %s", brainstore.StateUnsafeCode, branchEntry.Name())
			}
			branchInfo, infoErr := branchEntry.Info()
			if infoErr != nil {
				return nil, fmt.Errorf("%s: inspect facts entry %s: %w", brainstore.StateCorruptCode, branchEntry.Name(), infoErr)
			}
			if !branchInfo.IsDir() {
				if !branchInfo.Mode().IsRegular() {
					return nil, fmt.Errorf("%s: facts inventory contains a nonregular entry: %s", brainstore.StateUnsafeCode, branchEntry.Name())
				}
				continue
			}
			embedRel := filepath.ToSlash(filepath.Join(factstore.DirName, branchEntry.Name(), embedStoreDirName))
			embedEntries, embedPresent, readErr := brainstore.ReadPrivacyDirectory(brainDir, embedRel, "fact embedding directory")
			if readErr != nil {
				return nil, readErr
			}
			if !embedPresent {
				continue
			}
			for _, entry := range embedEntries {
				if entry.Type()&os.ModeSymlink != 0 {
					return nil, fmt.Errorf("%s: fact embedding inventory contains a symlink: %s/%s", brainstore.StateUnsafeCode, embedRel, entry.Name())
				}
				info, infoErr := entry.Info()
				if infoErr != nil {
					return nil, fmt.Errorf("%s: inspect fact embedding %s/%s: %w", brainstore.StateCorruptCode, embedRel, entry.Name(), infoErr)
				}
				if !info.Mode().IsRegular() {
					return nil, fmt.Errorf("%s: fact embedding inventory contains a nonregular artifact: %s/%s", brainstore.StateUnsafeCode, embedRel, entry.Name())
				}
				base := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), "-wal"), "-shm")
				if base != embedStoreFileName && base != historyVecStoreFileNamePortable {
					return nil, fmt.Errorf("%s: unknown fact embedding artifact: %s/%s", brainstore.StateCorruptCode, embedRel, entry.Name())
				}
				rels = append(rels, filepath.ToSlash(filepath.Join(embedRel, base)))
			}
		}
	}
	withSiblings := make([]string, 0, len(rels)*3)
	seen := make(map[string]bool, len(rels)*3)
	for _, rel := range rels {
		artifacts := []string{rel, rel + "-wal", rel + "-shm"}
		if rel == patterncore.CorpusPublicationMarkerPath || patterncore.IsCorpusPrivacyArtifactRel(rel) || patterncore.IsCorpusStagingArtifactRel(rel) {
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
	data, present, err := brainstore.ReadPrivacyArtifact(brainDir, patternsEpisodesPath, "pattern episodes", 256<<20)
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
			return nil, true, fmt.Errorf("%s: parse %s line %d: %w", brainstore.StateCorruptCode, patternsEpisodesPath, lineNumber+1, err)
		}
		lines = append(lines, privacyEpisodeLine{raw: line, record: record})
	}
	return lines, true, nil
}

// loadPrivacyEntityIndex reads the derived entity -> commit join strictly, for
// privacy cleanup and verification.
//
// The ordinary reader (loadEntityIndexCache) treats every read or parse failure
// as "no cache" so a query can degrade to a rebuild. A privacy gate must not:
// an unreadable or unparseable index is state whose session attribution cannot
// be inspected, and treating it as empty would let cleanup claim a session was
// erased from a file it never managed to read.
func loadPrivacyEntityIndex(brainDir string) (entities.IndexCache, bool, error) {
	data, present, err := brainstore.ReadPrivacyArtifact(brainDir, entities.CachePath, "entity index", clibase.DefaultMaxReadBytes)
	if err != nil || !present {
		return entities.IndexCache{}, present, err
	}
	var cache entities.IndexCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return entities.IndexCache{}, true, fmt.Errorf("%s: parse %s: %w", brainstore.StateCorruptCode, entities.CachePath, err)
	}
	// A forward-version index may attribute sessions in a shape this binary
	// cannot see, so neither stripping nor verifying it can be trusted. Refuse,
	// the way every other forward-state gate here does, rather than reading it
	// as empty and calling that clean.
	if cache.SchemaVersion > entities.CacheVersion {
		return entities.IndexCache{}, true, fmt.Errorf("%s: %s schema %d is newer than this binary supports (%d)",
			brainstore.UnsupportedVersionCode, entities.CachePath, cache.SchemaVersion, entities.CacheVersion)
	}
	return cache, true, nil
}

// filterEntityIndexSessions strips tombstoned session ids from the derived
// entity index and reports how many occurrences it touched. With write=false it
// only counts.
//
// It strips the session linkage, not the occurrence: the commit and its subject
// are ordinary git history that privacy cleanup does not claim to rewrite, and
// the checkpoint id is the capture identity that history carries in its trailer.
// The session id is the one field derived from the captured session, and it is
// what makes `entities history` answer "this symbol was changed by session X"
// for a session the user excluded.
func filterEntityIndexSessions(brainDir string, tombstoned map[string]sessionTombstone, write bool) (removed int, err error) {
	if len(tombstoned) == 0 {
		return 0, nil
	}
	cache, present, err := loadPrivacyEntityIndex(brainDir)
	if err != nil || !present {
		return 0, err
	}
	for key, occurrences := range cache.Entries {
		for i := range occurrences {
			kept := occurrences[i].SessionIDs[:0:0]
			for _, id := range occurrences[i].SessionIDs {
				if _, excluded := tombstoned[strings.TrimSpace(id)]; excluded {
					removed++
					continue
				}
				kept = append(kept, id)
			}
			occurrences[i].SessionIDs = kept
		}
		cache.Entries[key] = occurrences
	}
	if !write || removed == 0 {
		return removed, nil
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return removed, err
	}
	return removed, brainstore.WriteBrainRelativeFileAtomic(brainDir, entities.CachePath, append(data, '\n'), 0o600)
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
		if strings.TrimSpace(record.SessionID) == sessionID || transcriptRels[brainstore.NormalizePrivacyTranscriptPath(record.Source.Path)] {
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
	return kept, removed, brainstore.WriteBrainRelativeFileAtomic(brainDir, patternsEpisodesPath, []byte(out.String()), 0o600)
}

// purgeSessionFacts applies the fact rules: strip purged-session anchors,
// physically delete facts left with no provenance, and prune proposals that
// reference a deleted fact. Returns (deleted, strippedAnchors).
func purgeSessionFacts(brainDir, sessionID string, transcriptRels map[string]bool) (int, int, error) {
	_, err := loadAllFactBranchesForPrivacy(brainDir)
	if err != nil {
		return 0, 0, err
	}
	byView, err := factstore.LoadStoreFiles(brainDir)
	if err != nil {
		return 0, 0, err
	}
	deletedTotal, strippedTotal := 0, 0
	for dir, facts := range byView {
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
		if err := factstore.WriteStoreFile(brainDir, dir, kept); err != nil {
			return deletedTotal, strippedTotal, err
		}
		proposalRel := filepath.ToSlash(filepath.Join(factstore.DirName, dir, factsProposalsFileName))
		proposals, perr := loadFactProposalsAt(brainDir, proposalRel)
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
				if err := writeFactProposalsAt(brainDir, proposalRel, keptProposals); err != nil {
					return deletedTotal, strippedTotal, err
				}
			}
		}
	}
	// Persist reconciliation even when the old client already removed every
	// visible record. This removes hidden schema 1 bodies and abandoned temps.
	if err := factstore.MigrateRepository(brainDir); err != nil {
		return deletedTotal, strippedTotal, err
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
	if len(cache.Sessions) == 0 && len(cache.Chunks) == 0 {
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
	for key := range cache.Chunks {
		if strings.HasSuffix(key, suffix) {
			delete(cache.Chunks, key)
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

// Privacy transaction records live in brainstore; these aliases keep the
// package-local names for the existing flows and tests.
type (
	privacyTransaction = brainstore.PrivacyTransaction
	purgeArtifact      = brainstore.PurgeArtifact
)

const privacyTransactionErrorMaxBytes = 512

func removePrivacyTransaction(brainDir, sessionID string) error {
	return removeCheckedMemoryStateFile(brainDir, brainstore.PrivacyTransactionRel(sessionID), "privacy transaction", clibase.MaxManifestBytes, func(data []byte) error {
		_, err := brainstore.DecodePrivacyTransaction(data, sessionID)
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
	clean, err := clibase.CleanBrainRelativePath(brainstore.PrivacyTransactionRel(sessionID))
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
	if err := brainstore.SyncPinnedDirectory(pinnedParent); err != nil {
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
	if _, _, _, markerErr := patterncore.LoadCorpusPublicationForPrivacy(brainDir); markerErr != nil {
		return markerErr
	}
	stones, _, err := brainstore.LoadSessionTombstonesChecked(brainDir)
	if err != nil {
		return err
	}
	operation := "purge"
	if keepTranscripts {
		operation = "exclude"
	}
	tx := privacyTransaction{
		SessionID: sessionID, Operation: operation,
		State: brainstore.PrivacyStateRequested, StartedAt: now,
		Artifacts:   append(append(append([]purgeArtifact(nil), plan.Transcripts...), plan.DerivedStores...), plan.WorkMetadata...),
		SessionRefs: append([]string(nil), plan.SessionRefs...),
	}
	if txErr := brainstore.WritePrivacyTransaction(brainDir, tx, now); txErr != nil {
		return txErr
	}
	defer func() {
		if err == nil {
			return
		}
		// Best-effort durable failure state; the command error is authoritative.
		tx.State = brainstore.PrivacyStateError
		message := err.Error()
		if len(message) > privacyTransactionErrorMaxBytes {
			message = message[:privacyTransactionErrorMaxBytes]
		}
		tx.Error = message
		_ = brainstore.WritePrivacyTransaction(brainDir, tx, now)
	}()
	advance := func(state string) error {
		tx.State = state
		tx.Error = ""
		return brainstore.WritePrivacyTransaction(brainDir, tx, now)
	}
	stones.Excluded[sessionID] = sessionTombstone{At: now, Reason: reason}
	if err := brainstore.SaveSessionTombstones(brainDir, stones); err != nil {
		return err
	}
	if err := advance(brainstore.PrivacyStateGuarded); err != nil {
		return err
	}
	if err := advance(brainstore.PrivacyStateRebuilding); err != nil {
		return err
	}
	// Remove a supported marker first, with a fresh classification and exact
	// identity check. This prevents a stale plan from deleting its candidate
	// before discovering that a forward-version marker took ownership.
	if err := patterncore.RemoveCorpusPublicationForPrivacy(brainDir); err != nil {
		return fmt.Errorf("delete pattern corpus publication marker: %w", err)
	}
	if !keepTranscripts {
		for _, artifact := range plan.Transcripts {
			path := filepath.Join(brainDir, filepath.FromSlash(artifact.Path))
			if err := clibase.RejectSymlinkPathComponents(brainDir, filepath.FromSlash(artifact.Path)); err != nil {
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
		if artifact.Path == patterncore.CorpusPublicationMarkerPath {
			continue // classified and identity-checked above
		}
		if err := removePrivacyDerivedStoreArtifact(brainDir, artifact.Path); err != nil {
			return fmt.Errorf("delete derived store %s: %w", artifact.Path, err)
		}
	}
	transcriptRels := map[string]bool{}
	for _, artifact := range plan.Transcripts {
		transcriptRels[brainstore.NormalizePrivacyTranscriptPath(artifact.Path)] = true
	}
	deletedFacts, strippedAnchors, err := purgeSessionFacts(brainDir, sessionID, transcriptRels)
	if err != nil {
		return err
	}
	// Rebuild sources.facts from what survived. Every other fact mutation
	// (remember, review, promote, retract, gc, sync) resummarizes the manifest;
	// privacy cleanup deleted facts and did not, so the manifest kept declaring
	// facts this function had just removed. That is indistinguishable on disk
	// from the corruption the integrity cross-check exists to catch, and would
	// make a sanctioned purge report itself as data loss. This runs under the
	// caller's brain write lock.
	if deletedFacts > 0 || strippedAnchors > 0 {
		if err := updateFactSourceManifestLocked(brainDir, now); err != nil {
			return err
		}
	}
	if _, _, err := filterEpisodesFile(brainDir, sessionID, transcriptRels, true); err != nil {
		return err
	}
	// Strip the session linkage from the derived entity index. Sweeping against
	// the WHOLE tombstone set, not just this session, repairs an index rebuilt
	// by an `entities backfill` that ran between two cleanups; the builder now
	// filters too, so a later rebuild stays clean by construction.
	if _, err := filterEntityIndexSessions(brainDir, stones.Excluded, true); err != nil {
		return err
	}
	if err := purgeDistillCacheEntries(brainDir, sessionID); err != nil {
		return err
	}
	cleanupManifest, manifestErr := bmanifest.Load(brainDir)
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
		if err := brainstore.RemoveBrainRelativeFile(brainDir, artifact.Path); err != nil {
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
	if err := advance(brainstore.PrivacyStateVerified); err != nil {
		return err
	}
	return advance(brainstore.PrivacyStateComplete)
}

// purgeGitmetaSyncCaveats reports fact copies a purge can NOT clean: fact-set
// heads the retired local-gitmeta `facts sync` backend (COR-2236) left in the
// git-meta store under the plugin cache. Nothing syncs or merges them anymore,
// but the bytes persist, so a purge must not silently claim complete deletion.
// The store also holds the entity index, so it cannot simply be deleted here.
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
		"a git-meta store exists at %s; copies of purged facts synced by the retired `facts sync` local-gitmeta backend may remain there (nothing reads or merges them anymore, but the store has no deletion semantics)",
		gitDir,
	)}
}

// removePrivacyDerivedStoreArtifact deletes only Brain-relative, classified
// leaves. Ordinary derived stores retain the historical WAL/SHM expansion;
// the pattern corpus instead uses the publication layer's one canonical
// sidecar set, including its rollback journal. Missing files are fine, while
// unsafe path components and every other failure propagate.
func removePrivacyDerivedStoreArtifact(brainDir, rel string) error {
	clean, err := clibase.CleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return fmt.Errorf("%s: derived privacy store has an unsafe path %q: %w", brainstore.StateUnsafeCode, rel, err)
	}
	rel = filepath.ToSlash(clean)
	if rel == patterncore.CorpusPublicationMarkerPath {
		return patterncore.RemoveCorpusPublicationForPrivacy(brainDir)
	}
	var candidates []string
	switch {
	case rel == patterncore.CorpusPath:
		candidates = patterncore.CorpusPrivacyArtifactRels()
	case patterncore.IsCorpusPrivacyArtifactRel(rel), patterncore.IsCorpusStagingArtifactRel(rel):
		candidates = []string{rel}
	default:
		candidates = []string{rel, rel + "-wal", rel + "-shm"}
	}
	for _, candidate := range candidates {
		info, present, err := brainstore.PrivacyArtifactInfo(brainDir, candidate, "derived privacy store")
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if err := brainstore.RemoveBrainRelativeFileExpected(brainDir, candidate, info); err != nil {
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
