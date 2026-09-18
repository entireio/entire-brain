package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAbstractEgressInventoryMixedCorruptionPreservesCommittedReceipt(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 9, 18, 13, 0, 0, 0, time.UTC)
	ref := conversationSessionIDPrefix + "inventory-good"
	good := abstractEgressReceipt{
		OperationID: strings.Repeat("a", 20), SessionRef: ref,
		SessionDigest: "sha256:" + strings.Repeat("b", 64), Provider: "codex", Model: "hosted-test",
		StartedAt: now, FinishedAt: now.Add(time.Second), Status: "completed",
	}
	if err := writeAbstractEgressReceipt(brainDir, good); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(brainDir, filepath.FromSlash(abstractEgressDirRel))
	if err := os.WriteFile(filepath.Join(root, "README.txt"), []byte("not a receipt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "not-a-shard"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantUnsafe := 2
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(root, "README.txt"), filepath.Join(root, "unsafe-link")); err != nil {
			t.Fatal(err)
		}
		wantUnsafe++
	}

	legacy := good
	legacy.SchemaVersion = abstractEgressLegacyVersion
	legacy.OperationID = strings.Repeat("c", 20)
	legacyBytes, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// The filename deliberately disagrees with the durable operation identity.
	if err := os.WriteFile(filepath.Join(root, "wrong-operation.json"), append(legacyBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := abstractEgressSessionHash(ref)
	firstDir := filepath.Join(root, digest[:2])
	if err := os.WriteFile(filepath.Join(firstDir, "not-a-directory"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondDir := filepath.Join(firstDir, digest[2:4])
	if err := os.WriteFile(filepath.Join(secondDir, "junk.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(secondDir, "directory.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondDir, "malformed.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrong := good
	wrong.SessionRef = conversationSessionIDPrefix + "different-session"
	wrong.OperationID = strings.Repeat("d", 20)
	wrongBytes, err := json.Marshal(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondDir, "wrong-path.json"), append(wrongBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	inventory := loadAbstractEgressInventory(brainDir)
	wantIssues := 7
	if runtime.GOOS != "windows" {
		wantIssues++
	}
	if !inventory.Degraded || inventory.EntriesScanned < 5 || len(inventory.Issues) < wantIssues {
		t.Fatalf("mixed inventory was not fully classified: %+v", inventory)
	}
	if len(inventory.Receipts) != 1 || inventory.Receipts[0].Receipt.OperationID != good.OperationID || inventory.Receipts[0].Receipt.Status != "completed" {
		t.Fatalf("committed receipt was lost or replaced: %+v", inventory.Receipts)
	}
	codes := map[string]int{}
	for _, issue := range inventory.Issues {
		codes[issue.Code]++
	}
	if codes[memoryErrStateUnsafe] < wantUnsafe || codes[memoryErrStateCorrupt] < 3 {
		t.Fatalf("issue taxonomy = %#v, want unsafe and corrupt structural findings", codes)
	}
	if receipts, err := loadAbstractEgressReceiptsChecked(brainDir); err == nil || receipts != nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("checked inventory must fail closed: receipts=%+v err=%v", receipts, err)
	}
	file, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, ref)
	if err != nil || !present || file.Receipt.OperationID != good.OperationID {
		t.Fatalf("targeted committed receipt changed: file=%+v present=%v err=%v", file, present, err)
	}
}

func TestAbstractEgressInventoryRejectsNonDirectoryRoot(t *testing.T) {
	brainDir := t.TempDir()
	root := filepath.Join(brainDir, filepath.FromSlash(abstractEgressDirRel))
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("committed non-directory state"), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := loadAbstractEgressInventory(brainDir)
	if !inventory.Degraded || len(inventory.Issues) != 1 || inventory.Issues[0].File != abstractEgressDirRel || inventory.Issues[0].Code != memoryErrStateUnsafe || len(inventory.Receipts) != 0 {
		t.Fatalf("non-directory root classification: %+v", inventory)
	}
	got, err := os.ReadFile(root)
	if err != nil || string(got) != "committed non-directory state" {
		t.Fatalf("inventory mutated unsafe root: %q err=%v", got, err)
	}
}

func TestManualAbstractEnqueueRefusalDryRunAndDurableRetry(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	now := time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC)
	originalFactory := memoryAbstractorFactory
	t.Cleanup(func() { memoryAbstractorFactory = originalFactory })
	localOverride := memoryAbstractJobOverride{Provider: "ollama", Model: "local-test"}
	memoryAbstractorFactory = nil
	if _, _, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, localOverride, now, nil, false); err == nil || !strings.Contains(err.Error(), memoryErrProviderUnavail) {
		t.Fatalf("nil factory error = %v", err)
	}
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return nil, errors.New("factory unavailable")
	}
	if _, _, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, localOverride, now, nil, false); err == nil || !strings.Contains(err.Error(), "factory unavailable") {
		t.Fatalf("factory error = %v", err)
	}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	if _, _, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, memoryAbstractJobOverride{Provider: "codex", Model: "hosted", HostedEgressAllowed: true}, now, nil, false); err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("named hosted no-egress error = %v", err)
	}
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{hosted: true}, nil }
	if _, _, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, localOverride, now, nil, false); err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("resolved hosted no-egress error = %v", err)
	}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	if _, _, err := enqueueManualAbstractJobWithReceipt(brainDir, conversationSessionIDPrefix+"missing", localOverride, now, nil, false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing session error = %v", err)
	}

	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{hosted: true}, nil
	}
	for name, override := range map[string]memoryAbstractJobOverride{
		"invalid provider":       {Provider: "unknown-provider"},
		"hosted without consent": {Provider: "codex", Model: "test"},
	} {
		if job, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now, nil, false); err == nil || created || job.JobID != "" {
			t.Fatalf("%s must refuse before durable work: job=%+v created=%v err=%v", name, job, created, err)
		}
	}
	if jobs := loadMemoryJobs(brainDir); len(jobs) != 0 {
		t.Fatalf("refused provider selection created jobs: %+v", jobs)
	}

	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	override := localOverride
	dryDetail := manualAbstractEnqueueReceipt{}
	dryJob, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now, &dryDetail, true)
	if err != nil || !created || dryJob.JobID == "" || dryDetail.JobID != dryJob.JobID || dryDetail.Artifact.PriorState != "absent" || dryDetail.Artifact.NewState != memoryJobStatePending {
		t.Fatalf("dry run truth: job=%+v created=%v detail=%+v err=%v", dryJob, created, dryDetail, err)
	}
	if jobs := loadMemoryJobs(brainDir); len(jobs) != 0 {
		t.Fatalf("dry run persisted work: %+v", jobs)
	}

	commitDetail := manualAbstractEnqueueReceipt{}
	job, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now, &commitDetail, false)
	if err != nil || !created || commitDetail.JobID != job.JobID || commitDetail.Artifact.NewState != memoryJobStatePending {
		t.Fatalf("committed enqueue: job=%+v created=%v detail=%+v err=%v", job, created, commitDetail, err)
	}
	if jobs := loadMemoryJobs(brainDir); len(jobs) != 1 || jobs[0].JobID != job.JobID || jobs[0].AbstractOverride == nil || jobs[0].AbstractOverride.Model != "local-test" {
		t.Fatalf("durable job mismatch: %+v", jobs)
	}
	pendingDetail := manualAbstractEnqueueReceipt{}
	pending, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now.Add(30*time.Second), &pendingDetail, false)
	if err != nil || created || pending.State != memoryJobStatePending || pendingDetail.Artifact.PriorState != memoryJobStatePending || pendingDetail.Artifact.NewState != memoryJobStatePending {
		t.Fatalf("pending idempotence: job=%+v created=%v detail=%+v err=%v", pending, created, pendingDetail, err)
	}

	running, err := transitionMemoryJob(brainDir, pending, memoryJobStateRunning, now.Add(time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	runningDetail := manualAbstractEnqueueReceipt{}
	observed, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now.Add(90*time.Second), &runningDetail, false)
	if err != nil || created || observed.State != memoryJobStateRunning || runningDetail.Artifact.PriorState != memoryJobStateRunning || runningDetail.Artifact.NewState != memoryJobStateRunning {
		t.Fatalf("running ownership observation: job=%+v created=%v detail=%+v err=%v", observed, created, runningDetail, err)
	}
	changed := override
	changed.Model = "different-model"
	if _, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, changed, now.Add(90*time.Second), nil, false); err == nil || created || !strings.Contains(err.Error(), memoryErrLockBusy) {
		t.Fatalf("running provider takeover: created=%v err=%v", created, err)
	}
	retryable, err := transitionMemoryJob(brainDir, running, memoryJobStateRetryable, now.Add(2*time.Minute), "provider temporarily unavailable")
	if err != nil {
		t.Fatal(err)
	}
	dryRetryDetail := manualAbstractEnqueueReceipt{}
	dryRetry, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now.Add(150*time.Second), &dryRetryDetail, true)
	if err != nil || created || dryRetry.State != memoryJobStatePending || dryRetryDetail.Artifact.PriorState != memoryJobStateRetryable || dryRetryDetail.Artifact.NewState != memoryJobStatePending {
		t.Fatalf("dry retry truth: job=%+v created=%v detail=%+v err=%v", dryRetry, created, dryRetryDetail, err)
	}
	if persisted, err := memoryJobByID(brainDir, job.JobID); err != nil || persisted.State != memoryJobStateRetryable {
		t.Fatalf("dry retry mutated durable job: job=%+v err=%v", persisted, err)
	}
	retryDetail := manualAbstractEnqueueReceipt{}
	retried, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now.Add(3*time.Minute), &retryDetail, false)
	if err != nil || created || retried.JobID != retryable.JobID || retried.State != memoryJobStatePending || retryDetail.Artifact.PriorState != memoryJobStateRetryable || retryDetail.Artifact.NewState != memoryJobStatePending {
		t.Fatalf("durable retry truth: job=%+v created=%v detail=%+v err=%v", retried, created, retryDetail, err)
	}
	if retried.Error != nil || retried.StartedAt != nil || retried.FinishedAt != nil || retried.OwnerToken != "" || !retried.AvailableAt.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("retry ownership was not reset: %+v", retried)
	}
	if jobs := loadMemoryJobs(brainDir); len(jobs) != 1 || jobs[0].State != memoryJobStatePending {
		t.Fatalf("retry duplicated or failed to persist: %+v", jobs)
	}
	superseded, err := transitionMemoryJob(brainDir, retried, memoryJobStateSuperseded, now.Add(4*time.Minute), "newer session digest")
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now.Add(5*time.Minute), nil, false); err == nil || created || !strings.Contains(err.Error(), memoryErrSourceStale) || superseded.State != memoryJobStateSuperseded {
		t.Fatalf("superseded job must remain terminal: created=%v job=%+v err=%v", created, superseded, err)
	}
}

func TestManualAbstractEnqueueFailsClosedOnPrivateAndCorruptState(t *testing.T) {
	originalFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	t.Cleanup(func() { memoryAbstractorFactory = originalFactory })
	override := memoryAbstractJobOverride{Provider: "ollama", Model: "local-test"}
	now := time.Date(2026, 9, 18, 15, 0, 0, 0, time.UTC)

	t.Run("private session", func(t *testing.T) {
		brainDir, _ := writeSessionNavigationFixture(t)
		view := sessionViewForTest(t, brainDir)
		stones := loadSessionTombstones(brainDir)
		stones.Excluded[view.Records[0].SessionID] = sessionTombstone{At: now, Reason: "explicitly private"}
		if err := saveSessionTombstones(brainDir, stones); err != nil {
			t.Fatal(err)
		}
		if _, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now, nil, false); err == nil || created || !strings.Contains(err.Error(), memoryErrSourceStale) {
			t.Fatalf("private enqueue: created=%v err=%v", created, err)
		}
		if jobs := loadMemoryJobs(brainDir); len(jobs) != 0 {
			t.Fatalf("private enqueue persisted jobs: %+v", jobs)
		}
	})

	t.Run("corrupt manifest", func(t *testing.T) {
		brainDir, _ := writeSessionNavigationFixture(t)
		view := sessionViewForTest(t, brainDir)
		if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now, nil, false); err == nil || created || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("corrupt manifest enqueue: created=%v err=%v", created, err)
		}
	})

	t.Run("corrupt job inventory", func(t *testing.T) {
		brainDir, _ := writeSessionNavigationFixture(t)
		view := sessionViewForTest(t, brainDir)
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		jobID := memoryJobID(manifest.RepoKey, view.Ref, sessionViewDigest(view), memoryJobKindSessionAbstract)
		jobPath := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(jobID)))
		if err := os.MkdirAll(filepath.Dir(jobPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jobPath, []byte("{broken"), 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(jobPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, created, err := enqueueManualAbstractJobWithReceipt(brainDir, view.Ref, override, now, nil, false); err == nil || created || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("corrupt inventory enqueue: created=%v err=%v", created, err)
		}
		after, err := os.ReadFile(jobPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("corrupt committed bytes changed: before=%q after=%q err=%v", before, after, err)
		}
	})
}
