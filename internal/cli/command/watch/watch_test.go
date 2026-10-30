package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"github.com/spf13/cobra"
)

func TestWatchShouldSpend(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	hourAgo := now.Add(-time.Hour)
	cases := []struct {
		name    string
		w       Flags
		cursor  Cursor
		spent   int
		wantRun bool
	}{
		{"all disabled", Flags{Distill: false, SeedAgent: "none", DistillEvery: time.Hour}, Cursor{}, 0, false},
		{"distill first run", Flags{Distill: true, SeedAgent: "none", DistillEvery: time.Hour}, Cursor{}, 0, true},
		// --seed-agent alone is gated identically to --distill (the invariant the fix protects).
		{"seed-agent enabled, first run", Flags{Distill: false, SeedAgent: "codex", DistillEvery: time.Hour}, Cursor{}, 0, true},
		{"seed-agent enabled, interval not elapsed", Flags{Distill: false, SeedAgent: "codex", DistillEvery: time.Hour}, Cursor{LastAgentSpendAt: now.Add(-30 * time.Minute)}, 0, false},
		{"interval not elapsed", Flags{Distill: true, DistillEvery: time.Hour}, Cursor{LastAgentSpendAt: now.Add(-30 * time.Minute)}, 0, false},
		{"interval elapsed", Flags{Distill: true, DistillEvery: time.Hour}, Cursor{LastAgentSpendAt: hourAgo}, 0, true},
		{"budget reached (seed)", Flags{SeedAgent: "codex", DistillEvery: 0, Budget: 1}, Cursor{}, 1, false},
		{"budget not reached", Flags{Distill: true, DistillEvery: 0, Budget: 2}, Cursor{}, 1, true},
	}
	for _, tc := range cases {
		if got, _ := watchShouldSpend(tc.w, tc.cursor, tc.spent, now); got != tc.wantRun {
			t.Errorf("%s: shouldSpend = %v, want %v", tc.name, got, tc.wantRun)
		}
	}
}

// fakeWatchSteps records calls so the loop's behaviour can be asserted without a real refresh/seed/distill.
func fakeWatchSteps(fp string, refreshed, seeded, distilled *int) Steps {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	return Steps{
		Now:         func() time.Time { return now },
		Fingerprint: func(context.Context) string { return fp },
		Refresh:     func(context.Context) error { *refreshed++; return nil },
		Seed:        func(context.Context) error { *seeded++; return nil },
		Distill:     func(context.Context) error { *distilled++; return nil },
	}
}

func readWatchCursor(t *testing.T, path string) Cursor {
	t.Helper()
	var c Cursor
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
	w := Flags{Once: true, Distill: false, SeedAgent: "none"} // default: no agent work
	if err := watchLoop(context.Background(), &bytes.Buffer{}, LogFields{Repo: "gh/acme/app"}, time.Now, w, cursor, func(io.Writer) Steps { return fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled) }); err != nil {
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
	if err := SaveCursor(cursor, Cursor{LastFingerprint: "ck1:head1", LastRefreshAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var refreshed, seeded, distilled int
	w := Flags{Once: true, Distill: true, DistillEvery: 0}
	if err := watchLoop(context.Background(), &bytes.Buffer{}, LogFields{Repo: "gh/acme/app"}, time.Now, w, cursor, func(io.Writer) Steps { return fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled) }); err != nil {
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
		stats historystore.ShortTermStats
		err   error
	}{
		{name: "changed transcript", stats: historystore.ShortTermStats{Files: 1, Records: 2}},
		{name: "failed transcript", stats: historystore.ShortTermStats{Failed: 1}},
		{name: "overflow", stats: historystore.ShortTermStats{Files: 1, Truncated: true}},
		{name: "delta error", err: errors.New("overlay unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursorPath := filepath.Join(t.TempDir(), "watch.json")
			if err := SaveCursor(cursorPath, Cursor{LastFingerprint: "same", LastRefreshAt: now.Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			var reconciled, refreshed int
			steps := Steps{
				Now:         func() time.Time { return now },
				Fingerprint: func(context.Context) string { return "same" },
				Delta:       func(context.Context) (historystore.ShortTermStats, error) { return tc.stats, tc.err },
				Reconcile:   func(context.Context) error { reconciled++; return nil },
				Refresh:     func(context.Context) error { refreshed++; return nil },
			}
			calls := 0
			Tick(context.Background(), io.Discard, DefaultFlags(), cursorPath, steps, &calls)
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
	w := Flags{Once: true, Distill: true, SeedAgent: "none", DistillEvery: 0} // elapsed always true
	if err := watchLoop(context.Background(), &bytes.Buffer{}, LogFields{Repo: "gh/acme/app"}, time.Now, w, cursor, func(io.Writer) Steps { return fakeWatchSteps("ck2:head2", &refreshed, &seeded, &distilled) }); err != nil {
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

func TestRunWatchRejectsInvalidJobsBeforeWork(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := runWatch(context.Background(), cmd, Config{}, Flags{
		Once:        true,
		Distill:     true,
		DistillJobs: 0,
		SeedAgent:   "none",
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
	steps := Steps{
		Now:         func() time.Time { return now },
		Fingerprint: func(context.Context) string { return "ck:orig:head" },
		Refresh:     func(context.Context) error { refreshed.Add(1); return nil },
		Seed:        func(context.Context) error { return nil },
		Distill:     func(context.Context) error { distilled.Add(1); return nil },
	}
	w := Flags{Distill: true, SeedAgent: "none", DistillEvery: time.Hour}
	const workers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			calls := 0
			Tick(context.Background(), io.Discard, w, cursorPath, steps, &calls)
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
	w := Flags{Once: true, Distill: false, SeedAgent: "codex", DistillEvery: 0}
	if err := watchLoop(context.Background(), &bytes.Buffer{}, LogFields{Repo: "gh/acme/app"}, time.Now, w, cursor, func(io.Writer) Steps { return fakeWatchSteps("ck:orig:head", &refreshed, &seeded, &distilled) }); err != nil {
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
	if err := watchLoop(context.Background(), &bytes.Buffer{}, LogFields{Repo: "gh/acme/app"}, time.Now, w, cursor, func(io.Writer) Steps { return fakeWatchSteps("ck:orig:head", &refreshed, &seeded, &distilled) }); err != nil {
		t.Fatalf("watchLoop 2: %v", err)
	}
	if refreshed != 0 || seeded != 0 {
		t.Fatalf("unchanged fingerprint must not re-seed, got refresh=%d seed=%d", refreshed, seeded)
	}
}

func TestWatchTickSkipsAgentWorkWhenRefreshFails(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	var refreshed, seeded, distilled int
	steps := Steps{
		Now:         func() time.Time { return now },
		Fingerprint: func(context.Context) string { return "ck:orig:head" },
		Refresh:     func(context.Context) error { refreshed++; return errors.New("export failed") },
		Seed:        func(context.Context) error { seeded++; return nil },
		Distill:     func(context.Context) error { distilled++; return nil },
	}
	w := Flags{Distill: true, SeedAgent: "codex", DistillEvery: 0} // both WOULD fire if reached
	calls := 0
	Tick(context.Background(), &bytes.Buffer{}, w, cursorPath, steps, &calls)
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
	if c := LoadCursor(cursorPath); !c.LastRefreshAt.IsZero() || c.LastFingerprint != "" {
		t.Fatalf("cursor must not advance on failed refresh (else thrash): %+v", c)
	}
}

func TestWatchTickOnlyContinuesForDirtyWorktreeRefusal(t *testing.T) {
	for _, tc := range []struct {
		detail       string
		continueTick bool
	}{
		{brainstore.DirtyWorktreeCode + ": uncommitted edits", true},
		{`invalid manifest field "dirty_worktree"`, false},
	} {
		t.Run(tc.detail, func(t *testing.T) {
			cursorPath := filepath.Join(t.TempDir(), "watch.json")
			var refreshed, seeded, distilled, calls int
			steps := fakeWatchSteps("ck:orig:head", &refreshed, &seeded, &distilled)
			steps.Refresh = func(context.Context) error { refreshed++; return errors.New(tc.detail) }
			w := Flags{Distill: true, SeedAgent: "codex", DistillEvery: 0}
			Tick(context.Background(), &bytes.Buffer{}, w, cursorPath, steps, &calls)
			want := 0
			if tc.continueTick {
				want = 1
			}
			if refreshed != 1 || seeded != want || distilled != want {
				t.Fatalf("refresh=%d seed=%d distill=%d; want 1,%d,%d", refreshed, seeded, distilled, want, want)
			}
			if !tc.continueTick && (calls != 0 || LoadCursor(cursorPath).LastFingerprint != "") {
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
	w := Flags{Interval: time.Hour} // not --once: would loop forever if cancel weren't honored
	if err := watchLoop(ctx, &bytes.Buffer{}, LogFields{Repo: "gh/acme/app"}, time.Now, w, cursorPath, func(io.Writer) Steps { return fakeWatchSteps("ck", &refreshed, &seeded, &distilled) }); err != nil {
		t.Fatalf("graceful shutdown must return nil, got %v", err)
	}
	if refreshed != 0 {
		t.Fatalf("a cancelled context must not tick, got %d refreshes", refreshed)
	}
}
