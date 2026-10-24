package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestWatchShouldSpend(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	hourAgo := now.Add(-time.Hour)
	cases := []struct {
		name    string
		w       watchCommandOptions
		cursor  watchCursor
		spent   int
		wantRun bool
	}{
		{"all disabled", watchCommandOptions{distill: false, seedAgent: "none", distillEvery: time.Hour}, watchCursor{}, 0, false},
		{"distill first run", watchCommandOptions{distill: true, seedAgent: "none", distillEvery: time.Hour}, watchCursor{}, 0, true},
		// --seed-agent alone is gated identically to --distill (the invariant the fix protects).
		{"seed-agent enabled, first run", watchCommandOptions{distill: false, seedAgent: "codex", distillEvery: time.Hour}, watchCursor{}, 0, true},
		{"seed-agent enabled, interval not elapsed", watchCommandOptions{distill: false, seedAgent: "codex", distillEvery: time.Hour}, watchCursor{LastAgentSpendAt: now.Add(-30 * time.Minute)}, 0, false},
		{"interval not elapsed", watchCommandOptions{distill: true, distillEvery: time.Hour}, watchCursor{LastAgentSpendAt: now.Add(-30 * time.Minute)}, 0, false},
		{"interval elapsed", watchCommandOptions{distill: true, distillEvery: time.Hour}, watchCursor{LastAgentSpendAt: hourAgo}, 0, true},
		{"budget reached (seed)", watchCommandOptions{seedAgent: "codex", distillEvery: 0, budget: 1}, watchCursor{}, 1, false},
		{"budget not reached", watchCommandOptions{distill: true, distillEvery: 0, budget: 2}, watchCursor{}, 1, true},
	}
	for _, tc := range cases {
		if got, _ := watchShouldSpend(tc.w, tc.cursor, tc.spent, now); got != tc.wantRun {
			t.Errorf("%s: shouldSpend = %v, want %v", tc.name, got, tc.wantRun)
		}
	}
}

// fakeWatchSteps records calls so the loop's behaviour can be asserted without a real refresh/seed/distill.
func fakeWatchSteps(fp string, refreshed, seeded, distilled *int) watchSteps {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	return watchSteps{
		now:         func() time.Time { return now },
		fingerprint: func(context.Context) string { return fp },
		refresh:     func(context.Context) error { *refreshed++; return nil },
		seed:        func(context.Context) error { *seeded++; return nil },
		distill:     func(context.Context) error { *distilled++; return nil },
	}
}

func readWatchCursor(t *testing.T, path string) watchCursor {
	t.Helper()
	var c watchCursor
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("parse cursor: %v", err)
	}
	return c
}

func TestWatchLoopOnceRefreshesNoDistillByDefault(t *testing.T) {
	cursor := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled int
	w := watchCommandOptions{once: true, distill: false, seedAgent: "none"} // default: no agent work
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if refreshed != 1 {
		t.Fatalf("deterministic refresh should run once, got %d", refreshed)
	}
	if distilled != 0 || seeded != 0 {
		t.Fatalf("DEFAULT must spend ZERO agent tokens (no distill, no seed), got distill=%d seed=%d", distilled, seeded)
	}
	got := readWatchCursor(t, cursor)
	if got.LastFingerprint != "ck1:head1" || got.LastRefreshAt.IsZero() {
		t.Fatalf("cursor not persisted: %+v", got)
	}
}

func TestWatchLoopNoChangeIsNoop(t *testing.T) {
	cursor := filepath.Join(t.TempDir(), "watch.json")
	// Seed a cursor matching the fingerprint the steps will report, with a prior refresh.
	if err := saveWatchCursor(cursor, watchCursor{LastFingerprint: "ck1:head1", LastRefreshAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var refreshed, seeded, distilled int
	w := watchCommandOptions{once: true, distill: true, distillEvery: 0}
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if refreshed != 0 || distilled != 0 || seeded != 0 {
		t.Fatalf("unchanged fingerprint must be a no-op, got refresh=%d distill=%d seed=%d", refreshed, distilled, seeded)
	}
}

func TestWatchTickQueuesDeltaWorkWhenFingerprintIsUnchanged(t *testing.T) {
	now := time.Date(2026, 8, 9, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		stats shortTermStats
		err   error
	}{
		{name: "changed transcript", stats: shortTermStats{Files: 1, Records: 2}},
		{name: "failed transcript", stats: shortTermStats{Failed: 1}},
		{name: "overflow", stats: shortTermStats{Files: 1, Truncated: true}},
		{name: "delta error", err: errors.New("overlay unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursorPath := filepath.Join(t.TempDir(), "watch.json")
			if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "same", LastRefreshAt: now.Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			var reconciled, refreshed int
			steps := watchSteps{
				now:         func() time.Time { return now },
				fingerprint: func(context.Context) string { return "same" },
				delta:       func(context.Context) (shortTermStats, error) { return tc.stats, tc.err },
				reconcile:   func(context.Context) error { reconciled++; return nil },
				refresh:     func(context.Context) error { refreshed++; return nil },
			}
			calls := 0
			watchTick(context.Background(), io.Discard, defaultWatchOptions(), cursorPath, steps, &calls)
			if reconciled != 1 {
				t.Fatalf("reconcile calls = %d, want 1", reconciled)
			}
			if refreshed != 0 {
				t.Fatalf("unchanged fingerprint must not force seed/semantic refresh, got %d", refreshed)
			}
		})
	}
}

func TestWatchLoopDistillWhenEnabledAndElapsed(t *testing.T) {
	cursor := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled int
	w := watchCommandOptions{once: true, distill: true, seedAgent: "none", distillEvery: 0} // elapsed always true
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck2:head2", &refreshed, &seeded, &distilled)); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if refreshed != 1 || distilled != 1 {
		t.Fatalf("expected one refresh + one distill, got refresh=%d distill=%d", refreshed, distilled)
	}
	if seeded != 0 {
		t.Fatalf("seed should not run when --seed-agent is none, got %d", seeded)
	}
	if got := readWatchCursor(t, cursor); got.LastAgentSpendAt.IsZero() {
		t.Fatalf("agent-spend timestamp must advance so a restart does not re-spend: %+v", got)
	}
}

func TestWatchDistillOptionsDefaultAndExplicitJobs(t *testing.T) {
	// concurrency, not a separate `jobs` field: --jobs is now a spelling of
	// --concurrency, so there is one value for a caller to get wrong instead of
	// two to keep in step.
	defaultOpts := watchDistillOptions(defaultWatchOptions())
	if defaultOpts.concurrency != 1 {
		t.Fatalf("default watch distill concurrency = %d, want 1", defaultOpts.concurrency)
	}
	explicit := defaultWatchOptions()
	explicit.distillJobs = 4
	explicit.distillAgent = "ollama"
	explicit.model = "local-model"
	got := watchDistillOptions(explicit)
	if got.concurrency != 4 || got.agent != "ollama" || got.model != "local-model" {
		t.Fatalf("watch distill options not propagated: %+v", got)
	}
}

func TestRunWatchRejectsInvalidJobsBeforeWork(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWatch(context.Background(), cmd, Options{}, watchCommandOptions{
		once:        true,
		distill:     true,
		distillJobs: 0,
		seedAgent:   "none",
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--jobs must be greater than 0") {
		t.Fatalf("expected invalid jobs error, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("watch should fail before printing/running work, got %q", out.String())
	}
}

func TestWatchTickConcurrentAgentSpendReservedOnce(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	var refreshed atomic.Int32
	var distilled atomic.Int32
	steps := watchSteps{
		now:         func() time.Time { return now },
		fingerprint: func(context.Context) string { return "ck:orig:head" },
		refresh:     func(context.Context) error { refreshed.Add(1); return nil },
		seed:        func(context.Context) error { return nil },
		distill:     func(context.Context) error { distilled.Add(1); return nil },
	}
	w := watchCommandOptions{distill: true, seedAgent: "none", distillEvery: time.Hour}
	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			calls := 0
			watchTick(context.Background(), io.Discard, w, cursorPath, steps, &calls)
		}()
	}
	close(start)
	wg.Wait()

	if got := distilled.Load(); got != 1 {
		t.Fatalf("concurrent ticks reserved %d agent spends, want 1", got)
	}
	if got := readWatchCursor(t, cursorPath); got.LastAgentSpendAt.IsZero() || got.LastFingerprint != "ck:orig:head" {
		t.Fatalf("cursor did not record the single spend: %+v", got)
	}
	if refreshed.Load() == 0 {
		t.Fatalf("expected at least one deterministic refresh")
	}
}

func TestWatchSeedAgentIsGatedLikeDistill(t *testing.T) {
	cursor := filepath.Join(t.TempDir(), "watch.json")
	// --seed-agent set, --distill OFF. Seed synthesis must be GATED (interval/budget/cursor), not run on
	// every change — the invariant the review caught.
	var refreshed, seeded, distilled int
	w := watchCommandOptions{once: true, distill: false, seedAgent: "codex", distillEvery: 0}
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck:orig:head", &refreshed, &seeded, &distilled)); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if refreshed != 1 || seeded != 1 || distilled != 0 {
		t.Fatalf("expected free refresh + one gated seed + no distill, got refresh=%d seed=%d distill=%d", refreshed, seeded, distilled)
	}
	// The spend cursor advanced, so a second tick with the SAME fingerprint does NOT re-seed.
	got := readWatchCursor(t, cursor)
	if got.LastAgentSpendAt.IsZero() {
		t.Fatalf("seed spend must advance LastAgentSpendAt: %+v", got)
	}
	// Second pass, unchanged fingerprint -> no-op (proves seed isn't per-change).
	refreshed, seeded, distilled = 0, 0, 0
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck:orig:head", &refreshed, &seeded, &distilled)); err != nil {
		t.Fatalf("watchLoop 2: %v", err)
	}
	if refreshed != 0 || seeded != 0 {
		t.Fatalf("unchanged fingerprint must not re-seed, got refresh=%d seed=%d", refreshed, seeded)
	}
}

func TestWatchFingerprintUsesRefs(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", v1MainRef):   {stdout: "ckSHA\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", v1OriginRef): {stdout: "originSHA\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", "HEAD"):      {stdout: "headSHA\n"},
	}}
	if got := watchFingerprint(context.Background(), runner, "/repo"); got != "ckSHA:originSHA:headSHA" {
		t.Fatalf("fingerprint = %q, want ckSHA:originSHA:headSHA (incl. fetched origin checkpoint ref)", got)
	}
}

func TestWatchTickSkipsAgentWorkWhenRefreshFails(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	var refreshed, seeded, distilled int
	steps := watchSteps{
		now:         func() time.Time { return now },
		fingerprint: func(context.Context) string { return "ck:orig:head" },
		refresh:     func(context.Context) error { refreshed++; return errors.New("export failed") },
		seed:        func(context.Context) error { seeded++; return nil },
		distill:     func(context.Context) error { distilled++; return nil },
	}
	w := watchCommandOptions{distill: true, seedAgent: "codex", distillEvery: 0} // both WOULD fire if reached
	calls := 0
	watchTick(context.Background(), &bytes.Buffer{}, w, cursorPath, steps, &calls)
	if refreshed != 1 {
		t.Fatalf("refresh should be attempted once, got %d", refreshed)
	}
	if distilled != 0 || seeded != 0 {
		t.Fatalf("NO agent work after a failed refresh (no token spend on an unrefreshed brain), got distill=%d seed=%d", distilled, seeded)
	}
	if calls != 0 {
		t.Fatalf("budget counter must not advance on failed refresh, got %d", calls)
	}
	// Cursor must NOT advance, so the next tick retries the free refresh and never re-distills on failure.
	if c := loadWatchCursor(cursorPath); !c.LastRefreshAt.IsZero() || c.LastFingerprint != "" {
		t.Fatalf("cursor must not advance on failed refresh (else thrash): %+v", c)
	}
}

func TestWatchTickOnlyContinuesForDirtyWorktreeRefusal(t *testing.T) {
	for _, tc := range []struct {
		detail       string
		continueTick bool
	}{
		{dirtyWorktreeErrorCode + ": uncommitted edits", true},
		{`invalid manifest field "dirty_worktree"`, false},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			cursorPath := filepath.Join(t.TempDir(), "watch.json")
			var refreshed, seeded, distilled, calls int
			steps := fakeWatchSteps("ck:orig:head", &refreshed, &seeded, &distilled)
			steps.refresh = func(context.Context) error { refreshed++; return errors.New(tc.detail) }
			w := watchCommandOptions{distill: true, seedAgent: "codex", distillEvery: 0}
			watchTick(context.Background(), &bytes.Buffer{}, w, cursorPath, steps, &calls)
			want := 0
			if tc.continueTick {
				want = 1
			}
			if refreshed != 1 || seeded != want || distilled != want {
				t.Fatalf("refresh=%d seed=%d distill=%d; want 1,%d,%d", refreshed, seeded, distilled, want, want)
			}
			if !tc.continueTick && (calls != 0 || loadWatchCursor(cursorPath).LastFingerprint != "") {
				t.Fatal("an unrelated failure spent budget or advanced the cursor")
			}
		})
	}
}

func TestWatchLoopStopsOnContextCancel(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — the daemon loop must exit cleanly without ticking
	var refreshed, seeded, distilled int
	w := watchCommandOptions{interval: time.Hour} // not --once: would loop forever if cancel weren't honored
	if err := watchLoop(ctx, &bytes.Buffer{}, w, cursorPath, fakeWatchSteps("ck", &refreshed, &seeded, &distilled)); err != nil {
		t.Fatalf("graceful shutdown must return nil, got %v", err)
	}
	if refreshed != 0 {
		t.Fatalf("a cancelled context must not tick, got %d refreshes", refreshed)
	}
}

// brainLockedRefreshError is the shape runRefresh returns when a stage inside
// it timed out on the brain write lock: the sentinel wrapped under a stage
// prefix, exactly as the seed and export stages produce it.
func brainLockedRefreshError() error {
	return fmt.Errorf("seed baseline: %w", fileLockTimeoutError("brain_locked", filepath.Join("brain", "locks", "write.lock")))
}

// lockRetrySteps are fake steps whose refresh loses the brain write lock for
// the first `lostFor` attempts and whose sleep records the schedule instead of
// waiting.
func lockRetrySteps(now time.Time, fp string, lostFor int, refreshed, distilled *int, waits *[]time.Duration, order *[]string) watchSteps {
	return watchSteps{
		now:         func() time.Time { return now },
		fingerprint: func(context.Context) string { return fp },
		delta: func(context.Context) (shortTermStats, error) {
			*order = append(*order, "delta")
			return shortTermStats{Files: 1}, nil
		},
		reconcile: func(context.Context) error { *order = append(*order, "reconcile"); return nil },
		refresh: func(context.Context) error {
			*refreshed++
			*order = append(*order, "refresh")
			if *refreshed <= lostFor {
				return brainLockedRefreshError()
			}
			return nil
		},
		seed:    func(context.Context) error { return nil },
		distill: func(context.Context) error { *distilled++; *order = append(*order, "distill"); return nil },
		sleep: func(_ context.Context, d time.Duration) error {
			*waits = append(*waits, d)
			*order = append(*order, "sleep")
			return nil
		},
	}
}

func TestWatchTickRetriesRefreshAfterBrieflyHeldLock(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	var refreshed, distilled int
	var waits []time.Duration
	var order []string
	steps := lockRetrySteps(now, "ck:new", 1, &refreshed, &distilled, &waits, &order)
	w := watchCommandOptions{distill: true, seedAgent: "none", distillEvery: 24 * time.Hour}
	var out bytes.Buffer
	calls := 0
	watchTick(context.Background(), &out, w, cursorPath, steps, &calls)
	if refreshed != 2 {
		t.Fatalf("a lock lost once must be retried once, got %d refresh attempts", refreshed)
	}
	if len(waits) != 1 || waits[0] != watchRefreshLockRetryBaseWait {
		t.Fatalf("waits = %v, want one wait of %s", waits, watchRefreshLockRetryBaseWait)
	}
	if distilled != 1 || calls != 1 {
		t.Fatalf("a retried refresh must not starve the gated distill: distill=%d budget=%d", distilled, calls)
	}
	got := readWatchCursor(t, cursorPath)
	if got.LastFingerprint != "ck:new" || !got.LastRefreshAt.Equal(now) || got.PendingDistill {
		t.Fatalf("cursor must record the successful retry: %+v", got)
	}
	if !strings.Contains(out.String(), "retrying in 15s") || !strings.Contains(out.String(), "[watch] refreshed (deterministic, no agent tokens)") {
		t.Fatalf("tick output should show the retry and the refresh:\n%s", out.String())
	}
}

func TestWatchTickLaunchesReconcileAfterRefreshNotBefore(t *testing.T) {
	// The reconcile step spawns a detached memory worker that holds the brain
	// write lock for 10-25s; launching it BEFORE the heavy refresh made every
	// tick race its own child for the lock. It must run after the refresh on
	// the main path, and still run exactly once on every early exit.
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	t.Run("refresh succeeds", func(t *testing.T) {
		cursorPath := filepath.Join(t.TempDir(), "watch.json")
		var refreshed, distilled int
		var waits []time.Duration
		var order []string
		steps := lockRetrySteps(now, "ck:new", 0, &refreshed, &distilled, &waits, &order)
		calls := 0
		watchTick(context.Background(), io.Discard, watchCommandOptions{distill: true, seedAgent: "none"}, cursorPath, steps, &calls)
		want := []string{"delta", "refresh", "reconcile", "distill"}
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Fatalf("step order = %v, want %v", order, want)
		}
	})
	t.Run("refresh fails", func(t *testing.T) {
		cursorPath := filepath.Join(t.TempDir(), "watch.json")
		var refreshed, distilled int
		var waits []time.Duration
		var order []string
		steps := lockRetrySteps(now, "ck:new", 0, &refreshed, &distilled, &waits, &order)
		steps.refresh = func(context.Context) error { order = append(order, "refresh"); return errors.New("export failed") }
		calls := 0
		watchTick(context.Background(), io.Discard, watchCommandOptions{distill: true, seedAgent: "none"}, cursorPath, steps, &calls)
		want := []string{"delta", "refresh", "reconcile"}
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Fatalf("step order = %v, want %v (reconcile once, after the failed refresh)", order, want)
		}
	})
	t.Run("consolidation deferred", func(t *testing.T) {
		cursorPath := filepath.Join(t.TempDir(), "watch.json")
		if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "ck:old", LastRefreshAt: now.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
		var refreshed, distilled int
		var waits []time.Duration
		var order []string
		steps := lockRetrySteps(now, "ck:new", 0, &refreshed, &distilled, &waits, &order)
		calls := 0
		watchTick(context.Background(), io.Discard, watchCommandOptions{consolidateEvery: time.Hour}, cursorPath, steps, &calls)
		want := []string{"delta", "reconcile"}
		if strings.Join(order, ",") != strings.Join(want, ",") {
			t.Fatalf("step order = %v, want %v (reconcile still runs when the refresh is deferred)", order, want)
		}
	})
}

func TestWatchTickGivesUpAfterBoundedLockRetries(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	var refreshed, distilled int
	var waits []time.Duration
	var order []string
	// Lost on every attempt; no earlier refresh, so nothing is pending.
	steps := lockRetrySteps(now, "ck:new", 1<<30, &refreshed, &distilled, &waits, &order)
	w := watchCommandOptions{distill: true, seedAgent: "none", distillEvery: 24 * time.Hour}
	var out bytes.Buffer
	calls := 0
	watchTick(context.Background(), &out, w, cursorPath, steps, &calls)
	if refreshed != watchRefreshLockRetries+1 {
		t.Fatalf("refresh attempts = %d, want %d", refreshed, watchRefreshLockRetries+1)
	}
	wantWaits := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second}
	if len(waits) != len(wantWaits) {
		t.Fatalf("waits = %v, want %v", waits, wantWaits)
	}
	for i := range wantWaits {
		if waits[i] != wantWaits[i] {
			t.Fatalf("waits = %v, want %v", waits, wantWaits)
		}
	}
	if distilled != 0 || calls != 0 {
		t.Fatalf("no agent work against a brain that was never refreshed: distill=%d budget=%d", distilled, calls)
	}
	if c := loadWatchCursor(cursorPath); !c.LastRefreshAt.IsZero() || c.LastFingerprint != "" {
		t.Fatalf("cursor must not advance when every attempt lost the lock: %+v", c)
	}
	if !strings.Contains(out.String(), "[watch] refresh failed (skipping agent work this tick): seed baseline: brain_locked:") {
		t.Fatalf("the final failure must still be reported:\n%s", out.String())
	}
}

func TestWatchTickDoesNotRetryNonLockFailures(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	var refreshed, distilled int
	var waits []time.Duration
	var order []string
	steps := lockRetrySteps(now, "ck:new", 0, &refreshed, &distilled, &waits, &order)
	steps.refresh = func(context.Context) error { refreshed++; return errors.New("export failed") }
	calls := 0
	watchTick(context.Background(), io.Discard, watchCommandOptions{distill: true, seedAgent: "none"}, cursorPath, steps, &calls)
	if refreshed != 1 || len(waits) != 0 {
		t.Fatalf("an ordinary failure must not be retried: attempts=%d waits=%v", refreshed, waits)
	}
}

func TestWatchTickLockRetryStopsOnContextCancel(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	var refreshed, distilled int
	var waits []time.Duration
	var order []string
	steps := lockRetrySteps(now, "ck:new", 1<<30, &refreshed, &distilled, &waits, &order)
	ctx, cancel := context.WithCancel(context.Background())
	steps.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		cancel() // the daemon is asked to stop mid-wait
		return ctx.Err()
	}
	calls := 0
	watchTick(ctx, io.Discard, watchCommandOptions{distill: true, seedAgent: "none"}, cursorPath, steps, &calls)
	if refreshed != 1 || len(waits) != 1 {
		t.Fatalf("a cancelled wait must end the retries: attempts=%d waits=%v", refreshed, waits)
	}
	if distilled != 0 {
		t.Fatalf("no agent work after a cancelled retry, got %d", distilled)
	}
}

func TestWatchTickRunsPendingDistillWhenRefreshKeepsLosingLock(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		lastRefresh time.Time
		pending     bool
		wantDistill int
	}{
		{name: "pending from a recent refresh", lastRefresh: now.Add(-time.Hour), pending: true, wantDistill: 1},
		{name: "pending but last refresh older than one spend window", lastRefresh: now.Add(-48 * time.Hour), pending: true, wantDistill: 0},
		{name: "nothing pending", lastRefresh: now.Add(-time.Hour), pending: false, wantDistill: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursorPath := filepath.Join(t.TempDir(), "watch.json")
			if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "ck:old", LastRefreshAt: tc.lastRefresh, PendingDistill: tc.pending}); err != nil {
				t.Fatal(err)
			}
			var refreshed, distilled int
			var waits []time.Duration
			var order []string
			steps := lockRetrySteps(now, "ck:new", 1<<30, &refreshed, &distilled, &waits, &order)
			w := watchCommandOptions{distill: true, seedAgent: "none", distillEvery: 24 * time.Hour}
			var out bytes.Buffer
			calls := 0
			watchTick(context.Background(), &out, w, cursorPath, steps, &calls)
			if refreshed != watchRefreshLockRetries+1 {
				t.Fatalf("refresh attempts = %d, want %d", refreshed, watchRefreshLockRetries+1)
			}
			if distilled != tc.wantDistill {
				t.Fatalf("distill runs = %d, want %d\n%s", distilled, tc.wantDistill, out.String())
			}
			got := readWatchCursor(t, cursorPath)
			// A lost lock never claims a refresh: the next tick retries it.
			if got.LastFingerprint != "ck:old" || !got.LastRefreshAt.Equal(tc.lastRefresh) {
				t.Fatalf("refresh cursor must not advance on a lost lock: %+v", got)
			}
			if tc.wantDistill == 1 {
				if got.PendingDistill || got.LastAgentSpendAt.IsZero() || calls != 1 {
					t.Fatalf("the pending distill must be consumed and charged: cursor=%+v budget=%d", got, calls)
				}
				if !strings.Contains(out.String(), "running pending agent work against the last successful refresh (1h0m0s ago)") {
					t.Fatalf("tick should say why it spent without refreshing:\n%s", out.String())
				}
			} else if got.PendingDistill != tc.pending || !got.LastAgentSpendAt.IsZero() || calls != 0 {
				t.Fatalf("nothing may be spent or cleared: cursor=%+v budget=%d", got, calls)
			}
		})
	}
}

func TestWatchRefreshLockWaitIsCappedByInterval(t *testing.T) {
	if got := watchRefreshLockWait(watchCommandOptions{}, 2); got != 60*time.Second {
		t.Fatalf("uncapped third wait = %s, want 60s", got)
	}
	if got := watchRefreshLockWait(watchCommandOptions{interval: 20 * time.Second}, 2); got != 20*time.Second {
		t.Fatalf("a short --interval must cap the wait, got %s", got)
	}
}

func TestIsBrainLockContention(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("export failed"), false},
		{brainLockedRefreshError(), true},
		// A layer that formatted with %v keeps the code string.
		{errors.New("refresh: brain_locked: timed out acquiring lock at x: file lock timeout"), true},
		// Any timed-out file lock counts, not only write.lock.
		{fileLockTimeoutError("index_locked", "x"), true},
	}
	for _, tc := range cases {
		if got := isBrainLockContention(tc.err); got != tc.want {
			t.Errorf("isBrainLockContention(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
