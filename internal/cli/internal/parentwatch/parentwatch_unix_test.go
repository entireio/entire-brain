//go:build darwin || linux

package parentwatch

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMain doubles as the helper binary for the integration check: the test
// re-execs itself as a parent that spawns an armed child, so the watchdog is
// exercised against real processes and a real SIGKILL.
func TestMain(m *testing.M) {
	switch os.Getenv("PARENTWATCH_HELPER") {
	case "parent":
		runParentHelper()
	case "child":
		runChildHelper()
	default:
		os.Exit(m.Run())
	}
}

func runParentHelper() {
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "PARENTWATCH_HELPER=child")
	child.Stdin = os.Stdin
	child.Stdout = os.Stderr
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start child:", err)
		os.Exit(2)
	}
	fmt.Println(child.Process.Pid)
	hangOnStdin()
}

// hangOnStdin blocks in a read syscall on the pipe the test holds open: a
// bare select{} would trip Go's deadlock detector and kill the helper on its
// own, before the test's SIGKILL.
func hangOnStdin() {
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(2)
}

func runChildHelper() {
	dir := os.Getenv("PARENTWATCH_DIR")
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(2)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		os.Exit(2)
	}
	Arm()
	// Arm registers the watcher synchronously, so "ready" means "armed".
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1"), 0o600); err != nil {
		os.Exit(2)
	}
	hangOnStdin()
}

// The customer failure: SIGKILL to the dispatcher cannot be forwarded, the
// orphaned plugin keeps the brain write lock for its full runtime. An armed
// child must instead exit promptly on the parent-exit event and release its
// flock with it.
func TestArmedChildExitsWhenParentIsKilled(t *testing.T) {
	dir := t.TempDir()
	parent := exec.Command(os.Args[0])
	parent.Env = append(os.Environ(), "PARENTWATCH_HELPER=parent", "PARENTWATCH_DIR="+dir)
	parent.Stderr = os.Stderr
	hold, holdWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holdWrite.Close(); _ = hold.Close() })
	parent.Stdin = hold
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = parent.Process.Kill()
		_, _ = parent.Process.Wait()
	})

	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read child pid from parent helper: %v", err)
	}
	childPid, err := strconv.Atoi(line[:len(line)-1])
	if err != nil {
		t.Fatalf("parse child pid %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(childPid, syscall.SIGKILL) })

	waitFor(t, "child armed", func() bool {
		_, err := os.Stat(filepath.Join(dir, "ready"))
		return err == nil
	})

	if err := syscall.Kill(parent.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = parent.Process.Wait()

	waitFor(t, "armed child exits after parent SIGKILL", func() bool {
		return syscall.Kill(childPid, 0) == syscall.ESRCH
	})

	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("the dead child still holds its flock: %v", err)
	}
}

// waitFor polls only inside the test harness; the watchdog under test is
// event-driven, which is why the deadline can be this short.
func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
