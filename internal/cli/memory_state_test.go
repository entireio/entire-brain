package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMemoryHintCoalescingAndGenerationSafeRemoval(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	first, err := writeMemoryHint(brainDir, "repo", "sess-a", "main", "session_start", now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeMemoryHint(brainDir, "repo", "sess-a", "main", "session_end", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != 1 || second.Generation != 2 || second.LastEvent != "session_end" {
		t.Fatalf("coalescing: %+v %+v", first, second)
	}
	hints := loadMemoryHints(brainDir)
	if len(hints) != 1 {
		t.Fatalf("hints = %d, want 1 coalesced file", len(hints))
	}
	// Removing with a stale generation keeps the hint (a racing event wins).
	if err := removeMemoryHint(brainDir, first); err != nil {
		t.Fatal(err)
	}
	if len(loadMemoryHints(brainDir)) != 1 {
		t.Fatal("stale-generation removal must keep the newer hint")
	}
	if err := removeMemoryHint(brainDir, second); err != nil {
		t.Fatal(err)
	}
	if len(loadMemoryHints(brainDir)) != 0 {
		t.Fatal("current-generation removal must delete the hint")
	}
	// Invalid events and empty session ids are structured errors.
	if _, err := writeMemoryHint(brainDir, "repo", "sess-a", "", "resume", now); err == nil {
		t.Fatal("unknown event must error")
	}
	if _, err := writeMemoryHint(brainDir, "repo", " ", "", "session_end", now); err == nil {
		t.Fatal("empty session id must error")
	}
}

func TestMemoryJobTransitionsAndRetrySchedule(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	job := memoryJob{
		SchemaVersion: memoryJobSchemaVersion,
		JobID:         memoryJobID("repo", "conversation-session:x", "sha256:a", memoryJobKindProjection),
		Kind:          memoryJobKindProjection, RepoKey: "repo", SessionID: "sess-a",
		SessionRef: "conversation-session:x", InputDigest: "sha256:a", Trigger: "manual",
		State: memoryJobStatePending, CreatedAt: now, AvailableAt: now,
	}
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	// pending -> complete is not a legal transition.
	if _, err := transitionMemoryJob(brainDir, job, memoryJobStateComplete, now, ""); err == nil {
		t.Fatal("pending -> complete must be rejected")
	}
	running, err := transitionMemoryJob(brainDir, job, memoryJobStateRunning, now, "")
	if err != nil || running.Attempt != 1 {
		t.Fatalf("claim: %v attempt=%d", err, running.Attempt)
	}
	retry1, err := transitionMemoryJob(brainDir, running, memoryJobStateRetryable, now, "boom")
	if err != nil {
		t.Fatal(err)
	}
	if got := retry1.AvailableAt.Sub(now); got != time.Minute {
		t.Fatalf("attempt-1 retry delay = %v, want 1m", got)
	}
	if len(runnableMemoryJobs([]memoryJob{retry1}, now)) != 0 {
		t.Fatal("retryable_error must not be runnable")
	}
	// Walk the schedule to the manual-only tail.
	job = retry1
	delays := []time.Duration{5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for i, want := range delays {
		pending, err := transitionMemoryJob(brainDir, job, memoryJobStatePending, now, "")
		if err != nil {
			t.Fatal(err)
		}
		running, err := transitionMemoryJob(brainDir, pending, memoryJobStateRunning, now, "")
		if err != nil {
			t.Fatal(err)
		}
		job, err = transitionMemoryJob(brainDir, running, memoryJobStateRetryable, now, "boom")
		if err != nil {
			t.Fatal(err)
		}
		if got := job.AvailableAt.Sub(now); got != want {
			t.Fatalf("attempt-%d retry delay = %v, want %v", i+2, got, want)
		}
	}
	pending, err := transitionMemoryJob(brainDir, job, memoryJobStatePending, now, "")
	if err != nil {
		t.Fatal(err)
	}
	running, err = transitionMemoryJob(brainDir, pending, memoryJobStateRunning, now, "")
	if err != nil {
		t.Fatal(err)
	}
	parked, err := transitionMemoryJob(brainDir, running, memoryJobStateRetryable, now, "boom")
	if err != nil {
		t.Fatal(err)
	}
	if parked.Attempt != memoryJobMaxAttempts || parked.AvailableAt.Before(now.Add(24*time.Hour)) {
		t.Fatalf("attempt-%d must park for manual retry: %+v", parked.Attempt, parked)
	}
}

func TestMemoryReconcileWorkerAndReceipts(t *testing.T) {
	brainDir, changedRel, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)

	// The fixture appended to one indexed transcript and added a second
	// session after the full build: reconcile must enqueue both scopes.
	var created int
	if err := withBrainWriteLock(brainDir, func() error {
		var err error
		created, err = reconcileMemoryJobsLocked(brainDir, "manual", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if created != 2 {
		t.Fatalf("jobs created = %d, want 2", created)
	}
	// Idempotent: a second reconcile creates nothing new.
	if err := withBrainWriteLock(brainDir, func() error {
		again, err := reconcileMemoryJobsLocked(brainDir, "manual", now)
		if err == nil && again != 0 {
			t.Fatalf("re-reconcile created %d jobs", again)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// A hint for the changed session is consumed once the worker completes
	// the projection and the receipt covers the new digest.
	if err := withBrainWriteLock(brainDir, func() error {
		_, err := writeMemoryHint(brainDir, "test/stm", "sess-live", "main", "session_end", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var stats memoryWorkerStats
	if err := withBrainWriteLock(brainDir, func() error {
		var err error
		stats, err = runMemoryWorkerOnce(brainDir, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stats.JobsCompleted != 2 || stats.HintsConsumed != 1 {
		t.Fatalf("worker stats = %+v, want 2 completed and 1 hint consumed", stats)
	}
	if hints := loadMemoryHints(brainDir); len(hints) != 0 {
		t.Fatalf("hint must be consumed: %+v", hints)
	}
	for _, job := range loadMemoryJobs(brainDir) {
		if job.State != memoryJobStateComplete {
			t.Fatalf("job not complete: %+v", job)
		}
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	receipts, ok := loadProjectionState(brainDir, manifest.Sources.History)
	if !ok || len(receipts.Sessions) != 2 {
		t.Fatalf("receipts = %+v ok=%v", receipts, ok)
	}
	digest, err := sessionTranscriptDigest(brainDir, changedRel)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, receipt := range receipts.Sessions {
		if receipt.SessionID == "sess-live" && receipt.InputDigest == digest && receipt.ExchangeCount == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("receipt for the changed session missing: %+v", receipts.Sessions)
	}

	// A further append supersedes the completed job and enqueues a new one.
	full := filepath.Join(brainDir, filepath.FromSlash(changedRel))
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	extra := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"one more question"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"One more answer."}]}}` + "\n"
	if err := os.WriteFile(full, append(data, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		created, err := reconcileMemoryJobsLocked(brainDir, "manual", now.Add(time.Minute))
		if err == nil && created != 1 {
			t.Fatalf("append reconcile created %d jobs", created)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for _, job := range loadMemoryJobs(brainDir) {
		states[job.State]++
	}
	if states[memoryJobStateSuperseded] != 1 || states[memoryJobStatePending] != 1 || states[memoryJobStateComplete] != 1 {
		t.Fatalf("states after supersede = %+v", states)
	}

	// Tombstoning the session moves its active job to excluded and the next
	// worker pass consumes any hint for it.
	stones := loadSessionTombstones(brainDir)
	stones.Excluded["sess-live"] = sessionTombstone{At: now}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		_, err := reconcileMemoryJobsLocked(brainDir, "manual", now.Add(2*time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	excludedSeen := false
	for _, job := range loadMemoryJobs(brainDir) {
		if job.SessionID == "sess-live" && job.State == memoryJobStateExcluded {
			excludedSeen = true
		}
		if job.SessionID == "sess-live" && (job.State == memoryJobStatePending || job.State == memoryJobStateRunning) {
			t.Fatalf("tombstoned session still has active work: %+v", job)
		}
	}
	if !excludedSeen {
		t.Fatal("tombstoned session's pending job must move to excluded")
	}
}
