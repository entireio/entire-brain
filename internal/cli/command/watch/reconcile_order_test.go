package watch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
)

func TestWatchTickDoesNotRaceItsOwnWorker(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "write.lock")
	var releaseWorker func() error
	t.Cleanup(func() {
		if releaseWorker != nil {
			_ = releaseWorker()
		}
	})
	var completed []string
	writer := func(name string) func(context.Context) error {
		return func(context.Context) error {
			lock, err := brainstore.AcquireFileLock(lockPath, "brain_locked", 20*time.Millisecond)
			if err != nil {
				return err
			}
			defer func() { _ = lock.Close() }()
			completed = append(completed, name)
			return nil
		}
	}
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	steps := Steps{
		Now:         func() time.Time { return now },
		Fingerprint: func(context.Context) string { return "changed" },
		Delta: func(context.Context) (historystore.ShortTermStats, error) {
			return historystore.ShortTermStats{Files: 1}, nil
		},
		Reconcile: func(context.Context) error {
			// Model a child that obtains the real OS lock before its launcher
			// returns. The tick must finish all writers before starting it.
			lock, err := brainstore.AcquireFileLock(lockPath, "brain_locked", time.Second)
			if err != nil {
				return err
			}
			releaseWorker = lock.Close
			completed = append(completed, "worker")
			return nil
		},
		Refresh: writer("refresh"),
		Seed:    writer("seed"),
		Distill: writer("distill"),
	}
	flags := DefaultFlags()
	flags.SeedAgent, flags.Distill = "codex", true
	var output bytes.Buffer
	calls := 0
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	Tick(context.Background(), &output, flags, cursorPath, steps, &calls)
	if want := []string{"refresh", "seed", "distill", "worker"}; !reflect.DeepEqual(completed, want) {
		t.Fatalf("completed writers = %v, want %v; output: %s", completed, want, output.String())
	}
	cursor := LoadCursor(cursorPath)
	if calls != 1 || cursor.LastRefreshAt.IsZero() || cursor.PendingSeed || cursor.PendingDistill {
		t.Fatalf("worker race lost the refresh or agent work: calls=%d cursor=%+v", calls, cursor)
	}
}

func TestWatchTickReconcilesAfterForegroundWorkOnEveryExit(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		unchanged      bool
		throttled      bool
		paused         bool
		noDeltaChanges bool
		noAgents       bool
		budgetUsed     bool
		cursorError    bool
		cancelRefresh  bool
		refreshErr     error
		deltaErr       error
		seedErr        error
		distillErr     error
		reconcileErr   error
		want           []string
		wantSpend      int
	}{
		{name: "success", want: []string{"refresh", "seed", "distill", "reconcile"}, wantSpend: 1},
		{name: "refresh without delta changes", noDeltaChanges: true, want: []string{"refresh", "seed", "distill", "reconcile"}, wantSpend: 1},
		{name: "agent work disabled", noAgents: true, want: []string{"refresh", "reconcile"}},
		{name: "unchanged fingerprint", unchanged: true, want: []string{"reconcile"}},
		{name: "healthy unchanged tick", unchanged: true, noDeltaChanges: true},
		{name: "delta failure unchanged fingerprint", unchanged: true, deltaErr: errors.New("delta failed"), want: []string{"reconcile"}},
		{name: "consolidation throttled", throttled: true, want: []string{"reconcile"}},
		{name: "paused", paused: true},
		{name: "budget used", budgetUsed: true, want: []string{"refresh", "reconcile"}, wantSpend: 1},
		{name: "refresh failed", refreshErr: errors.New("refresh failed"), want: []string{"refresh", "reconcile"}},
		{name: "dirty refresh", refreshErr: errors.New(brainstore.DirtyWorktreeCode + ": edits"), want: []string{"refresh", "seed", "distill", "reconcile"}, wantSpend: 1},
		{name: "cursor error", cursorError: true, want: []string{"refresh", "reconcile"}},
		{name: "cancel during refresh", cancelRefresh: true, want: []string{"refresh", "reconcile"}},
		{name: "seed failed", seedErr: errors.New("seed failed"), want: []string{"refresh", "seed", "distill", "reconcile"}, wantSpend: 1},
		{name: "distill failed", distillErr: errors.New("distill failed"), want: []string{"refresh", "seed", "distill", "reconcile"}, wantSpend: 1},
		{name: "reconcile failed", reconcileErr: errors.New("queue unavailable"), want: []string{"refresh", "seed", "distill", "reconcile"}, wantSpend: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "watch.json")
			if tc.unchanged || tc.throttled {
				fp := "changed"
				if tc.throttled {
					fp = "previous"
				}
				if err := SaveCursor(path, Cursor{LastFingerprint: fp, LastRefreshAt: now.Add(-time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.cursorError {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, "not-a-directory.json")
			}
			type contextKey struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), contextKey{}, "retained"))
			defer cancel()
			var events []string
			steps := Steps{
				Now:         func() time.Time { return now },
				Paused:      func() bool { return tc.paused },
				Fingerprint: func(context.Context) string { return "changed" },
				Delta: func(context.Context) (historystore.ShortTermStats, error) {
					if tc.noDeltaChanges {
						return historystore.ShortTermStats{}, tc.deltaErr
					}
					return historystore.ShortTermStats{Files: 1}, tc.deltaErr
				},
				Refresh: func(context.Context) error {
					events = append(events, "refresh")
					if tc.cancelRefresh {
						cancel()
						return ctx.Err()
					}
					return tc.refreshErr
				},
				Seed:    func(context.Context) error { events = append(events, "seed"); return tc.seedErr },
				Distill: func(context.Context) error { events = append(events, "distill"); return tc.distillErr },
				Reconcile: func(c context.Context) error {
					events = append(events, "reconcile")
					if c.Err() != nil || c.Value(contextKey{}) != "retained" {
						t.Fatalf("durable reconciliation lost its context values or inherited cancellation: %v", c.Err())
					}
					return tc.reconcileErr
				},
			}
			flags := DefaultFlags()
			flags.SeedAgent, flags.Distill = "codex", true
			if tc.noAgents {
				flags.SeedAgent, flags.Distill = "none", false
			}
			calls := 0
			if tc.budgetUsed {
				flags.Budget, calls = 1, 1
			}
			var output bytes.Buffer
			Tick(ctx, &output, flags, path, steps, &calls)
			if !reflect.DeepEqual(events, tc.want) || calls != tc.wantSpend {
				t.Fatalf("events=%v want=%v calls=%d want=%d; output=%s", events, tc.want, calls, tc.wantSpend, output.String())
			}
			if tc.reconcileErr != nil && !strings.Contains(output.String(), "durable overlay retained") {
				t.Fatalf("reconciliation failure was hidden: %s", output.String())
			}
		})
	}
}

func TestWatchTickFailedRefreshDoesNotAuthorizePendingAgentWork(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "watch.json")
	prior := Cursor{LastFingerprint: "old", LastRefreshAt: now.Add(-time.Hour), PendingSeed: true, PendingDistill: true}
	if err := SaveCursor(path, prior); err != nil {
		t.Fatal(err)
	}
	steps := Steps{
		Now: func() time.Time { return now }, Fingerprint: func(context.Context) string { return "new" },
		Delta: func(context.Context) (historystore.ShortTermStats, error) {
			return historystore.ShortTermStats{Files: 1}, nil
		},
		Refresh:   func(context.Context) error { return errors.New("brain_locked: unrelated writer") },
		Reconcile: func(context.Context) error { return nil },
		Seed:      func(context.Context) error { t.Fatal("failed refresh authorized seed spend"); return nil },
		Distill:   func(context.Context) error { t.Fatal("failed refresh authorized distill spend"); return nil },
	}
	flags := DefaultFlags()
	flags.SeedAgent, flags.Distill = "codex", true
	calls := 0
	Tick(context.Background(), io.Discard, flags, path, steps, &calls)
	if got := LoadCursor(path); !reflect.DeepEqual(got, prior) || calls != 0 {
		t.Fatalf("failed refresh advanced state: cursor=%+v calls=%d", got, calls)
	}
}

func TestWatchReconcileContextBoundsShutdownWithoutTimingOutNormalWork(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	work, finish := watchReconcileContext(parent, 0)
	defer finish()
	if work.Err() != nil {
		t.Fatal("normal reconciliation inherited the shutdown-only limit")
	}
	cancelParent()
	select {
	case <-work.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelled reconciliation exceeded its grace period")
	}
}

func TestWatchReconcileContextAllowsFinalPublicationAndCleansUp(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	work, finish := watchReconcileContext(parent, time.Hour)
	if work.Err() != nil {
		t.Fatal("cancelled parent prevented final queue publication")
	}
	finish()
	select {
	case <-work.Done():
	case <-time.After(time.Second):
		t.Fatal("finished reconciliation retained its context")
	}
}

func TestWatchTickWaitsWithoutForegroundIntent(t *testing.T) {
	dir := t.TempDir()
	active, err := brainstore.AcquireFileLock(brainstore.BrainWriteLockPath(dir), "brain_locked", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	started := make(chan struct{})
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	steps := Steps{
		Now:         time.Now,
		Fingerprint: func(context.Context) string { return "changed" },
		Delta: func(ctx context.Context) (historystore.ShortTermStats, error) {
			close(started)
			err := brainstore.WithBrainWriteLockContext(ctx, dir, func() error { return nil })
			return historystore.ShortTermStats{}, err
		},
		Refresh: func(context.Context) error { return nil },
	}
	go func() {
		defer close(done)
		calls := 0
		Tick(ctx, io.Discard, DefaultFlags(), filepath.Join(t.TempDir(), "watch.json"), steps, &calls)
	}()
	<-started
	// If watch lost its background classification it would register an intent.
	select {
	case <-done:
		t.Fatal("watch did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	intents, err := filepath.Glob(filepath.Join(dir, brainstore.BrainLockDirName, "write-foreground-*.lock"))
	if err != nil || len(intents) != 0 {
		t.Fatalf("watch claimed foreground priority: %v %v", intents, err)
	}
	_ = active.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not resume")
	}
}
