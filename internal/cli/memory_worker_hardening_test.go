package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryProjectionClaimCapStillSettlesAllReceiptBackedJobs(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 0, 30, 0, 0, time.UTC)
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		RepoKey:       "test/capped-projection",
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main"}},
	}
	count := memoryProjectionClaimsPerPass + 3
	for i := 0; i < count; i++ {
		rel := fmt.Sprintf("sessions/main/session-%03d.jsonl", i)
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		payload := fmt.Sprintf("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"request %d\"}]}}\n", i)
		if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{SessionID: fmt.Sprintf("session-%03d", i), Branch: "main", TranscriptPath: rel, CreatedAt: now})
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	stats := runMemoryProjectionLaneTest(t, brainDir, now.Add(time.Minute))
	if stats.JobsCompleted != count {
		t.Fatalf("bounded pass did not settle shared receipt truth: completed=%d want=%d stats=%+v", stats.JobsCompleted, count, stats)
	}
	jobs := loadMemoryJobs(brainDir)
	if len(jobs) != count {
		t.Fatalf("jobs=%d want=%d", len(jobs), count)
	}
	for _, job := range jobs {
		if job.State != memoryJobStateComplete {
			t.Fatalf("unsettled job after shared publication: %+v", job)
		}
	}
}

func TestMemoryInventoriesAreBoundedAndMutationsFailClosed(t *testing.T) {
	oldLimit := memoryStateInventoryMaxEntries
	memoryStateInventoryMaxEntries = 2
	t.Cleanup(func() { memoryStateInventoryMaxEntries = oldLimit })

	brainDir := t.TempDir()
	jobDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		if err := os.WriteFile(filepath.Join(jobDir, name), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inventory := loadMemoryJobInventory(brainDir)
	if !inventory.Truncated || inventory.ScanComplete || inventory.EntriesObserved != 3 || inventory.EntriesScanned != 2 {
		t.Fatalf("bounded inventory metadata = %+v", inventory)
	}
	if err := memoryJobEnumerationError(inventory.Issues); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("completeness-dependent mutation did not fail closed: %v", err)
	}
}

func TestMemoryJobsPageReadsThroughNonMatchesWithoutLoadingAllLeaves(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 1, 0, 0, 0, time.UTC)
	for i, state := range []string{memoryJobStateComplete, memoryJobStateComplete, memoryJobStatePending, memoryJobStatePending} {
		job := memoryJob{Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: string(rune('a' + i)), SessionRef: "conversation-session:" + string(rune('a'+i)), InputDigest: "sha256:" + string(rune('a'+i)), Trigger: "test", State: state, CreatedAt: now, AvailableAt: now}
		job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
		if err := saveMemoryJob(brainDir, job); err != nil {
			t.Fatal(err)
		}
	}
	page := loadMemoryJobInventoryPage(brainDir, 1, func(job memoryJob) bool { return job.State == memoryJobStatePending })
	if len(page.Jobs) != 1 || page.Jobs[0].State != memoryJobStatePending {
		t.Fatalf("filtered page = %+v", page)
	}
	if page.EntriesScanned >= page.EntriesObserved || page.ScanComplete {
		t.Fatalf("page loaded every leaf or hid partialness: %+v", page)
	}
}

func TestMemoryHintIdentityIsBoundToRepoAndCanonicalBasename(t *testing.T) {
	brainDir := t.TempDir()
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, RepoKey: "repo/current"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 2, 0, 0, 0, time.UTC)
	canonical, err := writeMemoryHint(brainDir, "repo/current", "session-a", "main", "session_end", now)
	if err != nil {
		t.Fatal(err)
	}
	rogue := canonical
	rogue.RepoKey = "repo/other"
	data, _ := json.Marshal(rogue)
	roguePath := filepath.Join(brainDir, filepath.FromSlash(memoryHintsDirRel), "copied.json")
	if err := os.WriteFile(roguePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := loadMemoryHintInventory(brainDir)
	if len(inventory.Hints) != 1 || len(inventory.Issues) != 1 || inventory.Issues[0].Code != memoryErrStateUnsafe {
		t.Fatalf("hint identity inventory = %+v", inventory)
	}
	if _, err := removeMemoryHintIfCurrent(brainDir, rogue); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("rogue hint removal = %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(memoryHintRel(canonical.RepoKey, canonical.SessionID, canonical.Branch)))); err != nil {
		t.Fatalf("rogue hint affected canonical hint: %v", err)
	}
}

func TestMemoryStateReadersRejectLeafAliases(t *testing.T) {
	brainDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{memoryConfigRel, memoryCoordinatorStateRel, memoryCancellationRel("job:test")} {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, _, err := readMemoryStateFile(brainDir, rel, "test state", maxManifestBytes); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
			t.Fatalf("alias %s read error = %v", rel, err)
		}
	}
}

func TestMemorySidecarFailuresSurfaceWithoutReversingCompletion(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 2, 15, 0, 0, time.UTC)
	job := memoryJob{Kind: memoryJobKindProjection, RepoKey: "repo", SessionID: "session", SessionRef: "conversation-session:session", InputDigest: "sha256:input", Trigger: "test", State: memoryJobStateRunning, Attempt: 1, CreatedAt: now, AvailableAt: now, OwnerToken: "owner"}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(outside, []byte("canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, logPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	completed, err := transitionMemoryJob(brainDir, job, memoryJobStateComplete, now.Add(time.Second), "")
	if err != nil || completed.State != memoryJobStateComplete || len(completed.HealthIssues) == 0 {
		t.Fatalf("completion was reversed or degradation hidden: job=%+v err=%v", completed, err)
	}
	reloaded, err := memoryJobByID(brainDir, job.JobID)
	if err != nil || reloaded.State != memoryJobStateComplete || len(reloaded.HealthIssues) == 0 {
		t.Fatalf("durable health state = %+v err=%v", reloaded, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "canary" {
		t.Fatalf("worker log alias target changed: data=%q err=%v", data, err)
	}
}

func TestMemoryHeartbeatFailureIsReportedAsHealthDegradation(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 2, 20, 0, 0, time.UTC)
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(brainDir, filepath.FromSlash(memoryCoordinatorStateRel))
	outside := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(outside, []byte("canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, statePath); err != nil {
		coordinator.close(now, "failed")
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := coordinator.heartbeat(now.Add(time.Second)); err == nil {
		t.Fatal("unsafe heartbeat target unexpectedly succeeded")
	}
	found := false
	for _, issue := range coordinator.healthIssuesSnapshot() {
		found = found || issue == "memory_heartbeat_write_failed"
	}
	coordinator.close(now.Add(2*time.Second), "failed")
	if !found {
		t.Fatalf("heartbeat failure missing from health: %v", coordinator.healthIssuesSnapshot())
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "canary" {
		t.Fatalf("heartbeat followed alias: data=%q err=%v", data, err)
	}
}

func TestProductionReconcileHashesOutsideBrainWriteLockAndRevalidates(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 9, 2, 30, 0, 0, time.UTC)
	originalDigest := memoryReconcileTranscriptDigest
	var checked, changed bool
	var lockErr error
	memoryReconcileTranscriptDigest = func(dir, rel string) (string, error) {
		unlock, err := acquireBrainWriteLockTimeout(brainDir, 0)
		if err != nil {
			lockErr = err
		} else {
			checked = true
			unlock()
		}
		digest, digestErr := originalDigest(dir, rel)
		if !changed {
			changed = true
			manifest, manifestErr := loadBrainManifest(brainDir)
			if manifestErr == nil {
				manifest.GeneratedAt = manifest.GeneratedAt.Add(time.Nanosecond)
				_ = writeBrainManifestAndReadme(brainDir, *manifest)
			}
		}
		return digest, digestErr
	}
	t.Cleanup(func() { memoryReconcileTranscriptDigest = originalDigest })
	created, err := reconcileMemoryJobs(context.Background(), brainDir, "test", now)
	if err != nil || !checked || lockErr != nil || !changed || created == 0 {
		t.Fatalf("outside-lock CAS reconcile: created=%d checked=%v changed=%v lockErr=%v err=%v", created, checked, changed, lockErr, err)
	}
}

func TestProjectionCancellationRestoresUnrelatedClaimWithoutPenalty(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 9, 3, 0, 0, 0, time.UTC)
	originalWrite := historyProjectionWriteFile
	var once sync.Once
	var cancelledID string
	historyProjectionWriteFile = func(dir, rel string, data []byte, perm os.FileMode) error {
		if err := originalWrite(dir, rel, data, perm); err != nil {
			return err
		}
		if strings.Contains(filepath.ToSlash(rel), "/staging/") && strings.HasSuffix(rel, ".json") {
			once.Do(func() {
				for _, job := range loadMemoryJobs(brainDir) {
					if job.Kind == memoryJobKindProjection && job.State == memoryJobStateRunning {
						cancelledID = job.JobID
						_ = writeMemoryCancellationRequest(brainDir, job, now.Add(time.Second))
						break
					}
				}
			})
		}
		return nil
	}
	t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := runMemoryProjectionLane(context.Background(), brainDir, now, coordinator.token)
	coordinator.close(now.Add(time.Second), "complete")
	if err != nil || stats.JobsCancelled != 1 || cancelledID == "" {
		t.Fatalf("cancelled pass stats=%+v id=%q err=%v", stats, cancelledID, err)
	}
	var cancelled, pending memoryJob
	for _, job := range loadMemoryJobs(brainDir) {
		if job.JobID == cancelledID {
			cancelled = job
		} else if job.Kind == memoryJobKindProjection {
			pending = job
		}
	}
	if cancelled.State != memoryJobStateCancelled || pending.State != memoryJobStatePending || pending.Attempt != 0 || pending.StartedAt != nil || pending.FinishedAt != nil {
		t.Fatalf("cancelled=%+v unrelated=%+v", cancelled, pending)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if receipts, _ := loadProjectionState(brainDir, manifest.Sources.History); func() bool {
		receipt, ok := receipts.receiptFor(cancelled.SessionRef)
		return ok && receipt.InputDigest == cancelled.InputDigest
	}() {
		t.Fatal("aborted replacement published a receipt for the cancelled job")
	}

	historyProjectionWriteFile = originalWrite
	second := runMemoryProjectionLaneTest(t, brainDir, now.Add(2*time.Second))
	if second.JobsCompleted < 2 {
		t.Fatalf("later shared projection did not reconcile receipt truth: %+v", second)
	}
	for _, job := range loadMemoryJobs(brainDir) {
		if job.JobID == cancelledID && job.State != memoryJobStateComplete {
			t.Fatalf("receipt-backed cancelled job stayed contradictory: %+v", job)
		}
	}
}

type blockingContextAbstractor struct {
	started chan struct{}
	done    chan struct{}
}

type memoryVectorTestStore struct{ vectors map[string][]float32 }

func (s *memoryVectorTestStore) ids() (map[string]struct{}, bool) {
	ids := make(map[string]struct{}, len(s.vectors))
	for id := range s.vectors {
		ids[id] = struct{}{}
	}
	return ids, len(s.vectors) > 0
}
func (s *memoryVectorTestStore) upsert(add map[string][]float32, drop []string) error {
	if s.vectors == nil {
		s.vectors = map[string][]float32{}
	}
	for _, id := range drop {
		delete(s.vectors, id)
	}
	for id, vector := range add {
		s.vectors[id] = append([]float32(nil), vector...)
	}
	return nil
}
func (s *memoryVectorTestStore) knnCos([]float32, int) (map[string]float64, bool) { return nil, false }

type memoryContextTestEmbedder struct{ calls int }

func (e *memoryContextTestEmbedder) ID() string             { return "test-context" }
func (e *memoryContextTestEmbedder) Dim() int               { return 2 }
func (e *memoryContextTestEmbedder) Embed(string) []float32 { panic("context path required") }
func (e *memoryContextTestEmbedder) EmbedContext(ctx context.Context, _ string) []float32 {
	e.calls++
	select {
	case <-ctx.Done():
		return nil
	default:
		return []float32{1, 0}
	}
}

func TestMemoryVectorLaneUsesDurableCappedContextBatches(t *testing.T) {
	store := &memoryVectorTestStore{}
	embedder := &memoryContextTestEmbedder{}
	index := historyIndex{}
	for i := 0; i < memoryVectorBatchRecords+9; i++ {
		index.Records = append(index.Records, historyRecord{ID: memoryJobID("repo", "record", string(rune(i)), "vector"), Kind: "decision", Summary: string(rune('a' + (i % 20)))})
	}
	added, _, total, pending, err := syncVectorsForKindsContextBatch(context.Background(), store, index, embedder, func(string) bool { return false }, func(record historyRecord) string { return record.Summary }, memoryVectorBatchRecords)
	if err != nil || added != memoryVectorBatchRecords || total != len(index.Records) || !pending || len(store.vectors) != memoryVectorBatchRecords {
		t.Fatalf("first vector batch: added=%d total=%d pending=%v stored=%d err=%v", added, total, pending, len(store.vectors), err)
	}
	added, _, _, pending, err = syncVectorsForKindsContextBatch(context.Background(), store, index, embedder, func(string) bool { return false }, func(record historyRecord) string { return record.Summary }, memoryVectorBatchRecords)
	if err != nil || added != 9 || pending || len(store.vectors) != len(index.Records) {
		t.Fatalf("resumed vector batch: added=%d pending=%v stored=%d err=%v", added, pending, len(store.vectors), err)
	}
}

func TestMemoryVectorLaneExcludesAmbiguousRecordIDs(t *testing.T) {
	store := &memoryVectorTestStore{vectors: map[string][]float32{
		"collision": {0, 1},
	}}
	embedder := &memoryContextTestEmbedder{}
	index := historyIndex{Records: []historyRecord{
		{ID: "collision", Kind: conversationKind, Branch: "main", SessionID: "one", Summary: "first"},
		{ID: "collision", Kind: conversationKind, Branch: "feature", SessionID: "two", Summary: "second"},
		{ID: "unique", Kind: conversationKind, Branch: "main", SessionID: "one", Summary: "third"},
	}}
	added, dropped, total, pending, err := syncVectorsForKindsContextBatch(context.Background(), store, index, embedder, func(string) bool { return false }, func(record historyRecord) string { return record.Summary }, memoryVectorBatchRecords)
	if err != nil || added != 1 || dropped != 1 || total != 1 || pending {
		t.Fatalf("collision-safe sync: added=%d dropped=%d total=%d pending=%v err=%v", added, dropped, total, pending, err)
	}
	if _, exists := store.vectors["collision"]; exists {
		t.Fatal("ambiguous ID retained a semantic vector")
	}
	if _, exists := store.vectors["unique"]; !exists {
		t.Fatal("unique ID was not embedded")
	}
}

func TestMemoryVectorProgressResetsWhenProjectionTextChangesUnderStableIDs(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("1", 64)
	newDigest := "sha256:" + strings.Repeat("2", 64)
	progress := memoryVectorProgress{ModelID: "model", SourceDigest: oldDigest, CompleteSourceDigest: oldDigest, ResetComplete: true}
	if !memoryVectorProgressNeedsReset(progress, true, "model", newDigest) {
		t.Fatal("changed source digest reused vectors keyed by stable record IDs")
	}
	if memoryVectorProgressNeedsReset(progress, true, "model", oldDigest) {
		t.Fatal("current complete generation unexpectedly requires reset")
	}
}

func TestMemoryVectorReadsCloseWhileNewGenerationIsPending(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 4, 15, 0, 0, time.UTC)
	oldDigest := "sha256:" + strings.Repeat("1", 64)
	newDigest := "sha256:" + strings.Repeat("2", 64)
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/vector-read", Sources: &brainSources{History: &historySourceManifest{IndexDigest: oldDigest, PrivacyIdentity: "absent"}}}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	embedder := &memoryContextTestEmbedder{}
	if err := saveMemoryVectorProgress(brainDir, memoryVectorProgress{ModelID: embedder.ID(), SourceDigest: oldDigest, CompleteSourceDigest: oldDigest, ResetComplete: true, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if !memoryProjectionVectorsCurrent(brainDir, embedder) {
		t.Fatal("completed vector generation was not readable")
	}
	manifest.Sources.History.IndexDigest = newDigest
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if memoryProjectionVectorsCurrent(brainDir, embedder) {
		t.Fatal("stale stable-ID vectors remained readable after source text changed")
	}
	if state := memoryProjectionVectorState(brainDir, embedder); state != "stale" {
		t.Fatalf("changed generation state = %q, want stale", state)
	}
	if err := saveMemoryVectorProgress(brainDir, memoryVectorProgress{ModelID: embedder.ID(), SourceDigest: newDigest, ResetComplete: true, Pending: true, UpdatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if state := memoryProjectionVectorState(brainDir, embedder); state != "pending" {
		t.Fatalf("incomplete bounded sync state = %q, want pending", state)
	}
	if err := writeBrainRelativeFileAtomic(brainDir, memoryVectorProgressRel, []byte(`{"schema_version":3,"future":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := memoryProjectionVectorState(brainDir, embedder); state != "unsupported" {
		t.Fatalf("newer progress state = %q, want unsupported", state)
	}
}

func TestMemoryVectorMissingCheckIgnoresAmbiguousIDs(t *testing.T) {
	store := &memoryVectorTestStore{vectors: map[string][]float32{"unique": {1, 0}}}
	index := historyIndex{Records: []historyRecord{
		{ID: "collision", Kind: conversationKind},
		{ID: "collision", Kind: conversationKind},
		{ID: "unique", Kind: conversationKind},
	}}
	if vectorStoreHasMissing(store, index, func(string) bool { return false }) {
		t.Fatal("ambiguous record ID kept a completed vector sync pending")
	}
}

func TestMemoryVectorProgressRejectsNewerUnknownAndTrailingState(t *testing.T) {
	brainDir := t.TempDir()
	write := func(data string) {
		t.Helper()
		if err := writeBrainRelativeFileAtomic(brainDir, memoryVectorProgressRel, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"schema_version":3,"future":true}`)
	if _, _, err := loadMemoryVectorProgress(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("newer schema error = %v", err)
	}
	validPrefix := fmt.Sprintf(`{"schema_version":%d,"model_id":"model","source_digest":"sha256:%s","complete_source_digest":"sha256:%s","reset_complete":true,"pending":false,"updated_at":"2026-08-09T00:00:00Z"`, memoryVectorSchema, strings.Repeat("1", 64), strings.Repeat("1", 64))
	write(validPrefix + `,"unknown":true}`)
	if _, _, err := loadMemoryVectorProgress(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("unknown field error = %v", err)
	}
	write(validPrefix + `} {}`)
	if _, _, err := loadMemoryVectorProgress(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("trailing data error = %v", err)
	}
}

func TestMemoryVectorWorkerPreservesUnreadableProgressState(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		code string
	}{
		{name: "unknown-newer", data: `{"schema_version":3,"future":true}`, code: memoryErrUnsupportedVersion},
		{name: "corrupt", data: `{"schema_version":2,"model_id":`, code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			now := time.Date(2026, 8, 9, 4, 20, 0, 0, time.UTC)
			digest := "sha256:" + strings.Repeat("1", 64)
			manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/vector-preserve", Sources: &brainSources{History: &historySourceManifest{IndexDigest: digest, PrivacyIdentity: "absent"}}}
			if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
				t.Fatal(err)
			}
			before := []byte(tc.data)
			if err := writeBrainRelativeFileAtomic(brainDir, memoryVectorProgressRel, before, 0o600); err != nil {
				t.Fatal(err)
			}
			_, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, &memoryContextTestEmbedder{}, 1)
			if err == nil || !strings.Contains(err.Error(), tc.code) || !pending {
				t.Fatalf("sync pending=%v error=%v", pending, err)
			}
			after, readErr := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel)))
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("progress bytes changed: after=%q err=%v", after, readErr)
			}
		})
	}
}

func TestMemoryMigrationDetectionClassifiesVectorProgressState(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		code string
	}{
		{name: "unknown-newer", data: `{"schema_version":3,"future":true}`, code: memoryErrUnsupportedVersion},
		{name: "corrupt", data: `{`, code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			if err := writeBrainRelativeFileAtomic(brainDir, memoryVectorProgressRel, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, finding := range detectMemoryMigrations(brainDir, nil) {
				if finding.Path == memoryVectorProgressRel {
					found = finding.State == tc.code
				}
			}
			if !found {
				t.Fatalf("vector progress migration findings = %+v", detectMemoryMigrations(brainDir, nil))
			}
		})
	}
}

func TestMemoryGuardedVectorStoreRejectsPostPurgeCommit(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 9, 4, 30, 0, 0, time.UTC)
	_ = runMemoryProjectionLaneTest(t, brainDir, now)
	epoch, manifest, err := captureMemoryVectorEpoch(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveMemoryVectorProgress(brainDir, memoryVectorProgress{
		ModelID: "test-model", SourceDigest: manifest.Sources.History.IndexDigest,
		ResetComplete: true, Pending: true, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	_, _, ownership, err := loadMemoryVectorProgressWithOwnership(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	raw := &memoryVectorTestStore{}
	guarded := &memoryGuardedVectorStore{brainDir: brainDir, epoch: epoch, ownership: ownership, store: raw}
	if err := guarded.upsert(map[string][]float32{"before": {1, 0}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		stones := emptySessionTombstones()
		stones.Excluded["private"] = sessionTombstone{At: now.Add(time.Second), Reason: "test"}
		if err := saveSessionTombstones(brainDir, stones); err != nil {
			return err
		}
		// Model the purge's wholesale vector-store deletion while it owns the
		// same lock used by guarded commits.
		raw.vectors = map[string][]float32{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err = guarded.upsert(map[string][]float32{"private-canary": {0, 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) {
		t.Fatalf("post-purge commit error = %v", err)
	}
	if _, exists := raw.vectors["private-canary"]; exists {
		t.Fatal("stale vector commit repopulated a purged store")
	}
}

func (b *blockingContextAbstractor) Identity() (string, string, string) {
	return "local", "ollama", "fake"
}
func (b *blockingContextAbstractor) Abstract(ctx context.Context, _ string, _ conversationAbstractInput) (sessionAbstract, error) {
	close(b.started)
	<-ctx.Done()
	close(b.done)
	return sessionAbstract{}, errors.New("provider drained after cancellation")
}

func TestDeterministicFailureCancelsDrainsAndSettlesAbstractProvider(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)
	_ = runMemoryProjectionLaneTest(t, brainDir, now)
	config := loadMemoryConfig(brainDir)
	config.Abstracts.Enabled = true
	config.Abstracts.Automatic = true
	config.Abstracts.Provider = "fake"
	if err := saveMemoryConfig(brainDir, config); err != nil {
		t.Fatal(err)
	}
	provider := &blockingContextAbstractor{started: make(chan struct{}), done: make(chan struct{})}
	originalFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
	t.Cleanup(func() { memoryAbstractorFactory = originalFactory })
	go func() {
		<-provider.started
		_ = os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{"), 0o600)
	}()
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats, runErr := runMemoryAbstractLane(ctx, t.TempDir(), brainDir, now, coordinator.token)
	coordinator.close(now.Add(time.Minute), "failed")
	if runErr == nil || !strings.Contains(runErr.Error(), "brain manifest") || !strings.Contains(runErr.Error(), "provider drained") {
		t.Fatalf("combined deterministic/provider error = %v stats=%+v", runErr, stats)
	}
	select {
	case <-provider.done:
	default:
		t.Fatal("provider goroutine was not drained before coordinator release")
	}
	if stats.JobsRetried != 1 {
		t.Fatalf("claimed provider job was not settled: %+v", stats)
	}
}
