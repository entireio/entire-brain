package cli

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchDeferredWorkRetriesWithoutCommit(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "interval_deferred", true: "busy_distill"}[busy], func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "watch.json")
			now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
			w := defaultWatchOptions()
			w.distill = true
			w.consolidateEvery = 0
			if !busy {
				if e := saveWatchCursor(p, watchCursor{LastFingerprint: "old", LastRefreshAt: now.Add(-time.Hour), LastAgentSpendAt: now.Add(-time.Hour)}); e != nil {
					t.Fatal(e)
				}
			}
			calls, distills := 0, 0
			s := watchSteps{now: func() time.Time { return now }, fingerprint: func(context.Context) string { return "new" }, refresh: func(context.Context) error { return nil }, distill: func(context.Context) error {
				distills++
				if busy {
					return errDistillPassBusy
				}
				return nil
			}}
			watchTick(context.Background(), io.Discard, w, p, s, &calls)
			before := distills
			now = now.Add(48 * time.Hour)
			watchTick(context.Background(), io.Discard, w, p, s, &calls)
			if distills != before+1 {
				t.Fatalf("due work not retried: before=%d after=%d", before, distills)
			}
		})
	}
}

func TestWatchCappedWorkSurvivesRestartWithoutRepeatingSeed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := defaultWatchOptions()
	w.distill = true
	w.seedAgent = "codex"
	w.consolidateEvery = 0
	calls, seeds, distills, refreshes := 0, 0, 0, 0
	s := watchSteps{now: func() time.Time { return now }, fingerprint: func(context.Context) string { return "stable" }, refresh: func(context.Context) error { refreshes++; return nil }, seed: func(context.Context) error { seeds++; return nil }, distill: func(context.Context) error {
		distills++
		if distills == 1 {
			return errDistillDeferred
		}
		return nil
	}}
	tick := func() { watchTick(context.Background(), io.Discard, w, p, s, &calls) }
	tick()
	if c := loadWatchCursor(p); !c.PendingDistill || c.PendingSeed {
		t.Fatalf("pending state=%+v", c)
	}
	calls = 0 // process restart must not bypass the persisted spend interval.
	now = now.Add(time.Hour)
	tick()
	if distills != 1 {
		t.Fatal("restart bypassed interval")
	}
	now = now.Add(24 * time.Hour)
	tick()
	if distills != 2 || seeds != 1 || refreshes != 1 {
		t.Fatalf("distills=%d seeds=%d refreshes=%d", distills, seeds, refreshes)
	}
	now = now.Add(48 * time.Hour)
	tick()
	if distills != 2 || loadWatchCursor(p).PendingDistill {
		t.Fatal("completed work was retried")
	}
}
