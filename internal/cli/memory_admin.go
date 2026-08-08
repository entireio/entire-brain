package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	memoryErrLockBusy           = "memory_lock_busy"
	memoryErrSourceStale        = "memory_source_stale"
	memoryErrCancelled          = "memory_cancelled"
	memoryErrMigrationRequired  = "memory_migration_required"
	memoryErrUnsupportedVersion = "memory_unsupported_version"
	memoryErrQueryTooBroad      = "memory_query_too_broad"
	memoryErrInputTooLarge      = "memory_input_too_large"
	memoryErrPrivacyExcluded    = "memory_privacy_excluded"
	memoryErrIdentityAmbiguous  = "memory_identity_ambiguous"
	memoryErrProviderUnavail    = "memory_provider_unavailable"
)

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
	}
}

// memoryDerivedArtifactStates reports the current state of each disposable
// derived artifact for receipts and dry-runs.
func memoryDerivedArtifactStates(brainDir string) []memoryReceiptArtifact {
	var artifacts []memoryReceiptArtifact
	state := func(rel string) string {
		if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(rel))); err == nil {
			return "present"
		}
		return "absent"
	}
	for _, rel := range []string{
		historyIndexPath,
		historyFTSDBRelPath(),
		historyShortTermPath,
		projectionStateRel,
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
		filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable)),
	} {
		s := state(rel)
		artifacts = append(artifacts, memoryReceiptArtifact{Path: rel, PriorState: s, NewState: s})
	}
	return artifacts
}

func newMemoryRepairCommand(opts Options) *cobra.Command {
	var dryRun, jsonOut bool
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
			now := opts.Now().UTC()
			receipt := newMemoryOperationReceipt("repair", now)
			receipt.DryRun = dryRun
			receipt.Artifacts = memoryDerivedArtifactStates(brainDir)
			// Dependency verification: privacy invariants plus receipt
			// currentness decide whether any rebuild is needed at all.
			verify, err := verifySessionPrivacy(brainDir)
			if err != nil {
				return err
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			var source *historySourceManifest
			if manifest.Sources != nil {
				source = manifest.Sources.History
			}
			_, receiptsCurrent := loadProjectionState(brainDir, source)
			fingerprintCurrent := source != nil && source.SessionsFingerprint == brainSessionsFingerprint(brainDir)
			needsRebuild := !verify.Clean || !receiptsCurrent || !fingerprintCurrent
			if needsRebuild && !dryRun {
				if err := withBrainWriteLock(brainDir, func() error {
					_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
					return err
				}); err != nil {
					receipt.ErrorCode = memoryErrStateCorrupt
					_ = writeJSON(cmd, receipt)
					return err
				}
				for i := range receipt.Artifacts {
					receipt.Artifacts[i].NewState = "rebuilt"
				}
			}
			receipt.FinishedAt = opts.Now().UTC()
			if jsonOut {
				return writeJSON(cmd, map[string]any{
					"receipt": receipt, "verify_clean": verify.Clean,
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
				fmt.Fprintf(out, "repair: %s the history projection (verify_clean=%v receipts_current=%v fingerprint_current=%v)\n", mode, verify.Clean, receiptsCurrent, fingerprintCurrent)
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what repair would do without changing anything")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newMemoryRebuildCommand(opts Options) *cobra.Command {
	var all, dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Recreate the disposable projections from the canonical captured sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all {
				return fmt.Errorf("pass --all (per-scope rebuild resolves through `memory reconcile` + the worker; the full rebuild is the recovery primitive)")
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			brainDir := storage.BrainDir
			now := opts.Now().UTC()
			receipt := newMemoryOperationReceipt("rebuild", now)
			receipt.DryRun = dryRun
			receipt.Artifacts = memoryDerivedArtifactStates(brainDir)
			if !dryRun {
				if err := withBrainWriteLock(brainDir, func() error {
					// Delete-then-rebuild is safe here because every artifact
					// regenerates from the canonical sessions in this same
					// locked pass; the index/manifest writes stay atomic.
					for _, rel := range []string{historyFTSDBRelPath(),
						filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable)),
						filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, conversationVecStoreFileNamePortable))} {
						if err := removeSQLiteStoreFiles(filepath.Join(brainDir, filepath.FromSlash(rel))); err != nil {
							return fmt.Errorf("%s: delete %s: %w", memoryErrStateCorrupt, rel, err)
						}
					}
					_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
					return err
				}); err != nil {
					receipt.ErrorCode = memoryErrStateCorrupt
					_ = writeJSON(cmd, receipt)
					return err
				}
				for i := range receipt.Artifacts {
					receipt.Artifacts[i].NewState = "rebuilt"
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

// detectMemoryMigrations inspects derived schema versions. Disposable formats
// migrate by rebuilding beside the old artifact and atomically switching
// (that is exactly what the existing atomic writers do); unknown NEWER
// versions are read-only errors that name the required action.
func detectMemoryMigrations(brainDir string, source *historySourceManifest) []memoryMigrationFinding {
	var findings []memoryMigrationFinding
	if overlay, state := loadHistoryShortTermRaw(brainDir); state == shortTermStateCurrent {
		switch {
		case overlay.Version > historyShortTermVersion:
			findings = append(findings, memoryMigrationFinding{Path: historyShortTermPath, State: memoryErrUnsupportedVersion, Action: "newer-version overlay is read-only for this binary; upgrade the binary or delete the file to rebuild"})
		case overlay.Version < historyShortTermVersion || overlay.ReconcilerVersion != historyShortTermReconcilerVersion:
			findings = append(findings, memoryMigrationFinding{Path: historyShortTermPath, State: memoryErrMigrationRequired, Action: "rebuild the overlay (refresh delta)"})
		}
	}
	if source != nil && source.ProjectionStateDigest == "" {
		findings = append(findings, memoryMigrationFinding{Path: projectionStateRel, State: memoryErrMigrationRequired, Action: "publish projection receipts (full refresh)"})
	}
	return findings
}

func newMemoryMigrateCommand(opts Options) *cobra.Command {
	var dryRun, jsonOut bool
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
			now := opts.Now().UTC()
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			var source *historySourceManifest
			if manifest.Sources != nil {
				source = manifest.Sources.History
			}
			findings := detectMemoryMigrations(brainDir, source)
			receipt := newMemoryOperationReceipt("migrate", now)
			receipt.DryRun = dryRun
			migrated := 0
			if !dryRun {
				for _, finding := range findings {
					if finding.State != memoryErrMigrationRequired {
						continue // unsupported-newer stays read-only
					}
					if err := withBrainWriteLock(brainDir, func() error {
						switch finding.Path {
						case historyShortTermPath:
							_, err := buildHistoryShortTermLocked(brainDir, now)
							return err
						case projectionStateRel:
							_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
							return err
						}
						return nil
					}); err != nil {
						receipt.ErrorCode = memoryErrStateCorrupt
						_ = writeJSON(cmd, receipt)
						return err
					}
					receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{Path: finding.Path, PriorState: finding.State, NewState: "current"})
					migrated++
				}
			}
			receipt.FinishedAt = opts.Now().UTC()
			if jsonOut {
				return writeJSON(cmd, map[string]any{"receipt": receipt, "findings": findings, "migrated": migrated})
			}
			return writeText(cmd, func(out io.Writer) {
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
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// memoryInstallHealth reports binary capabilities and directory health for
// memory status (C5 installation and integration health).
func memoryInstallHealth(brainDir string) map[string]any {
	writable := func(rel string) bool {
		dir := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false
		}
		probe := filepath.Join(dir, ".probe")
		if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
			return false
		}
		_ = os.Remove(probe)
		return true
	}
	return map[string]any{
		"cgo_build":              brainCGOBuild,
		"work_dir_writable":      writable(memoryWorkDirRel),
		"history_dir_writable":   writable(historyDirName),
		"overlay_schema_version": historyShortTermVersion,
		"job_schema_version":     memoryJobSchemaVersion,
		"receipt_schema_version": projectionSchemaVersion,
		"reconciler_version":     historyShortTermReconcilerVersion,
		"fts_schema":             historyFTSSchema,
		"filtered_scan_ceiling":  historyFTSFilteredScanCeiling,
		"get_batch_ceiling":      maxGetBatchIDs,
		"host_adapter":           "not installed (hook contract parked)",
		"abstract_provider":      memoryAbstractProviderSummary(brainDir),
	}
}

func memoryAbstractProviderSummary(brainDir string) string {
	config := loadMemoryConfig(brainDir)
	if !config.Abstracts.Enabled {
		return "disabled"
	}
	if strings.TrimSpace(config.Abstracts.Provider) == "" {
		return "enabled but no provider configured"
	}
	summary := config.Abstracts.Provider
	if config.Abstracts.HostedEgressAllowed {
		summary += " (hosted egress allowed)"
	}
	return summary
}
