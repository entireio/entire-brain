//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// mkfifoForTest creates a FIFO at path, skipping the test when the platform or
// filesystem cannot provide one (some CI sandboxes forbid mknod).
func mkfifoForTest(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Skipf("mkfifo did not produce a FIFO: info=%v err=%v", info, err)
	}
}

// indexedTraceIngestFixture builds a real semantic index and returns the options
// plus the brain directory the index lock lives under.
func indexedTraceIngestFixture(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	return opts, brainDir
}

// TestIngestTracesRejectsFIFOPathPromptly proves the trace reader refuses a
// non-regular file instead of blocking in open(2). A FIFO with no writer parks
// the calling thread inside open until a writer appears, so an unfixed build
// never returns here.
func TestIngestTracesRejectsFIFOPathPromptly(t *testing.T) {
	opts, _ := indexedTraceIngestFixture(t)
	fifoPath := filepath.Join(t.TempDir(), "trace.ndjson")
	mkfifoForTest(t, fifoPath)

	done := make(chan error, 1)
	// Deliberately leaked on the unfixed path: the goroutine is parked in the
	// open(2) syscall and cannot be cancelled, so the test must never wait on it.
	go func() {
		done <- runSemanticIngestTraces(&cobra.Command{Use: "ingest"}, opts, semanticTraceIngestOptions{json: true}, fifoPath)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ingesting a FIFO trace path succeeded; want a rejection")
		}
		if !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO rejection error = %v, want it to name the regular-file requirement", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runSemanticIngestTraces blocked on a FIFO trace path (unbounded open(2))")
	}
}

// TestIngestTracesFIFODoesNotWedgeSemanticIndexLock is the security claim: a
// blocked trace read must not deny the semantic index lock to every other
// caller on the same brain directory. On an unfixed build the FIFO ingest parks
// inside open(2) while already holding the index lock, so the second caller
// below is refused with "index_locked" after the lock timeout.
func TestIngestTracesFIFODoesNotWedgeSemanticIndexLock(t *testing.T) {
	opts, brainDir := indexedTraceIngestFixture(t)
	traceDir := t.TempDir()
	fifoPath := filepath.Join(traceDir, "trace.ndjson")
	mkfifoForTest(t, fifoPath)

	blocked := make(chan error, 1)
	// Leaked on purpose on the unfixed path; see the note above.
	go func() {
		blocked <- runSemanticIngestTraces(&cobra.Command{Use: "ingest-fifo"}, opts, semanticTraceIngestOptions{json: true}, fifoPath)
	}()

	// Let the FIFO ingest reach either its rejection (fixed) or its park inside
	// open(2) while holding the index lock (unfixed).
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
	}

	regularPath := filepath.Join(traceDir, "regular.ndjson")
	if err := os.WriteFile(regularPath, []byte(`{"from":"caller","to":"auth.ValidateToken","type":"CALLS"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := make(chan error, 1)
	go func() {
		second <- runSemanticIngestTraces(&cobra.Command{Use: "ingest-regular"}, opts, semanticTraceIngestOptions{json: true}, regularPath)
	}()

	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second ingest on brain %s was wedged by the FIFO read: %v", brainDir, err)
		}
	case <-time.After(semanticIndexLockTimeout + 10*time.Second):
		t.Fatalf("second ingest on brain %s never returned; the FIFO read is holding the index lock", brainDir)
	}
}

// TestIngestTracesReadsTracePathBeforeTakingIndexLock isolates the ordering fix:
// the caller-supplied trace file must be read BEFORE the semantic index lock is
// acquired, so time spent on an untrusted path is never time the lock is held.
//
// A FIFO is not needed here — the point is the ordering, not the file type — so
// the read is stalled at a releasable barrier instead. If the lock were taken
// first, the second caller below would be refused with "index_locked".
func TestIngestTracesReadsTracePathBeforeTakingIndexLock(t *testing.T) {
	opts, brainDir := indexedTraceIngestFixture(t)
	traceDir := t.TempDir()
	stalledPath := filepath.Join(traceDir, "stalled.ndjson")
	traceLine := []byte(`{"from":"caller","to":"auth.ValidateToken","type":"CALLS"}` + "\n")
	if err := os.WriteFile(stalledPath, traceLine, 0o600); err != nil {
		t.Fatal(err)
	}

	reading := make(chan struct{})
	release := make(chan struct{})
	originalOpen := safeReadOpenFile
	var once sync.Once
	safeReadOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == stalledPath {
			once.Do(func() {
				close(reading)
				<-release
			})
		}
		return originalOpen(name, flag, perm)
	}
	t.Cleanup(func() { safeReadOpenFile = originalOpen })

	stalled := make(chan error, 1)
	go func() {
		stalled <- runSemanticIngestTraces(&cobra.Command{Use: "ingest-stalled"}, opts, semanticTraceIngestOptions{json: true}, stalledPath)
	}()

	select {
	case <-reading:
	case err := <-stalled:
		t.Fatalf("stalled ingest returned before reaching the trace read: %v", err)
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("stalled ingest never reached the trace read")
	}

	// The trace read is in flight. The index lock must still be available.
	lockFree := make(chan error, 1)
	go func() {
		unlock, err := acquireSemanticIndexLock(brainDir)
		if err == nil {
			unlock()
		}
		lockFree <- err
	}()

	select {
	case err := <-lockFree:
		close(release)
		if err != nil {
			t.Fatalf("index lock was held across the untrusted trace read: %v", err)
		}
	case <-time.After(semanticIndexLockTimeout + 10*time.Second):
		close(release)
		t.Fatal("acquiring the index lock never returned while a trace read was in flight")
	}

	select {
	case err := <-stalled:
		if err != nil {
			t.Fatalf("stalled ingest failed after release: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stalled ingest never completed after release")
	}
}

// TestIngestTracesRejectsRegularToFIFOSwapDuringOpen covers the second layer:
// a path that passes the lstat type check and is replaced by a FIFO before the
// open must still be refused, and must not park. O_NONBLOCK lets the open on
// the swapped-in FIFO return, and the post-open SameFile/IsRegular recheck
// rejects the descriptor.
func TestIngestTracesRejectsRegularToFIFOSwapDuringOpen(t *testing.T) {
	opts, _ := indexedTraceIngestFixture(t)
	tracePath := filepath.Join(t.TempDir(), "swapped.ndjson")
	if err := os.WriteFile(tracePath, []byte(`{"from":"caller","to":"auth.ValidateToken","type":"CALLS"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalOpen := safeReadOpenFile
	swapped := false
	safeReadOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == tracePath && !swapped {
			swapped = true
			if err := os.Remove(tracePath); err != nil {
				return nil, err
			}
			if err := syscall.Mkfifo(tracePath, 0o600); err != nil {
				return nil, err
			}
		}
		return originalOpen(name, flag, perm)
	}
	t.Cleanup(func() { safeReadOpenFile = originalOpen })

	done := make(chan error, 1)
	go func() {
		done <- runSemanticIngestTraces(&cobra.Command{Use: "ingest-swap"}, opts, semanticTraceIngestOptions{json: true}, tracePath)
	}()

	select {
	case err := <-done:
		if !swapped {
			t.Fatal("the swap hook never fired; the test proved nothing")
		}
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("swap rejection error = %v, want it to name the regular-file requirement", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runSemanticIngestTraces blocked on a regular-to-FIFO swap during open")
	}
}
