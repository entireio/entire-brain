package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runMemoryProjectionLaneTest(t *testing.T, brainDir string, now time.Time) memoryWorkerStats {
	t.Helper()
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.close(now.Add(time.Second), "complete")
	stats, err := runMemoryProjectionLane(context.Background(), brainDir, now, coordinator.token)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func reconcileMemoryJobsForTestLocked(brainDir, trigger string, now time.Time) (int, error) {
	snapshot, err := prepareMemoryReconcileSnapshot(context.Background(), brainDir)
	if err != nil {
		return 0, err
	}
	return reconcileMemoryJobsLocked(brainDir, trigger, now, snapshot)
}

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

func TestMemoryNotifyIgnoresWorkerOriginBeforeRepoResolution(t *testing.T) {
	t.Setenv(memoryWorkerOriginEnv, "1")
	out, err := execute(t, newMemoryNotifyCommand(Options{}), "--event", "session_end", "--session", "session", "--repo-key", "repo", "--json")
	if err != nil || !strings.Contains(out, `"ignored": true`) || !strings.Contains(out, `"reason": "worker_origin"`) {
		t.Fatalf("worker-origin json: err=%v out=%s", err, out)
	}
	out, err = execute(t, newMemoryNotifyCommand(Options{}), "--event", "session_end", "--session", "session", "--repo-key", "repo")
	if err != nil || !strings.Contains(out, "ignored memory notification") {
		t.Fatalf("worker-origin text: err=%v out=%s", err, out)
	}
}

func TestMemoryWorkerChildEnvironmentReplacesRepositoryRouting(t *testing.T) {
	environ := []string{
		"PATH=/bin",
		envRepoRoot + "=/repo/a",
		memoryWorkerOriginEnv + "=0",
		envRepoRoot + "=/repo/stale",
	}
	got := memoryWorkerChildEnvironment(environ, "/repo/b")
	wantValues := map[string]string{envRepoRoot: "/repo/b", memoryWorkerOriginEnv: "1"}
	counts := map[string]int{}
	values := map[string]string{}
	for _, entry := range got {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, tracked := wantValues[key]; tracked {
			counts[key]++
			values[key] = value
		}
	}
	for key, want := range wantValues {
		if counts[key] != 1 || values[key] != want {
			t.Fatalf("child environment %s count=%d value=%q, want one value %q: %v", key, counts[key], values[key], want, got)
		}
	}
	pathPreserved := false
	for _, entry := range got {
		pathPreserved = pathPreserved || entry == "PATH=/bin"
	}
	if !pathPreserved {
		t.Fatalf("unrelated environment was not preserved: %v", got)
	}
}

func TestMemoryStartupNudgeIsSilentAndSkipsAbsentBrain(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 12, 30, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	oldLaunch := memoryWorkerLaunch
	defer func() { memoryWorkerLaunch = oldLaunch }()
	launches := 0
	memoryWorkerLaunch = func(string) error { launches++; return nil }
	nudgeMemoryAtStartup(context.Background(), opts)
	if launches != 0 {
		t.Fatal("startup must not create or launch for an absent Brain")
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, RepoKey: storage.Key}); err != nil {
		t.Fatal(err)
	}
	nudgeMemoryAtStartup(context.Background(), opts)
	if launches != 1 {
		t.Fatalf("existing Brain startup launches = %d, want 1", launches)
	}
	t.Setenv(memoryWorkerOriginEnv, "1")
	nudgeMemoryAtStartup(context.Background(), opts)
	if launches != 1 {
		t.Fatal("worker-origin startup recursively launched another worker")
	}
}

func TestMemoryWorkerPrepassFailurePreservesProgressAndSchedulesRetry(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 12, 45, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, RepoKey: storage.Key}); err != nil {
		t.Fatal(err)
	}
	pending := memoryJob{Kind: memoryJobKindProjection, RepoKey: storage.Key, SessionID: "already-exported", SessionRef: "conversation-session:already-exported", InputDigest: "sha256:already-exported", Trigger: "startup", State: memoryJobStatePending, CreatedAt: now, AvailableAt: now}
	pending.JobID = memoryJobID(pending.RepoKey, pending.SessionRef, pending.InputDigest, pending.Kind)
	if err := saveMemoryJob(storage.BrainDir, pending); err != nil {
		t.Fatal(err)
	}
	oldPrepass, oldLaunchAfter := memoryWorkerPrepass, memoryWorkerLaunchAfter
	defer func() { memoryWorkerPrepass, memoryWorkerLaunchAfter = oldPrepass, oldLaunchAfter }()
	prepassCalls := 0
	memoryWorkerPrepass = func(context.Context, Options, string) error {
		prepassCalls++
		return errors.New("temporary export failure")
	}
	var scheduled time.Duration
	memoryWorkerLaunchAfter = func(_ string, delay time.Duration) error { scheduled = delay; return nil }
	out, err := execute(t, newMemoryWorkerCommand(opts), "--once")
	if err != nil {
		t.Fatalf("worker must continue already-exported work after prepass failure: %v\n%s", err, out)
	}
	if prepassCalls != 1 || scheduled != time.Minute || !strings.Contains(out, `"prepass_failed": true`) || !strings.Contains(out, `"jobs_retried": 1`) {
		t.Fatalf("prepass calls=%d scheduled=%v output=%s", prepassCalls, scheduled, out)
	}
	job, err := memoryJobByID(storage.BrainDir, pending.JobID)
	if err != nil || job.State != memoryJobStateRetryable {
		t.Fatalf("already-exported pending work did not continue: job=%+v err=%v", job, err)
	}
}

func TestDelayedMemoryWorkerClaimsSchedulerBeforeSleeping(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 12, 50, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, RepoKey: storage.Key}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := acquireMemoryCoordinator(storage.BrainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.close(now.Add(time.Second), "complete")
	started := time.Now()
	out, err := execute(t, newMemoryWorkerCommand(opts), "--once", "--delay", "2s")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("duplicate delayed worker slept before coordinator dedupe: %v", elapsed)
	}
	if !strings.Contains(out, `"already_active": true`) {
		t.Fatalf("duplicate delayed worker output = %s", out)
	}
}

func TestReconcileMemoryAndLaunchPreservesDurableWorkOnLaunchFailure(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		RepoKey:       storage.Key,
		Sources: &brainSources{Sessions: &sessionSourceManifest{
			DefaultBranch: "main",
			Sessions: []exportSession{{
				SessionID:      "missed",
				Branch:         "main",
				TranscriptPath: "sessions/main/missing.jsonl",
			}},
		}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	oldLaunch := memoryWorkerLaunch
	defer func() { memoryWorkerLaunch = oldLaunch }()
	memoryWorkerLaunch = func(string) error { return errors.New("launch failed") }
	created, warning, err := reconcileMemoryAndLaunch(context.Background(), opts, repoDir, "watch")
	if err != nil {
		t.Fatalf("reconciliation must remain successful after launch failure: %v", err)
	}
	if created != 1 || !strings.Contains(warning, "launch failed") {
		t.Fatalf("created=%d warning=%q", created, warning)
	}
	jobs, issues := loadMemoryJobsChecked(storage.BrainDir)
	if len(issues) != 0 || len(jobs) != 1 || jobs[0].SessionID != "missed" || jobs[0].State != memoryJobStateInvalid {
		t.Fatalf("durable reconciliation result missing: jobs=%+v issues=%+v", jobs, issues)
	}
}

func TestMemoryJobFilenameIsPortable(t *testing.T) {
	rel := memoryJobRel("job:0123456789abcdef")
	if strings.Contains(filepath.Base(rel), ":") {
		t.Fatalf("job filename is not Windows-portable: %q", rel)
	}
	if rel != memoryJobRel("job:0123456789abcdef") {
		t.Fatal("job filename must be deterministic")
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
		t.Fatal("retryable_error must not run before available_at")
	}
	if got := runnableMemoryJobs([]memoryJob{retry1}, retry1.AvailableAt); len(got) != 1 || got[0].JobID != retry1.JobID {
		t.Fatalf("scheduled retry was not made runnable: %+v", got)
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

func TestMemoryInterruptedOwnerRecoveryAndCancellation(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	job := memoryJob{
		JobID: memoryJobID("repo", "conversation-session:x", "sha256:a", memoryJobKindProjection),
		Kind:  memoryJobKindProjection, RepoKey: "repo", SessionID: "sess-a",
		SessionRef: "conversation-session:x", InputDigest: "sha256:a", Trigger: "manual",
		State: memoryJobStatePending, CreatedAt: now, AvailableAt: now,
	}
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	running, err := transitionMemoryJob(brainDir, job, memoryJobStateRunning, now, "")
	if err != nil {
		t.Fatal(err)
	}
	running.OwnerToken = "dead-owner"
	running.HeartbeatAt = &now
	if err := saveMemoryJob(brainDir, running); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverStaleMemoryJobsLocked(brainDir, []memoryJob{running}, now, ""); err == nil {
		t.Fatal("recovery without the coordinator lock owner must fail")
	}
	recovered, err := recoverStaleMemoryJobsLocked(brainDir, []memoryJob{running}, now.Add(time.Second), "new-owner")
	if err != nil || recovered != 1 {
		t.Fatalf("recover interrupted owner: recovered=%d err=%v", recovered, err)
	}
	retry, err := memoryJobByID(brainDir, job.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.State != memoryJobStateRetryable || retry.OwnerToken != "" || retry.HeartbeatAt != nil {
		t.Fatalf("recovered job = %+v", retry)
	}
	if len(runnableMemoryJobs([]memoryJob{retry}, retry.AvailableAt)) != 1 {
		t.Fatal("recovered job must re-enter the automatic retry schedule")
	}

	pending, err := transitionMemoryJob(brainDir, retry, memoryJobStatePending, now.Add(2*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	running, err = transitionMemoryJob(brainDir, pending, memoryJobStateRunning, now.Add(2*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := requestMemoryJobCancellation(brainDir, running, now.Add(3*time.Minute))
	if err != nil || requested.CancelRequestedAt == nil || requested.State != memoryJobStateRunning {
		t.Fatalf("cancellation request = %+v err=%v", requested, err)
	}
	cancelled, err := transitionMemoryJob(brainDir, requested, memoryJobStateCancelled, now.Add(3*time.Minute), "")
	if err != nil || cancelled.State != memoryJobStateCancelled || cancelled.OwnerToken != "" {
		t.Fatalf("cancelled = %+v err=%v", cancelled, err)
	}
}

func TestMemoryStateCorruptionAndUnsupportedVersionsAreSurfaced(t *testing.T) {
	brainDir := t.TempDir()
	jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
	hintDir := filepath.Join(brainDir, filepath.FromSlash(memoryHintsDirRel))
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hintDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "broken.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownKind := memoryJob{
		SchemaVersion: 1, JobID: "job:unknown", Kind: "future_kind", RepoKey: "repo", SessionID: "session",
		SessionRef: "conversation-session:x", InputDigest: "sha256:x", State: memoryJobStatePending,
		CreatedAt: time.Now(), AvailableAt: time.Now(),
	}
	unknownData, _ := json.Marshal(unknownKind)
	if err := os.WriteFile(filepath.Join(jobDir, "unknown.json"), unknownData, 0o600); err != nil {
		t.Fatal(err)
	}
	future := memoryJob{SchemaVersion: memoryJobSchemaVersion + 1, Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: "session", SessionRef: "conversation-session:future", InputDigest: "sha256:future", State: memoryJobStatePending, CreatedAt: time.Now(), AvailableAt: time.Now()}
	future.JobID = memoryJobID(future.RepoKey, future.SessionRef, future.InputDigest, future.Kind)
	futureData, _ := json.Marshal(future)
	if err := os.WriteFile(filepath.Join(jobDir, "future.json"), futureData, 0o600); err != nil {
		t.Fatal(err)
	}
	unsupported, _ := json.Marshal(memoryLifecycleHint{SchemaVersion: 99, RepoKey: "repo", SessionID: "session", LastEvent: "session_end", Generation: 1})
	if err := os.WriteFile(filepath.Join(hintDir, "future.json"), unsupported, 0o600); err != nil {
		t.Fatal(err)
	}
	_, jobIssues := loadMemoryJobsChecked(brainDir)
	_, hintIssues := loadMemoryHintsChecked(brainDir)
	if len(jobIssues) != 3 {
		t.Fatalf("job issues = %+v", jobIssues)
	}
	codes := map[string]int{}
	for _, issue := range jobIssues {
		codes[issue.Code]++
	}
	if codes["memory_state_corrupt"] != 2 || codes["memory_unsupported_version"] != 1 {
		t.Fatalf("job issue codes = %+v", jobIssues)
	}
	if len(hintIssues) != 1 || hintIssues[0].Code != "memory_unsupported_version" {
		t.Fatalf("hint issues = %+v", hintIssues)
	}
	err := memoryStateError(append(jobIssues, hintIssues...))
	if err == nil || !strings.Contains(err.Error(), "memory_state_corrupt") {
		t.Fatalf("state error = %v", err)
	}
}

func TestMemoryJobIssuesAreIsolatedAndNeverOverwritten(t *testing.T) {
	for _, tc := range []struct {
		name                string
		code                string
		canonicalMustBeFree bool
		put                 func(t *testing.T, brainDir string, blocked memoryJob) (path string, contents []byte)
	}{
		{
			name: "corrupt deterministic path",
			code: "memory_state_corrupt",
			put: func(t *testing.T, brainDir string, blocked memoryJob) (string, []byte) {
				t.Helper()
				contents := []byte("{not-json")
				path := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(blocked.JobID)))
				if err := os.WriteFile(path, contents, 0o600); err != nil {
					t.Fatal(err)
				}
				return path, contents
			},
		},
		{
			name: "future schema at deterministic path",
			code: "memory_unsupported_version",
			put: func(t *testing.T, brainDir string, blocked memoryJob) (string, []byte) {
				t.Helper()
				blocked.SchemaVersion = memoryJobSchemaVersion + 10
				contents, err := json.MarshalIndent(blocked, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				contents = append(contents, '\n')
				path := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(blocked.JobID)))
				if err := os.WriteFile(path, contents, 0o600); err != nil {
					t.Fatal(err)
				}
				return path, contents
			},
		},
		{
			name:                "future schema at noncanonical path",
			code:                "memory_unsupported_version",
			canonicalMustBeFree: true,
			put: func(t *testing.T, brainDir string, blocked memoryJob) (string, []byte) {
				t.Helper()
				blocked.SchemaVersion = memoryJobSchemaVersion + 10
				contents, err := json.MarshalIndent(blocked, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				contents = append(contents, '\n')
				path := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel), "future-owned-identity.json")
				if err := os.WriteFile(path, contents, 0o600); err != nil {
					t.Fatal(err)
				}
				return path, contents
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, _, _ := shortTermFixture(t)
			now := time.Date(2026, 8, 9, 14, 0, 0, 0, time.UTC)
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			var expected []memoryJob
			for _, session := range manifest.Sources.Sessions.Sessions {
				digest, err := sessionTranscriptDigest(brainDir, session.TranscriptPath)
				if err != nil {
					t.Fatal(err)
				}
				branch := session.Branch
				if branch == "" {
					branch = manifest.Sources.Sessions.DefaultBranch
				}
				ref, _ := conversationSessionRef(manifest.RepoKey, branch, session.SessionID, digest)
				job := memoryJob{
					SchemaVersion: memoryJobSchemaVersion, Kind: memoryJobKindProjection,
					RepoKey: manifest.RepoKey, SessionID: session.SessionID, SessionRef: ref, Branch: branch,
					InputDigest: digest, Trigger: "worker", State: memoryJobStatePending,
					CreatedAt: now, AvailableAt: now,
				}
				job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
				expected = append(expected, job)
			}
			if len(expected) != 2 {
				t.Fatalf("fixture projection jobs = %d, want 2", len(expected))
			}
			jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
			if err := os.MkdirAll(jobDir, 0o700); err != nil {
				t.Fatal(err)
			}
			blockedPath, original := tc.put(t, brainDir, expected[0])

			var created int
			if err := withBrainWriteLock(brainDir, func() error {
				var err error
				created, err = reconcileMemoryJobsForTestLocked(brainDir, "worker", now)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if created != 1 {
				t.Fatalf("unrelated jobs created = %d, want 1", created)
			}
			if _, err := memoryJobsForSelector(brainDir, expected[0].JobID, ""); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("manual mutation error = %v, want %s", err, tc.code)
			}

			stats := runMemoryProjectionLaneTest(t, brainDir, now.Add(time.Second))
			if stats.JobsCompleted != 1 {
				t.Fatalf("valid work was starved: stats=%+v", stats)
			}
			after, err := os.ReadFile(blockedPath)
			if err != nil || string(after) != string(original) {
				t.Fatalf("blocked file was modified: err=%v before=%q after=%q", err, original, after)
			}
			if tc.canonicalMustBeFree {
				if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(expected[0].JobID)))); !os.IsNotExist(err) {
					t.Fatalf("reconcile created duplicate canonical identity: %v", err)
				}
			}
			inventory := loadMemoryJobInventory(brainDir)
			if len(inventory.Jobs) != 1 || inventory.Jobs[0].State != memoryJobStateComplete || len(inventory.Issues) != 1 || inventory.Issues[0].Code != tc.code {
				t.Fatalf("isolated inventory: jobs=%+v issues=%+v", inventory.Jobs, inventory.Issues)
			}
		})
	}
}

func TestMemoryJobIssueEnumerationIsBounded(t *testing.T) {
	brainDir := t.TempDir()
	jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < memoryStateIssueLimit+7; i++ {
		name := filepath.Join(jobDir, fmt.Sprintf("broken-%03d.json", i))
		if err := os.WriteFile(name, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inventory := loadMemoryJobInventory(brainDir)
	if len(inventory.Issues) != memoryStateIssueLimit || inventory.IssueCount != memoryStateIssueLimit+7 {
		t.Fatalf("issues = %d total=%d, want bounded %d total %d", len(inventory.Issues), inventory.IssueCount, memoryStateIssueLimit, memoryStateIssueLimit+7)
	}
}

func TestMemoryManualMutationIgnoresUnrelatedJobIssue(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 15, 30, 0, 0, time.UTC)
	job := memoryJob{
		Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: "valid",
		SessionRef: "conversation-session:valid", InputDigest: "sha256:valid", Trigger: "manual",
		State: memoryJobStateRetryable, Attempt: 1, CreatedAt: now, AvailableAt: now,
	}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	brokenPath := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel), "unrelated.json")
	broken := []byte("{broken")
	if err := os.WriteFile(brokenPath, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := memoryJobsForSelector(brainDir, job.JobID, "")
	if err != nil || len(selected) != 1 {
		t.Fatalf("valid retry selection blocked: jobs=%+v err=%v", selected, err)
	}
	pending, err := transitionMemoryJob(brainDir, selected[0], memoryJobStatePending, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transitionMemoryJob(brainDir, pending, memoryJobStateCancelled, now.Add(time.Second), ""); err != nil {
		t.Fatalf("valid cancellation blocked: %v", err)
	}
	if after, err := os.ReadFile(brokenPath); err != nil || string(after) != string(broken) {
		t.Fatalf("unrelated issue was modified: err=%v", err)
	}
}

func TestMemoryJobInventoryRejectsUnsafeAndArbitraryEntries(t *testing.T) {
	newJob := func(now time.Time) memoryJob {
		job := memoryJob{
			SchemaVersion: memoryJobSchemaVersion, Kind: memoryJobKindProjection,
			RepoKey: "repo", SessionID: "session", SessionRef: "conversation-session:secure",
			InputDigest: "sha256:secure", Trigger: "worker", State: memoryJobStatePending,
			CreatedAt: now, AvailableAt: now,
		}
		job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
		return job
	}
	t.Run("arbitrary current-schema filename", func(t *testing.T) {
		brainDir := t.TempDir()
		jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		job := newJob(time.Date(2026, 8, 9, 16, 0, 0, 0, time.UTC))
		data, _ := json.Marshal(job)
		path := filepath.Join(jobDir, "arbitrary.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		inventory := loadMemoryJobInventory(brainDir)
		if len(inventory.Jobs) != 0 || len(inventory.Issues) != 1 || inventory.Issues[0].Code != memoryErrStateCorrupt {
			t.Fatalf("arbitrary filename inventory = %+v", inventory)
		}
		if _, err := memoryJobsForSelector(brainDir, job.JobID, ""); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("manual mutation error = %v", err)
		}
		if after, err := os.ReadFile(path); err != nil || string(after) != string(data) {
			t.Fatalf("invalid source was modified: err=%v", err)
		}
	})

	t.Run("arbitrary legacy filename", func(t *testing.T) {
		brainDir := t.TempDir()
		jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		job := newJob(time.Date(2026, 8, 9, 16, 1, 0, 0, time.UTC))
		job.SchemaVersion = 1
		data, _ := json.Marshal(job)
		path := filepath.Join(jobDir, "legacy-arbitrary.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		inventory := loadMemoryJobInventory(brainDir)
		if len(inventory.Jobs) != 0 || len(inventory.Migrations) != 0 || len(inventory.Issues) != 1 || inventory.Issues[0].Code != memoryErrStateCorrupt {
			t.Fatalf("arbitrary legacy inventory = %+v", inventory)
		}
		if after, err := os.ReadFile(path); err != nil || string(after) != string(data) {
			t.Fatalf("invalid legacy source was modified: err=%v", err)
		}
	})

	t.Run("symlinked entry", func(t *testing.T) {
		brainDir := t.TempDir()
		jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		job := newJob(time.Date(2026, 8, 9, 16, 5, 0, 0, time.UTC))
		data, _ := json.Marshal(job)
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, data, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(job.JobID)))
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		inventory := loadMemoryJobInventory(brainDir)
		if len(inventory.Jobs) != 0 || len(inventory.Issues) != 1 || inventory.Issues[0].Code != memoryErrStateUnsafe {
			t.Fatalf("symlink inventory = %+v", inventory)
		}
		if after, err := os.ReadFile(outside); err != nil || string(after) != string(data) {
			t.Fatalf("symlink target was modified: err=%v", err)
		}
	})

	t.Run("non-regular entry", func(t *testing.T) {
		brainDir := t.TempDir()
		path := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel), "nested.json")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		inventory := loadMemoryJobInventory(brainDir)
		if len(inventory.Jobs) != 0 || len(inventory.Issues) != 1 || inventory.Issues[0].Code != memoryErrStateUnsafe {
			t.Fatalf("non-regular inventory = %+v", inventory)
		}
	})

	t.Run("symlinked jobs directory", func(t *testing.T) {
		brainDir := t.TempDir()
		parent := filepath.Dir(filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel)))
		if err := os.MkdirAll(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		inventory := loadMemoryJobInventory(brainDir)
		if len(inventory.Jobs) != 0 || len(inventory.Issues) != 1 || inventory.Issues[0].Kind != "job_directory" || inventory.Issues[0].Code != memoryErrStateUnsafe {
			t.Fatalf("symlinked directory inventory = %+v", inventory)
		}
		if err := memoryJobEnumerationError(inventory.Issues); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
			t.Fatalf("directory enumeration error = %v", err)
		}
	})
}

func TestMemoryLegacyJobUsesActualPathForPrivacyCleanup(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 9, 16, 30, 0, 0, time.UTC)
	job := memoryJob{
		SchemaVersion: 1, Kind: memoryJobKindProjection, RepoKey: "test/privacy", SessionID: "secret-sess",
		SessionRef: "conversation-session:legacy", InputDigest: "sha256:legacy", Trigger: "worker",
		State: memoryJobStatePending, CreatedAt: now, AvailableAt: now,
	}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	legacyRel := legacyMemoryJobRel(job.JobID)
	if err := os.MkdirAll(filepath.Dir(filepath.Join(brainDir, filepath.FromSlash(legacyRel))), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(legacyRel)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var plan sessionPurgePlan
	if err := withBrainWriteLock(brainDir, func() error {
		var err error
		plan, err = buildSessionPurgePlan(brainDir, job.SessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, artifact := range plan.WorkMetadata {
		if artifact.Path == legacyRel {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("legacy source path absent from privacy inventory: %+v", plan.WorkMetadata)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		return purgeMemoryWorkForSessionLocked(brainDir, job.SessionID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(legacyRel))); !os.IsNotExist(err) {
		t.Fatalf("legacy source survived privacy cleanup: %v", err)
	}
}

func TestMemoryJobV1StringErrorAdaptsToV2(t *testing.T) {
	brainDir := t.TempDir()
	jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	jobID := memoryJobID("repo", "conversation-session:v1", "sha256:v1", memoryJobKindProjection)
	legacy := map[string]any{
		"schema_version": 1, "job_id": jobID, "kind": memoryJobKindProjection, "repo_key": "repo",
		"session_id": "session", "session_ref": "conversation-session:v1", "input_digest": "sha256:v1",
		"trigger": "worker", "state": memoryJobStateRetryable, "attempt": 1, "created_at": now, "available_at": now,
		"error": "provider leaked /private/path and secret response",
	}
	data, _ := json.Marshal(legacy)
	legacyRel := legacyMemoryJobRel(jobID)
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(legacyRel)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, issues := loadMemoryJobsChecked(brainDir)
	if err := memoryStateError(issues); err != nil || len(jobs) != 1 {
		t.Fatalf("adapt jobs=%+v issues=%+v err=%v", jobs, issues, err)
	}
	if jobs[0].SchemaVersion != 2 || jobs[0].Error == nil || jobs[0].Error.Code != "memory_operation_failed" || jobs[0].Error.Detail != "redacted" {
		t.Fatalf("adapted v1 job = %+v", jobs[0])
	}
	findings := scanMemoryJobSchemas(brainDir)
	if len(findings) != 1 || findings[0].Code != "memory_migration_required" || findings[0].Version != 1 {
		t.Fatalf("v1 schema findings = %+v", findings)
	}
	if err := saveMemoryJob(brainDir, jobs[0]); err != nil {
		t.Fatal(err)
	}
	// The exact legacy filename remains discoverable until migration removes it;
	// the canonical rewrite itself must be v2.
	canonical, err := safeReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(jobID))), maxManifestBytes)
	if err != nil || !strings.Contains(string(canonical), `"schema_version": 2`) || strings.Contains(string(canonical), "private/path") {
		t.Fatalf("canonical v2 rewrite err=%v data=%s", err, canonical)
	}
}

func TestMemoryCoordinatorIsSingleInstanceAndWritesContentFreeHealth(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	first, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer first.close(now.Add(time.Second), "complete")
	if _, err := acquireMemoryCoordinator(brainDir, now, "test"); err == nil || !strings.Contains(err.Error(), "memory_worker_active") {
		t.Fatalf("second coordinator acquire = %v", err)
	}
	state, err := loadMemoryCoordinatorState(brainDir)
	if err != nil || state.State != "running" || state.OwnerToken != first.token {
		t.Fatalf("coordinator state = %+v err=%v", state, err)
	}
	logData, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "event=worker_started") || strings.Contains(string(logData), brainDir) {
		t.Fatalf("unexpected worker log: %q", logData)
	}
}

func TestMemoryBranchlessHintRequiresExactlyOneScope(t *testing.T) {
	branchless := memoryLifecycleHint{}
	if memoryHintConsumable(branchless, 0, true) || memoryHintConsumable(branchless, 2, true) || memoryHintConsumable(branchless, 1, false) {
		t.Fatal("branchless hint must require one satisfied canonical scope")
	}
	if !memoryHintConsumable(branchless, 1, true) {
		t.Fatal("one satisfied scope must consume a branchless hint")
	}
	if !memoryHintConsumable(memoryLifecycleHint{Branch: "main"}, 2, true) {
		t.Fatal("an explicitly scoped hint may consume its satisfied matching scope")
	}
}

func TestMemoryAutomaticAbstractUsesDurableLane(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	config := loadMemoryConfig(brainDir)
	config.Abstracts.Enabled = true
	config.Abstracts.Automatic = true
	config.Abstracts.Provider = "fake"
	if err := saveMemoryConfig(brainDir, config); err != nil {
		t.Fatal(err)
	}
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{}, nil
	}
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.close(now.Add(time.Minute), "complete")
	stats, err := runMemoryAbstractLane(context.Background(), t.TempDir(), brainDir, now, coordinator.token)
	if err != nil {
		t.Fatal(err)
	}
	if stats.JobsCreated != 1 || stats.JobsCompleted != 1 {
		t.Fatalf("abstract lane stats = %+v", stats)
	}
	jobs, issues := loadMemoryJobsChecked(brainDir)
	if err := memoryStateError(issues); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Kind != memoryJobKindSessionAbstract || jobs[0].State != memoryJobStateComplete {
		t.Fatalf("abstract jobs = %+v", jobs)
	}
	if status, artifact := sessionAbstractStatus(brainDir, sessionViewForTest(t, brainDir)); status != abstractStatusCurrent || artifact == nil {
		t.Fatalf("abstract status = %s artifact=%+v", status, artifact)
	}
	view := sessionViewForTest(t, brainDir)
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(abstractRel(sessionViewDigest(view))))); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		_, err := reconcileAbstractJobsLocked(brainDir, "repair", now.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reactivated, err := memoryJobByID(brainDir, jobs[0].JobID)
	if err != nil || reactivated.State != memoryJobStatePending {
		t.Fatalf("missing abstract did not reactivate complete job: %+v err=%v", reactivated, err)
	}
}

func TestMemoryMissingProjectionReceiptReactivatesCompletedJob(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 9, 17, 0, 0, 0, time.UTC)
	_ = runMemoryProjectionLaneTest(t, brainDir, now)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.History.ProjectionStatePath))); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		_, err := reconcileMemoryJobsForTestLocked(brainDir, "repair", now.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	jobs := loadMemoryJobs(brainDir)
	pending := 0
	for _, job := range jobs {
		if job.State == memoryJobStatePending {
			pending++
		}
	}
	if pending != 2 {
		t.Fatalf("jobs after lost receipt = %+v, want both complete identities reactivated", jobs)
	}
}

func TestMemoryUnreadableSessionIsInvalidWithoutBlockingPeers(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	now := time.Date(2026, 8, 9, 17, 30, 0, 0, time.UTC)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	bad := manifest.Sources.Sessions.Sessions[0]
	bad.SessionID = "unreadable"
	bad.TranscriptPath = "sessions/main/missing.jsonl"
	manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, bad)
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		_, err := reconcileMemoryJobsForTestLocked(brainDir, "manual", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	jobs := loadMemoryJobs(brainDir)
	states := map[string]memoryJob{}
	for _, job := range jobs {
		states[job.SessionID] = job
	}
	unreadable, ok := states["unreadable"]
	if !ok || unreadable.State != memoryJobStateInvalid || unreadable.Error == nil || unreadable.Error.Code != "memory_source_unreadable" {
		t.Fatalf("unreadable reconciliation jobs = %+v", jobs)
	}
	// Adding the unreadable manifest entry changes the generation-wide source
	// fingerprint, so the prior receipt set is stale. The readable peer must be
	// independently queued rather than blocked behind the invalid job.
	peer, ok := states["nav-sess"]
	if !ok || peer.State != memoryJobStatePending {
		t.Fatalf("readable peer was not queued independently: %+v", jobs)
	}
}

func TestMemoryJobAdministrationSelectors(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	ref := "conversation-session:scope"
	jobs := []memoryJob{
		{Kind: memoryJobKindProjection, RepoKey: storage.Key, SessionID: "sess", SessionRef: ref, InputDigest: "sha256:a", State: memoryJobStateRetryable, Attempt: 1, CreatedAt: now, AvailableAt: now},
		{Kind: memoryJobKindSessionAbstract, RepoKey: storage.Key, SessionID: "sess", SessionRef: ref, InputDigest: "sha256:b", State: memoryJobStateInvalid, CreatedAt: now.Add(time.Second), AvailableAt: now},
		{Kind: memoryJobKindProjection, RepoKey: storage.Key, SessionID: "other", SessionRef: "conversation-session:other", InputDigest: "sha256:c", State: memoryJobStatePending, CreatedAt: now.Add(2 * time.Second), AvailableAt: now},
	}
	for i := range jobs {
		jobs[i].JobID = memoryJobID(jobs[i].RepoKey, jobs[i].SessionRef, jobs[i].InputDigest, jobs[i].Kind)
		if err := saveMemoryJob(storage.BrainDir, jobs[i]); err != nil {
			t.Fatal(err)
		}
	}
	out, err := execute(t, NewRootCommand(opts), "memory", "jobs", "--session-ref", ref, "--kind", memoryJobKindProjection, "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("jobs json: %v\n%s", err, out)
	}
	var payload struct {
		Jobs []memoryJob `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || len(payload.Jobs) != 1 || payload.Jobs[0].Kind != memoryJobKindProjection {
		t.Fatalf("jobs payload = %+v decode=%v\n%s", payload, err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "memory", "jobs", "--session-id", "other")
	if err != nil || !strings.Contains(out, jobs[2].JobID) || strings.Contains(out, jobs[0].JobID) {
		t.Fatalf("jobs text selector: err=%v\n%s", err, out)
	}
	if _, err := execute(t, NewRootCommand(opts), "memory", "retry", "--session-ref", ref); err != nil {
		t.Fatal(err)
	}
	for _, job := range loadMemoryJobs(storage.BrainDir) {
		if job.SessionRef == ref && job.State != memoryJobStatePending {
			t.Fatalf("retry selector left job in %s: %+v", job.State, job)
		}
	}
	if _, err := execute(t, NewRootCommand(opts), "memory", "cancel", "--session-ref", ref); err != nil {
		t.Fatal(err)
	}
	for _, job := range loadMemoryJobs(storage.BrainDir) {
		if job.SessionRef == ref && job.State != memoryJobStateCancelled {
			t.Fatalf("cancel selector left job in %s: %+v", job.State, job)
		}
	}
}

func TestMemoryPruneRequiresReplacementTruthProof(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 19, 0, 0, 0, time.UTC)
	finished := now.Add(-31 * 24 * time.Hour)
	old := memoryJob{Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: "sess", SessionRef: "conversation-session:x", InputDigest: "sha256:old", State: memoryJobStateCancelled, CreatedAt: finished, AvailableAt: finished, FinishedAt: &finished}
	old.JobID = memoryJobID(old.RepoKey, old.SessionRef, old.InputDigest, old.Kind)
	pending := memoryJob{Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: "sess", SessionRef: old.SessionRef, InputDigest: "sha256:new", State: memoryJobStatePending, CreatedAt: now, AvailableAt: now}
	pending.JobID = memoryJobID(pending.RepoKey, pending.SessionRef, pending.InputDigest, pending.Kind)
	for _, job := range []memoryJob{old, pending} {
		if err := saveMemoryJob(brainDir, job); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := pruneMemoryJobsLocked(brainDir, []memoryJob{old, pending}, now)
	if err != nil || pruned != 0 {
		t.Fatalf("pending sibling must not prove pruning: pruned=%d err=%v", pruned, err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(old.JobID)))); err != nil {
		t.Fatalf("cancelled intent was pruned without replacement truth: %v", err)
	}
}

func TestMemoryCancelRaceReportsTooLateAndRemovesMarker(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 19, 30, 0, 0, time.UTC)
	job := memoryJob{Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: "sess", SessionRef: "conversation-session:race", InputDigest: "sha256:race", State: memoryJobStatePending, CreatedAt: now, AvailableAt: now}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	running, err := transitionMemoryJob(brainDir, job, memoryJobStateRunning, now, "")
	if err != nil {
		t.Fatal(err)
	}
	running.OwnerToken = "owner"
	if err := saveMemoryJob(brainDir, running); err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireBrainWriteLock(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		done <- requestRunningMemoryJobCancellationVerified(brainDir, running, now.Add(time.Second))
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	if request, readErr := loadMemoryCancellationRequest(brainDir, job.JobID); readErr != nil || request != nil {
		unlock()
		t.Fatalf("cancel marker escaped commit lock: request=%+v err=%v", request, readErr)
	}
	if _, err := transitionMemoryJob(brainDir, running, memoryJobStateComplete, now.Add(2*time.Second), ""); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
	cancelErr := <-done
	if cancelErr == nil || !strings.Contains(cancelErr.Error(), memoryErrCancelTooLate) {
		t.Fatalf("cancel race error = %v", cancelErr)
	}
	if code := memoryErrorCode(cancelErr); code != memoryErrCancelTooLate {
		t.Fatalf("cancel race stable code = %q, want %q", code, memoryErrCancelTooLate)
	}
	if request, err := loadMemoryCancellationRequest(brainDir, job.JobID); err != nil || request != nil {
		t.Fatalf("stale marker after too-late race: request=%+v err=%v", request, err)
	}
}

func TestMemoryPrivacyWorkPurgeRemovesMetadata(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	if _, err := writeMemoryHint(brainDir, "repo", "purged", "main", "session_end", now); err != nil {
		t.Fatal(err)
	}
	job := memoryJob{Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: "purged", SessionRef: "conversation-session:x", InputDigest: "sha256:x", State: memoryJobStateRunning, CreatedAt: now, AvailableAt: now, OwnerToken: "owner"}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	if _, err := requestMemoryJobCancellation(brainDir, job, now); err != nil {
		t.Fatal(err)
	}
	if err := purgeMemoryWorkForSessionLocked(brainDir, "purged"); err != nil {
		t.Fatal(err)
	}
	if len(loadMemoryHints(brainDir)) != 0 || len(loadMemoryJobs(brainDir)) != 0 {
		t.Fatal("privacy purge retained hint or job metadata")
	}
	if request, err := loadMemoryCancellationRequest(brainDir, job.JobID); err != nil || request != nil {
		t.Fatalf("privacy purge retained cancellation marker: request=%+v err=%v", request, err)
	}
}

type firstFailureAbstractor struct {
	calls int
}

func (f *firstFailureAbstractor) Identity() (string, string, string) {
	return "local", "ollama", "fake-1"
}

func (f *firstFailureAbstractor) Abstract(ctx context.Context, repoDir string, input conversationAbstractInput) (sessionAbstract, error) {
	f.calls++
	if f.calls == 1 {
		return sessionAbstract{}, errors.New("simulated provider failure with secret detail")
	}
	return (&fakeAbstractor{}).Abstract(ctx, repoDir, input)
}

type advancingFailureAbstractor struct {
	advance func()
}

func (f *advancingFailureAbstractor) Identity() (string, string, string) {
	return "local", "ollama", "fake-1"
}

func (f *advancingFailureAbstractor) Abstract(context.Context, string, conversationAbstractInput) (sessionAbstract, error) {
	f.advance()
	return sessionAbstract{}, errors.New("simulated long provider failure")
}

func TestMemoryLongProviderFailureBacksOffFromSettlementTime(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	startedAt := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	_ = runMemoryProjectionLaneTest(t, brainDir, startedAt)
	config := loadMemoryConfig(brainDir)
	config.Abstracts.Enabled = true
	config.Abstracts.Automatic = true
	config.Abstracts.Provider = "fake"
	if err := saveMemoryConfig(brainDir, config); err != nil {
		t.Fatal(err)
	}
	current := startedAt
	provider := &advancingFailureAbstractor{advance: func() { current = current.Add(2 * time.Minute) }}
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
	coordinator, err := acquireMemoryCoordinator(brainDir, startedAt, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.close(current, "complete")
	stats, err := runMemoryAbstractLaneWithClock(context.Background(), t.TempDir(), brainDir, func() time.Time { return current }, coordinator.token)
	if err != nil {
		t.Fatal(err)
	}
	if stats.JobsRetried != 1 {
		t.Fatalf("abstract lane stats = %+v", stats)
	}
	var retry memoryJob
	for _, job := range loadMemoryJobs(brainDir) {
		if job.Kind == memoryJobKindSessionAbstract && job.State == memoryJobStateRetryable {
			retry = job
			break
		}
	}
	if retry.JobID == "" || retry.FinishedAt == nil {
		t.Fatalf("retryable abstract job missing: %+v", retry)
	}
	settledAt := startedAt.Add(2 * time.Minute)
	if !retry.FinishedAt.Equal(settledAt) || !retry.AvailableAt.Equal(settledAt.Add(time.Minute)) {
		t.Fatalf("retry timing: finished=%v available=%v, want settlement=%v available=%v", retry.FinishedAt, retry.AvailableAt, settledAt, settledAt.Add(time.Minute))
	}
	if runnable := runnableMemoryJobs([]memoryJob{retry}, settledAt); len(runnable) != 0 {
		t.Fatalf("long-running failure retried immediately: %+v", runnable)
	}
	logData, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel)))
	if err != nil || !strings.Contains(string(logData), settledAt.Format(time.RFC3339)+" event=job_retryable_error_") {
		t.Fatalf("retry transition log did not use settlement clock: err=%v log=%s", err, logData)
	}
}

func TestMemoryAbstractDrainContinuesAfterFailure(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 9, 21, 0, 0, 0, time.UTC)
	_ = runMemoryProjectionLaneTest(t, brainDir, now)
	config := loadMemoryConfig(brainDir)
	config.Abstracts.Enabled = true
	config.Abstracts.Automatic = true
	config.Abstracts.Provider = "fake"
	if err := saveMemoryConfig(brainDir, config); err != nil {
		t.Fatal(err)
	}
	provider := &firstFailureAbstractor{}
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.close(now.Add(time.Minute), "complete")
	stats, err := runMemoryAbstractDrain(context.Background(), t.TempDir(), brainDir, now, coordinator.token, 8)
	if err != nil {
		t.Fatal(err)
	}
	if stats.JobsCreated != 2 || stats.JobsRetried != 1 || stats.JobsCompleted != 1 || provider.calls != 2 {
		t.Fatalf("drain stats=%+v provider_calls=%d", stats, provider.calls)
	}
	for _, job := range loadMemoryJobs(brainDir) {
		if job.Kind == memoryJobKindSessionAbstract && job.State == memoryJobStateRetryable {
			if job.Error == nil || job.Error.Code != "memory_operation_failed" || strings.Contains(job.Error.Detail, "secret") {
				t.Fatalf("job error is not structured/redacted: %+v", job.Error)
			}
		}
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
		created, err = reconcileMemoryJobsForTestLocked(brainDir, "manual", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if created != 2 {
		t.Fatalf("jobs created = %d, want 2", created)
	}
	// Idempotent: a second reconcile creates nothing new.
	if err := withBrainWriteLock(brainDir, func() error {
		again, err := reconcileMemoryJobsForTestLocked(brainDir, "manual", now)
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
	stats := runMemoryProjectionLaneTest(t, brainDir, now)
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
		created, err := reconcileMemoryJobsForTestLocked(brainDir, "manual", now.Add(time.Minute))
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
		_, err := reconcileMemoryJobsForTestLocked(brainDir, "manual", now.Add(2*time.Minute))
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
