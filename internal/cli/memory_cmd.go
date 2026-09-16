package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// memory_cmd.go is the administration surface over the durable work
// record: content-free lifecycle hints (`memory notify`, the endpoint
// implemented host adapters call across the external Entire CLI boundary),
// reconciliation against the canonical manifest, a bounded synchronous
// worker, and inspect/retry/cancel verbs. Reconciliation remains the
// correctness authority: losing every hint and job file loses no memory.

func newMemoryCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Manage conversation indexing and background jobs",
		Long:  "Inspect and drive the durable conversation-projection work record (hints, jobs, receipts)",
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

// memoryWorkerMaxLeaseSleep bounds how long a scheduled worker may hold the
// per-Brain coordinator while it is only WAITING. Taking the lease before the
// sleep is what keeps repeated notifications from piling up detached sleepers,
// but holding it for the WHOLE wait froze the Brain: nextMemoryWorkerDelay
// returns the time until the earliest pending job, a backed-off retry puts that
// nearly two hours out, and for that whole window every watch tick, session hook
// and MCP nudge got `already_active` and did nothing. A long wait is now served
// WITHOUT the lease and only its last minute with it, so one backed-off job can
// no longer block unrelated work.
//
// It is a variable only so tests can exercise the boundary without sleeping for
// production time.
var memoryWorkerMaxLeaseSleep = time.Minute

const (
	// A failed prepass reschedules itself. These bound that chain. Without a
	// bound it ran at a fixed one-minute interval forever: a permanently broken
	// delta export (a repository where `entire` is disabled, say) produced 1285
	// consecutive failures across 21.6 unbroken hours on one real machine.
	memoryWorkerPrepassRetryBase     = time.Minute
	memoryWorkerPrepassRetryCeiling  = 30 * time.Minute
	memoryWorkerPrepassRetryAttempts = 8

	// memoryWorkerOutcomeDegraded is the coordinator outcome for a pass that
	// completed its projection work but could not export new sessions. It is
	// neither "complete" (the pass did not do what it was launched to do) nor
	// "failed" (the projection lanes ran). Before it existed a failed prepass
	// left last_outcome="complete", so the health surfaces reported success
	// while the failures accumulated.
	memoryWorkerOutcomeDegraded = "degraded"
)

// memoryWorkerPrepassRetry returns the delay before the next attempt in a
// prepass-failure relaunch chain, and whether the chain continues at all.
// failures counts the consecutive failures INCLUDING the one just observed, so
// the schedule is 1m, 2m, 4m, 8m, 16m, 30m, 30m and then give up — about two
// hours of retries rather than a day and a half. A chain gives up silently only
// in the sense that it stops relaunching: the degraded outcome and the worker
// log both record why, and any new lifecycle event starts a fresh chain,
// because an external event is new evidence that the condition may have changed.
func memoryWorkerPrepassRetry(failures int) (time.Duration, bool) {
	if failures < 1 || failures >= memoryWorkerPrepassRetryAttempts {
		return 0, false
	}
	delay := memoryWorkerPrepassRetryBase
	for i := 1; i < failures; i++ {
		delay *= 2
		if delay >= memoryWorkerPrepassRetryCeiling {
			return memoryWorkerPrepassRetryCeiling, true
		}
	}
	return delay, true
}

// memoryWorkerLaunch is the best-effort, non-blocking worker launch used by
// notify. Injectable for tests; a launch failure never fails the host.
var memoryWorkerLaunch = func(repoDir string) error {
	return memoryWorkerLaunchAfter(repoDir, 0, 0)
}

var memoryWorkerLaunchAfter = func(repoDir string, delay time.Duration, prepassFailures int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"memory", "worker", "--once"}
	if delay > 0 {
		args = append(args, "--delay", delay.String())
	}
	if prepassFailures > 0 {
		args = append(args, "--prepass-failures", strconv.Itoa(prepassFailures))
	}
	command := exec.Command(exe, args...)
	command.Dir = repoDir
	command.Env = memoryWorkerChildEnvironment(os.Environ(), repoDir)
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return err
	}
	// Reap the child instead of releasing the handle. Process.Release() drops
	// the Go-side handle but the process stays this process's child at the OS
	// level, and the Go runtime installs no SIGCHLD reaper, so every launch
	// left a <defunct> entry for the parent's whole lifetime. A long-lived
	// `entire brain watch` or MCP server launches one worker per tick with changes
	// and accumulated a PID each time, up to RLIMIT_NPROC. Waiting in a
	// goroutine keeps the launch non-blocking and does not tie the child's
	// lifetime to ours: if this process exits first, the child is reparented
	// and keeps running exactly as before.
	go func() { _ = command.Wait() }()
	return nil
}

// memoryAbstractCancellationPollInterval bounds how long a running abstract
// provider can miss a durable cancellation request. It is a variable only so
// race tests can prove cancellation without sleeping for production time.
var memoryAbstractCancellationPollInterval = 5 * time.Second

// memoryWorkerChildEnvironment replaces inherited routing values instead of
// appending duplicates. Workspace/watch processes can be bound to repo A while
// launching a worker for repo B; the child must resolve only B's Brain and work
// queue. Windows environment keys are case-insensitive.
func memoryWorkerChildEnvironment(environ []string, repoDir string) []string {
	replacements := []struct{ key, value string }{
		{key: envRepoRoot, value: repoDir},
		{key: memoryWorkerOriginEnv, value: "1"},
	}
	out := make([]string, 0, len(environ)+len(replacements))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		replaced := false
		for _, replacement := range replacements {
			if key == replacement.key || (runtime.GOOS == "windows" && strings.EqualFold(key, replacement.key)) {
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	for _, replacement := range replacements {
		out = append(out, replacement.key+"="+replacement.value)
	}
	return out
}

var memoryWorkerPrepass = func(ctx context.Context, opts Options, repoDir string) error {
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	cmd := &cobra.Command{Use: "memory-worker-prepass"}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_, err := runRefreshDeltaStats(cmd, perRepo, true)
	return err
}

// recordMemoryLifecycleAndLaunch is the host/startup/watch integration seam.
// It durably records the content-free event before best-effort launch, so a
// launch failure can never lose work or fail the host lifecycle operation.
func recordMemoryLifecycleAndLaunch(brainDir, repoDir, repoKey, sessionID, branch, event string, now time.Time) (memoryLifecycleHint, string, error) {
	var hint memoryLifecycleHint
	err := withBrainWriteLock(brainDir, func() error {
		var writeErr error
		hint, writeErr = writeMemoryHint(brainDir, repoKey, sessionID, strings.TrimSpace(branch), event, now)
		return writeErr
	})
	if err != nil {
		return memoryLifecycleHint{}, "", err
	}
	if err := memoryWorkerLaunch(repoDir); err != nil {
		return hint, "worker launch failed (reconciliation will repair): " + err.Error(), nil
	}
	return hint, "", nil
}

func reconcileMemoryAndLaunch(ctx context.Context, opts Options, repoDir, trigger string) (int, string, error) {
	perRepo := opts
	perRepo.Env.RepoRoot = repoDir
	storage, err := resolveSessionsBrain(ctx, perRepo)
	if err != nil {
		return 0, "", err
	}
	created := 0
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	created, err = reconcileMemoryJobs(ctx, storage.BrainDir, trigger, now().UTC())
	if err != nil {
		return created, "", err
	}
	if err := memoryWorkerLaunch(repoDir); err != nil {
		return created, "worker launch failed (reconciliation will repair): " + err.Error(), nil
	}
	return created, "", nil
}

func nudgeMemoryAtStartup(ctx context.Context, opts Options) {
	if memoryWorkerOrigin() {
		return
	}
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil || !local {
		return
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(storage.BrainDir, exportManifestFileName)); err != nil {
		return
	}
	// Process start is non-blocking and the worker owns reconciliation. Do not
	// hash transcripts on the MCP startup path; the launched worker performs the
	// bounded prepass and precomputes reconciliation outside its write lock.
	_ = memoryWorkerLaunch(repoDir)
}

func newMemoryNotifyCommand(opts Options) *cobra.Command {
	var event, session, branch, repoKey string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "notify",
		Short: "Record a content-free host lifecycle hint (session_start | checkpoint | session_end) and nudge the worker",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if memoryWorkerOrigin() {
				if jsonOut {
					return writeJSON(cmd, map[string]any{"ignored": true, "reason": "worker_origin"})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "ignored memory notification from worker origin")
				return nil
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			repoDir, local, err := resolveLocalTargetRepoDir(cmd.Context(), opts.Runner, agentSurfaceTarget(opts, nil))
			if err != nil {
				return err
			}
			if !local {
				return fmt.Errorf("memory worker requires a local repository")
			}
			// Brain resolution is the repository-identity authority. A host may
			// omit the redundant key; when it supplies one, retain strict equality
			// validation so the value can never select or redirect storage.
			suppliedRepoKey := strings.TrimSpace(repoKey)
			if suppliedRepoKey != "" && suppliedRepoKey != storage.Key {
				return fmt.Errorf("repo-key %q does not match the resolved repository %q", repoKey, storage.Key)
			}
			hint, warning, err := recordMemoryLifecycleAndLaunch(storage.BrainDir, repoDir, storage.Key, session, branch, event, opts.Now().UTC())
			if err != nil {
				return err
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
	cmd.Flags().StringVar(&repoKey, "repo-key", "", "Optional canonical repository key; when supplied, must match the resolved repository")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// reconcileMemoryJobsLocked compares the canonical manifest with the current
// projection receipts and enqueues a pending job for every session whose
// input digest is not yet represented. Existing jobs of the same identity are
// never recreated (an invalid job stays visible for repair); a newer digest
// supersedes older non-excluded jobs for the same session scope. Caller
// holds the brain write lock.
type memoryTranscriptDigest struct {
	digest string
	err    error
}

type memoryReconcileSnapshot struct {
	manifestIdentity string
	digests          map[string]memoryTranscriptDigest
}

var memoryReconcileTranscriptDigest = sessionTranscriptDigest

func memoryManifestIdentity(manifest *exportManifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func prepareMemoryReconcileSnapshot(ctx context.Context, brainDir string) (*memoryReconcileSnapshot, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	identity, err := memoryManifestIdentity(manifest)
	if err != nil {
		return nil, err
	}
	snapshot := &memoryReconcileSnapshot{manifestIdentity: identity, digests: map[string]memoryTranscriptDigest{}}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return snapshot, nil
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		if _, exists := snapshot.digests[rel]; exists {
			continue
		}
		digest, digestErr := memoryReconcileTranscriptDigest(brainDir, rel)
		snapshot.digests[rel] = memoryTranscriptDigest{digest: digest, err: digestErr}
	}
	return snapshot, nil
}

func reconcileMemoryJobs(ctx context.Context, brainDir, trigger string, now time.Time) (int, error) {
	return reconcileMemoryJobsForAdmin(ctx, brainDir, trigger, now, false, nil)
}

// reconcileMemoryJobsForAdmin shares the correctness path used by the worker
// while optionally collecting a content-free operation receipt. Dry-run calls
// the same decision logic but never acquires the filesystem write lock and
// never invokes a state writer, so even its coordination footprint is read-only.
func reconcileMemoryJobsForAdmin(ctx context.Context, brainDir, trigger string, now time.Time, dryRun bool, receipt *memoryOperationReceipt) (int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, err := prepareMemoryReconcileSnapshot(ctx, brainDir)
		if err != nil {
			return 0, err
		}
		created := 0
		apply := func() error {
			var applyErr error
			created, applyErr = reconcileMemoryJobsLockedDetailed(brainDir, trigger, now, snapshot, dryRun, receipt)
			return applyErr
		}
		if dryRun {
			err = apply()
		} else {
			err = withBrainWriteLock(brainDir, apply)
		}
		if err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) {
			return created, err
		}
	}
	return 0, fmt.Errorf("%s: manifest changed during reconciliation", memoryErrSourceStale)
}

func reconcileMemoryJobsLocked(brainDir, trigger string, now time.Time, snapshot *memoryReconcileSnapshot) (created int, err error) {
	return reconcileMemoryJobsLockedDetailed(brainDir, trigger, now, snapshot, false, nil)
}

func reconcileMemoryJobsLockedDetailed(brainDir, trigger string, now time.Time, snapshot *memoryReconcileSnapshot, dryRun bool, receipt *memoryOperationReceipt) (created int, err error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return 0, err
	}
	identity, err := memoryManifestIdentity(manifest)
	if err != nil {
		return 0, err
	}
	if snapshot == nil || identity != snapshot.manifestIdentity {
		return 0, fmt.Errorf("%s: manifest changed during reconciliation", memoryErrSourceStale)
	}
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	receipts, receiptState, receiptErr := loadProjectionStateChecked(brainDir, source)
	if receiptState == projectionStateUnsupported || receiptState == projectionStateUnsafe {
		return 0, receiptErr
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return 0, err
	}
	inventory := loadMemoryJobInventory(brainDir)
	if err := memoryJobEnumerationError(inventory.Issues); err != nil {
		return 0, err
	}
	jobs := inventory.Jobs
	byID := map[string]memoryJob{}
	for _, job := range jobs {
		byID[job.JobID] = job
	}
	recordTransition := func(job memoryJob, state string) (memoryJob, error) {
		artifactIndex := -1
		if receipt != nil {
			artifactIndex = len(receipt.Artifacts)
			newState := state
			if !dryRun {
				newState = "write_outcome_unknown"
			}
			receipt.JobIDs = append(receipt.JobIDs, job.JobID)
			receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
				Path: memoryJobRel(job.JobID), PriorState: job.State, NewState: newState,
			})
		}
		if dryRun {
			job.State = state
			return job, nil
		}
		updated, transitionErr := transitionMemoryJob(brainDir, job, state, now, "")
		if transitionErr == nil && artifactIndex >= 0 {
			receipt.Artifacts[artifactIndex].NewState = state
		}
		return updated, transitionErr
	}
	recordCreate := func(job memoryJob) error {
		artifactIndex := -1
		if receipt != nil {
			artifactIndex = len(receipt.Artifacts)
			newState := job.State
			if !dryRun {
				newState = "write_outcome_unknown"
			}
			receipt.JobIDs = append(receipt.JobIDs, job.JobID)
			receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
				Path: memoryJobRel(job.JobID), PriorState: "absent", NewState: newState,
			})
		}
		if dryRun {
			return nil
		}
		writeErr := saveMemoryJob(brainDir, job)
		if writeErr == nil && artifactIndex >= 0 {
			receipt.Artifacts[artifactIndex].NewState = job.State
		}
		return writeErr
	}
	recordProofCompletion := func(job memoryJob) (memoryJob, error) {
		var cancellation *memoryCancellationRequest
		if receipt != nil {
			var cancellationErr error
			cancellation, cancellationErr = loadMemoryCancellationRequest(brainDir, job.JobID)
			if cancellationErr != nil {
				return job, cancellationErr
			}
		}
		artifactIndex := -1
		cancellationArtifactIndex := -1
		if receipt != nil {
			artifactIndex = len(receipt.Artifacts)
			newState := memoryJobStateComplete
			if !dryRun {
				newState = "write_outcome_unknown"
			}
			receipt.JobIDs = append(receipt.JobIDs, job.JobID)
			receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
				Path: memoryJobRel(job.JobID), PriorState: job.State, NewState: newState,
			})
			if cancellation != nil {
				cancellationArtifactIndex = len(receipt.Artifacts)
				cancellationState := "absent"
				if !dryRun {
					cancellationState = "write_outcome_unknown"
				}
				receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
					Path: memoryCancellationRel(job.JobID), PriorState: "requested", NewState: cancellationState,
				})
			}
		}
		if dryRun {
			job.State = memoryJobStateComplete
			return job, nil
		}
		updated, completeErr := completeMemoryJobFromProof(brainDir, job, now)
		if completeErr == nil && artifactIndex >= 0 {
			receipt.Artifacts[artifactIndex].NewState = memoryJobStateComplete
			if cancellationArtifactIndex >= 0 {
				receipt.Artifacts[cancellationArtifactIndex].NewState = "absent"
			}
		}
		return updated, completeErr
	}
	// Tombstoned sessions: active jobs move to excluded before anything else.
	for _, job := range jobs {
		if _, excluded := stones.Excluded[job.SessionID]; !excluded {
			continue
		}
		switch job.State {
		case memoryJobStatePending, memoryJobStateRunning, memoryJobStateRetryable, memoryJobStateInvalid:
			updated, terr := recordTransition(job, memoryJobStateExcluded)
			if terr != nil {
				return created, terr
			}
			byID[updated.JobID] = updated
		}
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return 0, nil
	}
	for _, session := range manifest.Sources.Sessions.Sessions {
		id := strings.TrimSpace(session.SessionID)
		if id == "" {
			// Mirror buildProjectionReceiptsFromSnapshot, which skips sessions
			// with no canonical receipt identity. Enqueueing one here created a
			// job the receipt builder can never satisfy: the worker rebuilt and
			// republished the whole history projection, found no receipt, and
			// settled retryable_error, once per attempt, before parking the job
			// permanently and leaving `memory status` degraded forever.
			continue
		}
		if _, excluded := stones.Excluded[id]; excluded {
			continue
		}
		rel := filepath.ToSlash(strings.TrimSpace(session.TranscriptPath))
		if rel == "" {
			continue
		}
		branch := strings.TrimSpace(session.Branch)
		if branch == "" {
			branch = strings.TrimSpace(manifest.Sources.Sessions.DefaultBranch)
		}
		preparedDigest, ok := snapshot.digests[rel]
		if !ok {
			return created, fmt.Errorf("%s: transcript set changed during reconciliation", memoryErrSourceStale)
		}
		digest, digestErr := preparedDigest.digest, preparedDigest.err
		if digestErr != nil {
			fallback := sha256.Sum256([]byte(manifest.RepoKey + "\x00" + branch + "\x00" + id + "\x00" + rel))
			digest = "sha256:" + hex.EncodeToString(fallback[:])
			ref, _ := conversationSessionRef(manifest.RepoKey, branch, id, digest)
			jobID := memoryJobID(manifest.RepoKey, ref, digest, memoryJobKindProjection)
			if _, blocked := inventory.issueForJobID(jobID); blocked {
				continue
			}
			if _, exists := byID[jobID]; exists {
				continue
			}
			finished := now
			job := memoryJob{
				SchemaVersion: memoryJobSchemaVersion, JobID: jobID, Kind: memoryJobKindProjection,
				RepoKey: manifest.RepoKey, SessionID: id, SessionRef: ref, Branch: branch,
				InputDigest: digest, Trigger: trigger, State: memoryJobStateInvalid,
				CreatedAt: now, AvailableAt: now, FinishedAt: &finished,
				Error: newMemoryJobError("memory_source_unreadable"),
			}
			if err := recordCreate(job); err != nil {
				return created, err
			}
			byID[jobID] = job
			created++
			continue
		}
		ref, _ := conversationSessionRef(manifest.RepoKey, branch, id, digest)
		jobID := memoryJobID(manifest.RepoKey, ref, digest, memoryJobKindProjection)
		if receipt, ok := receipts.receiptFor(ref); ok && receipt.InputDigest == digest {
			if existing, exists := byID[jobID]; exists && existing.State != memoryJobStateComplete && existing.State != memoryJobStateExcluded {
				updated, settleErr := recordProofCompletion(existing)
				if settleErr != nil {
					return created, settleErr
				}
				byID[jobID] = updated
			}
			continue // current: receipts, not jobs, are the proof
		}
		if _, blocked := inventory.issueForJobID(jobID); blocked {
			continue
		}
		if existing, exists := byID[jobID]; exists {
			// A completed job is history, not proof. If its receipt is missing
			// or stale, reactivate the same identity immediately so loss of a
			// disposable receipt cannot suppress repair until pruning.
			if existing.State == memoryJobStateComplete {
				existing.AvailableAt = now
				existing.AbstractOverride = nil // automatic retry uses current repository configuration
				updated, terr := recordTransition(existing, memoryJobStatePending)
				if terr != nil {
					return created, terr
				}
				byID[jobID] = updated
			}
			continue
		}
		// A new digest supersedes every older non-excluded job for the scope.
		for _, job := range byID {
			if job.SessionRef != ref || job.Kind != memoryJobKindProjection || job.InputDigest == digest {
				continue
			}
			switch job.State {
			case memoryJobStatePending, memoryJobStateRunning, memoryJobStateRetryable, memoryJobStateInvalid, memoryJobStateComplete:
				updated, terr := recordTransition(job, memoryJobStateSuperseded)
				if terr != nil {
					return created, terr
				}
				byID[updated.JobID] = updated
			}
		}
		job := memoryJob{
			SchemaVersion: memoryJobSchemaVersion, JobID: jobID, Kind: memoryJobKindProjection,
			RepoKey: manifest.RepoKey, SessionID: id, SessionRef: ref, Branch: branch,
			InputDigest: digest, Trigger: trigger, State: memoryJobStatePending,
			CreatedAt: now, AvailableAt: now,
		}
		if err := recordCreate(job); err != nil {
			return created, err
		}
		byID[jobID] = job
		created++
	}
	return created, nil
}

func reconcileAbstractJobsLocked(brainDir, trigger string, now time.Time) (created int, err error) {
	if err := rejectAutomaticAbstractReconciliationForGlobalNoEgress(brainDir); err != nil {
		return 0, err
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return 0, err
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		return 0, nil
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		return 0, nil // deterministic projection is not current yet
	}
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return 0, err
	}
	views := buildConversationSessionViews(fresh, manifest.RepoKey, manifest, guard)
	inventory := loadMemoryJobInventory(brainDir)
	if err := memoryJobEnumerationError(inventory.Issues); err != nil {
		return 0, err
	}
	jobs := inventory.Jobs
	byID := make(map[string]memoryJob, len(jobs))
	for _, job := range jobs {
		byID[job.JobID] = job
	}
	abstracts := newSessionAbstractResolver(brainDir)
	refs := make([]string, 0, len(views))
	for ref := range views {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		view := views[ref]
		if len(view.Records) == 0 {
			continue
		}
		request, requestErr := automaticAbstractWorkRequestWithResolver(brainDir, view, abstracts)
		if requestErr != nil {
			return created, requestErr
		}
		if request == nil {
			continue
		}
		jobID := memoryJobID(manifest.RepoKey, request.SessionRef, request.SessionDigest, memoryJobKindSessionAbstract)
		if _, blocked := inventory.issueForJobID(jobID); blocked {
			continue
		}
		if existing, exists := byID[jobID]; exists {
			// The current abstract artifact, not a terminal work record, is the
			// proof. Missing/corrupt disposable output reactivates its job.
			if existing.State == memoryJobStateComplete {
				existing.AvailableAt = now
				updated, terr := transitionMemoryJob(brainDir, existing, memoryJobStatePending, now, "")
				if terr != nil {
					return created, terr
				}
				byID[jobID] = updated
			}
			continue
		}
		for _, old := range byID {
			if old.Kind != memoryJobKindSessionAbstract || old.SessionRef != request.SessionRef || old.InputDigest == request.SessionDigest {
				continue
			}
			switch old.State {
			case memoryJobStatePending, memoryJobStateRunning, memoryJobStateRetryable, memoryJobStateInvalid, memoryJobStateComplete:
				updated, terr := transitionMemoryJob(brainDir, old, memoryJobStateSuperseded, now, "")
				if terr != nil {
					return created, terr
				}
				byID[updated.JobID] = updated
			}
		}
		job := memoryJob{
			SchemaVersion: memoryJobSchemaVersion,
			JobID:         jobID,
			Kind:          memoryJobKindSessionAbstract,
			RepoKey:       manifest.RepoKey,
			SessionID:     view.Records[0].SessionID,
			SessionRef:    request.SessionRef,
			Branch:        conversationCanonicalBranch(view.Records[0], manifest),
			InputDigest:   request.SessionDigest,
			Trigger:       trigger,
			State:         memoryJobStatePending,
			CreatedAt:     now,
			AvailableAt:   now,
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
	JobsCreated   int  `json:"jobs_created"`
	JobsCompleted int  `json:"jobs_completed"`
	JobsRetried   int  `json:"jobs_retried"`
	JobsRecovered int  `json:"jobs_recovered"`
	JobsCancelled int  `json:"jobs_cancelled"`
	JobsPruned    int  `json:"jobs_pruned"`
	HintsConsumed int  `json:"hints_consumed"`
	AlreadyActive bool `json:"already_active,omitempty"`
	PrepassFailed bool `json:"prepass_failed,omitempty"`
	// PrepassGaveUp reports that the relaunch chain this failure belongs to has
	// exhausted memoryWorkerPrepassRetryAttempts and will not reschedule
	// itself. The condition is durable, so the honest answer is to stop rather
	// than keep a failing pass running once a minute for a day.
	PrepassGaveUp  bool     `json:"prepass_gave_up,omitempty"`
	VectorPending  bool     `json:"vector_pending,omitempty"`
	HealthDegraded bool     `json:"health_degraded,omitempty"`
	HealthIssues   []string `json:"health_issues,omitempty"`
	// VectorContinue reports that vector work remains AND the pass ended in a
	// state worth continuing immediately: either it completed a batch or it hit
	// the deliberate lane deadline. It is deliberately narrower than
	// VectorPending, which is also true after a FAILED sync. The fast relaunch
	// exists to keep embedding batches after a lane yield, never as a retry
	// mechanism: retrying a permanent failure (embed server down, corrupt
	// progress leaf, privacy-epoch mismatch) at that cadence re-spawns the
	// worker about ten times a second for as long as the failure lasts. Real
	// retries come from the next trigger (watch tick, lifecycle hint, job
	// schedule), which are all rate-limited.
	VectorContinue bool `json:"-"`
}

func memoryHintConsumable(hint memoryLifecycleHint, scopes int, satisfied bool) bool {
	return scopes > 0 && satisfied && (hint.Branch != "" || scopes == 1)
}

func maintainMemoryWorkerStateLocked(brainDir string, now time.Time, snapshot *memoryReconcileSnapshot) (memoryWorkerStats, error) {
	var stats memoryWorkerStats
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return stats, err
	}
	identity, err := memoryManifestIdentity(manifest)
	if err != nil {
		return stats, err
	}
	if snapshot == nil || identity != snapshot.manifestIdentity {
		return stats, fmt.Errorf("%s: manifest changed during maintenance", memoryErrSourceStale)
	}
	var source *historySourceManifest
	if manifest.Sources != nil {
		source = manifest.Sources.History
	}
	receipts, receiptState, receiptErr := loadProjectionStateChecked(brainDir, source)
	if receiptState == projectionStateUnsupported || receiptState == projectionStateUnsafe {
		return stats, receiptErr
	}
	stones, _, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return stats, err
	}
	hints, issues := loadMemoryHintsChecked(brainDir)
	if err := memoryStateError(issues); err != nil {
		return stats, err
	}
	for _, hint := range hints {
		if _, excluded := stones.Excluded[hint.SessionID]; excluded {
			removed, err := removeMemoryHintIfCurrent(brainDir, hint)
			if err != nil {
				return stats, err
			}
			if removed {
				stats.HintsConsumed++
			}
			continue
		}
		scopes, satisfied := 0, true
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
				prepared, ok := snapshot.digests[rel]
				if !ok || prepared.err != nil {
					satisfied = false
					break
				}
				digest := prepared.digest
				ref, _ := conversationSessionRef(manifest.RepoKey, branch, hint.SessionID, digest)
				receipt, ok := receipts.receiptFor(ref)
				if !ok || receipt.InputDigest != digest {
					satisfied = false
					break
				}
			}
		}
		if !memoryHintConsumable(hint, scopes, satisfied) {
			continue
		}
		removed, err := removeMemoryHintIfCurrent(brainDir, hint)
		if err != nil {
			return stats, err
		}
		if removed {
			stats.HintsConsumed++
		}
	}
	inventory := loadMemoryJobInventory(brainDir)
	if err := memoryJobEnumerationError(inventory.Issues); err != nil {
		return stats, err
	}
	if _, err := reconcileStaleAbstractEgressReceiptsLocked(brainDir, now); err != nil {
		return stats, err
	}
	stats.JobsPruned, err = pruneMemoryJobsLocked(brainDir, inventory.Jobs, now)
	return stats, err
}

func runMemoryMaintenance(brainDir string, now time.Time) (memoryWorkerStats, error) {
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, err := prepareMemoryReconcileSnapshot(context.Background(), brainDir)
		if err != nil {
			return memoryWorkerStats{}, err
		}
		var stats memoryWorkerStats
		err = withBrainWriteLock(brainDir, func() error {
			var applyErr error
			stats, applyErr = maintainMemoryWorkerStateLocked(brainDir, now, snapshot)
			return applyErr
		})
		if err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) {
			return stats, err
		}
	}
	return memoryWorkerStats{}, fmt.Errorf("%s: manifest changed during maintenance", memoryErrSourceStale)
}

// runMemoryProjectionLane performs source scanning outside the Brain write
// lock, then revalidates and publishes through the short atomic commit API.
type memoryClock func() time.Time

func fixedMemoryClock(now time.Time) memoryClock {
	return func() time.Time { return now }
}

func memoryClockNow(clock memoryClock) time.Time {
	if clock == nil {
		return time.Now().UTC()
	}
	return clock().UTC()
}

func runMemoryProjectionLane(ctx context.Context, brainDir string, now time.Time, owner string) (memoryWorkerStats, error) {
	return runMemoryProjectionLaneWithClock(ctx, brainDir, fixedMemoryClock(now), owner)
}

const memoryProjectionClaimsPerPass = 64

func runMemoryProjectionLaneWithClock(ctx context.Context, brainDir string, clock memoryClock, owner string) (memoryWorkerStats, error) {
	var stats memoryWorkerStats
	if _, _, err := loadSessionTombstonesChecked(brainDir); err != nil {
		return stats, err
	}
	reconcileSnapshot, err := prepareMemoryReconcileSnapshot(ctx, brainDir)
	if err != nil {
		return stats, err
	}
	var claimed []memoryJob
	claimAt := memoryClockNow(clock)
	err = withBrainWriteLock(brainDir, func() error {
		inventory := loadMemoryJobInventory(brainDir)
		if err := memoryJobEnumerationError(inventory.Issues); err != nil {
			return err
		}
		jobs := inventory.Jobs
		recovered, err := recoverStaleMemoryJobsLocked(brainDir, jobs, claimAt, owner)
		if err != nil {
			return err
		}
		stats.JobsRecovered = recovered
		created, err := reconcileMemoryJobsLocked(brainDir, "worker", claimAt, reconcileSnapshot)
		if err != nil {
			return err
		}
		stats.JobsCreated = created
		inventory = loadMemoryJobInventory(brainDir)
		if err := memoryJobEnumerationError(inventory.Issues); err != nil {
			return err
		}
		jobs = inventory.Jobs
		for _, job := range runnableMemoryJobs(jobs, claimAt) {
			if job.Kind != memoryJobKindProjection {
				continue
			}
			if len(claimed) >= memoryProjectionClaimsPerPass {
				break
			}
			if job.State == memoryJobStateRetryable {
				job, err = transitionMemoryJob(brainDir, job, memoryJobStatePending, claimAt, "")
				if err != nil {
					return err
				}
			}
			running, err := transitionMemoryJob(brainDir, job, memoryJobStateRunning, claimAt, "")
			if err != nil {
				return err
			}
			running.OwnerToken = owner
			heartbeat := claimAt
			running.HeartbeatAt = &heartbeat
			if err := saveMemoryJob(brainDir, running); err != nil {
				return err
			}
			request, err := loadMemoryCancellationRequest(brainDir, running.JobID)
			if err != nil {
				return err
			}
			if request != nil {
				running.CancelRequestedAt = &request.RequestedAt
				if _, err := transitionMemoryJob(brainDir, running, memoryJobStateCancelled, memoryClockNow(clock), ""); err != nil {
					return err
				}
				if err := removeMemoryCancellationRequest(brainDir, running.JobID); err != nil {
					return err
				}
				stats.JobsCancelled++
				for _, prior := range claimed {
					if _, restoreErr := restoreClaimedMemoryJob(brainDir, prior, memoryClockNow(clock)); restoreErr != nil {
						return restoreErr
					}
				}
				claimed = nil
				break
			}
			claimed = append(claimed, running)
		}
		return nil
	})
	if err != nil {
		return stats, err
	}
	if len(claimed) == 0 {
		maintenance, err := runMemoryMaintenance(brainDir, memoryClockNow(clock))
		stats.add(maintenance)
		return stats, err
	}

	prepared, prepareErr := prepareBrainHistoryProjectionContext(ctx, brainDir, memoryClockNow(clock), nil)
	published := false
	// publishBrainHistoryProjectionLocked is the ONLY caller of
	// discardPreparedHistoryProjection, so every path that never reaches it
	// leaked the staged generation: withBrainWriteLock timing out with
	// memory_lock_busy, or an early return inside the closure below.
	// pruneInactiveHistoryGenerations walks only the generations directory,
	// never history/staging, so each contended worker pass left behind a full
	// copy of the history index permanently.
	publishAttempted := false
	defer func() {
		if prepareErr != nil || publishAttempted {
			return
		}
		_ = withBrainWriteLock(brainDir, func() error {
			return discardPreparedHistoryProjection(brainDir, prepared)
		})
	}()
	err = withBrainWriteLock(brainDir, func() error {
		cancelled := map[string]*memoryCancellationRequest{}
		for _, job := range claimed {
			request, err := loadMemoryCancellationRequest(brainDir, job.JobID)
			if err != nil {
				return err
			}
			if request != nil {
				cancelled[job.JobID] = request
			}
		}
		publishErr := prepareErr
		if publishErr == nil {
			publishAttempted = true
			_, publishErr = publishBrainHistoryProjectionLocked(brainDir, prepared, func() error {
				if err := ctx.Err(); err != nil {
					return errMemoryCancellationRequested
				}
				for _, job := range claimed {
					request, requestErr := loadMemoryCancellationRequest(brainDir, job.JobID)
					if requestErr != nil {
						return requestErr
					}
					if request != nil {
						return errMemoryCancellationRequested
					}
				}
				return nil
			})
			published = publishErr == nil
		}
		settledAt := memoryClockNow(clock)
		manifest, manifestErr := loadBrainManifest(brainDir)
		var receipts projectionState
		if published && manifestErr == nil && manifest.Sources != nil {
			receipts, _ = loadProjectionState(brainDir, manifest.Sources.History)
		}
		stones, _, err := loadSessionTombstonesChecked(brainDir)
		if err != nil {
			return err
		}
		for _, claimedJob := range claimed {
			current, err := memoryJobByID(brainDir, claimedJob.JobID)
			if err != nil {
				return err
			}
			if current.State != memoryJobStateRunning || current.OwnerToken != owner {
				return fmt.Errorf("memory_state_corrupt: projection job ownership changed during execution")
			}
			request := cancelled[current.JobID]
			if request == nil {
				request, err = loadMemoryCancellationRequest(brainDir, current.JobID)
				if err != nil {
					return err
				}
			}
			if request != nil && !published {
				current.CancelRequestedAt = &request.RequestedAt
				if _, err := transitionMemoryJob(brainDir, current, memoryJobStateCancelled, settledAt, ""); err != nil {
					return err
				}
				if err := removeMemoryCancellationRequest(brainDir, current.JobID); err != nil {
					return err
				}
				stats.JobsCancelled++
				continue
			}
			if request != nil {
				// The commit callback already crossed the atomic replacement
				// boundary. Completion wins; the late marker is only cleanup.
				if err := removeMemoryCancellationRequest(brainDir, current.JobID); err != nil {
					return err
				}
			}
			switch {
			case !published && errors.Is(publishErr, errMemoryCancellationRequested):
				if _, err := restoreClaimedMemoryJob(brainDir, current, settledAt); err != nil {
					return err
				}
			case !published:
				if _, err := transitionMemoryJob(brainDir, current, memoryJobStateRetryable, settledAt, memoryOperationalError(publishErr)); err != nil {
					return err
				}
				stats.JobsRetried++
			case func() bool { _, excluded := stones.Excluded[current.SessionID]; return excluded }():
				if _, err := transitionMemoryJob(brainDir, current, memoryJobStateExcluded, settledAt, ""); err != nil {
					return err
				}
			default:
				receipt, ok := receipts.receiptFor(current.SessionRef)
				if ok && receipt.InputDigest == current.InputDigest {
					if _, err := transitionMemoryJob(brainDir, current, memoryJobStateComplete, settledAt, ""); err != nil {
						return err
					}
					stats.JobsCompleted++
				} else {
					if _, err := transitionMemoryJob(brainDir, current, memoryJobStateRetryable, settledAt, "memory_source_stale"); err != nil {
						return err
					}
					stats.JobsRetried++
				}
			}
		}
		if published {
			inventory := loadMemoryJobInventory(brainDir)
			if err := memoryJobEnumerationError(inventory.Issues); err != nil {
				return err
			}
			for _, job := range inventory.Jobs {
				if job.Kind != memoryJobKindProjection || job.State == memoryJobStateComplete || job.State == memoryJobStateExcluded {
					continue
				}
				receipt, ok := receipts.receiptFor(job.SessionRef)
				if !ok || receipt.InputDigest != job.InputDigest {
					continue
				}
				if _, err := completeMemoryJobFromProof(brainDir, job, settledAt); err != nil {
					return err
				}
				stats.JobsCompleted++
			}
		}
		return nil
	})
	if published {
		finalizePreparedHistoryProjection(brainDir, prepared)
	}
	if err == nil {
		maintenance, maintenanceErr := runMemoryMaintenance(brainDir, memoryClockNow(clock))
		stats.add(maintenance)
		if maintenanceErr != nil {
			err = maintenanceErr
		}
	}
	return stats, err
}

func syncMemoryProjectionVectors(ctx context.Context, brainDir string, now time.Time) (bool, error) {
	if strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_EMBEDDER")) == "" {
		return false, nil
	}
	e := historySemanticEmbedder(defaultEmbedder())
	if e == nil {
		return false, nil
	}
	_, pending, err := syncMemoryProjectionVectorsWithEmbedder(ctx, brainDir, now, e, memoryVectorBatchRecords)
	return pending, err
}

// syncMemoryProjectionVectorsWithEmbedder is the only writer for projection
// vector stores. Every destructive reset and every incremental upsert is bound
// to the exact manifest and tombstone identities captured before embedding,
// plus the exact current progress leaf owned by this pass. The pass advances
// that ownership after each of its progress writes. This makes a concurrent
// privacy purge, projection replacement, or progress takeover a closed failure:
// stale vectors cannot be published after any authority changes.
func syncMemoryProjectionVectorsWithEmbedder(ctx context.Context, brainDir string, now time.Time, e Embedder, batchRecords int) (memoryVectorSyncPass, bool, error) {
	var pass memoryVectorSyncPass
	if e == nil {
		return pass, false, nil
	}
	if batchRecords <= 0 {
		batchRecords = memoryVectorBatchRecords
	}
	epoch, manifest, err := captureMemoryVectorEpoch(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		return pass, false, err
	}
	privacyIdentity := manifest.Sources.History.PrivacyIdentity
	if privacyIdentity != epoch.tombstoneIdentity && !(privacyIdentity == "" && epoch.tombstoneIdentity == "absent") {
		return pass, true, fmt.Errorf("%s: history generation does not match the current privacy epoch", memoryErrSourceStale)
	}
	targetDigest := manifest.Sources.History.IndexDigest
	if !validSHA256Identity(targetDigest) {
		// A history source published before generation-addressed projections
		// carries no index_digest, and the codebase models that as a supported
		// state. SourceDigest is the vector store's binding to the exact
		// projection it was built from, and decodeMemoryVectorProgress requires
		// a valid sha256, so writing a digest-less progress leaf would create a
		// file this very process can never read back: the next ownership
		// validation fails memory_state_corrupt and every later pass refuses to
		// run, wedging vector sync with its own output. There is nothing to
		// bind to yet, so do no work and stay un-pending; the next projection
		// publish stamps a digest and the pass resumes normally.
		return pass, false, nil
	}
	prior, present, ownership, progressErr := loadMemoryVectorProgressWithOwnership(brainDir)
	if progressErr != nil {
		// Content-free progress is still durable state. Unknown-newer, unsafe,
		// and corrupt leaves remain byte-for-byte read-only until an explicit
		// repair/migration action replaces them.
		return pass, true, progressErr
	}
	rawHistoryStore, ok := historyVectorStoreFor(brainDir, e)
	if !ok {
		return pass, false, nil
	}
	historyStore := &memoryGuardedVectorStore{brainDir: brainDir, epoch: epoch, ownership: ownership, store: rawHistoryStore}
	var conversationStore historyVectorStore
	if rawConversationStore, available := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim()); available {
		conversationStore = &memoryGuardedVectorStore{brainDir: brainDir, epoch: epoch, ownership: ownership, store: rawConversationStore}
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		return pass, false, err
	}
	needsReset := memoryVectorProgressNeedsReset(prior, present, e.ID(), targetDigest)
	progress := memoryVectorProgress{ModelID: e.ID(), SourceDigest: targetDigest, UpdatedAt: now, Pending: true}
	if !needsReset {
		progress = prior
		progress.HistoryAdded = 0
		progress.ConversationAdded = 0
		progress.ErrorCode = ""
		progress.UpdatedAt = now
	}
	if needsReset {
		// Persist the reset intent before deleting anything. A crash at any point
		// leaves reset_complete=false, so the next pass finishes clearing stale
		// vectors before it can treat stable IDs as reusable.
		if err := saveMemoryVectorProgressAtEpochOwned(brainDir, epoch, ownership, progress); err != nil {
			return pass, true, err
		}
		pass.HistoryDropped, err = clearMemoryVectorStore(ctx, historyStore)
		if err != nil {
			return pass, true, err
		}
		if conversationStore != nil {
			pass.ConversationDrop, err = clearMemoryVectorStore(ctx, conversationStore)
			if err != nil {
				return pass, true, err
			}
		}
		progress.ResetComplete = true
		if err := saveMemoryVectorProgressAtEpochOwned(brainDir, epoch, ownership, progress); err != nil {
			return pass, true, err
		}
	}
	historyAdded, historyDropped, historyTotal, historyPending, syncErr := syncVectorsForKindsContextBatch(ctx, historyStore, index, e, historyGeneralRankingHiddenKind, func(r historyRecord) string { return r.Summary }, batchRecords)
	pass.HistoryAdded = historyAdded
	pass.HistoryDropped += historyDropped
	pass.HistoryTotal = historyTotal
	progress.HistoryAdded = historyAdded
	remainingBudget := batchRecords - historyAdded
	if remainingBudget < 0 {
		remainingBudget = 0
	}
	conversationPending := false
	if syncErr == nil && conversationStore != nil {
		if remainingBudget == 0 {
			conversationPending = vectorStoreHasMissing(conversationStore, index, conversationSemanticHiddenKind)
		} else {
			conversationAdded, conversationDropped, conversationTotal, pending, conversationErr := syncVectorsForKindsContextBatch(ctx, conversationStore, index, e, conversationSemanticHiddenKind, conversationEmbeddingText, remainingBudget)
			pass.ConversationAdded = conversationAdded
			pass.ConversationDrop += conversationDropped
			pass.ConversationTotal = conversationTotal
			progress.ConversationAdded = conversationAdded
			conversationPending = pending
			if conversationErr != nil {
				syncErr = conversationErr
			}
		}
	}
	progress.Pending = historyPending || conversationPending || syncErr != nil
	if syncErr != nil {
		progress.ErrorCode = "memory_vector_sync_failed"
	} else if !progress.Pending {
		progress.CompleteSourceDigest = targetDigest
	}
	if progressErr := saveMemoryVectorProgressAtEpochOwned(brainDir, epoch, ownership, progress); progressErr != nil {
		syncErr = errors.Join(syncErr, fmt.Errorf("memory_vector_progress_write_failed: %w", progressErr))
	}
	return pass, progress.Pending, syncErr
}

func syncMemoryProjectionVectorsFully(ctx context.Context, brainDir string, now time.Time, e Embedder, progress func(memoryVectorSyncPass)) (memoryVectorSyncPass, error) {
	var total memoryVectorSyncPass
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(ctx, brainDir, now, e, memoryVectorBatchRecords)
		total.HistoryAdded += pass.HistoryAdded
		total.HistoryDropped += pass.HistoryDropped
		total.HistoryTotal = pass.HistoryTotal
		total.ConversationAdded += pass.ConversationAdded
		total.ConversationDrop += pass.ConversationDrop
		total.ConversationTotal = pass.ConversationTotal
		if progress != nil {
			progress(total)
		}
		if err != nil {
			return total, err
		}
		if !pending {
			return total, nil
		}
		if pass.HistoryAdded+pass.ConversationAdded == 0 {
			return total, errors.New("memory_vector_sync_failed: vector sync made no progress")
		}
	}
}

func runMemoryVectorLane(ctx context.Context, brainDir string, now time.Time, owner string) memoryWorkerStats {
	var stats memoryWorkerStats
	if strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_EMBEDDER")) == "" {
		return stats
	}
	// This lane always runs after deterministic and abstract work. The overall
	// deadline, in addition to each call timeout, caps coordinator occupancy and
	// guarantees a prompt opportunity for shutdown or newly exported work.
	laneCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	pending, err := syncMemoryProjectionVectors(laneCtx, brainDir, now)
	laneYielded := errors.Is(laneCtx.Err(), context.DeadlineExceeded)
	stats.VectorPending = pending || laneYielded
	// Only a clean batch or a deliberate lane yield earns the fast relaunch; a
	// failed sync must wait for the next trigger instead of self-spinning.
	stats.VectorContinue = stats.VectorPending && (err == nil || laneYielded)
	if err != nil {
		stats.addHealthIssue("memory_vector_sync_failed")
		if logErr := appendMemoryWorkerLog(brainDir, now, "vector_sync_failed", owner); logErr != nil {
			stats.addHealthIssue("memory_worker_log_write_failed")
		}
	} else if logErr := appendMemoryWorkerLog(brainDir, now, "vector_sync_complete", owner); logErr != nil {
		stats.addHealthIssue("memory_worker_log_write_failed")
	}
	return stats
}

func runMemoryAbstractLane(ctx context.Context, repoDir, brainDir string, now time.Time, owner string) (memoryWorkerStats, error) {
	return runMemoryAbstractLaneWithClock(ctx, repoDir, brainDir, fixedMemoryClock(now), owner)
}

func runMemoryAbstractLaneWithClock(ctx context.Context, repoDir, brainDir string, clock memoryClock, owner string) (memoryWorkerStats, error) {
	var stats memoryWorkerStats
	if _, _, err := loadSessionTombstonesChecked(brainDir); err != nil {
		return stats, err
	}
	var claimed *memoryJob
	claimAt := memoryClockNow(clock)
	err := withBrainWriteLock(brainDir, func() error {
		created, err := reconcileAbstractJobsLocked(brainDir, "worker", claimAt)
		if err != nil {
			return err
		}
		stats.JobsCreated = created
		inventory := loadMemoryJobInventory(brainDir)
		if err := memoryJobEnumerationError(inventory.Issues); err != nil {
			return err
		}
		jobs := inventory.Jobs
		for _, candidate := range runnableMemoryJobs(jobs, claimAt) {
			if candidate.Kind != memoryJobKindSessionAbstract {
				continue
			}
			if err := rejectMemoryAbstractJobForGlobalNoEgress(brainDir, candidate); err != nil {
				return err
			}
			if candidate.State == memoryJobStateRetryable {
				candidate, err = transitionMemoryJob(brainDir, candidate, memoryJobStatePending, claimAt, "")
				if err != nil {
					return err
				}
			}
			running, err := transitionMemoryJob(brainDir, candidate, memoryJobStateRunning, claimAt, "")
			if err != nil {
				return err
			}
			running.OwnerToken = owner
			heartbeat := claimAt
			running.HeartbeatAt = &heartbeat
			if err := saveMemoryJob(brainDir, running); err != nil {
				return err
			}
			if request, err := loadMemoryCancellationRequest(brainDir, running.JobID); err != nil {
				return err
			} else if request != nil {
				running.CancelRequestedAt = &request.RequestedAt
				if _, err := transitionMemoryJob(brainDir, running, memoryJobStateCancelled, memoryClockNow(clock), ""); err != nil {
					return err
				}
				if err := removeMemoryCancellationRequest(brainDir, running.JobID); err != nil {
					return err
				}
				stats.JobsCancelled++
				return nil
			}
			claimed = &running
			break // one bounded abstract lane item per pass
		}
		return nil
	})
	if err != nil || claimed == nil {
		return stats, err
	}

	// The provider holds only the dedicated privacy-side-effect lock. Ordinary
	// Brain-locked deterministic work and cancellation requests can therefore
	// continue while egress is in flight; privacy mutations serialize before
	// their Brain lock so they cannot cross the irreversible provider boundary.
	providerCtx, cancelProvider := context.WithCancel(ctx)
	defer cancelProvider()
	runDone := make(chan error, 1)
	go func() {
		runDone <- runSessionAbstractJobGuardedWithOverride(providerCtx, repoDir, brainDir, claimed.SessionRef, claimed.InputDigest, claimAt, claimed.AbstractOverride, func() error {
			current, err := memoryJobByID(brainDir, claimed.JobID)
			if err != nil {
				return err
			}
			if current.State != memoryJobStateRunning || current.OwnerToken != owner || !equalMemoryAbstractJobOverride(current.AbstractOverride, claimed.AbstractOverride) {
				return fmt.Errorf("%s: abstract job ownership or provider selection changed before publication", memoryErrStateCorrupt)
			}
			request, err := loadMemoryCancellationRequest(brainDir, claimed.JobID)
			if err != nil {
				return err
			}
			if request != nil {
				return errMemoryCancellationRequested
			}
			return nil
		})
	}()
	runDeterministicPass := func() error {
		pass, err := runMemoryProjectionLaneWithClock(ctx, brainDir, clock, owner)
		stats.add(pass)
		return err
	}
	ticker := time.NewTicker(memoryAbstractCancellationPollInterval)
	defer ticker.Stop()
	var runErr error
	var deterministicErr error
	waiting := true
	for waiting {
		select {
		case runErr = <-runDone:
			waiting = false
		case <-ticker.C:
			request, cancelErr := loadMemoryCancellationRequest(brainDir, claimed.JobID)
			if cancelErr != nil {
				deterministicErr = cancelErr
				cancelProvider()
				runErr = <-runDone
				waiting = false
				continue
			}
			if request != nil {
				cancelProvider()
				runErr = <-runDone
				waiting = false
				continue
			}
			if err := runDeterministicPass(); err != nil {
				deterministicErr = err
				cancelProvider()
				runErr = <-runDone
				waiting = false
			}
		case <-ctx.Done():
			cancelProvider()
			runErr = <-runDone
			waiting = false
		}
	}
	if deterministicErr == nil {
		if err := runDeterministicPass(); err != nil {
			deterministicErr = err
		}
	}
	// The abstract job's outcome is the PROVIDER result alone. The deterministic
	// pass runs alongside it as a liveness and cancellation check, and its
	// failure is reported through this lane's return value below; folding it
	// into the settle decision marked a job whose artifact was already
	// published (and whose hosted egress was already paid) as retryable_error,
	// so the next pass re-ran it and paid the provider a second time for
	// identical content. Where a deterministic failure genuinely aborted the
	// run, it cancelled the provider first, so runErr is itself non-nil and the
	// retryable path still fires.
	settleErr := runErr
	err = withBrainWriteLock(brainDir, func() error {
		if _, _, err := loadSessionTombstonesChecked(brainDir); err != nil {
			return err
		}
		settledAt := memoryClockNow(clock)
		current, err := memoryJobByID(brainDir, claimed.JobID)
		if err != nil {
			return err
		}
		if current.State != memoryJobStateRunning || current.OwnerToken != owner {
			return fmt.Errorf("memory_state_corrupt: abstract job ownership changed during execution")
		}
		cancelRequest, err := loadMemoryCancellationRequest(brainDir, current.JobID)
		if err != nil {
			return err
		}
		if errors.Is(settleErr, errMemoryCancellationRequested) || (cancelRequest != nil && settleErr != nil) {
			requestedAt := settledAt
			if cancelRequest != nil {
				requestedAt = cancelRequest.RequestedAt
			}
			current.CancelRequestedAt = &requestedAt
			_, err = transitionMemoryJob(brainDir, current, memoryJobStateCancelled, settledAt, "")
			if err != nil {
				return err
			}
			if err := removeMemoryCancellationRequest(brainDir, current.JobID); err != nil {
				return err
			}
			stats.JobsCancelled++
			return nil
		}
		if cancelRequest != nil {
			if err := removeMemoryCancellationRequest(brainDir, current.JobID); err != nil {
				return err
			} // publish already committed: cancellation is too late
		}
		if settleErr != nil {
			_, err = transitionMemoryJob(brainDir, current, memoryJobStateRetryable, settledAt, memoryOperationalError(settleErr))
			if err == nil {
				stats.JobsRetried++
			}
			return err
		}
		_, err = transitionMemoryJob(brainDir, current, memoryJobStateComplete, settledAt, "")
		if err == nil {
			stats.JobsCompleted++
		}
		return err
	})
	if err != nil {
		return stats, err
	}
	if deterministicErr != nil {
		// The lane still REPORTS both failures; only the job's settle decision
		// above is provider-only.
		return stats, errors.Join(deterministicErr, runErr)
	}
	return stats, nil
}

func runMemoryAbstractDrain(ctx context.Context, repoDir, brainDir string, now time.Time, owner string, limit int) (memoryWorkerStats, error) {
	return runMemoryAbstractDrainWithClock(ctx, repoDir, brainDir, fixedMemoryClock(now), owner, limit)
}

func runMemoryAbstractDrainWithClock(ctx context.Context, repoDir, brainDir string, clock memoryClock, owner string, limit int) (memoryWorkerStats, error) {
	var total memoryWorkerStats
	for i := 0; i < limit; i++ {
		item, err := runMemoryAbstractLaneWithClock(ctx, repoDir, brainDir, clock, owner)
		if err != nil {
			return total, err
		}
		total.add(item)
		if item.JobsCompleted+item.JobsRetried+item.JobsCancelled == 0 {
			break
		}
	}
	return total, nil
}

func newMemoryWorkerCommand(opts Options) *cobra.Command {
	var once bool
	var delay time.Duration
	var prepassFailures int
	cmd := &cobra.Command{
		Use:    "worker",
		Short:  "Run one bounded deterministic projection pass (hidden; hints/watch/startup launch it)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !once {
				return fmt.Errorf("only --once is supported")
			}
			if delay < 0 || delay > 2*time.Hour {
				return fmt.Errorf("worker delay must be between 0 and 2h")
			}
			if prepassFailures < 0 {
				return fmt.Errorf("worker prepass failure count must not be negative")
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			repoDir, local, err := resolveLocalTargetRepoDir(cmd.Context(), opts.Runner, agentSurfaceTarget(opts, nil))
			if err != nil {
				return err
			}
			if !local {
				return fmt.Errorf("memory worker requires a local repository")
			}
			// Privacy policy is an input to every worker lane, including the
			// export/delta prepass. Validate it before coordinator metadata or
			// any derived state can be written.
			if _, _, err := loadSessionTombstonesChecked(storage.BrainDir); err != nil {
				return err
			}
			// Wait out everything beyond the last minute BEFORE taking the
			// scheduler lease. Holding it across the whole wait made one
			// backed-off job freeze every other launch for this Brain (see
			// memoryWorkerMaxLeaseSleep); nothing is lost by yielding here,
			// because a contender that wins the lease meanwhile does the same
			// work, and this process then finds nothing to do.
			leaseSleep := delay
			if unleased := delay - memoryWorkerMaxLeaseSleep; unleased > 0 {
				leaseSleep = memoryWorkerMaxLeaseSleep
				timer := time.NewTimer(unleased)
				select {
				case <-timer.C:
				case <-cmd.Context().Done():
					timer.Stop()
					return cmd.Context().Err()
				}
				timer.Stop()
			}
			coordinator, err := acquireMemoryCoordinator(storage.BrainDir, opts.Now().UTC(), opts.Version)
			if err != nil {
				if strings.Contains(err.Error(), "memory_worker_active") {
					return writeJSON(cmd, memoryWorkerStats{AlreadyActive: true})
				}
				return err
			}
			outcome := "complete"
			var relaunchDelay *time.Duration
			relaunchPrepassFailures := 0
			defer func() {
				coordinator.close(opts.Now().UTC(), outcome)
				if relaunchDelay != nil {
					_ = memoryWorkerLaunchAfter(repoDir, *relaunchDelay, relaunchPrepassFailures)
				}
			}()
			clock := memoryClock(opts.Now)
			stopHeartbeat := coordinator.startHeartbeat(5*time.Second, clock)
			heartbeatStopped := false
			defer func() {
				if !heartbeatStopped {
					stopHeartbeat()
				}
			}()
			if err := coordinator.heartbeat(opts.Now().UTC()); err != nil {
				outcome = "failed"
				return err
			}
			// The last stretch of a delayed retry owns the coordinator. This is
			// the scheduler lease: repeated startup/watch notifications can
			// launch contenders, but they fail fast instead of accumulating
			// detached sleepers that all wake for the same retry. Only this
			// bounded tail is held; anything longer was waited out above,
			// unleased.
			if leaseSleep > 0 {
				timer := time.NewTimer(leaseSleep)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-cmd.Context().Done():
					outcome = "cancelled"
					return cmd.Context().Err()
				}
			}
			var stats memoryWorkerStats
			var prepassRetry *time.Duration
			if err := memoryWorkerPrepass(cmd.Context(), opts, repoDir); err != nil {
				// The durable hint remains. Continue with already-exported work and
				// schedule another bounded prepass instead of hiding the failure.
				stats.PrepassFailed = true
				// A pass that could not export new sessions did not do what it
				// was launched to do. Say so: reporting "complete" here is what
				// let 1285 consecutive failures accumulate with every health
				// surface showing a healthy last outcome.
				outcome = memoryWorkerOutcomeDegraded
				_ = appendMemoryWorkerLog(storage.BrainDir, opts.Now().UTC(), "prepass_failed", coordinator.token)
				failures := prepassFailures + 1
				if retry, ok := memoryWorkerPrepassRetry(failures); ok {
					prepassRetry = &retry
					relaunchPrepassFailures = failures
				} else {
					// The chain has retried for as long as it is worth retrying.
					// Stop relaunching for this reason; the degraded outcome and
					// this event are the durable record, and the next lifecycle
					// hint starts a fresh chain.
					stats.PrepassGaveUp = true
					_ = appendMemoryWorkerLog(storage.BrainDir, opts.Now().UTC(), "prepass_gave_up", coordinator.token)
				}
			}
			projectionStats, err := runMemoryProjectionLaneWithClock(cmd.Context(), storage.BrainDir, clock, coordinator.token)
			if err != nil {
				outcome = "failed"
				return err
			}
			stats.add(projectionStats)
			if err := coordinator.heartbeat(opts.Now().UTC()); err != nil {
				outcome = "failed"
				return err
			}
			const abstractJobsPerPass = 8
			abstractStats, err := runMemoryAbstractDrainWithClock(cmd.Context(), repoDir, storage.BrainDir, clock, coordinator.token, abstractJobsPerPass)
			if err != nil {
				outcome = "failed"
				return err
			}
			stats.add(abstractStats)
			stats.add(runMemoryVectorLane(cmd.Context(), storage.BrainDir, opts.Now().UTC(), coordinator.token))
			if stats.VectorContinue {
				vectorDelay := 100 * time.Millisecond
				relaunchDelay = &vectorDelay
			}
			if next, ok, err := nextMemoryWorkerDelay(storage.BrainDir, opts.Now().UTC()); err != nil {
				outcome = "failed"
				return err
			} else if ok && (relaunchDelay == nil || next < *relaunchDelay) {
				relaunchDelay = &next
			}
			// The prepass retry is the last relaunch candidate, and the soonest
			// one still wins. Vector continuation and a pending job are
			// self-terminating reasons to come back; the prepass is not, which
			// is why only it carries a failure count forward. A chain that has
			// given up contributes no candidate at all, so a permanently broken
			// prepass stops being a reason to relaunch.
			if prepassRetry != nil && (relaunchDelay == nil || *prepassRetry < *relaunchDelay) {
				relaunchDelay = prepassRetry
			}
			stopHeartbeat()
			heartbeatStopped = true
			for _, issue := range coordinator.healthIssuesSnapshot() {
				stats.addHealthIssue(issue)
			}
			return writeJSON(cmd, stats)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "Run one pass and exit")
	cmd.Flags().DurationVar(&delay, "delay", 0, "Delay before running (internal retry scheduler)")
	cmd.Flags().IntVar(&prepassFailures, "prepass-failures", 0, "Consecutive prepass failures already seen in this relaunch chain (internal retry scheduler)")
	_ = cmd.Flags().MarkHidden("prepass-failures")
	return cmd
}

func (s *memoryWorkerStats) add(other memoryWorkerStats) {
	s.JobsCreated += other.JobsCreated
	s.JobsCompleted += other.JobsCompleted
	s.JobsRetried += other.JobsRetried
	s.JobsRecovered += other.JobsRecovered
	s.JobsCancelled += other.JobsCancelled
	s.JobsPruned += other.JobsPruned
	s.HintsConsumed += other.HintsConsumed
	s.AlreadyActive = s.AlreadyActive || other.AlreadyActive
	s.PrepassFailed = s.PrepassFailed || other.PrepassFailed
	s.PrepassGaveUp = s.PrepassGaveUp || other.PrepassGaveUp
	s.VectorPending = s.VectorPending || other.VectorPending
	s.VectorContinue = s.VectorContinue || other.VectorContinue
	s.HealthDegraded = s.HealthDegraded || other.HealthDegraded
	for _, issue := range other.HealthIssues {
		s.addHealthIssue(issue)
	}
}

func (s *memoryWorkerStats) addHealthIssue(code string) {
	if code == "" {
		return
	}
	s.HealthDegraded = true
	for _, existing := range s.HealthIssues {
		if existing == code {
			return
		}
	}
	s.HealthIssues = append(s.HealthIssues, code)
}

func newMemoryReconcileCommand(opts Options) *cobra.Command {
	var dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Compare canonical sessions with projection receipts and enqueue missing work",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			startedAt := opts.Now().UTC()
			receipt := newMemoryOperationReceipt("reconcile", startedAt)
			receipt.DryRun = dryRun
			fail := func(cause error) error {
				return finishMemoryOperationFailure(cmd, &receipt, jsonOut, opts.Now(), cause)
			}
			plannedCreated, err := reconcileMemoryJobsForAdmin(cmd.Context(), storage.BrainDir, "manual", startedAt, dryRun, &receipt)
			if err != nil {
				return fail(err)
			}
			jobsCreated := plannedCreated
			jobsWouldCreate := 0
			if dryRun {
				jobsCreated = 0
				jobsWouldCreate = plannedCreated
			}
			return emitMemoryOperationSuccess(cmd, &receipt, jsonOut, opts.Now().UTC(), func(receipt memoryOperationReceipt) any {
				return map[string]any{
					"receipt": receipt, "jobs_created": jobsCreated,
					"jobs_would_create": jobsWouldCreate, "jobs_changed": len(receipt.Artifacts),
				}
			}, func(out io.Writer, receipt memoryOperationReceipt) {
				renderMemoryOperationReceiptText(out, receipt, "completed")
				if dryRun {
					fmt.Fprintf(out, "reconcile would enqueue %d jobs (%d state changes)\n", jobsWouldCreate, len(receipt.Artifacts))
					return
				}
				fmt.Fprintf(out, "reconcile enqueued %d jobs\n", jobsCreated)
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report reconciliation transitions without changing durable work")
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
			snapshot := memoryReadOnlyHealth(storage.BrainDir, opts.Now().UTC())
			payload := snapshot.Payload
			if projection, ok := payload["projection_receipts"].(map[string]any); ok {
				if generated, ok := projection["generated_at"]; ok {
					payload["receipts_generated_at"] = generated
				}
			}
			if jsonOut {
				return writeJSON(cmd, payload)
			}
			return writeMemoryStatusText(cmd, payload)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// writeMemoryStatusText renders the exact JSON contract as a deterministic
// human-readable tree. Normalizing through encoding/json keeps structs,
// omitempty fields, numbers, and future payload sections in parity with
// --json; sorted map keys prevent Go map iteration from changing the output.
func writeMemoryStatusText(cmd *cobra.Command, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode memory status for text rendering: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var normalized map[string]any
	if err := decoder.Decode(&normalized); err != nil {
		return fmt.Errorf("normalize memory status for text rendering: %w", err)
	}
	return writeText(cmd, func(out io.Writer) {
		renderMemoryStatusMap(out, normalized, 0)
	})
}

func renderMemoryStatusMap(out io.Writer, values map[string]any, depth int) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	indent := strings.Repeat("  ", depth)
	for _, key := range keys {
		value := values[key]
		switch nested := value.(type) {
		case map[string]any:
			if len(nested) == 0 {
				fmt.Fprintf(out, "%s%s: {}\n", indent, key)
				continue
			}
			fmt.Fprintf(out, "%s%s:\n", indent, key)
			renderMemoryStatusMap(out, nested, depth+1)
		case []any:
			if len(nested) == 0 {
				fmt.Fprintf(out, "%s%s: []\n", indent, key)
				continue
			}
			fmt.Fprintf(out, "%s%s:\n", indent, key)
			renderMemoryStatusSlice(out, nested, depth+1)
		default:
			fmt.Fprintf(out, "%s%s: %s\n", indent, key, memoryStatusScalar(value))
		}
	}
}

func renderMemoryStatusSlice(out io.Writer, values []any, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, value := range values {
		switch nested := value.(type) {
		case map[string]any:
			if len(nested) == 0 {
				fmt.Fprintf(out, "%s- {}\n", indent)
				continue
			}
			fmt.Fprintf(out, "%s-\n", indent)
			renderMemoryStatusMap(out, nested, depth+1)
		case []any:
			if len(nested) == 0 {
				fmt.Fprintf(out, "%s- []\n", indent)
				continue
			}
			fmt.Fprintf(out, "%s-\n", indent)
			renderMemoryStatusSlice(out, nested, depth+1)
		default:
			fmt.Fprintf(out, "%s- %s\n", indent, memoryStatusScalar(value))
		}
	}
}

func memoryStatusScalar(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// The tree was already normalized through JSON, so this is defensive
		// only. Keep the field visible even if a future decoder value cannot be
		// re-encoded.
		return "null"
	}
	return string(encoded)
}

func newMemoryJobsCommand(opts Options) *cobra.Command {
	var state, sessionRef, sessionID, kind string
	var limit int
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List content-free work metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > 1000 {
				return fmt.Errorf("limit must be between 1 and 1000")
			}
			if state != "" && !validMemoryJobState(state) {
				return fmt.Errorf("unknown memory job state %q", state)
			}
			if kind != "" && !validMemoryJobKind(kind) {
				return fmt.Errorf("unknown memory job kind %q", kind)
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			// This is a deliberately partial leaf-read surface: directory names
			// are inventoried to the shared ceiling, but job files are opened only
			// until the requested number of matching records has been decoded.
			inventory := loadMemoryJobInventoryPage(storage.BrainDir, limit, func(job memoryJob) bool {
				if state != "" && job.State != state {
					return false
				}
				if sessionRef != "" && job.SessionRef != sessionRef {
					return false
				}
				if sessionID != "" && job.SessionID != sessionID {
					return false
				}
				return kind == "" || job.Kind == kind
			})
			jobs := inventory.Jobs
			if len(jobs) > limit {
				jobs = jobs[:limit]
			}
			if jobs == nil {
				jobs = []memoryJob{}
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"jobs": jobs, "state_issues": inventory.Issues, "inventory_entries_observed": inventory.EntriesObserved, "inventory_entries_scanned": inventory.EntriesScanned, "inventory_truncated": inventory.Truncated, "inventory_scan_complete": inventory.ScanComplete, "inventory_degraded": inventory.Degraded})
			}
			return writeText(cmd, func(out io.Writer) {
				for _, job := range jobs {
					errorCode := ""
					if job.Error != nil {
						errorCode = job.Error.Code
					}
					fmt.Fprintf(out, "%-16s %s  session %s  attempt %d  %s\n", job.State, job.JobID, job.SessionID, job.Attempt, errorCode)
				}
				for _, issue := range inventory.Issues {
					fmt.Fprintf(out, "%-16s %s  %s\n", issue.Code, issue.File, "left untouched")
				}
				if !inventory.ScanComplete {
					fmt.Fprintf(out, "showing a bounded partial inventory (%d entries observed, %d leaves scanned)\n", inventory.EntriesObserved, inventory.EntriesScanned)
				}
			})
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "Filter by job state")
	cmd.Flags().StringVar(&sessionRef, "session-ref", "", "Filter by canonical session reference")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Filter by raw session id")
	cmd.Flags().StringVar(&kind, "kind", "", "Filter by job kind")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum jobs to return (1-1000)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func memoryJobsForSelector(brainDir, jobID, sessionRef string) ([]memoryJob, error) {
	jobID = strings.TrimSpace(jobID)
	sessionRef = strings.TrimSpace(sessionRef)
	if (jobID == "") == (sessionRef == "") {
		return nil, fmt.Errorf("provide exactly one job id or --session-ref")
	}
	inventory := loadMemoryJobInventory(brainDir)
	if err := memoryJobEnumerationError(inventory.Issues); err != nil {
		return nil, err
	}
	if jobID != "" {
		if issue, affected := inventory.issueForJobID(jobID); affected {
			return nil, &memoryStateLoadError{Issues: []memoryStateIssue{issue}}
		}
	} else {
		if issue, affected := inventory.issueForSessionRef(sessionRef); affected {
			return nil, &memoryStateLoadError{Issues: []memoryStateIssue{issue}}
		}
		// A bulk session selector cannot prove completeness when a damaged leaf
		// has no readable session identity. Exact job-id selection can isolate an
		// unrelated filename; session selection must fail closed instead of
		// silently mutating only the visible subset.
		for _, issue := range inventory.Issues {
			if strings.TrimSpace(issue.sessionRef) == "" {
				return nil, &memoryStateLoadError{Issues: []memoryStateIssue{issue}}
			}
		}
	}
	var selected []memoryJob
	for _, job := range inventory.Jobs {
		if (jobID != "" && job.JobID == jobID) || (sessionRef != "" && job.SessionRef == sessionRef) {
			selected = append(selected, job)
		}
	}
	if len(selected) == 0 {
		if jobID != "" {
			return nil, fmt.Errorf("%s: job %s not found", memoryErrSourceStale, jobID)
		}
		return nil, fmt.Errorf("%s: no jobs found for session-ref %s", memoryErrSourceStale, sessionRef)
	}
	return selected, nil
}

func memoryJobByID(brainDir, jobID string) (memoryJob, error) {
	inventory := loadMemoryJobInventory(brainDir)
	if err := memoryJobEnumerationError(inventory.Issues); err != nil {
		return memoryJob{}, err
	}
	if issue, affected := inventory.issueForJobID(jobID); affected {
		return memoryJob{}, &memoryStateLoadError{Issues: []memoryStateIssue{issue}}
	}
	for _, job := range inventory.Jobs {
		if job.JobID == jobID {
			return job, nil
		}
	}
	return memoryJob{}, fmt.Errorf("job %s not found", jobID)
}

func requestRunningMemoryJobCancellationVerified(brainDir string, observed memoryJob, now time.Time) error {
	return withBrainWriteLock(brainDir, func() error {
		current, err := memoryJobByID(brainDir, observed.JobID)
		if err != nil {
			return err
		}
		if current.State != memoryJobStateRunning || current.OwnerToken != observed.OwnerToken {
			if err := removeMemoryCancellationRequest(brainDir, observed.JobID); err != nil {
				return err
			}
			return fmt.Errorf("%s: job %s already crossed its commit boundary", memoryErrCancelTooLate, observed.JobID)
		}
		_, err = requestMemoryJobCancellation(brainDir, current, now)
		return err
	})
}

// These narrow seams exercise the receipt contract at atomic-write ambiguity
// boundaries. Production uses the state helpers directly; tests can model a
// helper that committed its rename but returned a later sync error.
var (
	memoryAdminTransitionJob       = transitionMemoryJob
	memoryAdminRequestCancellation = requestMemoryJobCancellation
	memoryAdminRemoveCancellation  = removeMemoryCancellationRequest
)

func newMemoryRetryCommand(opts Options) *cobra.Command {
	var sessionRef string
	var dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "retry [job-id]",
		Short: "Move failed or invalid work to pending; repeated pending retries are idempotent",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selectedSessionRef := strings.TrimSpace(sessionRef)
			selectedJobID := ""
			if len(args) == 1 {
				selectedJobID = strings.TrimSpace(args[0])
			}
			if (selectedJobID == "") == (selectedSessionRef == "") {
				return fmt.Errorf("provide exactly one job id or --session-ref")
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			startedAt := opts.Now().UTC()
			receipt := newMemoryOperationReceipt("retry", startedAt)
			receipt.DryRun = dryRun
			receipt.SessionRef = selectedSessionRef
			fail := func(cause error) error {
				return finishMemoryOperationFailure(cmd, &receipt, jsonOut, opts.Now(), cause)
			}
			matched, eligible := 0, 0
			retry := func() error {
				jobs, err := memoryJobsForSelector(storage.BrainDir, selectedJobID, receipt.SessionRef)
				if err != nil {
					return err
				}
				matched = len(jobs)
				for _, job := range jobs {
					receipt.JobIDs = append(receipt.JobIDs, job.JobID)
				}
				for _, job := range jobs {
					switch job.State {
					case memoryJobStatePending:
						receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
							Path: memoryJobRel(job.JobID), PriorState: job.State, NewState: job.State,
						})
						eligible++
					case memoryJobStateRetryable, memoryJobStateInvalid:
						artifactIndex := len(receipt.Artifacts)
						newState := memoryJobStatePending
						if !dryRun {
							newState = "write_outcome_unknown"
						}
						receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
							Path: memoryJobRel(job.JobID), PriorState: job.State, NewState: newState,
						})
						if !dryRun {
							job.AvailableAt = startedAt
							if _, err = memoryAdminTransitionJob(storage.BrainDir, job, memoryJobStatePending, startedAt, ""); err != nil {
								return err
							}
							receipt.Artifacts[artifactIndex].NewState = memoryJobStatePending
						}
						eligible++
					default:
						receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
							Path: memoryJobRel(job.JobID), PriorState: job.State, NewState: job.State,
						})
						continue
					}
				}
				if eligible == 0 {
					return fmt.Errorf("%s: no retryable_error, invalid, or already-pending jobs matched", memoryErrSourceStale)
				}
				return nil
			}
			if dryRun {
				err = retry()
			} else {
				err = withBrainWriteLock(storage.BrainDir, retry)
			}
			if err != nil {
				return fail(err)
			}
			return emitMemoryOperationSuccess(cmd, &receipt, jsonOut, opts.Now().UTC(), func(receipt memoryOperationReceipt) any {
				return map[string]any{"receipt": receipt, "jobs_matched": matched, "jobs_eligible": eligible}
			}, func(out io.Writer, receipt memoryOperationReceipt) {
				renderMemoryOperationReceiptText(out, receipt, "completed")
				fmt.Fprintf(out, "result jobs_matched=%d jobs_eligible=%d\n", matched, eligible)
			})
		},
	}
	cmd.Flags().StringVar(&sessionRef, "session-ref", "", "Retry eligible jobs for this canonical session reference")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report eligible retry transitions without changing durable work")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newMemoryCancelCommand(opts Options) *cobra.Command {
	var sessionRef string
	var dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "cancel [job-id]",
		Short: "Cancel work cooperatively; repeats are idempotent unless completion already won",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selectedSessionRef := strings.TrimSpace(sessionRef)
			selectedJobID := ""
			if len(args) == 1 {
				selectedJobID = strings.TrimSpace(args[0])
			}
			if (selectedJobID == "") == (selectedSessionRef == "") {
				return fmt.Errorf("provide exactly one job id or --session-ref")
			}
			storage, err := resolveSessionsBrain(cmd.Context(), opts)
			if err != nil {
				return err
			}
			startedAt := opts.Now().UTC()
			receipt := newMemoryOperationReceipt("cancel", startedAt)
			receipt.DryRun = dryRun
			receipt.SessionRef = selectedSessionRef
			fail := func(cause error) error {
				return finishMemoryOperationFailure(cmd, &receipt, jsonOut, opts.Now(), cause)
			}
			matched, eligible := 0, 0
			cancel := func() error {
				jobs, err := memoryJobsForSelector(storage.BrainDir, selectedJobID, receipt.SessionRef)
				if err != nil {
					return err
				}
				matched = len(jobs)
				for _, job := range jobs {
					receipt.JobIDs = append(receipt.JobIDs, job.JobID)
				}
				if selectedJobID != "" && len(jobs) == 1 && jobs[0].State == memoryJobStateComplete {
					receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
						Path: memoryJobRel(jobs[0].JobID), PriorState: memoryJobStateComplete, NewState: memoryJobStateComplete,
					})
					return fmt.Errorf("%s: job %s already completed", memoryErrCancelTooLate, jobs[0].JobID)
				}
				for _, job := range jobs {
					switch job.State {
					case memoryJobStatePending, memoryJobStateRetryable, memoryJobStateInvalid:
						artifactIndex := len(receipt.Artifacts)
						newState := memoryJobStateCancelled
						if !dryRun {
							newState = "write_outcome_unknown"
						}
						receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
							Path: memoryJobRel(job.JobID), PriorState: job.State, NewState: newState,
						})
						if !dryRun {
							_, err = memoryAdminTransitionJob(storage.BrainDir, job, memoryJobStateCancelled, startedAt, "")
							if err == nil {
								receipt.Artifacts[artifactIndex].NewState = memoryJobStateCancelled
							}
						}
					case memoryJobStateRunning:
						request, requestErr := loadMemoryCancellationRequest(storage.BrainDir, job.JobID)
						if requestErr != nil {
							return requestErr
						}
						prior := "absent"
						if request != nil {
							prior = "requested"
						}
						artifactIndex := len(receipt.Artifacts)
						newState := "requested"
						if !dryRun && request == nil {
							newState = "write_outcome_unknown"
						}
						receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
							Path: memoryCancellationRel(job.JobID), PriorState: prior, NewState: newState,
						})
						if !dryRun && request == nil {
							_, err = memoryAdminRequestCancellation(storage.BrainDir, job, startedAt)
							if err == nil {
								receipt.Artifacts[artifactIndex].NewState = "requested"
							}
						}
					default:
						receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
							Path: memoryJobRel(job.JobID), PriorState: job.State, NewState: job.State,
						})
						request, requestErr := loadMemoryCancellationRequest(storage.BrainDir, job.JobID)
						if requestErr != nil {
							return requestErr
						}
						if request != nil {
							artifactIndex := len(receipt.Artifacts)
							newState := "absent"
							if !dryRun {
								newState = "write_outcome_unknown"
							}
							receipt.Artifacts = append(receipt.Artifacts, memoryReceiptArtifact{
								Path: memoryCancellationRel(job.JobID), PriorState: "requested", NewState: newState,
							})
							if !dryRun {
								if err := memoryAdminRemoveCancellation(storage.BrainDir, job.JobID); err != nil {
									return err
								}
								receipt.Artifacts[artifactIndex].NewState = "absent"
							}
							eligible++
						}
						continue
					}
					if err != nil {
						return err
					}
					eligible++
				}
				return nil
			}
			if dryRun {
				err = cancel()
			} else {
				err = withBrainWriteLock(storage.BrainDir, cancel)
			}
			if err != nil {
				return fail(err)
			}
			return emitMemoryOperationSuccess(cmd, &receipt, jsonOut, opts.Now().UTC(), func(receipt memoryOperationReceipt) any {
				return map[string]any{"receipt": receipt, "jobs_matched": matched, "jobs_eligible": eligible}
			}, func(out io.Writer, receipt memoryOperationReceipt) {
				renderMemoryOperationReceiptText(out, receipt, "completed")
				fmt.Fprintf(out, "result jobs_matched=%d jobs_eligible=%d\n", matched, eligible)
			})
		},
	}
	cmd.Flags().StringVar(&sessionRef, "session-ref", "", "Cancel eligible jobs for this canonical session reference")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report eligible cancellation transitions without changing durable work")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}
