package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

func TestMemoryLifecycleReconcileDryRunReceiptAndProofCompletion(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	snapshot, err := prepareMemoryReconcileSnapshot(context.Background(), brainDir)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &memoryOperationReceipt{}
	created, err := reconcileMemoryJobsLockedDetailed(brainDir, "regression", now, snapshot, true, receipt)
	if err != nil || created != 2 || len(receipt.JobIDs) != 2 || len(receipt.Artifacts) != 2 {
		t.Fatalf("dry-run created=%d receipt=%+v err=%v", created, receipt, err)
	}
	if jobs := loadMemoryJobs(brainDir); len(jobs) != 0 {
		t.Fatalf("dry-run wrote jobs: %+v", jobs)
	}
	for _, artifact := range receipt.Artifacts {
		if artifact.PriorState != "absent" || artifact.NewState != memoryJobStatePending {
			t.Fatalf("dry-run artifact=%+v", artifact)
		}
		if _, statErr := os.Stat(filepath.Join(brainDir, filepath.FromSlash(artifact.Path))); !os.IsNotExist(statErr) {
			t.Fatalf("dry-run artifact exists: %s (%v)", artifact.Path, statErr)
		}
	}

	stats := runMemoryProjectionLaneTest(t, brainDir, now.Add(time.Minute))
	if stats.JobsCompleted != 2 {
		t.Fatalf("projection stats=%+v", stats)
	}
	jobs := loadMemoryJobs(brainDir)
	job := jobs[0]
	job.State = memoryJobStatePending
	job.FinishedAt = nil
	job.Error = nil
	job.OwnerToken = ""
	job.HeartbeatAt = nil
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	snapshot, err = prepareMemoryReconcileSnapshot(context.Background(), brainDir)
	if err != nil {
		t.Fatal(err)
	}
	receipt = &memoryOperationReceipt{}
	created, err = reconcileMemoryJobsLockedDetailed(brainDir, "proof", now.Add(2*time.Minute), snapshot, true, receipt)
	if err != nil || created != 0 {
		t.Fatalf("proof dry-run created=%d err=%v", created, err)
	}
	if len(receipt.Artifacts) != 1 || receipt.Artifacts[0].PriorState != memoryJobStatePending || receipt.Artifacts[0].NewState != memoryJobStateComplete {
		t.Fatalf("proof receipt=%+v", receipt)
	}
	stored, err := memoryJobByID(brainDir, job.JobID)
	if err != nil || stored.State != memoryJobStatePending {
		t.Fatalf("dry-run changed proof-backed job: %+v err=%v", stored, err)
	}
	if err := writeMemoryCancellationRequest(brainDir, stored, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	receipt = &memoryOperationReceipt{}
	created, err = reconcileMemoryJobsLockedDetailed(brainDir, "proof", now.Add(4*time.Minute), snapshot, false, receipt)
	if err != nil || created != 0 {
		t.Fatalf("proof apply created=%d err=%v", created, err)
	}
	stored, err = memoryJobByID(brainDir, job.JobID)
	if err != nil || stored.State != memoryJobStateComplete || stored.FinishedAt == nil {
		t.Fatalf("proof apply job=%+v err=%v", stored, err)
	}
	if request, loadErr := loadMemoryCancellationRequest(brainDir, job.JobID); loadErr != nil || request != nil {
		t.Fatalf("proof apply retained cancellation: request=%+v err=%v", request, loadErr)
	}
	if len(receipt.Artifacts) != 2 || receipt.Artifacts[0].NewState != memoryJobStateComplete || receipt.Artifacts[1].PriorState != "requested" || receipt.Artifacts[1].NewState != "absent" {
		t.Fatalf("proof apply receipt=%+v", receipt)
	}
}

func TestMemoryLifecycleProjectionFailureRetriesThenCompletes(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)
	originalWrite := historyProjectionWriteFile
	failures := 0
	historyProjectionWriteFile = func(dir, rel string, data []byte, mode os.FileMode) error {
		if failures == 0 && filepath.Base(rel) == historystore.IndexFileName {
			failures++
			return os.ErrPermission
		}
		return brainstore.WriteBrainRelativeFileAtomic(dir, rel, data, mode)
	}
	t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
	first, err := runMemoryProjectionLane(context.Background(), brainDir, now, "retry-owner")
	if err != nil || first.JobsRetried != 2 {
		t.Fatalf("failed projection stats=%+v err=%v", first, err)
	}
	for _, job := range loadMemoryJobs(brainDir) {
		if job.State != memoryJobStateRetryable || job.Error == nil {
			t.Fatalf("failed projection job=%+v", job)
		}
	}
	historyProjectionWriteFile = originalWrite
	second, err := runMemoryProjectionLane(context.Background(), brainDir, now.Add(2*time.Minute), "retry-owner")
	if err != nil || second.JobsCompleted != 2 {
		t.Fatalf("retried projection stats=%+v err=%v", second, err)
	}
}

func TestMemoryLifecycleProjectionPreclaimCancellationRestoresEarlierClaim(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 9, 18, 11, 30, 0, 0, time.UTC)
	if _, err := reconcileMemoryJobs(context.Background(), brainDir, "regression", now); err != nil {
		t.Fatal(err)
	}
	runnable := runnableMemoryJobs(loadMemoryJobs(brainDir), now)
	if len(runnable) != 2 {
		t.Fatalf("runnable jobs=%+v", runnable)
	}
	if err := writeMemoryCancellationRequest(brainDir, runnable[1], now); err != nil {
		t.Fatal(err)
	}
	stats, err := runMemoryProjectionLane(context.Background(), brainDir, now, "cancel-owner")
	if err != nil || stats.JobsCancelled != 1 || stats.JobsCompleted != 0 || stats.JobsRetried != 0 {
		t.Fatalf("cancellation stats=%+v err=%v", stats, err)
	}
	first, err := memoryJobByID(brainDir, runnable[0].JobID)
	if err != nil || first.State != memoryJobStatePending || first.OwnerToken != "" || first.HeartbeatAt != nil {
		t.Fatalf("earlier claim was not restored: %+v err=%v", first, err)
	}
	second, err := memoryJobByID(brainDir, runnable[1].JobID)
	if err != nil || second.State != memoryJobStateCancelled || second.CancelRequestedAt == nil {
		t.Fatalf("marked job was not cancelled: %+v err=%v", second, err)
	}
	if request, loadErr := loadMemoryCancellationRequest(brainDir, second.JobID); loadErr != nil || request != nil {
		t.Fatalf("cancellation marker retained: request=%+v err=%v", request, loadErr)
	}
}

func TestMemoryLifecycleMaintenanceScopesHintsAgainstReceipts(t *testing.T) {
	brainDir, _, newRel := shortTermFixture(t)
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{
		SessionID: "sess-live", Branch: "feature", Agent: "claude", LatestCheckpoint: "cp2", TranscriptPath: newRel, CreatedAt: now.Add(-time.Hour),
	})
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeMemoryHint(brainDir, "test/stm", "sess-live", "", "session_end", now); err != nil {
		t.Fatal(err)
	}
	if _, err := writeMemoryHint(brainDir, "test/stm", "sess-live", "main", "checkpoint", now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := prepareMemoryReconcileSnapshot(context.Background(), brainDir)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := maintainMemoryWorkerStateLocked(brainDir, now, snapshot)
	if err != nil || stats.HintsConsumed != 0 || len(loadMemoryHints(brainDir)) != 2 {
		t.Fatalf("premature maintenance stats=%+v hints=%+v err=%v", stats, loadMemoryHints(brainDir), err)
	}
	projection := runMemoryProjectionLaneTest(t, brainDir, now.Add(time.Minute))
	if projection.JobsCompleted != 3 || projection.HintsConsumed != 1 {
		t.Fatalf("projection/maintenance stats=%+v", projection)
	}
	hints := loadMemoryHints(brainDir)
	if len(hints) != 1 || hints[0].Branch != "" {
		t.Fatalf("multi-scope branchless hint should remain: %+v", hints)
	}
}

func TestMemoryLifecycleProjectionCancellationDuringPreparationRestoresPeer(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 9, 18, 13, 0, 0, 0, time.UTC)
	originalWrite := historyProjectionWriteFile
	var once sync.Once
	var cancelledID string
	var cancellationWriteErr error
	historyProjectionWriteFile = func(dir, rel string, data []byte, mode os.FileMode) error {
		if err := originalWrite(dir, rel, data, mode); err != nil {
			return err
		}
		if strings.Contains(filepath.ToSlash(rel), "/staging/") && strings.HasSuffix(rel, ".json") {
			once.Do(func() {
				for _, job := range loadMemoryJobs(brainDir) {
					if job.Kind == memoryJobKindProjection && job.State == memoryJobStateRunning {
						cancelledID = job.JobID
						cancellationWriteErr = writeMemoryCancellationRequest(brainDir, job, now.Add(time.Second))
						break
					}
				}
			})
		}
		return nil
	}
	t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
	stats, err := runMemoryProjectionLane(context.Background(), brainDir, now, "prepare-cancel-owner")
	if err != nil || cancellationWriteErr != nil || stats.JobsCancelled != 1 || cancelledID == "" {
		t.Fatalf("preparation cancellation stats=%+v id=%q hook_err=%v err=%v", stats, cancelledID, cancellationWriteErr, err)
	}
	for _, job := range loadMemoryJobs(brainDir) {
		if job.JobID == cancelledID {
			if job.State != memoryJobStateCancelled || job.CancelRequestedAt == nil || job.OwnerToken != "" {
				t.Fatalf("cancelled job=%+v", job)
			}
		} else if job.Kind == memoryJobKindProjection {
			if job.State != memoryJobStatePending || job.Attempt != 0 || job.OwnerToken != "" || job.StartedAt != nil {
				t.Fatalf("restored peer=%+v", job)
			}
		}
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	receipts, _ := loadProjectionState(brainDir, manifest.Sources.History)
	for _, receipt := range receipts.Sessions {
		if receipt.InputDigest != "" {
			for _, job := range loadMemoryJobs(brainDir) {
				if job.JobID == cancelledID && receipt.SessionRef == job.SessionRef && receipt.InputDigest == job.InputDigest {
					t.Fatal("cancelled preparation published receipt truth")
				}
			}
		}
	}
}

func TestMemoryLifecycleProjectionOwnershipTakeoverRejectsStaleSettlement(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 9, 18, 13, 30, 0, 0, time.UTC)
	beforeManifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	beforeReceipts, _ := loadProjectionState(brainDir, beforeManifest.Sources.History)
	originalWrite := historyProjectionWriteFile
	var once sync.Once
	var takeoverID string
	var takeoverWriteErr error
	historyProjectionWriteFile = func(dir, rel string, data []byte, mode os.FileMode) error {
		if err := originalWrite(dir, rel, data, mode); err != nil {
			return err
		}
		if strings.Contains(filepath.ToSlash(rel), "/staging/") {
			once.Do(func() {
				for _, job := range loadMemoryJobs(brainDir) {
					if job.Kind == memoryJobKindProjection && job.State == memoryJobStateRunning {
						takeoverID = job.JobID
						job.OwnerToken = "new-owner"
						takeoverWriteErr = saveMemoryJob(brainDir, job)
						break
					}
				}
			})
		}
		return nil
	}
	t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
	stats, err := runMemoryProjectionLane(context.Background(), brainDir, now, "old-owner")
	if err == nil || takeoverWriteErr != nil || !strings.Contains(err.Error(), "ownership changed") || stats.JobsCompleted != 0 || takeoverID == "" {
		t.Fatalf("takeover stats=%+v id=%q hook_err=%v err=%v", stats, takeoverID, takeoverWriteErr, err)
	}
	job, loadErr := memoryJobByID(brainDir, takeoverID)
	if loadErr != nil || job.State != memoryJobStateRunning || job.OwnerToken != "new-owner" {
		t.Fatalf("takeover durable job=%+v err=%v", job, loadErr)
	}
	manifest, loadErr := bmanifest.Load(brainDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	receipts, _ := loadProjectionState(brainDir, manifest.Sources.History)
	beforeReceipt, beforeOK := beforeReceipts.receiptFor(job.SessionRef)
	afterReceipt, afterOK := receipts.receiptFor(job.SessionRef)
	if beforeOK && beforeReceipt.InputDigest == afterReceipt.InputDigest {
		t.Fatalf("fixture did not publish fresh shared receipt: before=%+v after=%+v", beforeReceipt, afterReceipt)
	}
	if !afterOK || afterReceipt.InputDigest != job.InputDigest {
		t.Fatalf("shared projection receipt is not exact truth: %+v/%t", afterReceipt, afterOK)
	}
}

func TestMemoryLifecycleCommandValidationAndStatusRendering(t *testing.T) {
	opts := Options{}
	for _, tc := range []struct {
		name string
		cmd  func(Options) *cobra.Command
		args []string
		want string
	}{
		{name: "worker-once", cmd: newMemoryWorkerCommand, want: "only --once"},
		{name: "worker-delay", cmd: newMemoryWorkerCommand, args: []string{"--once", "--delay=-1s"}, want: "delay must be"},
		{name: "worker-prepass", cmd: newMemoryWorkerCommand, args: []string{"--once", "--prepass-failures=-1"}, want: "must not be negative"},
		{name: "jobs-limit", cmd: newMemoryJobsCommand, args: []string{"--limit=0"}, want: "limit must be"},
		{name: "jobs-state", cmd: newMemoryJobsCommand, args: []string{"--state=bogus"}, want: "unknown memory job state"},
		{name: "jobs-kind", cmd: newMemoryJobsCommand, args: []string{"--kind=bogus"}, want: "unknown memory job kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.cmd(opts)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			_, err := execute(t, cmd, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v want %q", err, tc.want)
			}
		})
	}

	var out bytes.Buffer
	renderMemoryStatusSlice(&out, []any{
		map[string]any{}, map[string]any{"state": "ok"}, []any{}, []any{"value"}, true,
	}, 1)
	text := out.String()
	for _, want := range []string{"- {}", "state: \"ok\"", "- []", "- \"value\"", "- true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered status missing %q: %s", want, text)
		}
	}
	out.Reset()
	renderMemoryStatusMap(&out, map[string]any{"empty": []any{}}, 0)
	if out.String() != "empty: []\n" {
		t.Fatalf("empty status slice=%q", out.String())
	}
}

func TestMemoryLifecycleStatsMergeAndSelectors(t *testing.T) {
	stats := memoryWorkerStats{HealthIssues: []string{"first"}}
	stats.add(memoryWorkerStats{
		JobsCreated: 1, JobsCompleted: 2, JobsRetried: 3, JobsRecovered: 4,
		JobsCancelled: 5, JobsPruned: 6, HintsConsumed: 7, AlreadyActive: true,
		PrepassFailed: true, PrepassGaveUp: true,
		HealthDegraded: true, HealthIssues: []string{"first", "second"},
	})
	stats.addHealthIssue("")
	stats.addHealthIssue("second")
	if stats.JobsCreated != 1 || stats.JobsCompleted != 2 || stats.JobsRetried != 3 || stats.JobsRecovered != 4 || stats.JobsCancelled != 5 || stats.JobsPruned != 6 || stats.HintsConsumed != 7 || !stats.AlreadyActive || !stats.PrepassFailed || !stats.PrepassGaveUp || !stats.HealthDegraded || len(stats.HealthIssues) != 2 {
		t.Fatalf("merged stats=%+v", stats)
	}

	brainDir := t.TempDir()
	if _, err := memoryJobsForSelector(brainDir, "", ""); err == nil {
		t.Fatal("empty selector unexpectedly succeeded")
	}
	if _, err := memoryJobsForSelector(brainDir, "job:missing", ""); err == nil || !strings.Contains(err.Error(), brainstore.SourceStaleCode) {
		t.Fatalf("missing exact selector error=%v", err)
	}
	if _, err := memoryJobsForSelector(brainDir, "", "conversation-session:missing"); err == nil || !strings.Contains(err.Error(), brainstore.SourceStaleCode) {
		t.Fatalf("missing session selector error=%v", err)
	}
}

func TestMemoryLifecycleWorkerOriginNotificationContracts(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "1")
	textOut, err := execute(t, newMemoryNotifyCommand(Options{}), "--event", "session_end", "--session", "ignored")
	if err != nil || !strings.Contains(textOut, "ignored memory notification from worker origin") {
		t.Fatalf("worker-origin text=%q err=%v", textOut, err)
	}
	jsonOut, err := execute(t, newMemoryNotifyCommand(Options{}), "--event", "session_end", "--session", "ignored", "--json")
	if err != nil || !strings.Contains(jsonOut, `"ignored": true`) || !strings.Contains(jsonOut, `"reason": "worker_origin"`) {
		t.Fatalf("worker-origin json=%q err=%v", jsonOut, err)
	}
}

func TestMemoryLifecycleDefensiveHelpersAndCorruptInputs(t *testing.T) {
	if err := writeMemoryStatusText(&cobra.Command{}, map[string]any{"bad": make(chan int)}); err == nil || !strings.Contains(err.Error(), "encode memory status") {
		t.Fatalf("unencodable status error=%v", err)
	}
	if got := memoryStatusScalar(make(chan int)); got != "null" {
		t.Fatalf("unencodable scalar=%q", got)
	}
	if now := memoryClockNow(nil); now.Location() != time.UTC || time.Since(now) > time.Minute {
		t.Fatalf("nil clock=%v", now)
	}
	brainDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(brainDir, bmanifest.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileMemoryJobsLockedDetailed(brainDir, "corrupt", time.Now().UTC(), nil, false, nil); err == nil {
		t.Fatal("corrupt reconcile manifest unexpectedly succeeded")
	}
	if _, err := maintainMemoryWorkerStateLocked(brainDir, time.Now().UTC(), nil); err == nil {
		t.Fatal("corrupt maintenance manifest unexpectedly succeeded")
	}
	if _, err := runMemoryProjectionLane(context.Background(), brainDir, time.Now().UTC(), "corrupt-owner"); err == nil {
		t.Fatal("corrupt projection manifest unexpectedly succeeded")
	}
	blockedBrain := t.TempDir()
	jobsPath := filepath.Join(blockedBrain, filepath.FromSlash(memoryJobsDirRel))
	if err := os.MkdirAll(filepath.Dir(jobsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobsPath, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryJobByID(blockedBrain, "job:blocked"); err == nil {
		t.Fatal("blocked job inventory lookup unexpectedly succeeded")
	}
	if _, err := memoryJobsForSelector(blockedBrain, "job:blocked", ""); err == nil {
		t.Fatal("blocked job inventory selector unexpectedly succeeded")
	}
}

func TestMemoryLifecycleJobsTextIncludesErrorsIssuesAndFilters(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	now := opts.Now().UTC()
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	sessionRef, degraded := conversationSessionRef(manifest.RepoKey, "main", "session-a", digest)
	if degraded {
		t.Fatal("canonical session ref unexpectedly degraded")
	}
	job := memoryJob{
		SchemaVersion: memoryJobSchemaVersion, Kind: memoryJobKindProjection,
		RepoKey: manifest.RepoKey, SessionID: "session-a", SessionRef: sessionRef,
		Branch: "main", InputDigest: digest, Trigger: "regression", State: memoryJobStatePending,
		CreatedAt: now, AvailableAt: now,
	}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	running, err := transitionMemoryJob(brainDir, job, memoryJobStateRunning, now, "")
	if err != nil {
		t.Fatal(err)
	}
	job, err = transitionMemoryJob(brainDir, running, memoryJobStateRetryable, now, "memory_source_stale")
	if err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, newMemoryJobsCommand(opts), "--state", memoryJobStateRetryable, "--session-id", job.SessionID, "--session-ref", job.SessionRef, "--kind", job.Kind)
	if err != nil || !strings.Contains(out, job.JobID) || !strings.Contains(out, "memory_source_stale") {
		t.Fatalf("filtered jobs out=%q err=%v", out, err)
	}
	empty, err := execute(t, newMemoryJobsCommand(opts), "--state", memoryJobStatePending, "--session-id", "other", "--session-ref", "conversation-session:other", "--kind", memoryJobKindSessionAbstract, "--json")
	if err != nil || !strings.Contains(empty, `"jobs": []`) {
		t.Fatalf("nonmatching filters out=%q err=%v", empty, err)
	}
	for _, args := range [][]string{
		{"--state", memoryJobStateRetryable, "--session-id", "other", "--json"},
		{"--state", memoryJobStateRetryable, "--session-id", job.SessionID, "--session-ref", "conversation-session:other", "--json"},
		{"--state", memoryJobStateRetryable, "--session-id", job.SessionID, "--session-ref", job.SessionRef, "--kind", memoryJobKindSessionAbstract, "--json"},
	} {
		filtered, filterErr := execute(t, newMemoryJobsCommand(opts), args...)
		if filterErr != nil || !strings.Contains(filtered, `"jobs": []`) {
			t.Fatalf("filter args=%v out=%q err=%v", args, filtered, filterErr)
		}
	}
	corruptPath := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel), "corrupt.json")
	if err := os.WriteFile(corruptPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = execute(t, newMemoryJobsCommand(opts))
	if err != nil || !strings.Contains(out, "left untouched") || !strings.Contains(out, filepath.Base(corruptPath)) {
		t.Fatalf("degraded jobs out=%q err=%v", out, err)
	}
}

func TestMemoryLifecycleNotifyWarningsAndExactJobIssue(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	originalLaunch := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { memoryWorkerLaunch = originalLaunch })
	originalPushLaunch := memoryPushWorkLaunch
	memoryPushWorkLaunch = func(string, string) error { return nil }
	t.Cleanup(func() { memoryPushWorkLaunch = originalPushLaunch })
	textOut, err := execute(t, newMemoryNotifyCommand(opts), "--event", "session_start", "--session", "text-session")
	if err != nil || !strings.Contains(textOut, "recorded session_start hint") {
		t.Fatalf("notify text=%q err=%v", textOut, err)
	}
	jsonOut, err := execute(t, newMemoryNotifyCommand(opts), "--event", "checkpoint", "--session", "json-session", "--json")
	if err != nil || !strings.Contains(jsonOut, `"warning"`) || !strings.Contains(jsonOut, `"recorded": "checkpoint"`) {
		t.Fatalf("notify json=%q err=%v", jsonOut, err)
	}

	missingID := memoryJobID("repo", "conversation-session:missing", "sha256:"+strings.Repeat("b", 64), memoryJobKindProjection)
	if _, err := memoryJobByID(brainDir, missingID); err == nil {
		t.Fatal("missing memoryJobByID unexpectedly succeeded")
	}
	issuePath := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(missingID)))
	if err := os.MkdirAll(filepath.Dir(issuePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(issuePath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryJobByID(brainDir, missingID); err == nil {
		t.Fatal("exact corrupt job lookup unexpectedly succeeded")
	}
	if _, err := memoryJobsForSelector(brainDir, missingID, ""); err == nil {
		t.Fatal("exact corrupt selector unexpectedly succeeded")
	}
}
