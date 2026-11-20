//go:build windows

package brainstore

import (
	"errors"

	"golang.org/x/sys/windows"
)

// ProcessAlive reports whether a recorded backfill pid is still running.
//
// os.FindProcess is not sufficient on Windows, which is what this replaced. A
// process object outlives the process itself for as long as any handle to it is
// open, and os.FindProcess holds exactly such a handle — so a backfill that had
// already finished still answered "alive". Both callers read that as a live
// holder: backfillRunning then declines to start another backfill (the brain
// silently stops backfilling for good) and describeFileLockHolder names a dead
// process as the lock owner.
//
// WaitForSingleObject with a zero timeout is the unambiguous test. A process
// handle is signalled exactly when the process has exited, so WAIT_TIMEOUT means
// still running and WAIT_OBJECT_0 means finished. GetExitCodeProcess is the more
// obvious call and is deliberately not used: it reports STILL_ACTIVE (259) for a
// running process, but 259 is also a perfectly legal exit code, so it cannot
// distinguish a live process from one that exited with 259.
//
// PID reuse is NOT addressed here, and that is deliberate parity rather than an
// oversight: the Unix build has the same exposure, since Signal(0) against a
// recycled pid also succeeds. Closing it needs a process start-time or sequence
// number recorded alongside the pid, which is a change to what gets written, not
// to how it is read.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Access denied means the process is there and we are not allowed to ask
		// about it — an elevated or other-user process. That is existence, so it
		// answers alive. Anything else (above all ERROR_INVALID_PARAMETER, which
		// is what a missing pid returns) answers dead.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck // nothing actionable on close failure

	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false
	}
	return state == uint32(windows.WAIT_TIMEOUT)
}
