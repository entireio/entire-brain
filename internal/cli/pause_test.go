package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStopPausesAndStartResumesBackgroundWork(t *testing.T) {
	f := newSetupTestFixture(t)

	if out, err := execute(t, newStopCommand(f.opts)); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if !backgroundPaused(f.env) {
		t.Fatal("stop did not pause background work")
	}
	if out, err := execute(t, newStartCommand(f.opts)); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	if backgroundPaused(f.env) {
		t.Fatal("start did not resume background work")
	}
}

// The backfill is the one background process that spends tokens for hours, so
// pausing must not leave it running to completion.
func TestStopEndsThisRepositorysRunningBackfill(t *testing.T) {
	f := newSetupTestFixture(t)
	stateDir := filepath.Dir(f.storage.HeadPath)
	// A live pid AND a held distill pass lock is what a running backfill looks
	// like (see backfillRunning); our own pid stands in for the child.
	if err := writeSetupBackfillState(stateDir, setupBackfillState{SchemaVersion: setupBackfillStateVersion, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.storage.BrainDir, brainLockDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	passLock, err := acquireFileLock(filepath.Join(f.storage.BrainDir, brainLockDirName, brainDistillLockName), "distill_pass_locked", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = passLock.Close() }()

	var stopped []int
	pid, err := stopRepoBackfill(context.Background(), f.opts, func(pid int) error { stopped = append(stopped, pid); return nil })
	if err != nil {
		t.Fatalf("stop backfill: %v", err)
	}
	if pid != os.Getpid() || len(stopped) != 1 || stopped[0] != os.Getpid() {
		t.Fatalf("the running backfill was not stopped: pid=%d stopped=%v", pid, stopped)
	}
}

// A recorded pid whose distill lock is free belongs to some other process now;
// stopping it would kill a stranger.
func TestStopLeavesAStaleBackfillRecordAlone(t *testing.T) {
	f := newSetupTestFixture(t)
	stateDir := filepath.Dir(f.storage.HeadPath)
	if err := writeSetupBackfillState(stateDir, setupBackfillState{SchemaVersion: setupBackfillStateVersion, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	stops := 0
	pid, err := stopRepoBackfill(context.Background(), f.opts, func(int) error { stops++; return nil })
	if err != nil || pid != 0 || stops != 0 {
		t.Fatalf("a stale record must not be acted on: pid=%d stops=%d err=%v", pid, stops, err)
	}
}

// The installed watcher is KeepAlive: killing it only restarts it. Pausing has
// to make the running watcher idle instead.
func TestSupervisedWatchIdlesWhilePaused(t *testing.T) {
	f := newSetupTestFixture(t)
	if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: "alpha", Interval: "5m"}); err != nil {
		t.Fatal(err)
	}
	if err := setBackgroundPaused(f.env, true); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	base := defaultWatchOptions()
	base.once = true
	passes := 0
	if err := supervisedWatchLoop(context.Background(), out, f.env, base,
		func(string, watchCommandOptions, *int) error { passes++; return nil }, nil); err != nil {
		t.Fatalf("a paused watcher must keep running, not exit: %v", err)
	}
	if passes != 0 {
		t.Fatalf("paused, but %d workspace passes ran", passes)
	}
	if !strings.Contains(out.String(), "paused") {
		t.Fatalf("the log must say why nothing ran: %q", out.String())
	}
}

func TestSetupSkipsBackfillWhilePaused(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	if err := setBackgroundPaused(f.env, true); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon = true

	out := runSetupForTest(t, f, opts, rec)

	if rec.spawnCalls != 0 {
		t.Fatalf("paused, but setup spawned %d backfill(s)", rec.spawnCalls)
	}
	if !strings.Contains(out, "paused") {
		t.Fatalf("the skip must say why:\n%s", out)
	}
}

// Every launch path (hooks, watch ticks, MCP startup, self-relaunch) ends in a
// worker process, so the worker is the one place that has to honour the pause.
func TestMemoryWorkerDoesNothingWhilePaused(t *testing.T) {
	oldPrepass, oldLaunchAfter := memoryWorkerPrepass, memoryWorkerLaunchAfter
	defer func() { memoryWorkerPrepass, memoryWorkerLaunchAfter = oldPrepass, oldLaunchAfter }()
	prepassCalls, relaunches := 0, 0
	memoryWorkerPrepass = func(context.Context, Options, string) error { prepassCalls++; return nil }
	memoryWorkerLaunchAfter = func(string, time.Duration, int) error { relaunches++; return nil }

	opts, _ := newPrepassBackoffBrain(t, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if err := setBackgroundPaused(opts.Env, true); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, newMemoryWorkerCommand(opts), "--once")
	if err != nil {
		t.Fatalf("worker: %v\n%s", err, out)
	}
	if prepassCalls != 0 || relaunches != 0 {
		t.Fatalf("paused worker did work: prepass=%d relaunches=%d", prepassCalls, relaunches)
	}
	if !strings.Contains(out, `"paused": true`) {
		t.Fatalf("the worker must report that it was paused: %s", out)
	}
}
