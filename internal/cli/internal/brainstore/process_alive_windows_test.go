//go:build windows

package brainstore

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessAliveRejectsNonPositivePIDs(t *testing.T) {
	t.Parallel()

	for _, pid := range []int{0, -1} {
		if ProcessAlive(pid) {
			t.Fatalf("ProcessAlive(%d) = true, want false", pid)
		}
	}
}

func TestProcessAliveReportsThisProcess(t *testing.T) {
	t.Parallel()

	if !ProcessAlive(os.Getpid()) {
		t.Fatal("ProcessAlive(own pid) = false, want true")
	}
}

// The regression this file exists for. The child is started and never Wait()ed,
// so Go keeps its process handle open and the process OBJECT survives the
// process. Under the old os.FindProcess check that object was enough to report
// "alive" forever; the wait-based check sees the handle signalled and reports
// the exit.
//
// Not Wait()ing is the whole point of the setup, not an oversight — it is what
// reproduces a holder keeping the object alive. The handle is released by the
// test process exiting.
func TestProcessAliveSeesAnExitedChildWhoseHandleIsStillOpen(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		t.Fatalf("child pid = %d", pid)
	}

	// Poll rather than sleep a fixed amount: the child exits immediately, but how
	// soon Windows reflects that is not something to hard-code.
	deadline := time.Now().Add(30 * time.Second)
	for ProcessAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("ProcessAlive(%d) still true 30s after the child exited", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
