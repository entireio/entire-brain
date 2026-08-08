package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// memory_cmd.go is the C3/C5 administration surface over the durable work
// record: content-free lifecycle hints (`memory notify`, the endpoint a host
// adapter calls; the adapter itself stays parked), reconciliation against the
// canonical manifest, a bounded synchronous worker, and inspect/retry/cancel
// verbs. Reconciliation remains the correctness authority: losing every hint
// and job file loses no memory.

func newMemoryCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Inspect and drive the durable conversation-projection work record (hints, jobs, receipts)",
	}
	cmd.AddCommand(newMemoryNotifyCommand(opts))
	cmd.AddCommand(newMemoryStatusCommand(opts))
	cmd.AddCommand(newMemoryReconcileCommand(opts))
	cmd.AddCommand(newMemoryJobsCommand(opts))
	cmd.AddCommand(newMemoryRetryCommand(opts))
	cmd.AddCommand(newMemoryCancelCommand(opts))
	cmd.AddCommand(newMemoryWorkerCommand(opts))
	cmd.AddCommand(newMemoryRepairCommand(opts))
	cmd.AddCommand(newMemoryRebuildCommand(opts))
	cmd.AddCommand(newMemoryMigrateCommand(opts))
	cmd.AddCommand(newMemoryConfigureCommand(opts))
	cmd.AddCommand(newMemoryAbstractCommand(opts))
	return cmd
}

// memoryWorkerLaunch is the best-effort, non-blocking worker launch used by
// notify. Injectable for tests; a launch failure never fails the host.
var memoryWorkerLaunch = func() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(exe, "memory", "worker", "--once")
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

func newMemoryNotifyCommand(opts Options) *cobra.Command {
	var event, session, branch, repoKey string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Record a content-free host lifecycle hint (session_start | checkpoint | session_end) and nudge the worker",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			// The supplied repository key must match the resolved canonical
			// brain; it can never select a path.
			if strings.TrimSpace(repoKey) != storage.Key {
				return fmt.Errorf("repo-key %q does not match the resolved repository %q", repoKey, storage.Key)
			}
			var hint memoryLifecycleHint
			if err := withBrainWriteLock(storage.BrainDir, func() error {
				var hintErr error
				hint, hintErr = writeMemoryHint(storage.BrainDir, storage.Key, session, strings.TrimSpace(branch), event, opts.Now().UTC())
				return hintErr
			}); err != nil {
				return err
			}
			warning := ""
			if err := memoryWorkerLaunch(); err != nil {
				// Durable hint plus reconciliation repairs this; the host is
				// never blocked.
				warning = "worker launch failed (reconciliation will repair): " + err.Error()
			}
			if jsonOut {
				payload := map[string]any{"recorded": hint.LastEvent, "session_id": hint.SessionID, "generation": hint.Generation}
				if warning != "" {
					payload["warning"] = warning
				}
				return writeJSON(cmd, payload)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "recorded %s hint for session %s (generation %d)\n", hint.LastEvent, hint.SessionID, hint.Generation)
			if warning != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), warning)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&event, "event", "", "Lifecycle event: session_start | checkpoint | session_end")
	cmd.Flags().StringVar(&session, "session", "", "Canonical session id (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "Captured branch when the host knows it")
	cmd.Flags().StringVar(&repoKey, "repo-key", "", "Canonical repository key; must match the resolved repository")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// reconcileMemoryJobsLocked compares the canonical manifest with the current
// projection receipts and enqueues a pending job for every session whose
// input digest is not yet represented. Existing jobs of the same identity are
// never recreated (an invalid job stays visible for repair); a newer digest
// supersedes older non-excluded jobs for the same session scope. Caller
// holds the brain write lock.
func reconcileMemoryJobsLocked(brainDir, trigger string, now time.Time) (created int, err error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return 0, err
	}
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	receipts, _ := loadProjectionState(brainDir, source)
	stones := loadSessionTombstones(brainDir)
	jobs := loadMemoryJobs(brainDir)
	byID := map[string]memoryJob{}
	for _, job := range jobs {
		byID[job.JobID] = job
	}
	// Tombstoned sessions: active jobs move to excluded before anything else.
	for _, job := range jobs {
		if _, excluded := stones.Excluded[job.SessionID]; !excluded {
			continue
		}
		switch job.State {
		case memoryJobStatePending, memoryJobStateRunning, memoryJobStateRetryable, memoryJobStateInvalid:
			if updated, terr := transitionMemoryJob(brainDir, job, memoryJobStateExcluded, now, ""); terr == nil {
				byID[updated.JobID] = updated
			}
		}
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return 0, nil
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		id := strings.TrimSpace(session.SessionID)
		if _, excluded := stones.Excluded[id]; excluded {
			continue
		}
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		digest, digestErr := sessionTranscriptDigest(brainDir, rel)
		if digestErr != nil {
			continue // unreadable source: rediscovered next reconcile
		}
		branch := strings.TrimSpace(session.Branch)
		if branch == "" {
			branch = strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
		}
		ref, _ := conversationSessionRef(manifest.RepoKey, branch, id, digest)
		if receipt, ok := receipts.receiptFor(ref); ok && receipt.InputDigest == digest {
			continue // current: receipts, not jobs, are the proof
		}
		jobID := memoryJobID(manifest.RepoKey, ref, digest, memoryJobKindProjection)
		if _, exists := byID[jobID]; exists {
			continue
		}
		// A new digest supersedes every older non-excluded job for the scope.
		for _, job := range byID {
			if job.SessionRef != ref || job.Kind != memoryJobKindProjection || job.InputDigest == digest {
				continue
			}
			switch job.State {
			case memoryJobStatePending, memoryJobStateRunning, memoryJobStateRetryable, memoryJobStateInvalid, memoryJobStateComplete:
				if updated, terr := transitionMemoryJob(brainDir, job, memoryJobStateSuperseded, now, ""); terr == nil {
					byID[updated.JobID] = updated
				}
			}
		}
		job := memoryJob{
			SchemaVersion: memoryJobSchemaVersion, JobID: jobID, Kind: memoryJobKindProjection,
			RepoKey: manifest.RepoKey, SessionID: id, SessionRef: ref, Branch: branch,
			InputDigest: digest, Trigger: trigger, State: memoryJobStatePending,
			CreatedAt: now, AvailableAt: now,
		}
		if err := saveMemoryJob(brainDir, job); err != nil {
			return created, err
		}
		byID[jobID] = job
		created++
	}
	return created, nil
}

type memoryWorkerStats struct {
	JobsCreated   int `json:"jobs_created"`
	JobsCompleted int `json:"jobs_completed"`
	JobsRetried   int `json:"jobs_retried"`
	HintsConsumed int `json:"hints_consumed"`
}

// runMemoryWorkerOnce is the bounded synchronous worker pass: reconcile,
// claim runnable deterministic jobs, run one consolidation that absorbs them
// all, then settle each job against the published receipts and consume
// satisfied hints. Caller holds the brain write lock.
func runMemoryWorkerOnce(brainDir string, now time.Time) (memoryWorkerStats, error) {
	var stats memoryWorkerStats
	created, err := reconcileMemoryJobsLocked(brainDir, "worker", now)
	if err != nil {
		return stats, err
	}
	stats.JobsCreated = created
	runnable := runnableMemoryJobs(loadMemoryJobs(brainDir), now)
	if len(runnable) > 0 {
		claimed := make([]memoryJob, 0, len(runnable))
		for _, job := range runnable {
			running, terr := transitionMemoryJob(brainDir, job, memoryJobStateRunning, now, "")
			if terr != nil {
				continue
			}
			claimed = append(claimed, running)
		}
		// One full consolidation covers every pending projection: it rebuilds
		// the index from the canonical sessions and publishes the receipts.
		_, buildErr := writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
		manifest, manifestErr := loadBrainManifest(brainDir)
		var receipts projectionState
		if buildErr == nil && manifestErr == nil && manifest.Sources != nil {
			receipts, _ = loadProjectionState(brainDir, manifest.Sources.History)
		}
		stones := loadSessionTombstones(brainDir)
		for _, job := range claimed {
			switch {
			case buildErr != nil:
				if _, terr := transitionMemoryJob(brainDir, job, memoryJobStateRetryable, now, "consolidation failed: "+buildErr.Error()); terr == nil {
					stats.JobsRetried++
				}
			case func() bool { _, excluded := stones.Excluded[job.SessionID]; return excluded }():
				_, _ = transitionMemoryJob(brainDir, job, memoryJobStateExcluded, now, "")
			default:
				receipt, ok := receipts.receiptFor(job.SessionRef)
				if ok && receipt.InputDigest == job.InputDigest {
					if _, terr := transitionMemoryJob(brainDir, job, memoryJobStateComplete, now, ""); terr == nil {
						stats.JobsCompleted++
					}
				} else if _, terr := transitionMemoryJob(brainDir, job, memoryJobStateRetryable, now, "source not represented after rebuild (changed or unreadable)"); terr == nil {
					stats.JobsRetried++
				}
			}
		}
	}
	// Consume hints whose every matching canonical scope is represented by a
	// current receipt or a tombstone; generation-checked so a racing event is
	// never lost.
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return stats, err
	}
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	receipts, _ := loadProjectionState(brainDir, source)
	stones := loadSessionTombstones(brainDir)
	for _, hint := range loadMemoryHints(brainDir) {
		if _, excluded := stones.Excluded[hint.SessionID]; excluded {
			if removeMemoryHint(brainDir, hint) == nil {
				stats.HintsConsumed++
			}
			continue
		}
		scopes := 0
		satisfied := true
		if manifest.Sources != nil && manifest.Sources.Sessions != nil {
			for _, session := range manifest.Sources.Sessions.Sessions {
				if strings.TrimSpace(session.SessionID) != hint.SessionID {
					continue
				}
				branch := strings.TrimSpace(session.Branch)
				if branch == "" {
					branch = strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
				}
				if hint.Branch != "" && branch != hint.Branch {
					continue
				}
				scopes++
				rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
				digest, digestErr := sessionTranscriptDigest(brainDir, rel)
				if digestErr != nil {
					satisfied = false
					break
				}
				ref, _ := conversationSessionRef(manifest.RepoKey, branch, hint.SessionID, digest)
				receipt, ok := receipts.receiptFor(ref)
				if !ok || receipt.InputDigest != digest {
					satisfied = false
					break
				}
			}
		}
		// A hint for a session the manifest does not expose yet stays until a
		// canonical source appears; an ambiguous branchless hint stays
		// visible while scoped reconciliation proceeds independently.
		if scopes == 0 || !satisfied {
			continue
		}
		if removeMemoryHint(brainDir, hint) == nil {
			stats.HintsConsumed++
		}
	}
	return stats, nil
}

func newMemoryWorkerCommand(opts Options) *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:    "worker",
		Short:  "Run one bounded deterministic projection pass (hidden; hints/watch/startup launch it)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !once {
				return fmt.Errorf("only --once is supported")
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			var stats memoryWorkerStats
			if err := withBrainWriteLock(storage.BrainDir, func() error {
				var runErr error
				stats, runErr = runMemoryWorkerOnce(storage.BrainDir, opts.Now().UTC())
				return runErr
			}); err != nil {
				return err
			}
			return writeJSON(cmd, stats)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "Run one pass and exit")
	return cmd
}

func newMemoryReconcileCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Compare canonical sessions with projection receipts and enqueue missing work",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			var created int
			if err := withBrainWriteLock(storage.BrainDir, func() error {
				var rerr error
				created, rerr = reconcileMemoryJobsLocked(storage.BrainDir, "manual", opts.Now().UTC())
				return rerr
			}); err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"jobs_created": created})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reconcile enqueued %d jobs\n", created)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newMemoryStatusCommand(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report hint, job, and projection-receipt health",
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
			var source *historySourceManifest
			if manifest.Sources != nil {
				source = manifest.Sources.History
			}
			receipts, receiptsOK := loadProjectionState(brainDir, source)
			jobs := loadMemoryJobs(brainDir)
			byState := map[string]int{}
			for _, job := range jobs {
				byState[job.State]++
			}
			payload := map[string]any{
				"hints":            len(loadMemoryHints(brainDir)),
				"jobs_by_state":    byState,
				"receipts":         len(receipts.Sessions),
				"receipts_current": receiptsOK,
				"install":          memoryInstallHealth(brainDir),
				"migrations":       detectMemoryMigrations(brainDir, source),
			}
			if receiptsOK {
				payload["receipts_generated_at"] = receipts.GeneratedAt.UTC().Format(time.RFC3339)
			}
			if jsonOut {
				return writeJSON(cmd, payload)
			}
			return writeText(cmd, func(out io.Writer) {
				fmt.Fprintf(out, "hints: %d\n", payload["hints"])
				fmt.Fprintf(out, "receipts: %d (current: %v)\n", len(receipts.Sessions), receiptsOK)
				for state, count := range byState {
					fmt.Fprintf(out, "jobs %s: %d\n", state, count)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newMemoryJobsCommand(opts Options) *cobra.Command {
	var state string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List content-free work metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			jobs := loadMemoryJobs(storage.BrainDir)
			if state != "" {
				kept := jobs[:0:0]
				for _, job := range jobs {
					if job.State == state {
						kept = append(kept, job)
					}
				}
				jobs = kept
			}
			if jobs == nil {
				jobs = []memoryJob{}
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"jobs": jobs})
			}
			return writeText(cmd, func(out io.Writer) {
				for _, job := range jobs {
					fmt.Fprintf(out, "%-16s %s  session %s  attempt %d  %s\n", job.State, job.JobID, job.SessionID, job.Attempt, job.Error)
				}
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "Filter by job state")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func memoryJobByID(brainDir, jobID string) (memoryJob, error) {
	for _, job := range loadMemoryJobs(brainDir) {
		if job.JobID == jobID {
			return job, nil
		}
	}
	return memoryJob{}, fmt.Errorf("job %s not found", jobID)
}

func newMemoryRetryCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retry <job-id>",
		Short: "Move an eligible failed or invalid job back to pending (attempt history retained)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return withBrainWriteLock(storage.BrainDir, func() error {
				job, err := memoryJobByID(storage.BrainDir, args[0])
				if err != nil {
					return err
				}
				if job.State != memoryJobStateRetryable && job.State != memoryJobStateInvalid {
					return fmt.Errorf("job %s is %s; only retryable_error or invalid jobs can be retried", job.JobID, job.State)
				}
				job.AvailableAt = opts.Now().UTC()
				_, err = transitionMemoryJob(storage.BrainDir, job, memoryJobStatePending, opts.Now().UTC(), "")
				return err
			})
		},
	}
	return cmd
}

func newMemoryCancelCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel eligible pending or failed work",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return withBrainWriteLock(storage.BrainDir, func() error {
				job, err := memoryJobByID(storage.BrainDir, args[0])
				if err != nil {
					return err
				}
				switch job.State {
				case memoryJobStatePending, memoryJobStateRetryable, memoryJobStateInvalid:
					_, err = transitionMemoryJob(storage.BrainDir, job, memoryJobStateCancelled, opts.Now().UTC(), "")
					return err
				default:
					return fmt.Errorf("job %s is %s and cannot be cancelled", job.JobID, job.State)
				}
			})
		},
	}
	return cmd
}
