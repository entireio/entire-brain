package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
