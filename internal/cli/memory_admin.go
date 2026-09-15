package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// memory_admin.go is the maintenance surface: repair (verify dependencies,
// smallest deterministic rebuild), rebuild (recreate disposable projections
// from canonical sessions), and migrate (upgrade derived schemas atomically,
// build-beside-then-switch, never delete first). Every mutation returns a
// versioned, content-free receipt with stable error codes.

// Stable error codes (maintenance taxonomy). An empty result never stands in for any
// of these states.
const (
	memoryErrStateCorrupt       = "memory_state_corrupt"
	memoryErrStateUnsafe        = "memory_state_unsafe"
	memoryErrLockBusy           = "memory_lock_busy"
	memoryErrSourceStale        = "memory_source_stale"
	memoryErrCancelled          = "memory_cancelled"
	memoryErrCancelTooLate      = "memory_cancel_too_late"
	memoryErrMigrationRequired  = "memory_migration_required"
	memoryErrUnsupportedVersion = "memory_unsupported_version"
	memoryErrQueryTooBroad      = "memory_query_too_broad"
	memoryErrInputTooLarge      = "memory_input_too_large"
	memoryErrPrivacyExcluded    = "memory_privacy_excluded"
	memoryErrPrivacyDirty       = "memory_privacy_dirty"
	memoryErrPrivacyBusy        = "memory_privacy_busy"
	memoryErrIdentityAmbiguous  = "memory_identity_ambiguous"
	memoryErrProviderUnavail    = "memory_provider_unavailable"
	memoryErrSessionInventory   = "memory_session_inventory_degraded"
	// memoryErrWorkerDegraded reports that the last worker pass completed its
	// projection lanes but could not export new sessions. It is a health issue,
	// not a crash: the Brain keeps answering from what it already has, while
	// nothing captured since that failure can reach it.
	memoryErrWorkerDegraded             = "memory_worker_degraded"
	memoryOperationReceiptSchemaVersion = 1
	memoryMigrationProgressRel          = memoryWorkDirRel + "/migration-progress-v1.json"
)

var rebuildMemoryProjection = func(brainDir string, now time.Time) error {
	prepared, err := prepareBrainHistoryProjection(brainDir, now, nil)
	if err != nil {
		return err
	}
	err = withBrainWriteLock(brainDir, func() error {
		_, err := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return err
	})
	if err == nil {
		finalizePreparedHistoryProjection(brainDir, prepared)
	}
	return err
}

// memoryOperationReceipt is the versioned mutation receipt: operation
// identity, per-artifact prior/new states, stable codes; never content.
type memoryOperationReceipt struct {
	SchemaVersion int                     `json:"schema_version"`
	OperationID   string                  `json:"operation_id"`
	Operation     string                  `json:"operation"`
	DryRun        bool                    `json:"dry_run,omitempty"`
	StartedAt     time.Time               `json:"started_at"`
	FinishedAt    time.Time               `json:"finished_at"`
	Artifacts     []memoryReceiptArtifact `json:"artifacts"`
	ErrorCode     string                  `json:"error_code,omitempty"`
	SessionRef    string                  `json:"session_ref,omitempty"`
	JobIDs        []string                `json:"job_ids,omitempty"`
}

type memoryReceiptArtifact struct {
	Path       string `json:"path"` // brain-relative
	PriorState string `json:"prior_state"`
	NewState   string `json:"new_state"`
}

func newMemoryOperationReceipt(operation string, now time.Time) memoryOperationReceipt {
	h := sha256.New()
	h.Write([]byte(operation))
	h.Write([]byte{0})
	h.Write([]byte(now.UTC().Format(time.RFC3339Nano)))
	return memoryOperationReceipt{
		SchemaVersion: memoryOperationReceiptSchemaVersion,
		OperationID:   "op:" + hex.EncodeToString(h.Sum(nil))[:16],
		Operation:     operation,
		StartedAt:     now,
		Artifacts:     []memoryReceiptArtifact{},
	}
}

// finalizeMemoryOperationReceipt closes a receipt and canonicalizes its
// collection fields before either output mode observes it. Mutation commands
// may discover artifacts incrementally, but their receipts must not depend on
// directory iteration order or selector spelling.
func finalizeMemoryOperationReceipt(receipt *memoryOperationReceipt, now time.Time) {
	if receipt == nil {
		return
	}
	receipt.FinishedAt = now.UTC()
	if receipt.Artifacts == nil {
		receipt.Artifacts = []memoryReceiptArtifact{}
	}
	sort.SliceStable(receipt.Artifacts, func(i, j int) bool {
		if receipt.Artifacts[i].Path != receipt.Artifacts[j].Path {
			return receipt.Artifacts[i].Path < receipt.Artifacts[j].Path
		}
		if receipt.Artifacts[i].PriorState != receipt.Artifacts[j].PriorState {
			return receipt.Artifacts[i].PriorState < receipt.Artifacts[j].PriorState
		}
		return receipt.Artifacts[i].NewState < receipt.Artifacts[j].NewState
	})
	sort.Strings(receipt.JobIDs)
	if len(receipt.JobIDs) > 1 {
		unique := receipt.JobIDs[:1]
		for _, jobID := range receipt.JobIDs[1:] {
			if jobID != unique[len(unique)-1] {
				unique = append(unique, jobID)
			}
		}
		receipt.JobIDs = unique
	}
}

// renderMemoryOperationReceiptText is the plain-output counterpart to the
// JSON receipt. Keep it content-free but complete enough for operators and
// scripts to retain the same operation identity and per-artifact transitions.
func renderMemoryOperationReceiptText(out io.Writer, receipt memoryOperationReceipt, status string) {
	fmt.Fprintf(out, "%s %s schema_version=%d operation_id=%s dry_run=%v error_code=%s started_at=%s finished_at=%s artifacts=%d jobs=%d\n",
		receipt.Operation, status, receipt.SchemaVersion, receipt.OperationID, receipt.DryRun, receipt.ErrorCode,
		receipt.StartedAt.UTC().Format(time.RFC3339Nano), receipt.FinishedAt.UTC().Format(time.RFC3339Nano), len(receipt.Artifacts), len(receipt.JobIDs))
	if receipt.SessionRef != "" {
		fmt.Fprintf(out, "selected session_ref=%q\n", receipt.SessionRef)
	}
	for _, jobID := range receipt.JobIDs {
		fmt.Fprintf(out, "selected job_id=%q\n", jobID)
	}
	for _, artifact := range receipt.Artifacts {
		fmt.Fprintf(out, "artifact path=%q prior_state=%q new_state=%q\n", artifact.Path, artifact.PriorState, artifact.NewState)
	}
}

// finishMemoryOperationFailure closes and emits the one failure receipt for an
// operation after its receipt identity exists. The original failure remains
// the command error; an output failure is joined instead of replacing it.
func finishMemoryOperationFailure(cmd *cobra.Command, receipt *memoryOperationReceipt, jsonOut bool, now time.Time, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("%s: memory operation failed", memoryErrStateCorrupt)
	}
	finalizeMemoryOperationReceipt(receipt, now)
	if receipt.ErrorCode == "" {
		receipt.ErrorCode = memoryErrorCode(cause)
	}
	var emitErr error
	if jsonOut {
		emitErr = writeJSON(cmd, receipt)
	} else {
		emitErr = writeText(cmd, func(out io.Writer) {
			renderMemoryOperationReceiptText(out, *receipt, "failed")
		})
	}
	if emitErr != nil {
		return errors.Join(cause, fmt.Errorf("emit %s failure receipt: %w", receipt.Operation, emitErr))
	}
	return cause
}

// emitMemoryOperationSuccess finalizes and emits a successful mutation receipt.
// If stdout fails after the durable mutation, preserve the known artifact
// outcomes in a finalized failure receipt and keep the original writer error in
// the returned error chain. A writer may reject both attempts, but the command
// must never report success after losing its receipt.
func emitMemoryOperationSuccess(
	cmd *cobra.Command,
	receipt *memoryOperationReceipt,
	jsonOut bool,
	now time.Time,
	jsonValue func(memoryOperationReceipt) any,
	renderText func(io.Writer, memoryOperationReceipt),
) error {
	finalizeMemoryOperationReceipt(receipt, now)
	var emitErr error
	if jsonOut {
		value := any(*receipt)
		if jsonValue != nil {
			value = jsonValue(*receipt)
		}
		emitErr = writeJSON(cmd, value)
	} else {
		emitErr = writeText(cmd, func(out io.Writer) {
			if renderText != nil {
				renderText(out, *receipt)
				return
			}
			renderMemoryOperationReceiptText(out, *receipt, "completed")
		})
	}
	if emitErr == nil {
		return nil
	}
	cause := fmt.Errorf("%s: %s completed but success receipt emission failed: %w", memoryErrStateCorrupt, receipt.Operation, emitErr)
	return finishMemoryOperationFailure(cmd, receipt, jsonOut, now, cause)
}

// memoryDerivedArtifactStates reports the current state of each disposable
// derived artifact for receipts and dry-runs.
func memoryDerivedArtifactStates(brainDir string) []memoryReceiptArtifact {
	var artifacts []memoryReceiptArtifact
	indexRel := historyIndexPath
	receiptRel := projectionStateRel
	if manifest, err := loadBrainManifest(brainDir); err == nil && manifest.Sources != nil && manifest.Sources.History != nil {
		if manifest.Sources.History.IndexPath != "" {
			indexRel = manifest.Sources.History.IndexPath
		}
		if manifest.Sources.History.ProjectionStatePath != "" {
			receiptRel = manifest.Sources.History.ProjectionStatePath
		}
	}
	for _, rel := range []string{
		indexRel,
		historyFTSDBRelPath(),
		historyShortTermPath,
		receiptRel,
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
	} {
		s := memoryDerivedArtifactState(brainDir, rel)
		artifacts = append(artifacts, memoryReceiptArtifact{Path: rel, PriorState: s, NewState: s})
	}
	return artifacts
}

func markProjectionRefreshStates(brainDir string, artifacts *[]memoryReceiptArtifact) error {
	if artifacts == nil {
		return nil
	}
	current := map[string]bool{}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return fmt.Errorf("%s: load rebuilt manifest for receipt: %w", memoryErrStateCorrupt, err)
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		return fmt.Errorf("%s: rebuilt manifest has no history projection", memoryErrStateCorrupt)
	}
	if path := filepath.ToSlash(strings.TrimSpace(manifest.Sources.History.IndexPath)); path != "" {
		current[path] = true
	}
	if path := filepath.ToSlash(strings.TrimSpace(manifest.Sources.History.ProjectionStatePath)); path != "" {
		current[path] = true
	}
	if len(current) != 2 {
		return fmt.Errorf("%s: rebuilt manifest does not identify both projection artifacts", memoryErrStateCorrupt)
	}
	seen := map[string]bool{}
	for i := range *artifacts {
		path := (*artifacts)[i].Path
		seen[path] = true
		(*artifacts)[i].NewState = memoryProjectionRefreshArtifactOutcome(brainDir, (*artifacts)[i], current, false)
	}
	currentPaths := make([]string, 0, len(current))
	for path := range current {
		currentPaths = append(currentPaths, path)
	}
	sort.Strings(currentPaths)
	for _, path := range currentPaths {
		if seen[path] {
			continue
		}
		*artifacts = append(*artifacts, memoryReceiptArtifact{
			Path:       path,
			PriorState: "not_current",
			NewState:   "current",
		})
	}
	return nil
}

var markMemoryProjectionRefreshStates = markProjectionRefreshStates

func isHistoryProjectionPointerArtifact(path string) bool {
	return path == historyIndexPath || path == projectionStateRel ||
		(strings.HasPrefix(path, historyGenerationsDir+"/") &&
			(strings.HasSuffix(path, "/"+historyIndexFileName) || strings.HasSuffix(path, "/"+projectionStateFileName)))
}

// memoryProjectionRefreshArtifactOutcome is the shared transition planner for
// real and dry-run projection maintenance. The dry-run branch reports intent;
// the execution branch classifies the state observed after publication.
func memoryProjectionRefreshArtifactOutcome(brainDir string, artifact memoryReceiptArtifact, current map[string]bool, dryRun bool) string {
	path := artifact.Path
	if dryRun {
		switch {
		case isHistoryProjectionPointerArtifact(path):
			return "would_be_superseded_by_current_generation"
		case path == historyShortTermPath:
			return "would_be_cleared_after_consolidation"
		case path == historyFTSDBRelPath():
			switch artifact.PriorState {
			case "present":
				return "would_be_revalidated_or_rebuilt_lazily"
			case "absent":
				return "would_remain_absent_until_lazy_rebuild"
			default:
				return "would_remain_unavailable_unsafe"
			}
		default:
			return "would_be_preserved"
		}
	}

	switch {
	case isHistoryProjectionPointerArtifact(path):
		if current[path] {
			return "current"
		}
		switch state := memoryDerivedArtifactState(brainDir, path); state {
		case "present":
			return "superseded_preserved"
		case "unsafe", "corrupt":
			return "superseded_" + state
		default:
			return "superseded_missing"
		}
	case path == historyShortTermPath:
		return "cleared_after_consolidation"
	case path == historyFTSDBRelPath():
		switch memoryDerivedArtifactState(brainDir, path) {
		case "present":
			return "available"
		case "absent":
			return "absent_lazy_rebuild"
		default:
			return "unavailable_unsafe"
		}
	default:
		return "preserved"
	}
}

// planMemoryProjectionRefreshStates applies the same artifact classification
// used by a real projection refresh, but records explicit prospective outcomes
// without preparing, staging, or publishing a generation. Generation-addressed
// index and receipt paths are content-derived, so a side-effect-free dry-run
// cannot name the future path truthfully; it instead records what will happen
// to each currently selected artifact.
func planMemoryProjectionRefreshStates(artifacts *[]memoryReceiptArtifact) {
	if artifacts == nil {
		return
	}
	for i := range *artifacts {
		(*artifacts)[i].NewState = memoryProjectionRefreshArtifactOutcome("", (*artifacts)[i], nil, true)
	}
}

func memoryDerivedArtifactState(brainDir, rel string) string {
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil || filepath.ToSlash(clean) != filepath.ToSlash(strings.TrimSpace(rel)) {
		return "unsafe"
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return "unsafe"
	}
	info, err := os.Lstat(filepath.Join(brainDir, clean))
	switch {
	case err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0:
		return "present"
	case err == nil:
		return "unsafe"
	case os.IsNotExist(err):
		return "absent"
	default:
		return "corrupt"
	}
}

func canonicalSessionInputDigest(brainDir string, manifest *exportManifest, wantedRef string) (string, error) {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return "", fmt.Errorf("%s: canonical session source is unavailable", memoryErrSourceStale)
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return "", err
	}
	found := ""
	for _, session := range manifest.Sources.Sessions.Sessions {
		id := strings.TrimSpace(session.SessionID)
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if id == "" || rel == "" {
			continue
		}
		digest, err := sessionTranscriptDigest(brainDir, rel)
		if err != nil {
			continue
		}
		branch := strings.TrimSpace(session.Branch)
		if branch == "" {
			branch = strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
		}
		ref, _ := conversationSessionRef(manifest.RepoKey, branch, id, digest)
		if ref != wantedRef {
			continue
		}
		if _, excluded := stones.Excluded[id]; excluded {
			return "", fmt.Errorf("%s: session %s is excluded", memoryErrPrivacyExcluded, wantedRef)
		}
		if found != "" && found != digest {
			return "", fmt.Errorf("%s: session %s resolves to conflicting source digests", memoryErrIdentityAmbiguous, wantedRef)
		}
		found = digest
	}
	if found == "" {
		return "", fmt.Errorf("session %s not found", wantedRef)
	}
	return found, nil
}

func newMemoryRepairCommand(opts Options) *cobra.Command {
	var dryRun, jsonOut bool
	var sessionRef string
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Verify projection dependencies and perform the smallest deterministic rebuild",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			brainDir := storage.BrainDir
			if _, _, err := loadSessionTombstonesChecked(brainDir); err != nil {
				return err
			}
			sessionRef = strings.TrimSpace(sessionRef)
			now := opts.Now().UTC()
			receipt := newMemoryOperationReceipt("repair", now)
			receipt.SessionRef = sessionRef
			receipt.DryRun = dryRun
			receipt.Artifacts = memoryDerivedArtifactStates(brainDir)
			fail := func(cause error) error {
				return finishMemoryOperationFailure(cmd, &receipt, jsonOut, opts.Now(), cause)
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return fail(err)
			}
			var source *historySourceManifest
			if manifest.Sources != nil {
				source = manifest.Sources.History
			}
			// Verify index integrity before privacy verification: the latter reads
			// the index and cannot prove absence from a corrupt projection. Repair
			// treats a missing legacy digest or any bounded-read/digest failure as
			// a deterministic rebuild request instead of becoming unable to repair
			// the very state it is responsible for recovering.
			_, indexErr := loadBrainHistoryIndex(brainDir, source)
			indexCurrent := indexErr == nil && source != nil && source.IndexDigest != ""
			verifyAvailable := indexErr == nil
			verify := privacyVerifyReport{Findings: []privacyVerifyFinding{}}
			if verifyAvailable {
				verify, err = verifySessionPrivacy(brainDir)
				if err != nil {
					return fail(err)
				}
			}
			projection, receiptState, receiptErr := loadProjectionStateChecked(brainDir, source)
			if receiptState == projectionStateUnsupported || receiptState == projectionStateUnsafe {
				return fail(receiptErr)
			}
			receiptsCurrent := receiptState == projectionStateCurrent
			fingerprintCurrent := source != nil && sessionSourceFingerprintCurrent(source.SessionsFingerprint, brainSessionsFingerprint(brainDir))
			needsRebuild := !indexCurrent || !verifyAvailable || !verify.Clean || !receiptsCurrent || !fingerprintCurrent
			if sessionRef != "" {
				digest, scopeErr := canonicalSessionInputDigest(brainDir, manifest, sessionRef)
				if scopeErr != nil {
					return fail(scopeErr)
				}
				receipt, ok := projection.receiptFor(sessionRef)
				needsRebuild = !indexCurrent || !verifyAvailable || !verify.Clean || !receiptsCurrent || !ok || receipt.InputDigest != digest
			}
			if needsRebuild && !dryRun {
				if err := rebuildMemoryProjection(brainDir, now); err != nil {
					return fail(err)
				}
				if err := markMemoryProjectionRefreshStates(brainDir, &receipt.Artifacts); err != nil {
					return fail(err)
				}
				postVerify, verifyErr := verifySessionPrivacy(brainDir)
				if verifyErr != nil {
					return fail(verifyErr)
				}
				if !postVerify.Clean {
					return fail(fmt.Errorf("%s: repair rebuilt the active projection but %d privacy violations remain; run `entire brain privacy purge <session-id>` and retry repair", memoryErrPrivacyDirty, len(postVerify.Findings)))
				}
				verify = postVerify
				postManifest, manifestErr := loadBrainManifest(brainDir)
				if manifestErr != nil {
					return fail(manifestErr)
				}
				var postSource *historySourceManifest
				if postManifest.Sources != nil {
					postSource = postManifest.Sources.History
				}
				if postSource == nil || postSource.IndexDigest == "" {
					return fail(fmt.Errorf("%s: repair did not publish an integrity-protected history index", memoryErrStateCorrupt))
				}
				if _, postIndexErr := loadBrainHistoryIndex(brainDir, postSource); postIndexErr != nil {
					return fail(fmt.Errorf("%s: repair published an unreadable history index: %w", memoryErrStateCorrupt, postIndexErr))
				}
				indexCurrent = true
				verifyAvailable = true
				projection, receiptState, receiptErr = loadProjectionStateChecked(brainDir, postSource)
				if receiptState != projectionStateCurrent {
					if receiptErr != nil {
						return fail(receiptErr)
					}
					return fail(fmt.Errorf("%s: repair did not publish a current projection receipt", memoryErrStateCorrupt))
				}
				receiptsCurrent = true
				fingerprintCurrent = postSource != nil && sessionSourceFingerprintCurrent(postSource.SessionsFingerprint, brainSessionsFingerprint(brainDir))
			} else if needsRebuild {
				planMemoryProjectionRefreshStates(&receipt.Artifacts)
			}
			return emitMemoryOperationSuccess(cmd, &receipt, jsonOut, opts.Now().UTC(), func(receipt memoryOperationReceipt) any {
				return map[string]any{
					"receipt": receipt, "verify_clean": verify.Clean,
					"verify_available": verifyAvailable, "index_current": indexCurrent,
					"receipts_current": receiptsCurrent, "fingerprint_current": fingerprintCurrent,
					"rebuild_required": needsRebuild, "would_rebuild": needsRebuild && dryRun,
					"rebuilt": needsRebuild && !dryRun,
				}
			}, func(out io.Writer, receipt memoryOperationReceipt) {
				renderMemoryOperationReceiptText(out, receipt, "completed")
				if !needsRebuild {
					fmt.Fprintln(out, "repair: projections verify clean and current; nothing to do")
					return
				}
				mode := "rebuilt"
				if dryRun {
					mode = "dry-run: would rebuild"
				}
				fmt.Fprintf(out, "repair: %s the history projection (index_current=%v verify_clean=%v receipts_current=%v fingerprint_current=%v)\n", mode, indexCurrent, verify.Clean, receiptsCurrent, fingerprintCurrent)
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what repair would do without changing anything")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&sessionRef, "session-ref", "", "Repair the monolithic projection only when this canonical session is stale or missing")
	return cmd
}

func newMemoryRebuildCommand(opts Options) *cobra.Command {
	var all, dryRun, jsonOut bool
	var sessionRef string
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Recreate the disposable projections from the canonical captured sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionRef = strings.TrimSpace(sessionRef)
			if all == (sessionRef != "") {
				return fmt.Errorf("pass exactly one of --all or --session-ref")
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			brainDir := storage.BrainDir
			if _, _, err := loadSessionTombstonesChecked(brainDir); err != nil {
				return err
			}
			now := opts.Now().UTC()
			receipt := newMemoryOperationReceipt("rebuild", now)
			receipt.SessionRef = sessionRef
			receipt.DryRun = dryRun
			fail := func(cause error) error {
				return finishMemoryOperationFailure(cmd, &receipt, jsonOut, opts.Now(), cause)
			}
			if sessionRef != "" {
				manifest, err := loadBrainManifest(brainDir)
				if err != nil {
					return fail(err)
				}
				if _, err := canonicalSessionInputDigest(brainDir, manifest, sessionRef); err != nil {
					return fail(err)
				}
			}
			receipt.Artifacts = memoryDerivedArtifactStates(brainDir)
			if !dryRun {
				// Never delete first. Preparation writes an unreferenced
				// generation; a short locked manifest switch is the commit.
				// Vector stores are preserved and become unavailable by
				// identity if stale until semantic refresh resumes them.
				if err := rebuildMemoryProjection(brainDir, now); err != nil {
					return fail(err)
				}
				if err := markMemoryProjectionRefreshStates(brainDir, &receipt.Artifacts); err != nil {
					return fail(err)
				}
			} else {
				planMemoryProjectionRefreshStates(&receipt.Artifacts)
			}
			return emitMemoryOperationSuccess(cmd, &receipt, jsonOut, opts.Now().UTC(), nil, func(out io.Writer, receipt memoryOperationReceipt) {
				renderMemoryOperationReceiptText(out, receipt, "completed")
				mode := "rebuilt"
				if dryRun {
					mode = "dry-run: would rebuild"
				}
				fmt.Fprintf(out, "%s %d disposable projection artifacts from canonical sessions\n", mode, len(receipt.Artifacts))
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Rebuild every disposable projection")
	cmd.Flags().StringVar(&sessionRef, "session-ref", "", "Rebuild because this canonical session is stale (the shared projection is replaced atomically)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what rebuild would touch without changing anything")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// memoryMigrationFinding names one derived artifact whose schema is not
// current and the action migrate takes.
type memoryMigrationFinding struct {
	Path   string `json:"path"`
	State  string `json:"state"`
	Action string `json:"action"`
}

type memoryMigrationProgress struct {
	SchemaVersion int       `json:"schema_version"`
	OperationID   string    `json:"operation_id"`
	StartedAt     time.Time `json:"started_at"`
	Completed     []string  `json:"completed"`
}

func loadMemoryMigrationProgress(brainDir string) (memoryMigrationProgress, bool, error) {
	data, present, err := readMemoryStateFile(brainDir, memoryMigrationProgressRel, "migration progress", maxManifestBytes)
	if err != nil {
		return memoryMigrationProgress{}, present, err
	}
	if !present {
		return memoryMigrationProgress{}, false, nil
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return memoryMigrationProgress{}, true, fmt.Errorf("%s: decode migration progress header: %w", memoryErrStateCorrupt, err)
	}
	if header.SchemaVersion > 1 {
		return memoryMigrationProgress{}, true, fmt.Errorf("%s: migration progress schema version %d is newer than supported version 1", memoryErrUnsupportedVersion, header.SchemaVersion)
	}
	var progress memoryMigrationProgress
	if _, err := decodeVersionedJSONBody(data, &progress, false); err != nil {
		if errors.Is(err, errTrailingJSONData) {
			return memoryMigrationProgress{}, true, fmt.Errorf("%s: migration progress has trailing JSON data", memoryErrStateCorrupt)
		}
		return memoryMigrationProgress{}, true, fmt.Errorf("%s: decode migration progress: %w", memoryErrStateCorrupt, err)
	}
	if progress.SchemaVersion != 1 || !strings.HasPrefix(progress.OperationID, "op:") || progress.StartedAt.IsZero() || len(progress.Completed) > memoryStateInventoryMaxEntries {
		return memoryMigrationProgress{}, true, fmt.Errorf("%s: migration progress has invalid identity or bounds", memoryErrStateCorrupt)
	}
	seen := make(map[string]bool, len(progress.Completed))
	for _, rel := range progress.Completed {
		clean, cleanErr := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
		if cleanErr != nil || filepath.ToSlash(clean) != rel || seen[rel] {
			return memoryMigrationProgress{}, true, fmt.Errorf("%s: migration progress contains an invalid completed path", memoryErrStateCorrupt)
		}
		seen[rel] = true
	}
	return progress, true, nil
}

func saveMemoryMigrationProgress(brainDir string, progress memoryMigrationProgress) error {
	data, err := json.MarshalIndent(progress, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, memoryMigrationProgressRel, append(data, '\n'), 0o600)
}

var saveMemoryMigrationProgressForAdmin = saveMemoryMigrationProgress

// detectMemoryMigrations inspects derived schema versions. Disposable formats
// migrate by rebuilding beside the old artifact and atomically switching
// (that is exactly what the existing atomic writers do); unknown NEWER
// versions are read-only errors that name the required action.
func detectMemoryMigrations(brainDir string, source *historySourceManifest) []memoryMigrationFinding {
	var findings []memoryMigrationFinding
	if _, present, err := loadMemoryMigrationProgress(brainDir); err != nil {
		state := memoryErrorCode(err)
		action := "inspect and restore the migration progress file before resuming"
		if state == memoryErrUnsupportedVersion {
			action = "upgrade the binary; this newer migration progress remains read-only"
		}
		findings = append(findings, memoryMigrationFinding{Path: memoryMigrationProgressRel, State: state, Action: action})
	} else if present {
		// Valid progress is intentionally not a migration finding. The status
		// surface exposes it separately; --resume revalidates current artifacts
		// instead of trusting completed path names as proof.
	}
	if _, present, err := loadMemoryVectorProgress(brainDir); err != nil {
		state := memoryErrorCode(err)
		action := "delete and rebuild this disposable vector progress state from the current history generation"
		switch state {
		case memoryErrUnsupportedVersion:
			action = "upgrade the binary; this newer vector progress state remains read-only"
		case memoryErrStateUnsafe:
			action = "inspect the unsafe vector progress path; no file is removed automatically"
		}
		findings = append(findings, memoryMigrationFinding{Path: memoryVectorProgressRel, State: state, Action: action})
	} else if present {
		// Current vector progress is operational status, not a migration.
	}
	if source != nil {
		path := strings.TrimSpace(source.IndexPath)
		if path == "" {
			path = historyIndexPath
		}
		if source.IndexDigest == "" && validHistoryGenerationArtifactPath(path, historyIndexFileName) {
			findings = append(findings, memoryMigrationFinding{Path: path, State: memoryErrMigrationRequired, Action: "rebuild the generation and publish its manifest content digest (full refresh)"})
		} else if _, err := loadBrainHistoryIndex(brainDir, source); err != nil {
			findings = append(findings, memoryMigrationFinding{Path: path, State: memoryErrStateCorrupt, Action: "rebuild the disposable history projection from canonical sessions"})
		} else if source.IndexDigest == "" {
			findings = append(findings, memoryMigrationFinding{Path: path, State: memoryErrMigrationRequired, Action: "publish a manifest-committed index content digest (full refresh)"})
		}
	}
	if overlay, state := loadHistoryShortTermRaw(brainDir); state == shortTermStateCurrent {
		switch {
		case overlay.Version > historyShortTermVersion:
			findings = append(findings, memoryMigrationFinding{Path: historyShortTermPath, State: memoryErrUnsupportedVersion, Action: "newer-version overlay is read-only for this binary; upgrade the binary or delete the file to rebuild"})
		case overlay.Version < historyShortTermVersion || overlay.ReconcilerVersion != historyShortTermReconcilerVersion:
			findings = append(findings, memoryMigrationFinding{Path: historyShortTermPath, State: memoryErrMigrationRequired, Action: "rebuild the overlay (refresh delta)"})
		}
	} else if state == shortTermStateCorrupt {
		findings = append(findings, memoryMigrationFinding{Path: historyShortTermPath, State: memoryErrStateCorrupt, Action: "repair or rebuild the disposable overlay from canonical sessions"})
	} else if state == shortTermStateUnsupported {
		findings = append(findings, memoryMigrationFinding{Path: historyShortTermPath, State: memoryErrUnsupportedVersion, Action: "newer-version overlay is read-only for this binary; upgrade the binary"})
	}
	if source != nil {
		receiptPath := filepath.ToSlash(strings.TrimSpace(source.ProjectionStatePath))
		if receiptPath == "" {
			receiptPath = projectionStateRel
		}
		if source.ProjectionStateDigest == "" {
			findings = append(findings, memoryMigrationFinding{Path: receiptPath, State: memoryErrMigrationRequired, Action: "publish projection receipts (full refresh)"})
		} else {
			_, receiptState, receiptErr := loadProjectionStateChecked(brainDir, source)
			if receiptState != projectionStateCurrent {
				state := memoryErrStateCorrupt
				action := "rebuild the disposable projection receipts from canonical sessions"
				switch receiptState {
				case projectionStateUnsupported:
					state = memoryErrUnsupportedVersion
					action = "upgrade the binary; this newer projection receipt remains read-only"
				case projectionStateUnsafe:
					state = memoryErrStateUnsafe
					action = "inspect the unsafe projection receipt path; no file is removed automatically"
				case projectionStateStale:
					state = memoryErrSourceStale
				}
				if receiptErr != nil {
					state = memoryErrorCode(receiptErr)
				}
				findings = append(findings, memoryMigrationFinding{Path: receiptPath, State: state, Action: action})
			}
		}
	}
	_, configState, configErr := loadMemoryConfigChecked(brainDir)
	if configErr != nil {
		state := string(configState)
		findings = append(findings, memoryMigrationFinding{Path: memoryConfigRel, State: state, Action: "inspect and restore the content-free feature selection; configuration is not deleted automatically"})
	}
	abstractInventory := loadSessionAbstractInventory(brainDir)
	seenAbstractIssues := map[string]bool{}
	for _, issue := range abstractInventory.Issues {
		path := abstractsDirRel
		if issue.File != "" && issue.File != abstractsDirRel {
			path = filepath.ToSlash(filepath.Join(abstractsDirRel, issue.File))
		}
		key := path + "\x00" + issue.Code
		if seenAbstractIssues[key] {
			continue
		}
		seenAbstractIssues[key] = true
		action := "inspect the unsafe abstract state; no file is removed automatically"
		switch issue.Code {
		case memoryErrUnsupportedVersion:
			action = "upgrade the binary; this newer abstract remains read-only"
		case memoryErrStateCorrupt:
			action = "delete and regenerate this disposable abstract after verifying its session source"
		case memoryErrQueryTooBroad:
			action = fmt.Sprintf("reduce the abstract directory below the %d-file inventory ceiling", abstractInventoryMaxFiles)
		}
		findings = append(findings, memoryMigrationFinding{Path: path, State: issue.Code, Action: action})
	}
	if abstractInventory.Truncated {
		key := abstractsDirRel + "\x00" + memoryErrQueryTooBroad
		if !seenAbstractIssues[key] {
			findings = append(findings, memoryMigrationFinding{
				Path: abstractsDirRel, State: memoryErrQueryTooBroad,
				Action: fmt.Sprintf("reduce the abstract directory below the %d-file inventory ceiling", abstractInventoryMaxFiles),
			})
		}
	}
	for _, issue := range scanMemoryJobSchemas(brainDir) {
		action := "repair the corrupt content-free job file"
		switch issue.Code {
		case memoryErrMigrationRequired:
			action = "rewrite the legacy job record to the current structured-error schema"
		case memoryErrUnsupportedVersion:
			action = "newer-version job metadata is read-only for this binary; upgrade the binary"
		}
		findings = append(findings, memoryMigrationFinding{
			Path: filepath.ToSlash(filepath.Join(memoryJobsDirRel, issue.File)), State: issue.Code, Action: action,
		})
	}
	return findings
}

var detectMemoryMigrationsForAdmin = detectMemoryMigrations

func rewriteLegacyMemoryJob(brainDir, rel string) error {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if !strings.HasPrefix(filepath.ToSlash(clean), memoryJobsDirRel+"/") {
		return fmt.Errorf("%s: unsafe legacy job path", memoryErrStateCorrupt)
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return fmt.Errorf("%s: unsafe legacy job path", memoryErrStateCorrupt)
	}
	data, present, err := readMemoryStateFile(brainDir, filepath.ToSlash(clean), "legacy memory job", maxManifestBytes)
	if err != nil {
		return fmt.Errorf("%s: read legacy job: %w", memoryErrStateCorrupt, err)
	}
	if !present {
		return fmt.Errorf("%s: legacy job disappeared before migration: %s", memoryErrStateCorrupt, rel)
	}
	var job memoryJob
	if err := json.Unmarshal(data, &job); err != nil || job.SchemaVersion != 1 || job.JobID == "" ||
		job.JobID != memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind) || !validMemoryJobKind(job.Kind) || !validMemoryJobState(job.State) {
		return fmt.Errorf("%s: legacy job %s is invalid", memoryErrStateCorrupt, rel)
	}
	if err := saveMemoryJob(brainDir, job); err != nil {
		return err
	}
	canonical := memoryJobRel(job.JobID)
	if filepath.ToSlash(clean) != canonical {
		if err := removeBrainRelativeFile(brainDir, filepath.ToSlash(clean)); err != nil {
			return fmt.Errorf("%s: remove migrated legacy job: %w", memoryErrStateCorrupt, err)
		}
	}
	return nil
}

func newMemoryMigrateCommand(opts Options) *cobra.Command {
	var dryRun, jsonOut, resume bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Upgrade derived schemas atomically (build beside, switch, never delete first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			brainDir := storage.BrainDir
			if _, _, err := loadSessionTombstonesChecked(brainDir); err != nil {
				return err
			}
			now := opts.Now().UTC()
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			var source *historySourceManifest
			if manifest.Sources != nil {
				source = manifest.Sources.History
			}
			findings := detectMemoryMigrationsForAdmin(brainDir, source)
			receipt := newMemoryOperationReceipt("migrate", now)
			fail := func(cause error) error {
				return finishMemoryOperationFailure(cmd, &receipt, jsonOut, opts.Now(), cause)
			}
			progress := memoryMigrationProgress{SchemaVersion: 1, OperationID: receipt.OperationID, StartedAt: now}
			prior, priorPresent, progressErr := loadMemoryMigrationProgress(brainDir)
			if progressErr != nil {
				return fail(progressErr)
			}
			if resume {
				receipt.Operation = "migrate_resume"
				if priorPresent {
					progress = prior
					receipt.OperationID = prior.OperationID
					receipt.StartedAt = prior.StartedAt
				}
			}
			receipt.DryRun = dryRun
			if dryRun {
				for _, finding := range findings {
					if finding.State != memoryErrMigrationRequired {
						continue
					}
					receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
						Path: finding.Path, PriorState: finding.State, NewState: "would_be_current",
					})
				}
			}
			migrated := 0
			remaining := findings
			if !dryRun {
				projectionRebuilt := false
				indexPath := ""
				receiptPath := projectionStateRel
				if source != nil {
					indexPath = strings.TrimSpace(source.IndexPath)
					if indexPath == "" {
						indexPath = historyIndexPath
					}
					if configured := filepath.ToSlash(strings.TrimSpace(source.ProjectionStatePath)); configured != "" {
						receiptPath = configured
					}
				}
				completed := map[string]bool{}
				for _, path := range progress.Completed {
					completed[path] = true
				}
				for _, finding := range findings {
					if finding.State != memoryErrMigrationRequired {
						continue // unsupported-newer stays read-only
					}
					var migrateErr error
					handled := true
					switch finding.Path {
					case historyShortTermPath:
						migrateErr = withBrainWriteLock(brainDir, func() error {
							_, err := buildHistoryShortTermLocked(brainDir, now)
							return err
						})
					default:
						if finding.Path == receiptPath {
							if !projectionRebuilt {
								migrateErr = rebuildMemoryProjection(brainDir, now)
								projectionRebuilt = migrateErr == nil
							}
						} else if source != nil && finding.Path == indexPath && source.IndexDigest == "" {
							if !projectionRebuilt {
								migrateErr = rebuildMemoryProjection(brainDir, now)
								projectionRebuilt = migrateErr == nil
							}
						} else if strings.HasPrefix(finding.Path, memoryJobsDirRel+"/") {
							migrateErr = withBrainWriteLock(brainDir, func() error {
								return rewriteLegacyMemoryJob(brainDir, finding.Path)
							})
						} else {
							handled = false
						}
					}
					if !handled {
						return fail(fmt.Errorf("%s: no safe migration action exists for %s", memoryErrMigrationRequired, finding.Path))
					}
					if migrateErr != nil {
						return fail(migrateErr)
					}
					receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{Path: finding.Path, PriorState: finding.State, NewState: "current"})
					if !completed[finding.Path] {
						progress.Completed = append(progress.Completed, finding.Path)
						completed[finding.Path] = true
					}
					if err := saveMemoryMigrationProgressForAdmin(brainDir, progress); err != nil {
						return fail(fmt.Errorf("%s: save migration progress: %w", memoryErrStateCorrupt, err))
					}
					migrated++
				}
				postManifest, postManifestErr := loadBrainManifest(brainDir)
				if postManifestErr != nil {
					return fail(postManifestErr)
				}
				var postSource *historySourceManifest
				if postManifest.Sources != nil {
					postSource = postManifest.Sources.History
				}
				remaining = detectMemoryMigrationsForAdmin(brainDir, postSource)
				remainingRequired := false
				for _, finding := range remaining {
					if finding.State == memoryErrMigrationRequired {
						remainingRequired = true
						break
					}
				}
				if !remainingRequired {
					if err := removeBrainRelativeFile(brainDir, memoryMigrationProgressRel); err != nil {
						return fail(fmt.Errorf("%s: clear completed migration progress: %w", memoryErrStateCorrupt, err))
					}
				}
			}
			var blocker *memoryMigrationFinding
			for i := range remaining {
				if dryRun && remaining[i].State == memoryErrMigrationRequired {
					continue
				}
				blocker = &remaining[i]
				break
			}
			if blocker != nil {
				receipt.ErrorCode = blocker.State
				return fail(fmt.Errorf("%s: %s: %s", blocker.State, blocker.Path, blocker.Action))
			}
			return emitMemoryOperationSuccess(cmd, &receipt, jsonOut, opts.Now().UTC(), func(receipt memoryOperationReceipt) any {
				return map[string]any{"receipt": receipt, "findings": findings, "remaining_findings": remaining, "migrated": migrated}
			}, func(out io.Writer, receipt memoryOperationReceipt) {
				renderMemoryOperationReceiptText(out, receipt, "completed")
				if len(findings) == 0 {
					fmt.Fprintln(out, "migrate: every derived schema is current")
					return
				}
				for _, finding := range findings {
					fmt.Fprintf(out, "%s %s: %s\n", finding.State, finding.Path, finding.Action)
				}
				if !dryRun {
					fmt.Fprintf(out, "migrated %d artifacts\n", migrated)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report required migrations without changing anything")
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume idempotently from the remaining migration findings")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

type memoryInstallDirectoryHealth struct {
	Path              string `json:"path"`
	State             string `json:"state"`
	Exists            bool   `json:"exists"`
	Mode              string `json:"mode,omitempty"`
	ModeWriteHint     bool   `json:"mode_write_hint"`
	WritabilityProven bool   `json:"writability_proven"`
	Evidence          string `json:"evidence"`
	RecommendedAction string `json:"recommended_action"`
}

type memoryInstallBinaryHealth struct {
	State             string `json:"state"`
	Path              string `json:"path,omitempty"`
	Executed          bool   `json:"executed"`
	VersionVerified   bool   `json:"version_verified"`
	RecommendedAction string `json:"recommended_action"`
}

type memoryHostAdapterHealth struct {
	State               string                                   `json:"state"`
	Authority           string                                   `json:"authority"`
	ImplementationState string                                   `json:"implementation_state"`
	Observability       string                                   `json:"observability"`
	RecommendedAction   string                                   `json:"recommended_action"`
	Targets             map[string]memoryHostAdapterTargetHealth `json:"targets"`
}

type memoryHostAdapterTargetHealth struct {
	Presence string `json:"presence"`
	Enabled  string `json:"enabled"`
	Trust    string `json:"trust"`
	Evidence string `json:"evidence"`
}

type memoryManifestHealth struct {
	Path                   string `json:"path"`
	State                  string `json:"state"`
	Present                bool   `json:"present"`
	SchemaVersion          int    `json:"schema_version,omitempty"`
	SupportedSchemaVersion int    `json:"supported_schema_version"`
	ErrorCode              string `json:"error_code,omitempty"`
	RecommendedAction      string `json:"recommended_action"`
}

// memoryReadOnlyHealthSnapshot is deliberately separate from the ordinary
// manifest loader. Retrieval and mutation continue to fail closed when the
// manifest is unsafe, corrupt, or from a newer producer; status and doctor can
// still report independently readable operational state.
type memoryReadOnlyHealthSnapshot struct {
	Manifest       *exportManifest
	ManifestHealth memoryManifestHealth
	Payload        map[string]any
	Issues         []memoryHealthIssue
}

func inspectMemoryInstallDirectory(brainDir, rel string) memoryInstallDirectoryHealth {
	health := memoryInstallDirectoryHealth{
		Path:              filepath.ToSlash(rel),
		State:             "unavailable",
		Evidence:          "read_only_lstat_and_permission_mode_hint",
		RecommendedAction: "inspect the brain path and retry memory status",
	}
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		health.State = "unsafe"
		health.RecommendedAction = "configure a brain-relative directory path"
		return health
	}
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		health.State = "unsafe"
		health.RecommendedAction = "replace the symlinked brain root with a real directory"
		return health
	}
	rootInfo, err := os.Lstat(brainDir)
	if err != nil {
		if os.IsNotExist(err) {
			rootHealth := inspectAbsoluteInstallDirectory(brainDir)
			health.State = rootHealth.State
			health.Mode = rootHealth.Mode
			health.ModeWriteHint = rootHealth.ModeWriteHint
			health.Evidence = rootHealth.Evidence
			health.RecommendedAction = "let the normal build workflow create the brain and this derived directory; status performs no probe write"
			if !rootHealth.ModeWriteHint {
				health.RecommendedAction = "grant write access to the nearest existing brain ancestor before running the normal build workflow"
			}
		}
		return health
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		health.State = "unsafe"
		health.RecommendedAction = "replace the brain root with a real directory"
		return health
	}
	ancestor := rootInfo
	current := brainDir
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				health.State = "creatable_unproven"
				health.Mode = fmt.Sprintf("%#o", ancestor.Mode().Perm())
				health.ModeWriteHint = ancestor.Mode().Perm()&0o222 != 0
				health.RecommendedAction = "let the next memory mutation create this directory; ACLs and platform policy are checked then"
				if !health.ModeWriteHint {
					health.RecommendedAction = "grant write access to the nearest existing ancestor before running a memory mutation"
				}
				return health
			}
			return health
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			health.State = "unsafe"
			health.Exists = true
			health.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
			health.RecommendedAction = "replace the symlink or non-directory path component with a real directory"
			return health
		}
		ancestor = info
	}
	health.State = "present_unproven"
	health.Exists = true
	health.Mode = fmt.Sprintf("%#o", ancestor.Mode().Perm())
	health.ModeWriteHint = ancestor.Mode().Perm()&0o222 != 0
	health.RecommendedAction = "writability is proven only by the requested mutation because ACLs and platform policy may override mode bits"
	if !health.ModeWriteHint {
		health.RecommendedAction = "grant write access before running a memory mutation; mode bits currently deny writes"
	}
	return health
}

// inspectAbsoluteInstallDirectory reports only evidence available without a
// write probe. In particular, mode bits are a hint and an absent leaf is only
// "creatable_unproven" when an existing ancestor can be inspected.
func inspectAbsoluteInstallDirectory(path string) memoryInstallDirectoryHealth {
	health := memoryInstallDirectoryHealth{
		Path:              path,
		State:             "unavailable",
		Evidence:          "read_only_lstat_and_permission_mode_hint",
		RecommendedAction: "inspect the directory path",
	}
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil || strings.TrimSpace(path) == "" {
		health.State = "unsafe"
		health.RecommendedAction = "configure an absolute directory path"
		return health
	}
	health.Path = abs
	info, err := os.Lstat(abs)
	if err == nil {
		health.Exists = true
		health.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			health.State = "unsafe"
			health.RecommendedAction = "replace the symlink or non-directory path with a real directory"
			return health
		}
		health.State = "present_unproven"
		health.ModeWriteHint = info.Mode().Perm()&0o222 != 0
		health.RecommendedAction = "writability is proven only by the requested mutation because ACLs and platform policy may override mode bits"
		if !health.ModeWriteHint {
			health.RecommendedAction = "grant write access before running a mutation; mode bits currently deny writes"
		}
		return health
	}
	if !os.IsNotExist(err) {
		health.RecommendedAction = "inspect the directory and its access policy"
		return health
	}
	for ancestor := filepath.Dir(abs); ; ancestor = filepath.Dir(ancestor) {
		info, statErr := os.Stat(ancestor)
		if statErr == nil && info.IsDir() {
			health.State = "creatable_unproven"
			health.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
			health.ModeWriteHint = info.Mode().Perm()&0o222 != 0
			health.RecommendedAction = "let the requested mutation create this directory; status performs no probe write"
			if !health.ModeWriteHint {
				health.RecommendedAction = "grant write access to the nearest existing ancestor before running a mutation"
			}
			return health
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return health
		}
	}
}

func inspectEntireBinary() memoryInstallBinaryHealth {
	health := memoryInstallBinaryHealth{
		State:             "not_found",
		RecommendedAction: "install Entire and ensure the entire executable is on PATH",
	}
	path, err := exec.LookPath("entire")
	if err != nil {
		if path != "" {
			health.Path = path
		}
		if errors.Is(err, exec.ErrDot) {
			health.State = "unsafe_relative_path"
			health.RecommendedAction = "use an absolute PATH entry for the Entire executable"
		} else if !errors.Is(err, exec.ErrNotFound) {
			health.State = "lookup_failed"
			health.RecommendedAction = "inspect PATH and executable permissions"
		}
		return health
	}
	if absolute, absErr := filepath.Abs(path); absErr == nil {
		path = absolute
	}
	health.State = "present_unproven"
	health.Path = path
	health.RecommendedAction = "run Entire's own version or doctor command to verify compatibility"
	return health
}

// memoryInstallHealth reports binary capabilities and directory health for
// memory status (installation and integration health). It is strictly
// read-only: presence and permission-mode observations are never presented as
// proof that a later write, host adapter, or binary invocation will succeed.
func memoryInstallHealth(brainDir string) map[string]any {
	brainRoot := inspectAbsoluteInstallDirectory(brainDir)
	workDir := inspectMemoryInstallDirectory(brainDir, memoryWorkDirRel)
	historyDir := inspectMemoryInstallDirectory(brainDir, historyDirName)
	providerEgress := memoryProviderEgressHealth(brainDir)
	buildProfile := "pure_go"
	if brainCGOBuild {
		buildProfile = "cgo_vector"
	}
	hostAdapter := memoryHostAdapterHealth{
		State:               "not_observable",
		Authority:           "entire-cli",
		ImplementationState: "external_unverified",
		Observability:       "not_observable",
		RecommendedAction:   "use Entire CLI's integration diagnostics to verify host-adapter installation, enablement, and trust; entire-brain does not execute Entire during status",
		Targets: map[string]memoryHostAdapterTargetHealth{
			"claude_code": {Presence: "unknown", Enabled: "unknown", Trust: "unknown", Evidence: "external_entire_cli_state_not_observable"},
			"codex":       {Presence: "unknown", Enabled: "unknown", Trust: "unknown", Evidence: "external_entire_cli_state_not_observable"},
		},
	}
	return map[string]any{
		"cgo_build":     brainCGOBuild,
		"vector_build":  brainCGOBuild,
		"build_profile": buildProfile,
		"entire_binary": inspectEntireBinary(),
		"directories": map[string]memoryInstallDirectoryHealth{
			"brain": brainRoot, "work": workDir, "history": historyDir, "log_parent": workDir,
		},
		"legacy_mode_hints": map[string]any{
			"deprecated":           true,
			"semantics":            "permission mode hint only; not a writability proof",
			"work_dir_writable":    workDir.State == "present_unproven" && workDir.ModeWriteHint,
			"history_dir_writable": historyDir.State == "present_unproven" && historyDir.ModeWriteHint,
		},
		"overlay_schema_version": historyShortTermVersion,
		"job_schema_version":     memoryJobSchemaVersion,
		"receipt_schema_version": projectionSchemaVersion,
		"reconciler_version":     historyShortTermReconcilerVersion,
		"fts_schema":             historyFTSSchema,
		"filtered_scan_ceiling":  historyFTSFilteredScanCeiling,
		"get_batch_ceiling":      maxGetBatchIDs,
		"host_adapter":           hostAdapter,
		"abstract_provider":      providerEgress["provider_summary"],
		"provider_egress":        providerEgress,
		"abstracts":              memoryAbstractHealth(brainDir),
		"memory_config_schema":   memoryConfigSchemaVersion,
		"abstract_schema":        abstractSchemaVersion,
		"worker_log_path":        filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel)),
		"worker_log_max_bytes":   memoryWorkerLogMaxBytes,
	}
}

func memoryProviderEgressHealth(brainDir string) map[string]any {
	noEgress := securityToggleEnabled("ENTIRE_BRAIN_NO_EGRESS")
	localOnly := securityToggleEnabled("ENTIRE_BRAIN_LOCAL_ONLY")
	globalLocalOnly := noEgress || localOnly
	globalMode := "default"
	if globalLocalOnly {
		globalMode = "local_only"
	}
	config, state, err := loadMemoryConfigChecked(brainDir)
	health := map[string]any{
		"state":                       state,
		"supported_schema_version":    memoryConfigSchemaVersion,
		"config_path":                 memoryConfigRel,
		"global_policy":               globalMode,
		"global_no_egress":            noEgress,
		"global_local_only":           localOnly,
		"global_local_only_effective": globalLocalOnly,
		"effective_state":             "disabled",
		"effective_provider_allowed":  false,
		"effective_egress_class":      "none",
		"provider_summary":            "disabled",
	}
	if observedVersion, present := memoryConfigObservedSchemaVersion(brainDir); present {
		health["schema_version"] = observedVersion
	}
	if err != nil {
		health["error_code"] = memoryErrorCode(err)
		health["effective_state"] = "config_unavailable"
		health["provider_summary"] = string(state)
		health["action"] = "inspect the content-free provider configuration; status leaves it untouched"
		return health
	}
	health["enabled"] = config.Abstracts.Enabled
	health["automatic"] = config.Abstracts.Automatic
	health["provider"] = config.Abstracts.Provider
	health["model"] = config.Abstracts.Model
	health["hosted_egress_allowed"] = config.Abstracts.HostedEgressAllowed
	providerName := strings.ToLower(strings.TrimSpace(config.Abstracts.Provider))
	egressClass := "none"
	switch providerName {
	case "codex", "claude-code":
		egressClass = "hosted"
	case "ollama":
		egressClass = "local"
	}
	health["egress_class"] = egressClass
	health["effective_hosted_egress_allowed"] = egressClass == "hosted" && config.Abstracts.HostedEgressAllowed && !globalLocalOnly
	providerSummary := "disabled"
	if config.Abstracts.Enabled {
		providerSummary = config.Abstracts.Provider
		if config.Abstracts.HostedEgressAllowed {
			providerSummary += " (hosted egress allowed)"
		}
	}
	health["provider_summary"] = providerSummary
	if !config.Abstracts.Enabled {
		return health
	}
	if config.Abstracts.Enabled && strings.TrimSpace(config.Abstracts.Provider) == "" {
		health["provider_state"] = memoryErrProviderUnavail
		health["effective_state"] = "provider_unavailable"
		health["action"] = "configure exactly one abstract provider or disable abstracts"
	} else {
		health["provider_state"] = "configured_unverified"
	}
	if egressClass == "hosted" && globalLocalOnly {
		health["effective_state"] = "blocked_by_global_policy"
		health["effective_egress_class"] = "blocked"
		health["block_code"] = "no_egress"
		health["action"] = "use a local loopback provider, or explicitly disable the global no-egress/local-only policy before hosted generation"
		return health
	}
	if egressClass == "hosted" && !config.Abstracts.HostedEgressAllowed {
		health["effective_state"] = "blocked_by_configuration"
		health["effective_egress_class"] = "blocked"
		health["block_code"] = "hosted_egress_unacknowledged"
		health["action"] = "acknowledge hosted egress explicitly or choose a local loopback provider"
		return health
	}
	health["effective_state"] = "available_unverified"
	health["effective_provider_allowed"] = true
	health["effective_egress_class"] = egressClass
	return health
}

// memoryConfigObservedSchemaVersion decodes only the bounded version header.
// It does not turn a corrupt/unsafe config into a supported one; the strict
// loader above remains the authority for state and error classification.
func memoryConfigObservedSchemaVersion(brainDir string) (int, bool) {
	data, present, err := readMemoryStateFile(brainDir, memoryConfigRel, "memory config", maxManifestBytes)
	if err != nil || !present {
		return 0, false
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil || header.SchemaVersion == 0 {
		return 0, false
	}
	return header.SchemaVersion, true
}

func memoryAbstractProviderSummary(brainDir string) string {
	config, state, err := loadMemoryConfigChecked(brainDir)
	if err != nil {
		return string(state)
	}
	if !config.Abstracts.Enabled {
		return "disabled"
	}
	if strings.TrimSpace(config.Abstracts.Provider) == "" {
		return "enabled but no provider configured"
	}
	if memoryAbstractorFactory == nil {
		return memoryErrProviderUnavail + ": no provider factory available"
	}
	if _, err := memoryAbstractorFactory(config.Abstracts); err != nil {
		return memoryErrProviderUnavail + ": " + err.Error()
	}
	summary := config.Abstracts.Provider
	if config.Abstracts.HostedEgressAllowed {
		summary += " (hosted egress allowed)"
	}
	return summary
}

func memoryAbstractHealth(brainDir string) map[string]any {
	inventory := loadSessionAbstractInventory(brainDir)
	egress := loadAbstractEgressInventory(brainDir)
	abstractState := "absent"
	currentAbstracts := 0
	for _, entry := range inventory.ByDigest {
		if entry.State == sessionAbstractCurrent {
			currentAbstracts++
		}
	}
	if currentAbstracts > 0 {
		abstractState = "current"
	}
	if len(inventory.Issues) > 0 {
		abstractState, _ = memoryInventoryIssueState(inventory.Issues)
	}
	egressState := "absent"
	if len(egress.Receipts) > 0 {
		egressState = "current"
	}
	if len(egress.Migrations) > 0 {
		egressState = memoryErrMigrationRequired
	}
	if len(egress.Issues) > 0 {
		egressState, _ = memoryInventoryIssueState(egress.Issues)
	}
	abstractVersions := memoryObservedSchemaVersions(nil, inventory.Issues)
	if currentAbstracts > 0 {
		abstractVersions = memoryObservedSchemaVersions(append(abstractVersions, abstractSchemaVersion), nil)
	}
	egressCurrentVersions := make([]int, 0, len(egress.Receipts))
	for _, receipt := range egress.Receipts {
		if receipt.Receipt.SchemaVersion > 0 {
			egressCurrentVersions = append(egressCurrentVersions, receipt.Receipt.SchemaVersion)
		}
	}
	egressVersions := memoryObservedSchemaVersions(egressCurrentVersions, append(append([]memoryStateIssue{}, egress.Issues...), egress.Migrations...))
	health := map[string]any{
		"state":                    abstractState,
		"schema_state":             abstractState,
		"supported_schema_version": abstractSchemaVersion,
		"artifacts":                inventory.Scanned,
		"current_artifacts":        currentAbstracts,
		"entries_observed":         inventory.EntriesObserved,
		"scanned":                  inventory.Scanned,
		"scan_ceiling":             abstractInventoryMaxFiles,
		"scan_truncated":           inventory.Truncated,
		"scan_degraded":            inventory.Degraded,
		"issue_count":              inventory.IssueCount,
		"issues_truncated":         inventory.IssueCount > len(inventory.Issues),
		"corrupt":                  inventory.IssueCounts[memoryErrStateCorrupt],
		"unsafe":                   inventory.IssueCounts[memoryErrStateUnsafe],
		"unsupported":              inventory.IssueCounts[memoryErrUnsupportedVersion],
		"stale":                    0,
		"hosted_egress": map[string]any{
			"state":                    egressState,
			"schema_state":             egressState,
			"supported_schema_version": abstractEgressSchemaVersion,
			"receipts_scanned":         len(egress.Receipts),
			"entries_scanned":          egress.EntriesScanned,
			"scan_ceiling":             abstractEgressHealthMaxReceipts,
			"scan_truncated":           egress.Truncated,
			"scan_degraded":            egress.Degraded,
			"issue_count":              len(egress.Issues),
			"migration_required":       len(egress.Migrations),
		},
	}
	if currentAbstracts > 0 {
		health["schema_version"] = abstractSchemaVersion
	}
	if len(abstractVersions) > 0 {
		health["schema_versions_observed"] = abstractVersions
		health["schema_version"] = abstractVersions[len(abstractVersions)-1]
	}
	if len(egressVersions) > 0 {
		health["hosted_egress"].(map[string]any)["schema_versions_observed"] = egressVersions
		health["hosted_egress"].(map[string]any)["schema_version"] = egressVersions[len(egressVersions)-1]
	}
	if len(egress.Issues) > 0 {
		health["hosted_egress"].(map[string]any)["issues"] = memoryHealthIssues(egress.Issues)
	}
	if len(egress.Migrations) > 0 {
		health["hosted_egress"].(map[string]any)["migrations"] = memoryHealthIssues(egress.Migrations)
	}
	_, configState, configErr := loadMemoryConfigChecked(brainDir)
	health["config_state"] = configState
	if configErr != nil {
		health["error_code"] = configState
	}
	manifest, manifestErr := loadBrainManifest(brainDir)
	if manifestErr == nil && manifest.Sources != nil && manifest.Sources.History != nil {
		fresh, freshErr := loadFreshHistory(brainDir, manifest.Sources.History)
		guard, guardErr := loadSessionReadGuard(brainDir, manifest)
		if freshErr == nil && guardErr == nil {
			views := buildConversationSessionViews(fresh, manifest.RepoKey, manifest, guard)
			for _, entry := range inventory.ByDigest {
				if entry.State != sessionAbstractCurrent {
					continue
				}
				view, ok := views[entry.Artifact.SessionRef]
				if !ok || entry.Artifact.SessionDigest != sessionViewDigest(view) {
					health["stale"] = health["stale"].(int) + 1
					continue
				}
				if err := validateSessionAbstract(entry.Artifact, view); err != nil {
					health["corrupt"] = health["corrupt"].(int) + 1
				}
			}
		}
	}
	if len(inventory.Issues) > 0 {
		health["issues"] = memoryHealthIssues(inventory.Issues)
	}
	return health
}

type memoryHealthIssue struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Code    string `json:"code"`
	Version int    `json:"version,omitempty"`
	Action  string `json:"action"`
}

func inspectBrainManifestHealth(brainDir string) (*exportManifest, memoryManifestHealth, *memoryHealthIssue) {
	health := memoryManifestHealth{
		Path:                   exportManifestFileName,
		State:                  "absent",
		SupportedSchemaVersion: brainManifestSchemaVersion,
		RecommendedAction:      "run `entire brain refresh` to create the canonical manifest",
	}
	manifest, version, droppedUnknownFields, present, err := readBrainManifestFile(brainDir, true)
	if !present && err == nil {
		// Preserve the established status shape for a not-yet-built Brain while
		// keeping absence distinct from a current on-disk manifest.
		return &exportManifest{SchemaVersion: brainManifestSchemaVersion, Sources: &brainSources{}}, health, nil
	}
	health.Present = present
	health.SchemaVersion = version
	if err != nil {
		code := memoryErrorCode(err)
		health.ErrorCode = code
		health.State = "corrupt"
		health.RecommendedAction = "repair or rebuild the manifest from canonical sources"
		if code == memoryErrStateUnsafe {
			health.State = "unsafe"
			health.RecommendedAction = "inspect the manifest path; status leaves the exact leaf untouched"
		}
		if code == memoryErrUnsupportedVersion {
			health.State = "unsupported"
			// Which way the version is skewed decides the remedy, and only one
			// of the two directions is "upgrade". A manifest older than v1
			// declares no version this project ever wrote, so no build reads it
			// and there is nothing to upgrade INTO -- the way out is to rebuild
			// it from the canonical sources it was derived from.
			if version < brainManifestMinSchemaVersion {
				health.RecommendedAction = "run `entire brain refresh --force` to rebuild the manifest from canonical sources"
			} else {
				health.RecommendedAction = "upgrade entire-brain; this newer manifest remains read-only"
			}
		}
		issue := memoryHealthIssue{Kind: "manifest", Path: exportManifestFileName, Code: code, Version: version, Action: health.RecommendedAction}
		return nil, health, &issue
	}
	health.State = "current"
	health.SchemaVersion = manifest.SchemaVersion
	health.RecommendedAction = "none"
	if droppedUnknownFields {
		// Fully readable, but a writer still refuses to re-encode fields it
		// never saw, so this manifest is read-only in exactly the way the
		// unsupported-version branch above describes. Removing it is safe and is
		// the way out: it is derived state, and an absent manifest lets the next
		// refresh rebuild it from canonical sources.
		// State stays "current" on purpose: the manifest is present, usable and
		// this build's supported schema, and callers switch on that string.
		health.RecommendedAction = "written by a different build and read-only; run `entire brain refresh --force` to rebuild it from canonical sources"
	}
	return manifest, health, nil
}

func inspectMemoryLockLeaf(brainDir, rel string) map[string]any {
	health := map[string]any{
		"path":             filepath.Join(brainDir, filepath.FromSlash(rel)),
		"state":            "absent",
		"evidence":         "read_only_lstat",
		"ownership_proven": false,
	}
	clean, err := cleanBrainRelativePath(filepath.ToSlash(rel))
	if err != nil || rejectSymlinkedBrainRoot(brainDir) != nil || rejectExistingSymlinkPathComponents(brainDir, clean) != nil {
		health["state"] = "unsafe"
		health["error_code"] = memoryErrStateUnsafe
		health["action"] = "inspect the lock path; status never acquires or replaces it"
		return health
	}
	info, err := os.Lstat(filepath.Join(brainDir, clean))
	switch {
	case err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0:
		health["state"] = "present_unproven"
		health["action"] = "correlate the lock leaf with coordinator heartbeat state; read-only status does not probe-acquire locks"
	case err == nil:
		health["state"] = "unsafe"
		health["error_code"] = memoryErrStateUnsafe
		health["action"] = "replace the unsafe lock leaf only after confirming no worker is active"
	case os.IsNotExist(err):
		health["state"] = "absent"
		health["action"] = "none"
	default:
		health["state"] = "unavailable"
		health["error_code"] = memoryErrStateCorrupt
		health["action"] = "inspect the lock path and access policy"
	}
	return health
}

func memoryLockHealth(brainDir string) map[string]any {
	return map[string]any{
		"write":       inspectMemoryLockLeaf(brainDir, filepath.ToSlash(filepath.Join(brainLockDirName, brainWriteLockName))),
		"coordinator": inspectMemoryLockLeaf(brainDir, filepath.ToSlash(filepath.Join(brainLockDirName, memoryCoordinatorLockName))),
		"privacy":     inspectMemoryLockLeaf(brainDir, filepath.ToSlash(filepath.Join(brainLockDirName, brainPrivacyLockName))),
		"inspection":  "read_only_no_lock_probe",
	}
}

func memorySchemaHealth(manifestHealth memoryManifestHealth, payload map[string]any) map[string]any {
	compiled := map[string]any{
		"brain_manifest":     brainManifestSchemaVersion,
		"source_manifest":    brainManifestSchemaVersion,
		"history_scan_cache": historyScanCacheVersion,
		"overlay":            historyShortTermVersion,
		"overlay_reconciler": historyShortTermReconcilerVersion,
		"fts":                historyFTSSchema,
		"vector_progress":    memoryVectorSchema,
		"abstract_config":    memoryConfigSchemaVersion,
		"abstract_artifact":  abstractSchemaVersion,
		"abstract_egress":    abstractEgressSchemaVersion,
		"job":                memoryJobSchemaVersion,
		"hint":               memoryHintSchemaVersion,
		"projection_receipt": projectionSchemaVersion,
		"coordinator":        memoryCoordinatorSchema,
		"cancellation":       memoryCancellationSchema,
		"migration_progress": 1,
	}
	observed := map[string]any{
		"brain_manifest": map[string]any{
			"path":                     manifestHealth.Path,
			"state":                    manifestHealth.State,
			"observed":                 manifestHealth.SchemaVersion,
			"schema_version":           manifestHealth.SchemaVersion,
			"supported":                manifestHealth.SupportedSchemaVersion,
			"supported_schema_version": manifestHealth.SupportedSchemaVersion,
			"authority":                "canonical_brain_manifest",
		},
	}
	if manifestHealth.ErrorCode != "" {
		observed["brain_manifest"].(map[string]any)["error_code"] = manifestHealth.ErrorCode
	}
	copyObservation := func(name string, health map[string]any) {
		observation := map[string]any{}
		for _, key := range []string{"path", "schema_version", "schema_versions_observed", "supported_schema_version", "reconciler_version", "supported_reconciler_version", "version", "error_code", "mixed"} {
			if value, ok := health[key]; ok {
				observation[key] = value
			}
		}
		rawSchemaState := health["schema_state"]
		rawOperational := health["state"]
		schemaState := memoryObservedStateValue(rawSchemaState)
		operational := memoryObservedStateValue(rawOperational)
		if operational == "" {
			operational = schemaState
			rawOperational = rawSchemaState
		}
		if operational == "" {
			operational = "absent"
			rawOperational = operational
		}
		observation["state"] = rawOperational
		observation["normalized_state"] = normalizeMemoryObservedState(operational)
		if schemaState != "" {
			observation["schema_state"] = rawSchemaState
		}
		if len(observation) > 0 {
			observed[name] = observation
		}
	}
	for _, source := range []struct {
		name string
		key  string
	}{
		{name: "projection_receipt", key: "projection_receipts"},
		{name: "overlay", key: "overlay"},
		{name: "fts", key: "fts"},
		{name: "vector_progress", key: "vector_progress"},
		{name: "job", key: "job_inventory"},
		{name: "source_manifest", key: "source_manifest"},
		{name: "coordinator", key: "coordinator"},
		{name: "tombstones", key: "tombstones"},
		{name: "migration_progress", key: "migration_progress"},
	} {
		if health, ok := payload[source.key].(map[string]any); ok {
			copyObservation(source.name, health)
		}
	}
	if install, ok := payload["install"].(map[string]any); ok {
		if provider, ok := install["provider_egress"].(map[string]any); ok {
			copyObservation("abstract_config", provider)
		}
		if abstracts, ok := install["abstracts"].(map[string]any); ok {
			copyObservation("abstract_artifact", abstracts)
			if egress, ok := abstracts["hosted_egress"].(map[string]any); ok {
				copyObservation("abstract_egress", egress)
			}
		}
	}
	return map[string]any{
		"compiled_capabilities": compiled,
		"observed_health":       observed,
		"state_counts":          memoryObservedSchemaStateCounts(observed),
	}
}

func normalizeMemoryObservedState(state string) string {
	switch state {
	case memoryErrStateCorrupt:
		return "corrupt"
	case memoryErrStateUnsafe:
		return "unsafe"
	case memoryErrUnsupportedVersion:
		return "unsupported"
	case memoryErrMigrationRequired:
		return "migration_required"
	default:
		return state
	}
}

func memoryObservedStateValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func memoryObservedSchemaStateCounts(observed map[string]any) map[string]int {
	counts := map[string]int{
		"current": 0, "absent": 0, "stale": 0, "corrupt": 0,
		"unsafe": 0, "unsupported": 0, "migration_required": 0,
	}
	for _, raw := range observed {
		health, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		state := normalizeMemoryObservedState(memoryObservedStateValue(health["state"]))
		if _, tracked := counts[state]; tracked {
			counts[state]++
		}
	}
	return counts
}

func appendMemoryHealthIssueBounded(issues []memoryHealthIssue, issue memoryHealthIssue) []memoryHealthIssue {
	if issue.Code == "" || len(issues) >= memoryStateIssueLimit {
		return issues
	}
	return append(issues, issue)
}

func memoryHealthIssues(issues []memoryStateIssue) []memoryHealthIssue {
	limit := len(issues)
	if limit > memoryStateIssueLimit {
		limit = memoryStateIssueLimit
	}
	out := make([]memoryHealthIssue, 0, limit)
	for _, issue := range issues[:limit] {
		path := issue.File
		switch issue.Kind {
		case "job":
			path = filepath.ToSlash(filepath.Join(memoryJobsDirRel, issue.File))
		case "job_directory":
			path = memoryJobsDirRel
		case "hint":
			path = filepath.ToSlash(filepath.Join(memoryHintsDirRel, issue.File))
		case "abstract":
			if issue.File != abstractsDirRel {
				path = filepath.ToSlash(filepath.Join(abstractsDirRel, issue.File))
			}
		}
		action := "inspect this derived state file, then retry the operation"
		switch issue.Code {
		case memoryErrUnsupportedVersion:
			action = "upgrade entire-brain; this newer file is left untouched"
		case memoryErrMigrationRequired:
			action = "run `" + setupCommandPrefix(os.LookupEnv) + " memory migrate`"
		case memoryErrQueryTooBroad:
			action = fmt.Sprintf("reduce the directory below the %d-file scan ceiling", abstractInventoryMaxFiles)
		case memoryErrStateUnsafe:
			action = "remove the symlink or non-regular entry; abstract reads fail closed"
		}
		out = append(out, memoryHealthIssue{Kind: issue.Kind, Path: path, Code: issue.Code, Version: issue.Version, Action: action})
	}
	return out
}

func projectionReceiptHealth(brainDir string, source *historySourceManifest) (projectionState, map[string]any) {
	receipts, state, err := loadProjectionStateChecked(brainDir, source)
	health := map[string]any{
		"state":   state,
		"current": state == projectionStateCurrent,
		"count":   len(receipts.Sessions),
	}
	if state != projectionStateCurrent {
		health["action"] = projectionStateAction(state)
	}
	if err != nil {
		var loadErr *projectionStateLoadError
		if errors.As(err, &loadErr) {
			health["error_code"] = loadErr.Code
			health["path"] = loadErr.Path
			if loadErr.Version != 0 {
				health["version"] = loadErr.Version
			}
		} else {
			health["error_code"] = memoryErrStateCorrupt
		}
	}
	if state == projectionStateCurrent {
		health["generated_at"] = receipts.GeneratedAt.UTC().Format(time.RFC3339)
		health["schema_version"] = receipts.SchemaVersion
		health["reconciler_version"] = receipts.ReconcilerVersion
	}
	return receipts, health
}

// memoryAggregateHealth is the shared read-only status payload used by the
// CLI status namespace and the MCP brain_status preflight.
func memoryAggregateHealth(brainDir string, source *historySourceManifest) map[string]any {
	return memoryAggregateHealthAt(brainDir, source, time.Now().UTC())
}

func memoryAggregateHealthAt(brainDir string, source *historySourceManifest, now time.Time) map[string]any {
	jobInventory := loadMemoryJobInventory(brainDir)
	jobs, jobIssues := jobInventory.Jobs, jobInventory.Issues
	hintInventory := loadMemoryHintInventory(brainDir)
	hints, hintIssues := hintInventory.Hints, hintInventory.Issues
	byState := map[string]int{}
	jobHealthDegraded := 0
	for _, job := range jobs {
		byState[job.State]++
		if len(job.HealthIssues) > 0 {
			jobHealthDegraded++
		}
	}
	receipts, receiptHealth := projectionReceiptHealth(brainDir, source)
	receiptsCurrent, _ := receiptHealth["current"].(bool)
	_, tombstoneState, tombstoneErr := loadSessionTombstonesChecked(brainDir)
	tombstones := map[string]any{
		"state":          tombstoneState.State,
		"schema_version": tombstoneState.Version,
	}
	if tombstoneErr != nil {
		var loadErr *sessionTombstoneLoadError
		if errors.As(tombstoneErr, &loadErr) {
			tombstones["error_code"] = loadErr.Code
		} else {
			tombstones["error_code"] = memoryErrStateCorrupt
		}
	}
	payload := map[string]any{
		"hints": len(hints), "jobs_by_state": byState,
		"hint_inventory_entries_observed": hintInventory.EntriesObserved,
		"hint_inventory_truncated":        hintInventory.Truncated,
		"hint_inventory_degraded":         hintInventory.Degraded,
		"job_inventory_entries_observed":  jobInventory.EntriesObserved,
		"job_inventory_entries_scanned":   jobInventory.EntriesScanned,
		"job_inventory_truncated":         jobInventory.Truncated,
		"job_inventory_scan_complete":     jobInventory.ScanComplete,
		"job_inventory_degraded":          jobInventory.Degraded,
		"jobs_health_degraded":            jobHealthDegraded,
		"receipts":                        len(receipts.Sessions), "receipts_current": receiptsCurrent,
		"projection_receipts": receiptHealth,
		"overlay":             memoryOverlayHealth(brainDir, source),
		"fts":                 memoryHistoryFTSHealth(brainDir, source),
		"job_inventory":       memoryJobInventoryHealth(jobInventory),
		"install":             memoryInstallHealth(brainDir), "migrations": detectMemoryMigrations(brainDir, source),
		"tombstones": tombstones,
	}
	if progress, present, progressErr := loadMemoryMigrationProgress(brainDir); progressErr != nil {
		payload["migration_progress"] = map[string]any{
			"state": "unavailable", "error_code": memoryErrorCode(progressErr),
			"action": "inspect the content-free migration progress file before resuming",
		}
	} else if present {
		payload["migration_progress"] = map[string]any{
			"state": "in_progress", "operation_id": progress.OperationID,
			"started_at": progress.StartedAt, "completed_artifacts": len(progress.Completed),
		}
	} else {
		payload["migration_progress"] = map[string]any{"state": "absent"}
	}
	issues := make([]memoryStateIssue, 0, len(jobIssues)+len(jobInventory.Migrations)+len(hintIssues))
	for _, issue := range append(append(append([]memoryStateIssue{}, jobIssues...), jobInventory.Migrations...), hintIssues...) {
		issues = appendMemoryStateIssue(issues, issue)
	}
	if len(issues) > 0 {
		payload["state_error"] = issues[0].Code
		totalIssues := jobInventory.IssueCount + len(jobInventory.Migrations) + len(hintIssues)
		payload["state_issue_count"] = totalIssues
		payload["state_issues"] = memoryHealthIssues(issues)
		payload["state_issues_truncated"] = totalIssues > len(issues)
	}
	if coordinator, err := loadMemoryCoordinatorState(brainDir); err == nil && coordinator.State != "" {
		coordinatorHealth := memoryCoordinatorHealth(coordinator, now)
		coordinatorHealth["state"] = coordinator.State
		coordinatorHealth["schema_version"] = coordinator.SchemaVersion
		coordinatorHealth["pid"] = coordinator.PID
		coordinatorHealth["binary_version"] = coordinator.BinaryVersion
		coordinatorHealth["started_at"] = coordinator.StartedAt
		coordinatorHealth["heartbeat_at"] = coordinator.HeartbeatAt
		coordinatorHealth["last_outcome"] = coordinator.LastOutcome
		if coordinator.LastOutcome == memoryWorkerOutcomeDegraded {
			// A degraded pass is the one worker outcome that leaves the Brain
			// quietly falling behind: the projection ran, so nothing looks
			// broken, while no session captured since then can reach it. Make
			// it an issue so `status` and `doctor` report it instead of showing
			// a healthy coordinator beside a stalled export.
			coordinatorHealth["error_code"] = memoryErrWorkerDegraded
			coordinatorHealth["action"] = "the last worker pass could not export new sessions; run `entire-brain refresh delta` in the repository to see why"
		}
		payload["coordinator"] = coordinatorHealth
	} else if err == nil {
		payload["coordinator"] = map[string]any{"state": "absent", "heartbeat_present": false, "stale": false, "action": "none"}
	} else {
		payload["coordinator"] = map[string]any{"state": "unavailable", "error_code": memoryErrorCode(err), "action": "inspect the coordinator state file; workers will recover it when safe"}
	}
	if logHealth, err := memoryWorkerLogHealth(brainDir); err == nil {
		logHealth["path"] = filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel))
		payload["worker_log"] = logHealth
	} else {
		payload["worker_log"] = map[string]any{"state": "unavailable", "path": filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel)), "max_bytes": memoryWorkerLogMaxBytes, "error_code": memoryErrorCode(err), "action": "inspect the content-free worker log path"}
	}
	payload["vector_progress"] = memoryVectorProgressHealth(brainDir, source)
	return payload
}

// memoryReadOnlyHealth classifies the manifest once, then builds every health
// section that does not depend on interpreting a supported manifest. It never
// creates directories, probes writability, acquires locks, invokes providers,
// or rewrites forward-version state.
func memoryReadOnlyHealth(brainDir string, now time.Time) memoryReadOnlyHealthSnapshot {
	manifest, manifestHealth, manifestIssue := inspectBrainManifestHealth(brainDir)
	var source *historySourceManifest
	if manifest != nil && manifestHealth.State == "current" && manifest.Sources != nil {
		source = manifest.Sources.History
	}
	payload := memoryAggregateHealthAt(brainDir, source, now)
	payload["manifest_health"] = manifestHealth
	payload["locks"] = memoryLockHealth(brainDir)
	sourceHealth := map[string]any{
		"path":                     manifestHealth.Path,
		"state":                    manifestHealth.State,
		"schema_state":             manifestHealth.State,
		"supported_schema_version": manifestHealth.SupportedSchemaVersion,
		"authority":                "canonical_brain_manifest",
		"canonical_sessions":       "unavailable",
		"history_projection":       "unavailable",
	}
	if manifestHealth.SchemaVersion > 0 {
		sourceHealth["schema_version"] = manifestHealth.SchemaVersion
	}
	if manifestHealth.ErrorCode != "" {
		sourceHealth["error_code"] = manifestHealth.ErrorCode
		sourceHealth["action"] = manifestHealth.RecommendedAction
	}
	if manifestHealth.State == "current" && manifest != nil && manifest.Sources != nil {
		sourceHealth["canonical_sessions"] = "absent"
		if manifest.Sources.Sessions != nil {
			sourceHealth["canonical_sessions"] = "current"
		}
		sourceHealth["history_projection"] = "absent"
		if manifest.Sources.History != nil {
			sourceHealth["history_projection"] = "current"
		}
	}
	payload["source_manifest"] = sourceHealth

	canonical := map[string]any{"state": "unavailable", "count": 0}
	canonical["source_manifest_path"] = manifestHealth.Path
	canonical["source_manifest_state"] = manifestHealth.State
	canonical["supported_source_manifest_schema_version"] = manifestHealth.SupportedSchemaVersion
	if manifestHealth.SchemaVersion > 0 {
		canonical["source_manifest_schema_version"] = manifestHealth.SchemaVersion
	}
	if manifestHealth.State == "absent" {
		canonical["state"] = "manifest_absent"
		canonical["action"] = "run `entire brain refresh` to capture canonical sessions"
	} else if manifest != nil && manifest.Sources != nil && manifest.Sources.Sessions != nil {
		sessions := manifest.Sources.Sessions
		canonical["state"] = "available"
		canonical["count"] = len(sessions.Sessions)
		canonical["generated_at"] = sessions.GeneratedAt.UTC().Format(time.RFC3339)
		canonical["latest_checkpoint_id"] = sessions.LatestCheckpointID
	} else if manifest != nil {
		canonical["state"] = "absent"
		canonical["action"] = "run `entire brain refresh` (sessions stage)"
	} else {
		canonical["error_code"] = manifestHealth.ErrorCode
		canonical["action"] = manifestHealth.RecommendedAction
	}
	payload["canonical_sessions"] = canonical
	payload["schemas"] = memorySchemaHealth(manifestHealth, payload)

	reconciliation := map[string]any{"state": "never", "current": false}
	if receipts, ok := payload["projection_receipts"].(map[string]any); ok {
		if current, _ := receipts["current"].(bool); current {
			reconciliation["state"] = "current"
			reconciliation["current"] = true
		}
		if generatedAt, ok := receipts["generated_at"]; ok {
			reconciliation["last_successful_at"] = generatedAt
		}
		// Normalize to a plain string. receipts["state"] holds a
		// projectionStateReadState (a named string type), and copying it in
		// raw meant every reader doing the obvious `.(string)` assertion --
		// doctor's memory_reconciliation line among them -- silently got "".
		// JSON could not tell the difference, so the payload looked correct
		// while the rendered reason vanished.
		if state, ok := receipts["state"]; ok && reconciliation["state"] == "never" {
			reconciliation["state"] = memoryObservedStateValue(state)
		}
		if code, ok := receipts["error_code"]; ok {
			reconciliation["error_code"] = memoryObservedStateValue(code)
		}
	}
	payload["reconciliation"] = reconciliation

	issues := make([]memoryHealthIssue, 0, memoryStateIssueLimit)
	hiddenIssueCount := 0
	recordIssue := func(issue memoryHealthIssue) {
		if issue.Code == "" {
			return
		}
		before := len(issues)
		issues = appendMemoryHealthIssueBounded(issues, issue)
		if len(issues) == before {
			hiddenIssueCount++
		}
	}
	if manifestIssue != nil {
		recordIssue(*manifestIssue)
	}
	if stateIssues, ok := payload["state_issues"].([]memoryHealthIssue); ok {
		for _, issue := range stateIssues {
			recordIssue(issue)
		}
		if total, ok := payload["state_issue_count"].(int); ok && total > len(stateIssues) {
			hiddenIssueCount += total - len(stateIssues)
		}
	}
	appendMapIssue := func(kind, path string, health map[string]any) {
		code, _ := health["error_code"].(string)
		if code == "" {
			return
		}
		action, _ := health["action"].(string)
		if action == "" {
			action = "inspect this content-free state leaf; status leaves it untouched"
		}
		version, _ := health["version"].(int)
		recordIssue(memoryHealthIssue{Kind: kind, Path: path, Code: code, Version: version, Action: action})
	}
	if health, ok := payload["projection_receipts"].(map[string]any); ok {
		path, _ := health["path"].(string)
		if path == "" {
			path = projectionStateRel
		}
		appendMapIssue("projection_receipt", path, health)
	}
	if health, ok := payload["coordinator"].(map[string]any); ok {
		appendMapIssue("coordinator", memoryCoordinatorStateRel, health)
	}
	if health, ok := payload["worker_log"].(map[string]any); ok {
		appendMapIssue("worker_log", memoryWorkerLogRel, health)
	}
	if health, ok := payload["vector_progress"].(map[string]any); ok {
		appendMapIssue("vector_progress", memoryVectorProgressRel, health)
	}
	if health, ok := payload["overlay"].(map[string]any); ok {
		appendMapIssue("overlay", historyShortTermPath, health)
	}
	if health, ok := payload["fts"].(map[string]any); ok {
		appendMapIssue("fts", historyFTSDBRelPath(), health)
	}
	if health, ok := payload["job_inventory"].(map[string]any); ok {
		// Per-file job issues are already included through state_issues. Keep
		// inventory-level failures (for example an unsafe directory) visible
		// even if there was no readable leaf to name.
		if count, _ := health["issue_count"].(int); count == 0 {
			appendMapIssue("job_inventory", memoryJobsDirRel, health)
		}
	}
	if health, ok := payload["tombstones"].(map[string]any); ok {
		appendMapIssue("tombstones", sessionTombstonesPath, health)
	}
	if locks, ok := payload["locks"].(map[string]any); ok {
		for _, lock := range []struct {
			name string
			rel  string
		}{
			{name: "write", rel: filepath.ToSlash(filepath.Join(brainLockDirName, brainWriteLockName))},
			{name: "coordinator", rel: filepath.ToSlash(filepath.Join(brainLockDirName, memoryCoordinatorLockName))},
			{name: "privacy", rel: filepath.ToSlash(filepath.Join(brainLockDirName, brainPrivacyLockName))},
		} {
			if health, ok := locks[lock.name].(map[string]any); ok {
				appendMapIssue("lock", lock.rel, health)
			}
		}
	}
	if install, ok := payload["install"].(map[string]any); ok {
		if provider, ok := install["provider_egress"].(map[string]any); ok {
			appendMapIssue("provider_config", memoryConfigRel, provider)
			if providerState, _ := provider["provider_state"].(string); providerState == memoryErrProviderUnavail {
				action, _ := provider["action"].(string)
				recordIssue(memoryHealthIssue{Kind: "provider", Path: memoryConfigRel, Code: memoryErrProviderUnavail, Action: action})
			}
			if blockCode, _ := provider["block_code"].(string); blockCode != "" {
				action, _ := provider["action"].(string)
				recordIssue(memoryHealthIssue{Kind: "provider_policy", Path: memoryConfigRel, Code: blockCode, Action: action})
			}
		}
		if directories, ok := install["directories"].(map[string]memoryInstallDirectoryHealth); ok {
			for _, name := range []string{"brain", "history", "work"} {
				directory := directories[name]
				code := ""
				switch directory.State {
				case "unsafe":
					code = memoryErrStateUnsafe
				case "unavailable":
					code = memoryErrStateCorrupt
				}
				if code != "" {
					recordIssue(memoryHealthIssue{Kind: "installation_directory_" + name, Path: directory.Path, Code: code, Action: directory.RecommendedAction})
				}
			}
		}
		if abstracts, ok := install["abstracts"].(map[string]any); ok {
			if abstractIssues, ok := abstracts["issues"].([]memoryHealthIssue); ok {
				for _, issue := range abstractIssues {
					recordIssue(issue)
				}
				if total, ok := abstracts["issue_count"].(int); ok && total > len(abstractIssues) {
					hiddenIssueCount += total - len(abstractIssues)
				}
			}
			if egress, ok := abstracts["hosted_egress"].(map[string]any); ok {
				if egressIssues, ok := egress["issues"].([]memoryHealthIssue); ok {
					for _, issue := range egressIssues {
						recordIssue(issue)
					}
					if total, ok := egress["issue_count"].(int); ok && total > len(egressIssues) {
						hiddenIssueCount += total - len(egressIssues)
					}
				}
			}
		}
	}
	if migrations, ok := payload["migrations"].([]memoryMigrationFinding); ok {
		for _, finding := range migrations {
			recordIssue(memoryHealthIssue{Kind: "migration", Path: finding.Path, Code: finding.State, Action: finding.Action})
		}
	}
	payload["issues"] = issues
	payload["issue_count"] = len(issues) + hiddenIssueCount
	payload["issues_truncated"] = hiddenIssueCount > 0
	return memoryReadOnlyHealthSnapshot{Manifest: manifest, ManifestHealth: manifestHealth, Payload: payload, Issues: issues}
}
