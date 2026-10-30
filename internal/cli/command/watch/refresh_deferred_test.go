package watch

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
)

func TestWatchDirtyRefreshRetriesWithoutCommit(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "first_refresh", true: "existing_context"}[existing], func(t *testing.T) {
			now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "watch.json")
			var lastSuccess time.Time
			if existing {
				lastSuccess = now.Add(-time.Hour)
				if err := SaveCursor(path, Cursor{LastFingerprint: "old", LastRefreshAt: lastSuccess}); err != nil {
					t.Fatal(err)
				}
			}
			calls, agentCalls := 0, 0
			var out bytes.Buffer
			steps := Steps{
				Now:         func() time.Time { return now },
				Fingerprint: func(context.Context) string { return "same-head" },
				Refresh: func(context.Context) error {
					calls++
					if calls == 1 {
						return errors.New(brainstore.DirtyWorktreeCode + ": uncommitted edits")
					}
					return nil
				},
			}
			flags := DefaultFlags()
			Tick(context.Background(), &out, flags, path, steps, &agentCalls)
			if strings.Contains(out.String(), "[watch] refreshed (deterministic") {
				t.Errorf("blocked refresh reported success: %s", out.String())
			}
			if c := LoadCursor(path); !c.LastRefreshAt.Equal(lastSuccess) {
				t.Errorf("blocked refresh changed last success from %v to %v", lastSuccess, c.LastRefreshAt)
			}
			// Stashing changes and restarting the watcher change neither HEAD nor
			// checkpoint refs. The persisted deferred refresh must still run.
			now = now.Add(time.Hour)
			agentCalls = 0
			out.Reset()
			Tick(context.Background(), &out, flags, path, steps, &agentCalls)
			if calls != 2 || !LoadCursor(path).LastRefreshAt.Equal(now) {
				t.Fatalf("clean tree was not refreshed: calls=%d cursor=%+v output=%s", calls, LoadCursor(path), out.String())
			}
			now = now.Add(time.Hour)
			Tick(context.Background(), &out, flags, path, steps, &agentCalls)
			if calls != 2 || agentCalls != 0 {
				t.Fatalf("completed refresh repeated or spent tokens: refreshes=%d agent calls=%d", calls, agentCalls)
			}
		})
	}
}

func TestWatchDirtyRefreshRetriesAreBoundedWithoutRepeatingAgentWork(t *testing.T) {
	for _, consolidation := range []time.Duration{0, 30 * time.Minute} {
		t.Run(consolidation.String(), func(t *testing.T) {
			now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "watch.json")
			flags := DefaultFlags()
			flags.ConsolidateEvery = consolidation
			flags.Distill, flags.SeedAgent = true, "codex"
			flags.DistillEvery = 0 // No spend throttle to hide accidental requeueing.
			refreshes, seeds, distills, deltas, syncs, agentCalls := 0, 0, 0, 0, 0, 0
			dirty := true
			steps := Steps{
				Now:         func() time.Time { return now },
				Fingerprint: func(context.Context) string { return "same-head" },
				Delta: func(context.Context) (historystore.ShortTermStats, error) {
					deltas++
					return historystore.ShortTermStats{}, nil
				},
				FactsSync: func(context.Context) (bool, error) { syncs++; return true, nil },
				Refresh: func(context.Context) error {
					refreshes++
					if dirty {
						return errors.New(brainstore.DirtyWorktreeCode + ": uncommitted edits")
					}
					return nil
				},
				Seed:    func(context.Context) error { seeds++; return nil },
				Distill: func(context.Context) error { distills++; return nil },
			}
			var out bytes.Buffer
			tick := func() { Tick(context.Background(), &out, flags, path, steps, &agentCalls) }
			tick()
			agentCalls = 0 // Restart cannot bypass the persisted retry cadence.
			now = now.Add(time.Minute)
			tick()
			if refreshes != 1 {
				t.Fatalf("deferred refresh retried before its cadence: %d", refreshes)
			}
			now = now.Add(time.Hour)
			tick() // Still dirty; retry once without repeating completed agent work.
			if refreshes != 2 || seeds != 1 || distills != 1 || agentCalls != 0 {
				t.Fatalf("retries=%d seeds=%d distills=%d agent calls=%d", refreshes, seeds, distills, agentCalls)
			}
			dirty = false
			now = now.Add(time.Hour)
			tick()
			if refreshes != 3 || seeds != 1 || distills != 1 || agentCalls != 0 {
				t.Fatalf("recovery repeated agent work: refreshes=%d seeds=%d distills=%d agent calls=%d", refreshes, seeds, distills, agentCalls)
			}
			if deltas != 4 || syncs != 3 {
				t.Fatalf("deferred refresh blocked background work: delta=%d sync=%d", deltas, syncs)
			}
		})
	}
}

func TestWatchDeferredRefreshPreservesUrgentRepair(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration
		stats historystore.ShortTermStats
		err   error
	}{
		{name: "buffer_full", after: time.Minute, stats: historystore.ShortTermStats{Truncated: true}},
		{name: "delta_error", after: 5 * time.Minute, err: errors.New("delta failed")},
		{name: "partial_delta", after: 5 * time.Minute, stats: historystore.ShortTermStats{Failed: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "watch.json")
			refreshes, agentCalls := 0, 0
			urgent := false
			steps := Steps{
				Now:         func() time.Time { return now },
				Fingerprint: func(context.Context) string { return "same-head" },
				Delta: func(context.Context) (historystore.ShortTermStats, error) {
					if urgent {
						return tc.stats, tc.err
					}
					return historystore.ShortTermStats{}, nil
				},
				Refresh: func(context.Context) error {
					refreshes++
					if refreshes == 1 {
						return errors.New(brainstore.DirtyWorktreeCode + ": uncommitted edits")
					}
					return nil
				},
			}
			var out bytes.Buffer
			Tick(context.Background(), &out, DefaultFlags(), path, steps, &agentCalls)
			urgent = true
			now = now.Add(tc.after)
			Tick(context.Background(), &out, DefaultFlags(), path, steps, &agentCalls)
			if c := LoadCursor(path); refreshes != 2 || c.PendingRefresh || !c.LastRefreshAt.Equal(now) {
				t.Fatalf("urgent repair waited for normal consolidation: refreshes=%d cursor=%+v output=%s", refreshes, c, out.String())
			}
			if agentCalls != 0 {
				t.Fatal("repair spent agent budget")
			}
		})
	}
}

func TestWatchDeferredRefreshRepairBacksOffWhileStillDirty(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "watch.json")
	flags := DefaultFlags()
	refreshes, agentCalls := 0, 0
	deltaFails := false
	steps := Steps{
		Now:         func() time.Time { return now },
		Fingerprint: func(context.Context) string { return "same-head" },
		Delta: func(context.Context) (historystore.ShortTermStats, error) {
			if deltaFails {
				return historystore.ShortTermStats{}, errors.New("delta failed")
			}
			return historystore.ShortTermStats{}, nil
		},
		Refresh: func(context.Context) error {
			refreshes++
			return errors.New(brainstore.DirtyWorktreeCode + ": uncommitted edits")
		},
	}
	var out bytes.Buffer
	tick := func() { Tick(context.Background(), &out, flags, path, steps, &agentCalls) }
	tick()
	deltaFails = true
	for i := 1; i <= 20; i++ {
		now = now.Add(flags.Interval)
		tick()
		if i == 1 && refreshes != 2 {
			t.Errorf("first failed delta must repair on the next tick: refreshes=%d", refreshes)
		}
		if i == 2 && refreshes != 2 {
			t.Errorf("repeated dirty repair must back off: refreshes=%d", refreshes)
		}
	}
	if refreshes < 3 || refreshes > 7 {
		t.Fatalf("persistent failure must retry without refreshing every tick: refreshes=%d output=%s", refreshes, out.String())
	}
	if c := LoadCursor(path); !c.PendingRefresh || !c.LastRefreshAt.IsZero() || c.ConsolidationRepairs == 0 {
		t.Fatalf("deferred repair lost pending state, claimed success, or lost backoff: %+v", c)
	}
	deltaFails = false
	now = now.Add(flags.ConsolidateEvery)
	tick()
	if c := LoadCursor(path); c.ConsolidationRepairs != 0 || !c.PendingRefresh {
		t.Fatalf("healthy delta must clear repair backoff while retaining dirty refresh: %+v", c)
	}
}

func TestWatchDeferredRefreshDoesNotBlockDueAgentWork(t *testing.T) {
	for _, tc := range []struct {
		name         string
		priorSpend   bool
		seed         bool
		firstDistill error
	}{
		{name: "interval_deferred", priorSpend: true, seed: true},
		{name: "busy_distill", firstDistill: ErrDistillPassBusy},
		{name: "capped_distill", seed: true, firstDistill: ErrDistillDeferred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			now := start
			path := filepath.Join(t.TempDir(), "watch.json")
			flags := DefaultFlags()
			flags.Distill = true
			flags.DistillEvery = 5 * time.Minute
			if tc.seed {
				flags.SeedAgent = "codex"
			}
			if tc.priorSpend {
				if err := SaveCursor(path, Cursor{LastAgentSpendAt: start}); err != nil {
					t.Fatal(err)
				}
			}
			refreshes, seeds, distills, agentCalls := 0, 0, 0, 0
			steps := Steps{
				Now:         func() time.Time { return now },
				Fingerprint: func(context.Context) string { return "same-head" },
				Refresh: func(context.Context) error {
					refreshes++
					return errors.New(brainstore.DirtyWorktreeCode + ": uncommitted edits")
				},
				Seed: func(context.Context) error { seeds++; return nil },
				Distill: func(context.Context) error {
					distills++
					if distills == 1 {
						return tc.firstDistill
					}
					return nil
				},
			}
			var out bytes.Buffer
			tick := func() { Tick(context.Background(), &out, flags, path, steps, &agentCalls) }
			tick()
			before := distills
			agentCalls = 0 // Restart: eligibility is persisted in the cursor.
			now = now.Add(flags.DistillEvery)
			tick()
			if distills != before+1 || agentCalls != 1 {
				t.Fatalf("due agent work blocked by refresh cadence: distills=%d before=%d agent calls=%d output=%s", distills, before, agentCalls, out.String())
			}
			if c := LoadCursor(path); !c.PendingRefresh || !c.LastRefreshAt.IsZero() || !c.LastRefreshAttemptAt.Equal(start) || c.PendingSeed || c.PendingDistill {
				t.Fatalf("agent-only tick changed refresh state or lost completion: %+v", c)
			}
			now = now.Add(flags.DistillEvery)
			tick()
			if refreshes != 1 || distills != before+1 || agentCalls != 1 || tc.seed && seeds != 1 {
				t.Fatalf("agent-only ticks repeated work: refreshes=%d seeds=%d distills=%d agent calls=%d", refreshes, seeds, distills, agentCalls)
			}
		})
	}
}
