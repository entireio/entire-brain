package cli

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// distill_lock_test.go covers the token-spend race `setup` creates by design: it
// spawns the detached backfill AND installs a watcher whose first tick distills
// immediately, so two processes reach the agent over the same sessions within
// seconds of each other. The brain write lock does not help — it only wraps the
// flush at the END of a pass, long after the tokens are gone.

// TestDistillPassLockLetsExactlyOneSpend is the guard for the double-distill.
// Two passes over ONE brain: exactly one runs, the other skips cleanly (no
// error, no queueing behind an hours-long backfill only to re-spend afterwards).
func TestDistillPassLockLetsExactlyOneSpend(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()

	var spent atomic.Int32
	var skipped atomic.Int32
	// first holds the lock until second has definitely tried for it.
	inside := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := withDistillPassLock(brainDir, func() { skipped.Add(1) }, func() error {
			spent.Add(1)
			close(inside)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("first pass: %v", err)
		}
	}()

	<-inside
	err := withDistillPassLock(brainDir, func() { skipped.Add(1) }, func() error {
		spent.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("the second pass must skip cleanly, not fail: %v", err)
	}
	close(release)
	wg.Wait()

	if spent.Load() != 1 {
		t.Fatalf("exactly one pass may spend tokens, got %d", spent.Load())
	}
	if skipped.Load() != 1 {
		t.Fatalf("the second pass must report its skip, got %d", skipped.Load())
	}
}

// TestDistillPassLockIsReleasedForTheNextPass: the guard must not latch. Once
// the first pass finishes, the next one runs normally.
func TestDistillPassLockIsReleasedForTheNextPass(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	runs := 0
	for i := 0; i < 3; i++ {
		if err := withDistillPassLock(brainDir, func() { t.Fatal("sequential passes must never skip") }, func() error {
			runs++
			return nil
		}); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if runs != 3 {
		t.Fatalf("sequential passes must all run, got %d", runs)
	}
}

// TestDistillPassLockSurvivesAFailedPass: an error inside the pass must still
// release the lock, or one failure would wedge distillation forever.
func TestDistillPassLockSurvivesAFailedPass(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	want := fmt.Errorf("agent exploded")
	if err := withDistillPassLock(brainDir, nil, func() error { return want }); err != want {
		t.Fatalf("the pass error must propagate, got %v", err)
	}
	ran := false
	if err := withDistillPassLock(brainDir, func() { t.Fatal("the lock must be released after a failure") }, func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !ran {
		t.Fatal("a failed pass must not wedge the brain")
	}
}
