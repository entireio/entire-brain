package cli

import (
	"context"
	"testing"
	"time"
)

// Restored from the former memory_abstract_privacy_linearization_test.go: the
// cancellation-poll contract is independent of the removed privacy layer.
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
