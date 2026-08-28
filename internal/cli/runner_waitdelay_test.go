package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A cancelled command must not keep the runner blocked on a grandchild that
// inherited its output pipes.
//
// exec.CommandContext kills the child, but Run/Wait return only when every
// writer of the stdout pipe has closed. Before commandWaitDelay, a child that
// backgrounded a long-lived grandchild held the runner for the GRANDCHILD's
// lifetime — measured at 30s against a 300ms deadline. Since every timeout in
// this binary is built on this runner, that made all of them unenforceable.
func writeGrandchildScript(t *testing.T) (dir, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the probe is a POSIX shell script")
	}
	dir = t.TempDir()
	script = filepath.Join(dir, "grandchild.sh")
	// Background a grandchild that inherits stdout and outlives the child.
	body := "#!/bin/sh\nsleep 30 &\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, script
}

// bounded is the ceiling a cancelled command may take to return: the wait delay
// plus generous slack for a loaded CI host. It is far below the 30s the
// grandchild lives, so the assertion distinguishes "bounded" from "waits for the
// grandchild" without being flaky.
const bounded = commandWaitDelay + 8*time.Second

func TestRunReturnsAfterCancellationDespiteGrandchildHoldingStdout(t *testing.T) {
	t.Parallel()
	dir, script := writeGrandchildScript(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, _, err := (ExecRunner{}).Run(ctx, dir, "/bin/sh", script); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > bounded {
		t.Fatalf("Run blocked for %v after a 300ms deadline; a grandchild holding stdout must not "+
			"outlast commandWaitDelay (%v)", elapsed, commandWaitDelay)
	}
}

func TestStreamWaitReturnsAfterCancellationDespiteGrandchildHoldingStdout(t *testing.T) {
	t.Parallel()
	dir, script := writeGrandchildScript(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	stream, err := (ExecRunner{}).Stream(ctx, dir, "/bin/sh", script)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	start := time.Now()
	_, _ = io.Copy(io.Discard, stream.Stdout())
	if _, err := stream.Wait(); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > bounded {
		t.Fatalf("Stream.Wait blocked for %v after a 300ms deadline; a grandchild holding stdout "+
			"must not outlast commandWaitDelay (%v)", elapsed, commandWaitDelay)
	}
}

// A command that exits on its own must be entirely unaffected: WaitDelay is a
// cancellation backstop, not a timeout.
func TestWaitDelayDoesNotAffectACommandThatExitsNormally(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	stdout, _, err := (ExecRunner{}).Run(context.Background(), dir, "/bin/sh", "-c", "printf hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(stdout) != "hello" {
		t.Fatalf("stdout = %q, want %q", stdout, "hello")
	}
}
