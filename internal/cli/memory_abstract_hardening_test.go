package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLongAbstractRejectsMaliciousSynthesisCitations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		synthesis string
	}{
		{name: "invented evidence", synthesis: `{"overview":{"text":"invented","evidence_ids":["conversation:not-in-fragments"]}}`},
		{name: "missing required citation", synthesis: `{"overview":{"text":"uncited"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &agentConversationAbstractor{kind: "hosted", provider: "codex"}
			calls := 0
			agent.run = func(context.Context, string, []string, []byte, time.Duration) (string, error) {
				calls++
				if calls == 1 {
					return `{"overview":{"text":"first","evidence_ids":["conversation:e1"]}}`, nil
				}
				if calls == 2 {
					return `{"overview":{"text":"second","evidence_ids":["conversation:e2"]}}`, nil
				}
				return tc.synthesis, nil
			}
			_, err := agent.Abstract(context.Background(), t.TempDir(), conversationAbstractInput{Windows: [][]conversationTurn{
				{{ConversationID: "conversation:e1", TurnOrdinal: 1}},
				{{ConversationID: "conversation:e2", TurnOrdinal: 2}},
			}})
			if err == nil || !strings.Contains(err.Error(), "synthesis validation failed") || calls != 3 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestForgedStoredAbstractSchemaIsRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sessionAbstract, conversationSessionView)
	}{
		{name: "zero generated at", mutate: func(a *sessionAbstract, _ conversationSessionView) { a.GeneratedAt = time.Time{} }},
		{name: "unknown generator kind", mutate: func(a *sessionAbstract, _ conversationSessionView) { a.Generator.Kind = "remote-ish" }},
		{name: "empty provider", mutate: func(a *sessionAbstract, _ conversationSessionView) { a.Generator.Provider = "" }},
		{name: "unknown provider", mutate: func(a *sessionAbstract, _ conversationSessionView) { a.Generator.Provider = "custom" }},
		{name: "provider kind mismatch", mutate: func(a *sessionAbstract, _ conversationSessionView) {
			a.Generator.Provider = "codex"
			a.Generator.Kind = "local"
		}},
		{name: "empty model", mutate: func(a *sessionAbstract, _ conversationSessionView) { a.Generator.Model = "" }},
		{name: "unknown contract", mutate: func(a *sessionAbstract, _ conversationSessionView) { a.Generator.ContractVersion = 2 }},
		{name: "unordered ranges", mutate: func(a *sessionAbstract, v conversationSessionView) {
			a.Coverage.IncludedTurns = 2
			a.Coverage.IncludedRanges = []abstractCoverageRange{{StartTurn: v.Records[1].TurnOrdinal, EndTurn: v.Records[1].TurnOrdinal}, {StartTurn: v.Records[0].TurnOrdinal, EndTurn: v.Records[0].TurnOrdinal}}
		}},
		{name: "overlapping ranges", mutate: func(a *sessionAbstract, v conversationSessionView) {
			a.Coverage.IncludedTurns = 3
			a.Coverage.IncludedRanges = []abstractCoverageRange{{StartTurn: v.Records[0].TurnOrdinal, EndTurn: v.Records[1].TurnOrdinal}, {StartTurn: v.Records[1].TurnOrdinal, EndTurn: v.Records[2].TurnOrdinal}}
		}},
		{name: "range outside source", mutate: func(a *sessionAbstract, v conversationSessionView) {
			a.Coverage.IncludedRanges = []abstractCoverageRange{{StartTurn: v.Records[0].TurnOrdinal, EndTurn: v.Records[len(v.Records)-1].TurnOrdinal + 1}}
		}},
		{name: "included count mismatch", mutate: func(a *sessionAbstract, _ conversationSessionView) { a.Coverage.IncludedTurns-- }},
		{name: "included count above bound", mutate: func(a *sessionAbstract, _ conversationSessionView) {
			a.Coverage.TotalTurns = abstractMaxTurns + 10
			a.Coverage.IncludedTurns = abstractMaxTurns + 1
			a.Coverage.IncludedRanges = []abstractCoverageRange{{StartTurn: 1, EndTurn: abstractMaxTurns + 1}}
		}},
		{name: "partial coverage without truncation", mutate: func(a *sessionAbstract, v conversationSessionView) {
			a.Coverage.IncludedTurns = 1
			a.Coverage.IncludedRanges = []abstractCoverageRange{{StartTurn: v.Records[0].TurnOrdinal, EndTurn: v.Records[0].TurnOrdinal}}
			a.InputTruncated = false
		}},
		{name: "evidence outside coverage", mutate: func(a *sessionAbstract, v conversationSessionView) {
			a.Coverage.IncludedTurns = 1
			a.Coverage.IncludedRanges = []abstractCoverageRange{{StartTurn: v.Records[0].TurnOrdinal, EndTurn: v.Records[0].TurnOrdinal}}
			a.InputTruncated = true
			a.Overview.EvidenceIDs = []string{v.Records[len(v.Records)-1].ID}
		}},
		{name: "duplicate evidence", mutate: func(a *sessionAbstract, v conversationSessionView) {
			a.Overview.EvidenceIDs = []string{v.Records[0].ID, v.Records[0].ID}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, _ := writeSessionNavigationFixture(t)
			view := sessionViewForTest(t, brainDir)
			digest := sessionViewDigest(view)
			artifact := validAbstractForView(view, digest)
			tc.mutate(&artifact, view)
			data, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal(err)
			}
			writeAbstractBytesForTest(t, brainDir, digest, append(data, '\n'))
			if _, state, err := loadSessionAbstractChecked(brainDir, digest); err == nil && state == sessionAbstractCurrent {
				if validateSessionAbstract(artifact, view) == nil {
					t.Fatalf("forged artifact was accepted: %+v", artifact)
				}
			}
			if status, exposed := sessionAbstractStatus(brainDir, view); status == abstractStatusCurrent || exposed != nil {
				t.Fatalf("status=%s exposed=%+v", status, exposed)
			}
		})
	}
}

func TestAbstractResolverCompletenessOrdering(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	digest := sessionViewDigest(view)
	artifact := validAbstractForView(view, digest)
	config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}

	resolver := &sessionAbstractResolver{
		config: config,
		inventory: sessionAbstractInventory{
			ByDigest:     map[string]sessionAbstractInventoryEntry{digest: {Artifact: artifact, State: sessionAbstractCurrent}},
			BySessionRef: map[string][]string{}, Truncated: true, Degraded: true,
		},
		jobStatus: map[string]string{}, providerAvailable: true,
	}
	if status, exposed := resolver.status(view); status != abstractStatusCurrent || exposed == nil {
		t.Fatalf("exact current status=%s exposed=%+v", status, exposed)
	}
	delete(resolver.inventory.ByDigest, digest)
	resolver.inventory.BySessionRef[view.Ref] = []string{"sha256:" + strings.Repeat("a", 64)}
	resolver.inventory.Issues = []memoryStateIssue{{Kind: "abstract", Code: memoryErrQueryTooBroad}}
	if status, exposed := resolver.status(view); status != abstractStatusDegraded || exposed != nil {
		t.Fatalf("incomplete stale inference status=%s exposed=%+v", status, exposed)
	}
}

func TestAbstractResolverCarriesAffectedJobIssue(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	digest := sessionViewDigest(view)
	jobID := memoryJobID(manifest.RepoKey, view.Ref, digest, memoryJobKindSessionAbstract)
	jobPath := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(jobID)))
	if err := os.MkdirAll(filepath.Dir(jobPath), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, jobPath); err != nil {
		t.Fatal(err)
	}
	if status, exposed := sessionAbstractStatus(brainDir, view); status != memoryErrStateUnsafe || exposed != nil {
		t.Fatalf("affected unsafe job status=%s exposed=%+v", status, exposed)
	}
}

func TestAbstractResolverPreservesCorruptAndUnsupportedJobCodes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents func(string, string) []byte
		want     string
	}{
		{name: "corrupt", contents: func(_, _ string) []byte { return []byte("{broken") }, want: memoryErrStateCorrupt},
		{name: "unsupported", contents: func(jobID, ref string) []byte {
			data, _ := json.Marshal(map[string]any{"schema_version": memoryJobSchemaVersion + 1, "job_id": jobID, "session_ref": ref})
			return data
		}, want: memoryErrUnsupportedVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, _ := writeSessionNavigationFixture(t)
			view := sessionViewForTest(t, brainDir)
			if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}); err != nil {
				t.Fatal(err)
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			jobID := memoryJobID(manifest.RepoKey, view.Ref, sessionViewDigest(view), memoryJobKindSessionAbstract)
			path := filepath.Join(brainDir, filepath.FromSlash(memoryJobRel(jobID)))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.contents(jobID, view.Ref), 0o600); err != nil {
				t.Fatal(err)
			}
			if status, exposed := sessionAbstractStatus(brainDir, view); status != tc.want || exposed != nil {
				t.Fatalf("status=%s exposed=%+v want=%s", status, exposed, tc.want)
			}
		})
	}
}

func TestAutomaticAbstractEnqueueFailureDoesNotBreakRecall(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	oldFactory := memoryAbstractorFactory
	oldHook := enqueueAutomaticAbstractReconciliationHook
	defer func() {
		memoryAbstractorFactory = oldFactory
		enqueueAutomaticAbstractReconciliationHook = oldHook
	}()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Automatic: true, Provider: "fake"}}); err != nil {
		t.Fatal(err)
	}
	enqueueAutomaticAbstractReconciliationHook = func(string, string, time.Time) error {
		return errors.New(memoryErrStateCorrupt + ": simulated enqueue failure")
	}
	found, missing, err := getUnifiedBatchOptions(t.TempDir(), brainDir, "main", []string{view.Ref}, getOptions{})
	if err != nil || len(found) != 1 || len(missing) != 0 || found[0].AbstractIssue != memoryErrStateCorrupt {
		t.Fatalf("get found=%+v missing=%v err=%v", found, missing, err)
	}
	previews, _, err := abstractPreviewsForResults(brainDir, []unifiedResult{{ID: view.Records[0].ID, SessionRef: view.Ref}})
	if err != nil || len(previews) != 1 || previews[0].AbstractIssue != memoryErrStateCorrupt {
		t.Fatalf("previews=%+v err=%v", previews, err)
	}
}

func TestAbstractWindowsAreStreamingBoundedAndDeterministic(t *testing.T) {
	view := conversationSessionView{Ref: "conversation-session:large"}
	for i := 1; i <= 4097; i++ {
		summary := "bounded summary"
		if i == 2048 {
			summary = strings.Repeat("x", abstractWindowMaxBytes*2)
		}
		view.Records = append(view.Records, historyRecord{ID: "conversation:" + strings.Repeat("x", 8) + time.Unix(int64(i), 0).UTC().Format("150405.000000000"), TurnOrdinal: i, Summary: summary})
	}
	windows, ranges, truncated := abstractWindows(view)
	windowsAgain, rangesAgain, truncatedAgain := abstractWindows(view)
	if !truncated || !truncatedAgain || len(windows) > abstractMaxWindows || len(ranges) != len(windows) || !reflect.DeepEqual(windows, windowsAgain) || !reflect.DeepEqual(ranges, rangesAgain) {
		t.Fatalf("windows=%d ranges=%d truncated=%v deterministic=%v", len(windows), len(ranges), truncated, reflect.DeepEqual(windows, windowsAgain))
	}
	for _, window := range windows {
		if len(window) == 0 || len(window) > abstractWindowMaxTurns {
			t.Fatalf("window turns=%d", len(window))
		}
		data, err := json.Marshal(window)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > abstractWindowMaxBytes {
			t.Fatalf("window bytes=%d", len(data))
		}
	}
}

type countingBlockingAbstractor struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (a *countingBlockingAbstractor) Identity() (string, string, string) {
	return "local", "ollama", "test-1"
}

func (a *countingBlockingAbstractor) Abstract(_ context.Context, _ string, input conversationAbstractInput) (sessionAbstract, error) {
	if a.calls.Add(1) == 1 {
		close(a.started)
	}
	<-a.release
	var artifact sessionAbstract
	artifact.Overview = abstractStatement{Text: "one writer", EvidenceIDs: []string{input.Windows[0][0].ConversationID}}
	return artifact, nil
}

func TestConcurrentManualGenerationUsesOneDurableJobAndProviderCall(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	provider := &countingBlockingAbstractor{started: make(chan struct{}), release: make(chan struct{})}
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
	override := memoryAbstractJobOverride{Provider: "codex", Model: "test"}
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	job, created, err := enqueueManualAbstractJob(brainDir, view.Ref, override, now)
	if err != nil || !created {
		t.Fatalf("enqueue job=%+v created=%v err=%v", job, created, err)
	}
	opts := Options{Version: "test", Now: func() time.Time { return now }}
	repoDir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, _, err := driveManualAbstractJob(context.Background(), repoDir, brainDir, opts, job)
		done <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not start")
	}
	concurrent, created, err := enqueueManualAbstractJob(brainDir, view.Ref, override, now.Add(time.Second))
	if err != nil || created || concurrent.JobID != job.JobID || concurrent.State != memoryJobStateRunning {
		t.Fatalf("matching concurrent job=%+v created=%v err=%v", concurrent, created, err)
	}
	if !equalMemoryAbstractJobOverride(concurrent.AbstractOverride, &override) {
		t.Fatalf("running provider selection changed: %+v", concurrent.AbstractOverride)
	}
	if _, created, err := enqueueManualAbstractJob(brainDir, view.Ref, memoryAbstractJobOverride{Provider: "claude-code", Model: "other"}, now.Add(2*time.Second)); err == nil || !strings.Contains(err.Error(), memoryErrLockBusy) || created {
		t.Fatalf("different concurrent provider selection created=%v err=%v, want typed conflict", created, err)
	}
	close(provider.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls=%d", calls)
	}
	jobs := loadMemoryJobs(brainDir)
	count := 0
	for _, candidate := range jobs {
		if candidate.Kind == memoryJobKindSessionAbstract && candidate.SessionRef == view.Ref && candidate.InputDigest == sessionViewDigest(view) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("abstract jobs=%d, all=%+v", count, jobs)
	}
}

func TestManualGenerationIsNotStrandedBehindAutomaticDrainShare(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	now := time.Date(2026, 8, 9, 16, 0, 0, 0, time.UTC)
	manual, _, err := enqueueManualAbstractJob(brainDir, view.Ref, memoryAbstractJobOverride{Provider: "ollama", Model: "test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		ref := "conversation-session:older-" + time.Unix(int64(i+1), 0).UTC().Format("150405")
		digest := "sha256:" + strings.Repeat(string("abcdef01"[i]), 64)
		job := memoryJob{
			SchemaVersion: memoryJobSchemaVersion, JobID: memoryJobID(manifest.RepoKey, ref, digest, memoryJobKindSessionAbstract),
			Kind: memoryJobKindSessionAbstract, RepoKey: manifest.RepoKey, SessionRef: ref, InputDigest: digest,
			Trigger: "automatic", State: memoryJobStatePending, CreatedAt: now.Add(-time.Hour), AvailableAt: now.Add(-time.Hour),
		}
		if err := saveMemoryJob(brainDir, job); err != nil {
			t.Fatal(err)
		}
	}
	opts := Options{Version: "test", Now: func() time.Time { return now }}
	_, completed, err := driveManualAbstractJob(context.Background(), t.TempDir(), brainDir, opts, manual)
	if err != nil || !completed {
		t.Fatalf("manual completed=%v err=%v", completed, err)
	}
	current, err := memoryJobByID(brainDir, manual.JobID)
	if err != nil || current.State != memoryJobStateComplete {
		t.Fatalf("manual job=%+v err=%v", current, err)
	}
}

func TestDisablingAbstractsCancelsOnlyAutomaticWork(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 17, 0, 0, 0, time.UTC)
	makeJob := func(ref, digest, trigger string, override *memoryAbstractJobOverride) memoryJob {
		return memoryJob{
			SchemaVersion:    memoryJobSchemaVersion,
			JobID:            memoryJobID(manifest.RepoKey, ref, digest, memoryJobKindSessionAbstract),
			Kind:             memoryJobKindSessionAbstract,
			RepoKey:          manifest.RepoKey,
			SessionRef:       ref,
			InputDigest:      digest,
			Trigger:          trigger,
			State:            memoryJobStatePending,
			CreatedAt:        now,
			AvailableAt:      now,
			AbstractOverride: override,
		}
	}
	automaticPending := makeJob("conversation-session:auto-pending", "sha256:"+strings.Repeat("a", 64), "automatic", nil)
	automaticRunning := makeJob("conversation-session:auto-running", "sha256:"+strings.Repeat("b", 64), "worker", nil)
	manualOverride := &memoryAbstractJobOverride{Provider: "ollama", Model: "test"}
	manual := makeJob("conversation-session:manual", "sha256:"+strings.Repeat("c", 64), "manual", manualOverride)
	for _, job := range []memoryJob{automaticPending, automaticRunning, manual} {
		if err := saveMemoryJob(brainDir, job); err != nil {
			t.Fatal(err)
		}
	}
	automaticRunning, err = transitionMemoryJob(brainDir, automaticRunning, memoryJobStateRunning, now, "")
	if err != nil {
		t.Fatal(err)
	}
	var artifacts []memoryReceiptArtifact
	if err := withBrainWriteLock(brainDir, func() error {
		var cancelErr error
		artifacts, cancelErr = cancelAutomaticAbstractJobsLocked(brainDir, now.Add(time.Second))
		return cancelErr
	}); err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("cancellation artifacts=%+v", artifacts)
	}
	pending, err := memoryJobByID(brainDir, automaticPending.JobID)
	if err != nil || pending.State != memoryJobStateCancelled {
		t.Fatalf("automatic pending=%+v err=%v", pending, err)
	}
	running, err := memoryJobByID(brainDir, automaticRunning.JobID)
	if err != nil || running.State != memoryJobStateRunning {
		t.Fatalf("automatic running=%+v err=%v", running, err)
	}
	if request, err := loadMemoryCancellationRequest(brainDir, running.JobID); err != nil || request == nil {
		t.Fatalf("automatic running cancellation=%+v err=%v", request, err)
	}
	manualAfter, err := memoryJobByID(brainDir, manual.JobID)
	if err != nil || manualAfter.State != memoryJobStatePending || !equalMemoryAbstractJobOverride(manualAfter.AbstractOverride, manualOverride) {
		t.Fatalf("manual job=%+v err=%v", manualAfter, err)
	}
	if request, err := loadMemoryCancellationRequest(brainDir, manual.JobID); err != nil || request != nil {
		t.Fatalf("manual cancellation=%+v err=%v", request, err)
	}
}

func TestConfigureAbstractsReturnsValidatedMutationReceipt(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	out, err := execute(t, newMemoryConfigureCommand(opts), "abstracts", "--enable", "--provider", "codex")
	if err != nil {
		t.Fatalf("configure: %v\n%s", err, out)
	}
	var payload struct {
		Receipt memoryOperationReceipt `json:"receipt"`
		Config  memoryConfig           `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Receipt.Operation != "configure_abstracts" || payload.Receipt.FinishedAt.IsZero() || len(payload.Receipt.Artifacts) != 1 || payload.Receipt.Artifacts[0].Path != memoryConfigRel {
		t.Fatalf("receipt=%+v", payload.Receipt)
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	stored, state, err := loadMemoryConfigChecked(storage.BrainDir)
	if err != nil || state != memoryConfigCurrent || stored != payload.Config || !stored.Abstracts.Enabled {
		t.Fatalf("stored=%+v state=%s err=%v payload=%+v", stored, state, err, payload.Config)
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	repoKey := manifest.RepoKey
	if repoKey == "" {
		repoKey = "test/configure"
	}
	automatic := memoryJob{
		SchemaVersion: memoryJobSchemaVersion,
		Kind:          memoryJobKindSessionAbstract,
		RepoKey:       repoKey,
		SessionID:     "configure-disable",
		SessionRef:    "conversation-session:configure-disable",
		Branch:        "main",
		InputDigest:   "sha256:" + strings.Repeat("d", 64),
		Trigger:       "automatic",
		State:         memoryJobStatePending,
		CreatedAt:     now,
		AvailableAt:   now,
	}
	automatic.JobID = memoryJobID(automatic.RepoKey, automatic.SessionRef, automatic.InputDigest, automatic.Kind)
	if err := saveMemoryJob(storage.BrainDir, automatic); err != nil {
		t.Fatal(err)
	}
	if inventory := loadMemoryJobInventory(storage.BrainDir); len(inventory.Jobs) != 1 || len(inventory.Issues) != 0 {
		t.Fatalf("automatic pre-disable inventory=%+v", inventory)
	}
	out, err = execute(t, newMemoryConfigureCommand(opts), "abstracts", "--disable")
	if err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Config.Abstracts.Enabled || len(payload.Receipt.Artifacts) != 2 || payload.Receipt.Artifacts[1].NewState != memoryJobStateCancelled {
		t.Fatalf("disable payload=%+v", payload)
	}
	job, err := memoryJobByID(storage.BrainDir, automatic.JobID)
	if err != nil || job.State != memoryJobStateCancelled {
		t.Fatalf("automatic job after disable=%+v err=%v", job, err)
	}
}

func TestHostedReceiptUsesCompletionClock(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	oldFactory := memoryAbstractorFactory
	oldCompletionNow := abstractCompletionNow
	defer func() {
		memoryAbstractorFactory = oldFactory
		abstractCompletionNow = oldCompletionNow
	}()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{hosted: true}, nil }
	started := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	finished := started.Add(37 * time.Second)
	abstractCompletionNow = func() time.Time { return finished }
	config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake", HostedEgressAllowed: true}}
	if _, err := generateSessionAbstractContext(context.Background(), t.TempDir(), brainDir, view, config, started); err != nil {
		t.Fatal(err)
	}
	receipts, err := loadAbstractEgressReceiptsChecked(brainDir)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	if !receipts[0].Receipt.FinishedAt.Equal(finished) || !receipts[0].Receipt.FinishedAt.After(receipts[0].Receipt.StartedAt) {
		t.Fatalf("receipt did not use completion time: %+v", receipts[0].Receipt)
	}
}

func TestAbstractEgressPathsScaleBeyondFlatInventoryCeiling(t *testing.T) {
	seen := make(map[string]bool, abstractEgressHealthMaxReceipts+257)
	for i := 0; i < abstractEgressHealthMaxReceipts+257; i++ {
		ref := conversationSessionIDPrefix + "scale-" + strconv.Itoa(i)
		rel := abstractEgressReceiptRel(ref)
		if seen[rel] {
			t.Fatalf("path collision for %s", ref)
		}
		seen[rel] = true
		digest := abstractEgressSessionHash(ref)
		wantSuffix := filepath.ToSlash(filepath.Join(digest[:2], digest[2:4], digest+".json"))
		if !strings.HasSuffix(rel, wantSuffix) || !strings.HasPrefix(rel, abstractEgressDirRel+"/") {
			t.Fatalf("unbound sharded path %s for %s", rel, ref)
		}
	}
}

func TestAbstractEgressReceiptAtomicallyReplacesPerSession(t *testing.T) {
	brainDir := t.TempDir()
	ref := conversationSessionIDPrefix + "replace"
	now := time.Date(2026, 8, 9, 19, 0, 0, 0, time.UTC)
	receipt := abstractEgressReceipt{
		OperationID: strings.Repeat("a", 20), SessionRef: ref,
		SessionDigest: "sha256:" + strings.Repeat("b", 64), Provider: "codex", Model: "test",
		StartedAt: now, Status: "started",
	}
	if err := writeAbstractEgressReceipt(brainDir, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.OperationID = strings.Repeat("c", 20)
	receipt.FinishedAt = now.Add(time.Minute)
	receipt.Status = "completed"
	if err := writeAbstractEgressReceipt(brainDir, receipt); err != nil {
		t.Fatal(err)
	}
	file, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, ref)
	if err != nil || !present || file.Receipt.OperationID != receipt.OperationID || file.Path != abstractEgressReceiptRel(ref) {
		t.Fatalf("receipt=%+v present=%v err=%v", file, present, err)
	}
	inventory := loadAbstractEgressInventory(brainDir)
	if len(inventory.Receipts) != 1 || inventory.Degraded {
		t.Fatalf("inventory=%+v", inventory)
	}
}

func TestStaleAbstractEgressReconciliationUsesDurableJobTruth(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("d", 64)
	completedRef := conversationSessionIDPrefix + "completed-truth"
	orphanRef := conversationSessionIDPrefix + "orphan-truth"
	for i, ref := range []string{completedRef, orphanRef} {
		receipt := abstractEgressReceipt{
			OperationID: strings.Repeat(string("ef"[i]), 20), SessionRef: ref, SessionDigest: digest,
			Provider: "codex", Model: "test", StartedAt: now.Add(-abstractEgressStaleAfter - time.Minute), Status: "started",
		}
		if err := writeAbstractEgressReceipt(brainDir, receipt); err != nil {
			t.Fatal(err)
		}
	}
	job := memoryJob{
		SchemaVersion: memoryJobSchemaVersion,
		Kind:          memoryJobKindSessionAbstract, RepoKey: manifest.RepoKey, SessionRef: completedRef, InputDigest: digest,
		Trigger: "manual", State: memoryJobStatePending, CreatedAt: now.Add(-time.Hour), AvailableAt: now.Add(-time.Hour),
		AbstractOverride: &memoryAbstractJobOverride{Provider: "codex", Model: "test", HostedEgressAllowed: true},
	}
	job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
	if err := saveMemoryJob(brainDir, job); err != nil {
		t.Fatal(err)
	}
	job, err = transitionMemoryJob(brainDir, job, memoryJobStateRunning, now.Add(-50*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	job, err = transitionMemoryJob(brainDir, job, memoryJobStateComplete, now.Add(-40*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := withBrainWriteLock(brainDir, func() error {
		var reconcileErr error
		count, reconcileErr = reconcileStaleAbstractEgressReceiptsLocked(brainDir, now)
		return reconcileErr
	}); err != nil || count != 2 {
		t.Fatalf("reconciled=%d err=%v", count, err)
	}
	for ref, want := range map[string]string{completedRef: "completed", orphanRef: "failed"} {
		file, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, ref)
		if err != nil || !present || file.Receipt.Status != want || !file.Receipt.FinishedAt.Equal(now) {
			t.Fatalf("%s receipt=%+v present=%v err=%v", ref, file, present, err)
		}
	}
}

func TestAbstractEgressTargetedPurgePreservesOtherSessions(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 21, 0, 0, 0, time.UTC)
	target := conversationSessionIDPrefix + "target"
	other := conversationSessionIDPrefix + "other"
	for i, ref := range []string{target, other} {
		receipt := abstractEgressReceipt{
			OperationID: strings.Repeat(string("ab"[i]), 20), SessionRef: ref,
			SessionDigest: "sha256:" + strings.Repeat(string("cd"[i]), 64), Provider: "codex", Model: "test",
			StartedAt: now, FinishedAt: now.Add(time.Second), Status: "completed",
		}
		if err := writeAbstractEgressReceipt(brainDir, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := purgeAbstractEgressReceiptsForSessionRefs(brainDir, map[string]bool{target: true}); err != nil {
		t.Fatal(err)
	}
	if _, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, target); err != nil || present {
		t.Fatalf("target present=%v err=%v", present, err)
	}
	if _, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, other); err != nil || !present {
		t.Fatalf("other present=%v err=%v", present, err)
	}
}

func TestLegacyAbstractEgressReceiptIsMigrationRequiredAndPurgeable(t *testing.T) {
	brainDir := t.TempDir()
	ref := conversationSessionIDPrefix + "legacy"
	now := time.Date(2026, 8, 9, 22, 0, 0, 0, time.UTC)
	receipt := abstractEgressReceipt{
		SchemaVersion: abstractEgressLegacyVersion, OperationID: strings.Repeat("a", 20), SessionRef: ref,
		SessionDigest: "sha256:" + strings.Repeat("b", 64), Provider: "codex", Model: "test",
		StartedAt: now, FinishedAt: now.Add(time.Second), Status: "completed",
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	legacyRel := filepath.ToSlash(filepath.Join(abstractEgressDirRel, receipt.OperationID+".json"))
	if err := writeBrainRelativeFileAtomic(brainDir, legacyRel, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := loadAbstractEgressInventory(brainDir)
	if len(inventory.Migrations) != 1 || len(inventory.Issues) != 0 || !inventory.Receipts[0].Legacy {
		t.Fatalf("legacy inventory=%+v", inventory)
	}
	if receipts, err := loadAbstractEgressReceiptsChecked(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrMigrationRequired) || len(receipts) != 1 {
		t.Fatalf("checked receipts=%+v err=%v", receipts, err)
	}
	files, err := abstractEgressReceiptsForSessionRefsChecked(brainDir, map[string]bool{ref: true})
	if err != nil || len(files) != 1 || !files[0].Legacy {
		t.Fatalf("targeted legacy=%+v err=%v", files, err)
	}
	if err := purgeAbstractEgressReceiptsForSessionRefs(brainDir, map[string]bool{ref: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(legacyRel))); !os.IsNotExist(err) {
		t.Fatalf("legacy receipt survived purge: %v", err)
	}
}

func futureEgressBytesForTest(t *testing.T, receipt abstractEgressReceipt) []byte {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["future_audit_field"] = map[string]any{"retention": "new"}
	data, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func TestUnknownNewerAbstractEgressReceiptsStayTypedAndReadOnly(t *testing.T) {
	now := time.Date(2026, 8, 9, 23, 0, 0, 0, time.UTC)
	for _, legacy := range []bool{false, true} {
		name := "canonical"
		if legacy {
			name = "legacy-root"
		}
		t.Run(name, func(t *testing.T) {
			brainDir := t.TempDir()
			ref := conversationSessionIDPrefix + "future-" + name
			receipt := abstractEgressReceipt{
				SchemaVersion: abstractEgressSchemaVersion + 1, OperationID: strings.Repeat("a", 20), SessionRef: ref,
				SessionDigest: "sha256:" + strings.Repeat("b", 64), Provider: "codex", Model: "future",
				StartedAt: now, FinishedAt: now.Add(time.Second), Status: "completed",
			}
			rel := abstractEgressReceiptRel(ref)
			if legacy {
				rel = filepath.ToSlash(filepath.Join(abstractEgressDirRel, receipt.OperationID+".json"))
			}
			want := futureEgressBytesForTest(t, receipt)
			if err := writeBrainRelativeFileAtomic(brainDir, rel, want, 0o600); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if _, err := loadLegacyAbstractEgressReceiptsChecked(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
					t.Fatalf("legacy newer error=%v", err)
				}
			} else {
				if _, _, err := loadAbstractEgressReceiptForSessionChecked(brainDir, ref); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
					t.Fatalf("canonical newer error=%v", err)
				}
				current := receipt
				current.SchemaVersion = abstractEgressSchemaVersion
				current.OperationID = strings.Repeat("c", 20)
				if err := writeAbstractEgressReceipt(brainDir, current); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
					t.Fatalf("guarded upsert error=%v", err)
				}
			}
			inventory := loadAbstractEgressInventory(brainDir)
			if len(inventory.Issues) != 1 || inventory.Issues[0].Code != memoryErrUnsupportedVersion {
				t.Fatalf("newer inventory=%+v", inventory)
			}
			got, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(rel)))
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("newer bytes changed: got=%q err=%v", got, err)
			}
		})
	}
}

func TestUnknownNewerSessionAbstractIsReadOnlyForAutomaticAndManualGeneration(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	digest := sessionViewDigest(view)
	artifact := validAbstractForView(view, digest)
	artifact.SchemaVersion = abstractSchemaVersion + 1
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["future_summary_field"] = []string{"new"}
	want, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	writeAbstractBytesForTest(t, brainDir, digest, want)
	if _, state, err := loadSessionAbstractChecked(brainDir, digest); state != sessionAbstractUnsupported || err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("newer state=%s err=%v", state, err)
	}
	if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Automatic: true, Provider: "codex", Model: "test", HostedEgressAllowed: true}}); err != nil {
		t.Fatal(err)
	}
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{hosted: true}, nil }
	if request, err := automaticAbstractWorkRequest(brainDir, view); err != nil || request != nil {
		t.Fatalf("automatic request=%+v err=%v", request, err)
	}
	if _, _, err := enqueueManualAbstractJob(brainDir, view.Ref, memoryAbstractJobOverride{Provider: "codex", Model: "test", HostedEgressAllowed: true}, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("manual newer error=%v", err)
	}
	got, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(abstractRel(digest))))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("newer abstract bytes changed: got=%q err=%v", got, err)
	}
}

func TestMemoryConfigClassifiesAdditiveFieldsBySchemaVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		state   memoryConfigReadState
		code    string
	}{
		{name: "current strict", version: memoryConfigSchemaVersion, state: memoryConfigCorrupt, code: memoryErrStateCorrupt},
		{name: "newer read-only", version: memoryConfigSchemaVersion + 1, state: memoryConfigUnsupported, code: memoryErrUnsupportedVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			data := []byte(`{"schema_version":` + strconv.Itoa(tc.version) + `,"abstracts":{"enabled":false},"future_config":true}`)
			if err := writeBrainRelativeFileAtomic(brainDir, memoryConfigRel, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, state, err := loadMemoryConfigChecked(brainDir); state != tc.state || err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("state=%s err=%v", state, err)
			}
		})
	}
}

func TestAbstractEgressTargetReaderRejectsShardAndLeafSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(*testing.T, string, string, string)
	}{
		{name: "first shard", link: func(t *testing.T, _ string, target, outside string) {
			t.Helper()
			firstShard := filepath.Dir(filepath.Dir(target))
			if err := os.MkdirAll(filepath.Dir(firstShard), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(outside), firstShard); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "leaf", link: func(t *testing.T, brainDir, target, outside string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			ref := conversationSessionIDPrefix + "symlink-" + tc.name
			target := filepath.Join(brainDir, filepath.FromSlash(abstractEgressReceiptRel(ref)))
			outside := filepath.Join(t.TempDir(), "canary.json")
			want := []byte("external-canary")
			if err := os.WriteFile(outside, want, 0o600); err != nil {
				t.Fatal(err)
			}
			tc.link(t, brainDir, target, outside)
			if _, _, err := loadAbstractEgressReceiptForSessionChecked(brainDir, ref); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
				t.Fatalf("target reader error=%v", err)
			}
			got, err := os.ReadFile(outside)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("external canary changed: got=%q err=%v", got, err)
			}
		})
	}
}

func TestAbstractPrivacyCleanupNeverFollowsExternalSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(*testing.T, string, string)
	}{
		{name: "directory", link: func(t *testing.T, brainDir, outside string) {
			t.Helper()
			path := filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Dir(outside), path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "leaf", link: func(t *testing.T, brainDir, outside string) {
			t.Helper()
			dir := filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			name := strings.Repeat("a", 64) + ".json"
			if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, _ := writeSessionNavigationFixture(t)
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			outsideDir := t.TempDir()
			outside := filepath.Join(outsideDir, "canary.json")
			want := []byte("external-canary")
			if err := os.WriteFile(outside, want, 0o600); err != nil {
				t.Fatal(err)
			}
			tc.link(t, brainDir, outside)
			if err := purgeSessionAbstracts(brainDir, manifest, "nav-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
				t.Fatalf("cleanup error=%v", err)
			}
			got, err := os.ReadFile(outside)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("external canary changed: got=%q err=%v", got, err)
			}
		})
	}
}

func TestAbstractPrivacyCleanupRefusesRacedReplacementAtUnlink(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	view := sessionViewForTest(t, brainDir)
	oldFactory := memoryAbstractorFactory
	defer func() { memoryAbstractorFactory = oldFactory }()
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{}, nil }
	artifact, err := generateSessionAbstractContext(context.Background(), t.TempDir(), brainDir, view, memoryConfig{
		SchemaVersion: memoryConfigSchemaVersion,
		Abstracts:     memoryAbstractsConfig{Enabled: true, Provider: "ollama", Model: "test"},
	}, time.Date(2026, 8, 9, 19, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(brainDir, filepath.FromSlash(abstractRel(artifact.SessionDigest)))
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	replacement := target + ".replacement"
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	oldLstat := memoryStateLstat
	defer func() { memoryStateLstat = oldLstat }()
	observations := 0
	memoryStateLstat = func(path string) (os.FileInfo, error) {
		if path == target {
			observations++
			if observations == 3 {
				if err := os.Rename(replacement, target); err != nil {
					t.Fatalf("swap abstract before unlink: %v", err)
				}
			}
		}
		return oldLstat(path)
	}
	if err := purgeSessionAbstracts(brainDir, manifest, "nav-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("cleanup error=%v, want raced identity rejection", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("raced replacement was removed: %v", err)
	}
}
