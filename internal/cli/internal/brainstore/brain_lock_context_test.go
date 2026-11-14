package brainstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBrainWriteLockContextPreservesWaitBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unlock, err := AcquireBrainWriteLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// The outer context is only a test backstop. The requested lock budget
	// must expire first, with the existing file-lock timeout sentinel.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = WithBrainWriteLockTimeoutContext(ctx, dir, 20*time.Millisecond, func() error { t.Error("callback ran while the lock was held"); return nil })
	if !errors.Is(err, ErrFileLockTimeout) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait budget changed: %v", err)
	}
}

func TestBrainWriteLockContextCancellationAndRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WithBrainWriteLockTimeoutContext(ctx, dir, time.Second, func() error { t.Error("cancelled callback ran"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	expected := errors.New("callback failed")
	if err := WithBrainWriteLockTimeoutContext(context.Background(), dir, time.Second, func() error { return expected }); !errors.Is(err, expected) {
		t.Fatalf("callback error lost: %v", err)
	}
	if err := WithBrainWriteLockTimeoutContext(context.Background(), dir, time.Second, func() error { return nil }); err != nil {
		t.Fatalf("callback leaked its lock: %v", err)
	}
}
