package watch

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
			w := DefaultFlags()
			w.Distill = true
			w.ConsolidateEvery = 0
			if !busy {
				if e := SaveCursor(p, Cursor{LastFingerprint: "old", LastRefreshAt: now.Add(-time.Hour), LastAgentSpendAt: now.Add(-time.Hour)}); e != nil {
					t.Fatal(e)
				}
			}
			calls, distills := 0, 0
			s := Steps{Now: func() time.Time { return now }, Fingerprint: func(context.Context) string { return "new" }, Refresh: func(context.Context) error { return nil }, Distill: func(context.Context) error {
				distills++
				if busy {
					return ErrDistillPassBusy
				}
				return nil
			}}
			Tick(context.Background(), io.Discard, w, p, s, &calls)
			before := distills
			now = now.Add(48 * time.Hour)
			Tick(context.Background(), io.Discard, w, p, s, &calls)
			if distills != before+1 {
				t.Fatalf("due work not retried: before=%d after=%d", before, distills)
			}
		})
	}
}

func TestWatchCappedWorkSurvivesRestartWithoutRepeatingSeed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	w := DefaultFlags()
	w.Distill = true
	w.SeedAgent = "codex"
	w.ConsolidateEvery = 0
	calls, seeds, distills, refreshes := 0, 0, 0, 0
	s := Steps{Now: func() time.Time { return now }, Fingerprint: func(context.Context) string { return "stable" }, Refresh: func(context.Context) error { refreshes++; return nil }, Seed: func(context.Context) error { seeds++; return nil }, Distill: func(context.Context) error {
		distills++
		if distills == 1 {
			return ErrDistillDeferred
		}
		return nil
	}}
	tick := func() { Tick(context.Background(), io.Discard, w, p, s, &calls) }
	tick()
	if c := LoadCursor(p); !c.PendingDistill || c.PendingSeed {
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
	if distills != 2 || LoadCursor(p).PendingDistill {
		t.Fatal("completed work was retried")
	}
}
