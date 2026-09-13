package cli

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newPrepassBackoffBrain gives the worker a minimal, complete brain to run
// against so only the prepass outcome varies.
func newPrepassBackoffBrain(t *testing.T, now time.Time) (Options, repoStorage) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, exportManifest{
		SchemaVersion: brainManifestSchemaVersion, RepoKey: storage.Key,
	}); err != nil {
		t.Fatal(err)
	}
	return opts, storage
}

// A failed prepass relaunches itself. It used to do so at a fixed one minute,
// forever: one real machine ran 1285 consecutive failures across 21.6 unbroken
// hours because the delta export was permanently broken for that repository.
// The chain now backs off and, once retrying has clearly stopped being worth
// it, stops relaunching at all.
func TestFailedPrepassBacksOffAndEventuallyStopsRelaunching(t *testing.T) {
	oldPrepass, oldLaunchAfter := memoryWorkerPrepass, memoryWorkerLaunchAfter
	defer func() { memoryWorkerPrepass, memoryWorkerLaunchAfter = oldPrepass, oldLaunchAfter }()
	memoryWorkerPrepass = func(context.Context, Options, string) error {
		return errors.New("permanent export failure")
	}

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	type launch struct {
		delay    time.Duration
		failures int
	}
	var launches []launch
	memoryWorkerLaunchAfter = func(_ string, delay time.Duration, failures int) error {
		launches = append(launches, launch{delay, failures})
		return nil
	}

	// Walk the chain the way the relaunch actually does: each pass is launched
	// with the failure count the previous one handed it.
	carried := 0
	var delays []time.Duration
	for pass := 0; pass < 12; pass++ {
		opts, _ := newPrepassBackoffBrain(t, now)
		launches = nil
		args := []string{"--once"}
		if carried > 0 {
			args = append(args, "--prepass-failures", strconv.Itoa(carried))
		}
		out, err := execute(t, newMemoryWorkerCommand(opts), args...)
		if err != nil {
			t.Fatalf("pass %d: %v\n%s", pass, err, out)
		}
		if !strings.Contains(out, `"prepass_failed": true`) {
			t.Fatalf("pass %d did not report the prepass failure: %s", pass, out)
		}
		if len(launches) == 0 {
			if !strings.Contains(out, `"prepass_gave_up": true`) {
				t.Fatalf("pass %d stopped relaunching without saying so: %s", pass, out)
			}
			break
		}
		delays = append(delays, launches[0].delay)
		carried = launches[0].failures
	}

	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute,
	}
	if len(delays) != len(want) {
		t.Fatalf("relaunch chain = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("relaunch chain = %v, want %v", delays, want)
		}
	}
	total := time.Duration(0)
	for _, d := range delays {
		total += d
	}
	if total > 3*time.Hour {
		t.Fatalf("a permanently failing prepass still retries for %v", total)
	}
}

// The schedule itself, stated directly.
func TestMemoryWorkerPrepassRetrySchedule(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
		ok       bool
	}{
		{failures: 0, ok: false},
		{failures: 1, want: time.Minute, ok: true},
		{failures: 2, want: 2 * time.Minute, ok: true},
		{failures: 5, want: 16 * time.Minute, ok: true},
		{failures: 6, want: memoryWorkerPrepassRetryCeiling, ok: true},
		{failures: 7, want: memoryWorkerPrepassRetryCeiling, ok: true},
		{failures: memoryWorkerPrepassRetryAttempts, ok: false},
		{failures: memoryWorkerPrepassRetryAttempts + 50, ok: false},
	} {
		got, ok := memoryWorkerPrepassRetry(tc.failures)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("memoryWorkerPrepassRetry(%d) = (%v, %v), want (%v, %v)", tc.failures, got, ok, tc.want, tc.ok)
		}
	}
}

// A pass that could not export new sessions did not do what it was launched to
// do. Recording "complete" is what let those 1285 failures accumulate with
// every health surface showing a healthy last outcome.
func TestFailedPrepassRecordsADegradedOutcomeAndSurfacesIt(t *testing.T) {
	oldPrepass, oldLaunchAfter := memoryWorkerPrepass, memoryWorkerLaunchAfter
	defer func() { memoryWorkerPrepass, memoryWorkerLaunchAfter = oldPrepass, oldLaunchAfter }()
	memoryWorkerPrepass = func(context.Context, Options, string) error {
		return errors.New("export failure")
	}
	memoryWorkerLaunchAfter = func(string, time.Duration, int) error { return nil }

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	opts, storage := newPrepassBackoffBrain(t, now)
	if out, err := execute(t, newMemoryWorkerCommand(opts), "--once"); err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}

	state, err := loadMemoryCoordinatorState(storage.BrainDir)
	if err != nil {
		t.Fatalf("coordinator state: %v", err)
	}
	if state.LastOutcome != memoryWorkerOutcomeDegraded {
		t.Fatalf("last_outcome = %q, want %q", state.LastOutcome, memoryWorkerOutcomeDegraded)
	}

	health := memoryReadOnlyHealth(storage.BrainDir, now.Add(time.Second))
	coordinator, ok := health.Payload["coordinator"].(map[string]any)
	if !ok {
		t.Fatalf("no coordinator health section: %+v", health.Payload)
	}
	if code, _ := coordinator["error_code"].(string); code != memoryErrWorkerDegraded {
		t.Fatalf("coordinator health hides the degraded pass: %+v", coordinator)
	}
	found := false
	for _, issue := range health.Issues {
		if issue.Code == memoryErrWorkerDegraded {
			found = true
		}
	}
	if !found {
		t.Fatalf("a degraded worker pass raised no status issue: %+v", health.Issues)
	}
}

// A successful prepass ends the chain: the next pass starts from zero.
func TestSuccessfulPrepassResetsTheRelaunchChain(t *testing.T) {
	oldPrepass, oldLaunchAfter := memoryWorkerPrepass, memoryWorkerLaunchAfter
	defer func() { memoryWorkerPrepass, memoryWorkerLaunchAfter = oldPrepass, oldLaunchAfter }()
	memoryWorkerPrepass = func(context.Context, Options, string) error { return nil }
	launched := false
	memoryWorkerLaunchAfter = func(_ string, _ time.Duration, failures int) error {
		launched = true
		if failures != 0 {
			t.Errorf("a successful prepass carried a failure count of %d", failures)
		}
		return nil
	}
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	opts, storage := newPrepassBackoffBrain(t, now)
	out, err := execute(t, newMemoryWorkerCommand(opts), "--once", "--prepass-failures", "5")
	if err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}
	if strings.Contains(out, `"prepass_failed"`) || strings.Contains(out, `"prepass_gave_up"`) {
		t.Fatalf("successful prepass reported a failure: %s", out)
	}
	_ = launched
	state, err := loadMemoryCoordinatorState(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastOutcome != "complete" {
		t.Fatalf("last_outcome = %q, want complete", state.LastOutcome)
	}
}

// A scheduled worker used to take the per-Brain coordinator and then sleep the
// WHOLE delay holding it. nextMemoryWorkerDelay returns the time until the
// earliest pending job, a backed-off retry puts that nearly two hours out, and
// for that entire window every other launch -- watch tick, session hook, MCP
// nudge -- got `already_active` and did nothing. Only the tail of a long wait
// may hold the lease now.
func TestLongDelayedWorkerDoesNotHoldTheCoordinatorWhileWaiting(t *testing.T) {
	oldCap := memoryWorkerMaxLeaseSleep
	defer func() { memoryWorkerMaxLeaseSleep = oldCap }()
	memoryWorkerMaxLeaseSleep = 50 * time.Millisecond

	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	opts, storage := newPrepassBackoffBrain(t, now)

	// Long next to the cap, short next to the test: without the fix the lease is
	// held for all of it; with the fix, for its last 50ms.
	const delay = 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd := newMemoryWorkerCommand(opts)
		cmd.SetContext(ctx)
		cmd.SetArgs([]string{"--once", "--delay", delay.String()})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		_ = cmd.Execute()
	}()
	// Long enough that a worker which acquires immediately (the old behavior)
	// certainly holds the lease by now, and far short of the delay.
	time.Sleep(500 * time.Millisecond)

	// The contender must win well before the delay elapses. Under the old
	// design it could not win until the sleeper woke.
	budget := time.Now().Add(delay - time.Second)
	var acquired *memoryCoordinator
	for time.Now().Before(budget) {
		coordinator, err := acquireMemoryCoordinator(storage.BrainDir, now, "contender")
		if err == nil {
			acquired = coordinator
			break
		}
		if !strings.Contains(err.Error(), "memory_worker_active") {
			t.Fatalf("acquire: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if acquired == nil {
		t.Fatalf("a worker waiting out a %v delay still held the coordinator lease", delay)
	}
	acquired.close(now.Add(time.Second), "complete")
	cancel()
	<-done
}
