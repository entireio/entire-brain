package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type privacyCountingAbstractor struct {
	calls atomic.Int32
}

func (a *privacyCountingAbstractor) Identity() (string, string, string) {
	return "local", "ollama", "privacy-test"
}

func (a *privacyCountingAbstractor) Abstract(_ context.Context, _ string, input conversationAbstractInput) (sessionAbstract, error) {
	a.calls.Add(1)
	return sessionAbstract{Overview: abstractStatement{
		Text:        "private abstract",
		EvidenceIDs: []string{input.Windows[0][0].ConversationID},
	}}, nil
}

func TestAbstractProviderNotCalledUnderStaleDirtyOrUnknownPrivacyPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
		hook  func(*testing.T, string) func()
		code  string
	}{
		{
			name: "stale after capture",
			hook: func(t *testing.T, brainDir string) func() {
				return func() {
					if err := withBrainWriteLock(brainDir, func() error {
						stones := emptySessionTombstones()
						stones.Excluded["nav-sess"] = sessionTombstone{At: time.Now().UTC(), Reason: "race"}
						return saveSessionTombstones(brainDir, stones)
					}); err != nil {
						t.Fatalf("land concurrent tombstone: %v", err)
					}
				}
			},
			code: memoryErrPrivacyDirty,
		},
		{
			name: "dirty derived state",
			setup: func(t *testing.T, brainDir string) {
				if err := withBrainWriteLock(brainDir, func() error {
					stones := emptySessionTombstones()
					stones.Excluded["nav-sess"] = sessionTombstone{At: time.Now().UTC(), Reason: "dirty"}
					return saveSessionTombstones(brainDir, stones)
				}); err != nil {
					t.Fatal(err)
				}
			},
			code: memoryErrPrivacyDirty,
		},
		{
			name: "unknown newer policy",
			setup: func(t *testing.T, brainDir string) {
				data := []byte(fmt.Sprintf(`{"version":%d,"excluded":{}}`, sessionTombstonesVersion+1))
				if err := writeBrainRelativeFileAtomic(brainDir, sessionTombstonesPath, append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: memoryErrUnsupportedVersion,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, _ := writeSessionNavigationFixture(t)
			view := sessionViewForTest(t, brainDir)
			if tc.setup != nil {
				tc.setup(t, brainDir)
			}
			provider := &privacyCountingAbstractor{}
			originalFactory := memoryAbstractorFactory
			originalHook := beforeAbstractProviderPrivacyLock
			memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
			beforeAbstractProviderPrivacyLock = func() {}
			if tc.hook != nil {
				beforeAbstractProviderPrivacyLock = tc.hook(t, brainDir)
			}
			t.Cleanup(func() {
				memoryAbstractorFactory = originalFactory
				beforeAbstractProviderPrivacyLock = originalHook
			})
			config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}
			_, err := generateSessionAbstractContext(context.Background(), t.TempDir(), brainDir, view, config, time.Now().UTC())
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("generation error = %v, want %s", err, tc.code)
			}
			if calls := provider.calls.Load(); calls != 0 {
				t.Fatalf("provider calls = %d, want zero", calls)
			}
		})
	}
}

func TestManualAbstractOutputRejectsLateExclusionWithoutBytes(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	policy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	policy.RequireDerivedClean = true

	originalHook := beforeRetrievalResponsePrivacyEmissionCheck
	beforeRetrievalResponsePrivacyEmissionCheck = func() {
		if err := withBrainWriteLock(brainDir, func() error {
			stones := emptySessionTombstones()
			stones.Excluded["nav-sess"] = sessionTombstone{At: time.Now().UTC(), Reason: "late exclusion"}
			return saveSessionTombstones(brainDir, stones)
		}); err != nil {
			t.Fatalf("land late exclusion: %v", err)
		}
	}
	t.Cleanup(func() { beforeRetrievalResponsePrivacyEmissionCheck = originalHook })

	var external bytes.Buffer
	cmd := &cobra.Command{Use: "abstract-test"}
	cmd.SetOut(&external)
	err = bufferRetrievalCommandOutput(cmd, []retrievalPrivacyPolicy{policy}, func() error {
		_, err := fmt.Fprint(cmd.OutOrStdout(), "private abstract\n")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
		t.Fatalf("late-exclusion output error = %v, want %s", err, memoryErrPrivacyDirty)
	}
	if external.Len() != 0 {
		t.Fatalf("late exclusion leaked %d bytes: %q", external.Len(), external.String())
	}
}

func TestManualAbstractMCPOutputHoldsPrivacyLockThroughOuterFrame(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	policy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	policy.RequireDerivedClean = true
	privacyState := &mcpResponsePrivacyState{}
	out := &mcpToolOutputBuffer{privacy: privacyState}
	cmd := &cobra.Command{Use: "abstract-test"}
	cmd.SetOut(out)
	if err := bufferRetrievalCommandOutput(cmd, []retrievalPrivacyPolicy{policy}, func() error {
		_, err := fmt.Fprint(cmd.OutOrStdout(), "private abstract\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if privacyState.unlock == nil {
		t.Fatal("MCP output did not retain the privacy lock for the outer frame")
	}

	purgeStarted := make(chan struct{})
	purgeDone := make(chan error, 1)
	go func() {
		close(purgeStarted)
		purgeDone <- withBrainPrivacySideEffectLock(brainDir, func() error {
			return withBrainWriteLock(brainDir, func() error {
				stones := emptySessionTombstones()
				stones.Excluded["nav-sess"] = sessionTombstone{At: time.Now().UTC(), Reason: "MCP race"}
				return saveSessionTombstones(brainDir, stones)
			})
		})
	}()
	<-purgeStarted
	select {
	case err := <-purgeDone:
		t.Fatalf("purge crossed the MCP outer-frame boundary: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	privacyState.release()
	if err := <-purgeDone; err != nil {
		t.Fatal(err)
	}
	if out.String() != "private abstract\n" {
		t.Fatalf("buffered MCP output = %q", out.String())
	}
}

func TestPrivacySideEffectLockHasStableBoundedContentionFailure(t *testing.T) {
	brainDir := t.TempDir()
	unlock, err := acquireBrainPrivacySideEffectLock(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = acquireBrainPrivacySideEffectLockTimeout(brainDir, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyBusy) || memoryErrorCode(err) != memoryErrPrivacyBusy {
		t.Fatalf("contended privacy lock error = %v", err)
	}
}

func TestAbstractProviderSideEffectLockLeavesDeterministicBrainWorkAvailable(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	provider := &countingBlockingAbstractor{started: make(chan struct{}), release: make(chan struct{})}
	originalFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
	t.Cleanup(func() { memoryAbstractorFactory = originalFactory })
	config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}
	generationDone := make(chan error, 1)
	repoDir := t.TempDir()
	go func() {
		_, err := generateSessionAbstractContext(context.Background(), repoDir, brainDir, view, config, time.Now().UTC())
		generationDone <- err
	}()
	<-provider.started

	projectionDone := make(chan error, 1)
	go func() {
		projectionDone <- withBrainWriteLock(brainDir, func() error {
			_, err := writeBrainHistoryIndexAndSourceLocked(brainDir, time.Now().UTC(), nil)
			return err
		})
	}()
	select {
	case err := <-projectionDone:
		if err != nil {
			t.Fatalf("deterministic projection during provider: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider side-effect lock blocked deterministic Brain work")
	}

	close(provider.release)
	if err := <-generationDone; err != nil {
		t.Fatal(err)
	}
}

func TestAbstractLaneCancellationPollCancelsProviderAndSettlesJob(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 8, 9, 22, 0, 0, 0, time.UTC)
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
	originalPoll := memoryAbstractCancellationPollInterval
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
	memoryAbstractCancellationPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		memoryAbstractorFactory = originalFactory
		memoryAbstractCancellationPollInterval = originalPoll
	})
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "privacy-cancel-test")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.close(now.Add(time.Minute), "complete")

	type laneResult struct {
		stats memoryWorkerStats
		err   error
	}
	laneDone := make(chan laneResult, 1)
	repoDir := t.TempDir()
	go func() {
		stats, err := runMemoryAbstractLane(context.Background(), repoDir, brainDir, now, coordinator.token)
		laneDone <- laneResult{stats: stats, err: err}
	}()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	var running memoryJob
	for _, job := range loadMemoryJobs(brainDir) {
		if job.Kind == memoryJobKindSessionAbstract && job.State == memoryJobStateRunning {
			running = job
			break
		}
	}
	if running.JobID == "" {
		t.Fatal("running abstract job not found")
	}
	if err := requestRunningMemoryJobCancellationVerified(brainDir, running, now.Add(time.Second)); err != nil {
		t.Fatalf("request cancellation during provider: %v", err)
	}
	select {
	case result := <-laneDone:
		if result.err != nil {
			t.Fatalf("cancelled lane error = %v", result.err)
		}
		if result.stats.JobsCancelled != 1 || result.stats.JobsRetried != 0 || result.stats.JobsCompleted != 0 {
			t.Fatalf("cancelled lane stats = %+v", result.stats)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation poll did not stop the provider promptly")
	}
	select {
	case <-provider.done:
	default:
		t.Fatal("provider context was not cancelled and drained")
	}
}

func TestRetentionRecomputesPlanAfterWaitingForProviderSideEffect(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-24 * time.Hour)
	providerUnlock, err := acquireBrainPrivacySideEffectLock(brainDir)
	if err != nil {
		t.Fatal(err)
	}

	type retentionResult struct {
		plan []retentionPlanEntry
		err  error
	}
	retentionStarted := make(chan struct{})
	retentionDone := make(chan retentionResult, 1)
	go func() {
		close(retentionStarted)
		plan, _, err := applyPrivacyRetention(brainDir, EntireEnv{}, "test/privacy", now, cutoff, "", false, 24*time.Hour)
		retentionDone <- retentionResult{plan: plan, err: err}
	}()
	<-retentionStarted

	// Model a deterministic projection/session refresh completing while the
	// provider still owns the side-effect boundary and retention is waiting.
	lateRel := "sessions/main/20260803T000000Z_late.jsonl"
	lateFull := filepath.Join(brainDir, filepath.FromSlash(lateRel))
	if err := os.WriteFile(lateFull, []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"late eligible session"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return err
		}
		manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{
			SessionID: "late-eligible", Branch: "main", Agent: "codex", LatestCheckpoint: "cp-late",
			TranscriptPath: lateRel, CreatedAt: cutoff.Add(-time.Hour),
		})
		if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
			return err
		}
		_, err = writeBrainHistoryIndexAndSourceLocked(brainDir, now, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-retentionDone:
		t.Fatalf("retention crossed provider side-effect boundary: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	providerUnlock()

	result := <-retentionDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	found := false
	for _, entry := range result.plan {
		found = found || entry.SessionID == "late-eligible"
	}
	if !found {
		t.Fatalf("applied retention plan omitted newly eligible session: %+v", result.plan)
	}
	stones := loadSessionTombstones(brainDir)
	if _, excluded := stones.Excluded["late-eligible"]; !excluded {
		t.Fatal("newly eligible session was not excluded")
	}
}

func TestPrivacyCleanupRetryUsesDurableRefsAfterManifestDropsSession(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var secretView conversationSessionView
	for _, view := range buildConversationSessionViews(fresh, manifest.RepoKey, manifest, guard) {
		if len(view.Records) > 0 && view.Records[0].SessionID == "secret-sess" {
			secretView = view
			break
		}
	}
	if secretView.Ref == "" {
		t.Fatal("secret session view not found")
	}
	originalFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{hosted: true}, nil
	}
	t.Cleanup(func() { memoryAbstractorFactory = originalFactory })
	now := time.Now().UTC().Add(-time.Minute)
	config := memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
		Enabled: true, Provider: "fake", HostedEgressAllowed: true,
	}}
	artifact, err := generateSessionAbstractContext(context.Background(), t.TempDir(), brainDir, secretView, config, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, secretView.Ref); err != nil || !present {
		t.Fatalf("hosted receipt precondition: present=%t err=%v", present, err)
	}

	injected := fmt.Errorf("injected failure before C4 cleanup")
	originalHook := beforeSessionAbstractPrivacyCleanup
	beforeSessionAbstractPrivacyCleanup = func() error { return injected }
	t.Cleanup(func() { beforeSessionAbstractPrivacyCleanup = originalHook })
	var firstPlan sessionPurgePlan
	err = withBrainPrivacySideEffectLock(brainDir, func() error {
		return withBrainWriteLock(brainDir, func() error {
			var err error
			firstPlan, err = buildSessionPurgePlan(brainDir, "secret-sess")
			if err != nil {
				return err
			}
			return executeSessionCleanup(brainDir, "secret-sess", firstPlan, now.Add(time.Minute), "test", true)
		})
	})
	if err == nil || !strings.Contains(err.Error(), injected.Error()) {
		t.Fatalf("first cleanup error = %v", err)
	}
	tx, present, err := loadPrivacyTransactionChecked(brainDir, "secret-sess")
	if err != nil || !present || len(tx.SessionRefs) == 0 || tx.SessionRefs[0] != secretView.Ref {
		t.Fatalf("durable cleanup scope = %+v present=%t err=%v", tx.SessionRefs, present, err)
	}
	if _, present := loadSessionAbstract(brainDir, artifact.SessionDigest); !present {
		t.Fatal("injected failure did not leave abstract for retry")
	}

	// A later refresh drops the session from the manifest before the user
	// retries. Only the durable transaction can now identify its C4 artifacts.
	if err := withBrainWriteLock(brainDir, func() error {
		current, err := loadBrainManifest(brainDir)
		if err != nil {
			return err
		}
		kept := current.Sources.Sessions.Sessions[:0]
		for _, session := range current.Sources.Sessions.Sessions {
			if session.SessionID != "secret-sess" {
				kept = append(kept, session)
			}
		}
		current.Sources.Sessions.Sessions = kept
		if err := writeBrainManifestAndReadme(brainDir, *current); err != nil {
			return err
		}
		_, err = writeBrainHistoryIndexAndSourceLocked(brainDir, now.Add(2*time.Minute), nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dirtyReport, err := verifySessionPrivacy(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	dirtyKinds := map[string]bool{}
	for _, finding := range dirtyReport.Findings {
		dirtyKinds[finding.Artifact] = true
	}
	if dirtyReport.Clean || !dirtyKinds["session_abstract"] || !dirtyKinds["abstract_egress_receipt"] {
		t.Fatalf("verification false-cleaned durable C4 survivors: clean=%t findings=%+v", dirtyReport.Clean, dirtyReport.Findings)
	}
	beforeSessionAbstractPrivacyCleanup = func() error { return nil }
	err = withBrainPrivacySideEffectLock(brainDir, func() error {
		return withBrainWriteLock(brainDir, func() error {
			retryPlan, err := buildSessionPurgePlan(brainDir, "secret-sess")
			if err != nil {
				return err
			}
			return executeSessionCleanup(brainDir, "secret-sess", retryPlan, now.Add(3*time.Minute), "test", true)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := loadSessionAbstract(brainDir, artifact.SessionDigest); present {
		t.Fatal("retry left abstract after manifest session disappeared")
	}
	if _, present, err := loadAbstractEgressReceiptForSessionChecked(brainDir, secretView.Ref); err != nil || present {
		t.Fatalf("retry egress receipt: present=%t err=%v", present, err)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil || !report.Clean {
		t.Fatalf("post-retry privacy verify: clean=%t findings=%+v err=%v", report.Clean, report.Findings, err)
	}
}
