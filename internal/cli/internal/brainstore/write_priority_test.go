package brainstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func waitPriorityCondition(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("priority condition timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func priorityIntentCount(dir string) int {
	matches, _ := filepath.Glob(filepath.Join(dir, BrainLockDirName, "write-foreground-*.lock"))
	return len(matches)
}

func TestWriterPriorityForegroundBeforeWaitingBackground(t *testing.T) {
	dir := t.TempDir()
	// The existing write lock also models an older binary already committing.
	active, err := AcquireFileLock(BrainWriteLockPath(dir), "brain_locked", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	order := make(chan string, 3)
	release := make(chan struct{})
	go func() {
		unlock, err := AcquireBrainWriteLockContext(BackgroundWrites(ctx), dir)
		if err != nil {
			order <- err.Error()
			return
		}
		defer unlock()
		order <- "background"
	}()
	for range 2 {
		go func() {
			unlock, err := AcquireBrainWriteLockContext(ctx, dir)
			if err != nil {
				order <- err.Error()
				return
			}
			defer unlock()
			order <- "foreground"
			<-release
		}()
	}
	waitPriorityCondition(t, func() bool { return priorityIntentCount(dir) == 2 })
	select {
	case got := <-order:
		t.Fatalf("interrupted active write: %s", got)
	default:
	}
	_ = active.Close()
	for range 2 {
		select {
		case got := <-order:
			if got != "foreground" {
				t.Fatalf("order: %s", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("foreground stalled")
		}
		release <- struct{}{}
	}
	select {
	case got := <-order:
		if got != "background" {
			t.Fatalf("background: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("background did not resume")
	}
	waitPriorityCondition(t, func() bool { return priorityIntentCount(dir) == 0 })
}

func TestWriterPriorityCancellationRemovesIntent(t *testing.T) {
	dir := t.TempDir()
	active, err := AcquireFileLock(BrainWriteLockPath(dir), "brain_locked", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		unlock, err := AcquireBrainWriteLockContext(ctx, dir)
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	waitPriorityCondition(t, func() bool { return priorityIntentCount(dir) == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if priorityIntentCount(dir) != 0 {
		t.Fatal("cancel left intent")
	}
	bg, cancelBG := context.WithCancel(BackgroundWrites(context.Background()))
	cancelBG()
	if _, err := AcquireBrainWriteLockContext(bg, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("background cancel: %v", err)
	}
}

func TestWriterPriorityBackgroundWaitsPastOldTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("real legacy timeout boundary")
	}
	dir := t.TempDir()
	active, err := AcquireFileLock(BrainWriteLockPath(dir), "brain_locked", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		unlock, err := AcquireBrainWriteLockContext(BackgroundWrites(ctx), dir)
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("background stopped waiting: %v", err)
	case <-time.After(BrainWriteLockTimeout + 100*time.Millisecond):
	}
	_ = active.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWriterPriorityProcessHelper(t *testing.T) {
	dir := os.Getenv("BRAIN_PRIORITY_TEST_DIR")
	if dir == "" {
		return
	}
	ctx := context.Background()
	if os.Getenv("BRAIN_PRIORITY_TEST_ROLE") == "background" {
		ctx = BackgroundWrites(ctx)
	}
	unlock, err := AcquireBrainWriteLockContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := os.WriteFile(os.Getenv("BRAIN_PRIORITY_TEST_READY"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(os.Getenv("BRAIN_PRIORITY_TEST_RELEASE")); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startPriorityProcess(t *testing.T, dir, role string) (*exec.Cmd, string, string) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	release := ready + ".release"
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriterPriorityProcessHelper$")
	cmd.Env = append(os.Environ(), "BRAIN_PRIORITY_TEST_DIR="+dir, "BRAIN_PRIORITY_TEST_ROLE="+role, "BRAIN_PRIORITY_TEST_READY="+ready, "BRAIN_PRIORITY_TEST_RELEASE="+release)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, ready, release
}

func TestWriterPriorityAcrossProcessesAndKilledWaiter(t *testing.T) {
	dir := t.TempDir()
	active, err := AcquireFileLock(BrainWriteLockPath(dir), "brain_locked", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	_, bgReady, bgRelease := startPriorityProcess(t, dir, "background")
	dead, _, _ := startPriorityProcess(t, dir, "foreground")
	waitPriorityCondition(t, func() bool { return priorityIntentCount(dir) == 1 })
	if err := dead.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = dead.Wait()
	waitPriorityCondition(t, func() bool { return priorityIntentCount(dir) == 0 })
	_, fgReady, fgRelease := startPriorityProcess(t, dir, "foreground")
	waitPriorityCondition(t, func() bool { return priorityIntentCount(dir) == 1 })
	_ = active.Close()
	waitPriorityCondition(t, func() bool { _, err := os.Stat(fgReady); return err == nil })
	if _, err := os.Stat(bgReady); err == nil {
		t.Fatal("background overtook foreground")
	}
	if err := os.WriteFile(fgRelease, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitPriorityCondition(t, func() bool { _, err := os.Stat(bgReady); return err == nil })
	if err := os.WriteFile(bgRelease, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitPriorityCondition(t, func() bool { return priorityIntentCount(dir) == 0 })
}

func TestWriterPriorityUnsafeIntentFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BrainLockDirName, "write-foreground-unsafe.lock")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(BackgroundWrites(context.Background()), time.Second)
	defer cancel()
	if unlock, err := AcquireBrainWriteLockContext(ctx, dir); err == nil {
		unlock()
		t.Fatal("accepted unsafe intent")
	}
}
