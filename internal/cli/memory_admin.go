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

// memory_admin.go is the C5 maintenance surface: repair (verify dependencies,
// smallest deterministic rebuild), rebuild (recreate disposable projections
// from canonical sessions), and migrate (upgrade derived schemas atomically,
// build-beside-then-switch, never delete first). Every mutation returns a
// versioned, content-free receipt with stable error codes.

// Stable error codes (C5 taxonomy). An empty result never stands in for any
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
	memoryMigrationProgressRel  = memoryWorkDirRel + "/migration-progress-v1.json"
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
		SchemaVersion: 1,
		OperationID:   "op:" + hex.EncodeToString(h.Sum(nil))[:16],
		Operation:     operation,
		StartedAt:     now,
		Artifacts:     []memoryReceiptArtifact{},
	}
}

// finishMemoryOperationFailure closes and emits the one failure receipt for an
// operation after its receipt identity exists. The original failure remains
// the command error; an output failure is joined instead of replacing it.
func finishMemoryOperationFailure(cmd *cobra.Command, receipt *memoryOperationReceipt, jsonOut bool, now time.Time, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("%s: memory operation failed", memoryErrStateCorrupt)
	}
	receipt.FinishedAt = now.UTC()
	if receipt.ErrorCode == "" {
		receipt.ErrorCode = memoryErrorCode(cause)
	}
	var emitErr error
	if jsonOut {
		emitErr = writeJSON(cmd, receipt)
	} else {
		emitErr = writeText(cmd, func(out io.Writer) {
			fmt.Fprintf(out, "%s failed operation_id=%s error_code=%s started_at=%s finished_at=%s artifacts=%d\n",
				receipt.Operation, receipt.OperationID, receipt.ErrorCode,
				receipt.StartedAt.UTC().Format(time.RFC3339Nano), receipt.FinishedAt.Format(time.RFC3339Nano), len(receipt.Artifacts))
		})
	}
	if emitErr != nil {
		return errors.Join(cause, fmt.Errorf("emit %s failure receipt: %w", receipt.Operation, emitErr))
	}
	return cause
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
		switch {
		case path == historyIndexPath || path == projectionStateRel ||
			(strings.HasPrefix(path, historyGenerationsDir+"/") && (strings.HasSuffix(path, "/"+historyIndexFileName) || strings.HasSuffix(path, "/"+projectionStateFileName))):
			if current[path] {
				(*artifacts)[i].NewState = "current"
			} else if state := memoryDerivedArtifactState(brainDir, path); state == "present" {
				(*artifacts)[i].NewState = "superseded_preserved"
			} else if state == "unsafe" || state == "corrupt" {
				(*artifacts)[i].NewState = "superseded_" + state
			} else {
				(*artifacts)[i].NewState = "superseded_missing"
			}
		case path == historyShortTermPath:
			(*artifacts)[i].NewState = "cleared_after_consolidation"
		case path == historyFTSDBRelPath():
			switch memoryDerivedArtifactState(brainDir, (*artifacts)[i].Path) {
			case "present":
				(*artifacts)[i].NewState = "available"
			case "absent":
				(*artifacts)[i].NewState = "absent_lazy_rebuild"
			default:
				(*artifacts)[i].NewState = "unavailable_unsafe"
			}
		default:
			(*artifacts)[i].NewState = "preserved"
		}
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
			fingerprintCurrent := source != nil && source.SessionsFingerprint == brainSessionsFingerprint(brainDir)
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
				fingerprintCurrent = postSource != nil && postSource.SessionsFingerprint == brainSessionsFingerprint(brainDir)
			}
			receipt.FinishedAt = opts.Now().UTC()
			if jsonOut {
				return writeJSON(cmd, map[string]any{
					"receipt": receipt, "verify_clean": verify.Clean,
					"verify_available": verifyAvailable, "index_current": indexCurrent,
					"receipts_current": receiptsCurrent, "fingerprint_current": fingerprintCurrent,
					"rebuilt": needsRebuild && !dryRun,
				})
			}
			return writeText(cmd, func(out io.Writer) {
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
			}
			receipt.FinishedAt = opts.Now().UTC()
			if jsonOut {
				return writeJSON(cmd, receipt)
			}
			mode := "rebuilt"
			if dryRun {
				mode = "dry-run: would rebuild"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %d disposable projection artifacts from canonical sessions\n", mode, len(receipt.Artifacts))
			return nil
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
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&progress); err != nil {
		return memoryMigrationProgress{}, true, fmt.Errorf("%s: decode migration progress: %w", memoryErrStateCorrupt, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return memoryMigrationProgress{}, true, fmt.Errorf("%s: migration progress has trailing JSON data", memoryErrStateCorrupt)
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
			receipt.FinishedAt = opts.Now().UTC()
			if blocker != nil {
				receipt.ErrorCode = blocker.State
				return fail(fmt.Errorf("%s: %s: %s", blocker.State, blocker.Path, blocker.Action))
			}
			if jsonOut {
				if err := writeJSON(cmd, map[string]any{"receipt": receipt, "findings": findings, "remaining_findings": remaining, "migrated": migrated}); err != nil {
					return err
				}
				return nil
			}
			if err := writeText(cmd, func(out io.Writer) {
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
			}); err != nil {
				return err
			}
			return nil
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
	State               string `json:"state"`
	Authority           string `json:"authority"`
	ImplementationState string `json:"implementation_state"`
	Observability       string `json:"observability"`
	RecommendedAction   string `json:"recommended_action"`
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
			health.RecommendedAction = "create the brain directory through the normal build workflow"
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
	health.State = "present_unverified"
	health.Path = path
	health.RecommendedAction = "run Entire's own version or doctor command to verify compatibility"
	return health
}

// memoryInstallHealth reports binary capabilities and directory health for
// memory status (C5 installation and integration health). It is strictly
// read-only: presence and permission-mode observations are never presented as
// proof that a later write, host adapter, or binary invocation will succeed.
func memoryInstallHealth(brainDir string) map[string]any {
	workDir := inspectMemoryInstallDirectory(brainDir, memoryWorkDirRel)
	historyDir := inspectMemoryInstallDirectory(brainDir, historyDirName)
	hostAdapter := memoryHostAdapterHealth{
		State:               "external_not_implemented",
		Authority:           "entire-cli",
		ImplementationState: "external_not_implemented",
		Observability:       "not_observable",
		RecommendedAction:   "implement and verify the capture host adapter in entire-cli; this plugin cannot install or attest it",
	}
	return map[string]any{
		"cgo_build":     brainCGOBuild,
		"entire_binary": inspectEntireBinary(),
		"directories": map[string]memoryInstallDirectoryHealth{
			"work": workDir, "history": historyDir,
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
		"abstract_provider":      memoryAbstractProviderSummary(brainDir),
		"abstracts":              memoryAbstractHealth(brainDir),
		"memory_config_schema":   memoryConfigSchemaVersion,
		"abstract_schema":        abstractSchemaVersion,
		"worker_log_path":        filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel)),
	}
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
	health := map[string]any{
		"schema_version":   abstractSchemaVersion,
		"artifacts":        inventory.Scanned,
		"entries_observed": inventory.EntriesObserved,
		"scanned":          inventory.Scanned,
		"scan_ceiling":     abstractInventoryMaxFiles,
		"scan_truncated":   inventory.Truncated,
		"scan_degraded":    inventory.Degraded,
		"issue_count":      inventory.IssueCount,
		"issues_truncated": inventory.IssueCount > len(inventory.Issues),
		"corrupt":          inventory.IssueCounts[memoryErrStateCorrupt],
		"unsafe":           inventory.IssueCounts[memoryErrStateUnsafe],
		"unsupported":      inventory.IssueCounts[memoryErrUnsupportedVersion],
		"stale":            0,
		"hosted_egress": map[string]any{
			"schema_version":     abstractEgressSchemaVersion,
			"receipts_scanned":   len(egress.Receipts),
			"entries_scanned":    egress.EntriesScanned,
			"scan_ceiling":       abstractEgressHealthMaxReceipts,
			"scan_truncated":     egress.Truncated,
			"scan_degraded":      egress.Degraded,
			"issue_count":        len(egress.Issues),
			"migration_required": len(egress.Migrations),
		},
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
	} else if provider := memoryAbstractProviderSummary(brainDir); strings.HasPrefix(provider, memoryErrProviderUnavail) {
		health["provider_state"] = memoryErrProviderUnavail
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
			action = "run `entire memory migrate`"
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

// memoryAggregateHealth is the shared read-only C5 status payload used by the
// CLI status namespace and the MCP brain_status preflight.
func memoryAggregateHealth(brainDir string, source *historySourceManifest) map[string]any {
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
	issues := make([]memoryStateIssue, 0, len(jobIssues)+len(hintIssues))
	for _, issue := range append(append([]memoryStateIssue{}, jobIssues...), hintIssues...) {
		issues = appendMemoryStateIssue(issues, issue)
	}
	if len(issues) > 0 {
		payload["state_error"] = issues[0].Code
		totalIssues := jobInventory.IssueCount + len(hintIssues)
		payload["state_issue_count"] = totalIssues
		payload["state_issues"] = memoryHealthIssues(issues)
		payload["state_issues_truncated"] = totalIssues > len(issues)
	}
	if coordinator, err := loadMemoryCoordinatorState(brainDir); err == nil {
		payload["coordinator"] = memoryCoordinatorHealth(coordinator, time.Now().UTC())
	} else {
		payload["coordinator"] = map[string]any{"state": "unavailable", "error_code": memoryErrorCode(err), "action": "inspect the coordinator state file; workers will recover it when safe"}
	}
	if logHealth, err := memoryWorkerLogHealth(brainDir); err == nil {
		payload["worker_log"] = logHealth
	} else {
		payload["worker_log"] = map[string]any{"state": "unavailable", "error_code": memoryErrorCode(err), "action": "inspect the content-free worker log path"}
	}
	if vectorProgress, present, err := loadMemoryVectorProgress(brainDir); err == nil && present {
		vectorHealth := map[string]any{"state": "pending", "model_id": vectorProgress.ModelID, "source_digest": vectorProgress.SourceDigest, "complete_source_digest": vectorProgress.CompleteSourceDigest, "pending": true, "reset_complete": vectorProgress.ResetComplete, "updated_at": vectorProgress.UpdatedAt}
		if source != nil && source.IndexDigest != "" && vectorProgress.CompleteSourceDigest == source.IndexDigest && vectorProgress.ResetComplete && !vectorProgress.Pending {
			vectorHealth["state"] = "current"
			vectorHealth["pending"] = false
		} else if source != nil && source.IndexDigest != "" && vectorProgress.SourceDigest != source.IndexDigest {
			vectorHealth["state"] = "stale"
		}
		if vectorProgress.ErrorCode != "" {
			vectorHealth["state"] = "degraded"
			vectorHealth["error_code"] = vectorProgress.ErrorCode
		}
		payload["vector_progress"] = vectorHealth
	} else if err != nil {
		payload["vector_progress"] = map[string]any{"state": "unavailable", "error_code": memoryErrorCode(err), "action": "inspect the content-free vector progress state"}
	}
	return payload
}
