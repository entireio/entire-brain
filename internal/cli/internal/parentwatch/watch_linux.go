package parentwatch

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// watchParent opens a pidfd on the parent and returns a wait that blocks in
// poll(2); the pidfd becomes readable when the parent exits (kernel >= 5.3).
// pidfd_open can be missing (ENOSYS on older kernels) or blocked (EPERM under
// seccomp profiles that still allow prctl), so any failure falls back to
// PR_SET_PDEATHSIG, armed inside the wait.
func watchParent(ppid int) (func() bool, error) {
	fd, err := unix.PidfdOpen(ppid, 0)
	if err != nil {
		return func() bool { return pdeathsigWait(ppid) }, nil
	}
	return func() bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		return waitReady(func() (int, error) { return unix.Poll(fds, -1) })
	}, nil
}

// pdeathsigWait arms PR_SET_PDEATHSIG and keeps the thread that armed it
// alive. The parent-death signal is a per-thread attribute: if the Go runtime
// later destroyed the thread that called prctl, the fallback would silently
// disarm, so the thread is locked and parked in pause(2) for the process
// lifetime — in a syscall, not Go-blocked, so it cannot trip the runtime's
// deadlock detector. On parent death the kernel delivers SIGTERM process-wide
// and the normal signal path exits; this wait returns true only when the
// parent was already gone at arming time, closing the race that Arm's own
// ppid re-check cannot see from outside. Caveat: PDEATHSIG tracks the parent
// thread that spawned us, not strictly the process.
func pdeathsigWait(ppid int) bool {
	// Never unlocked: the armed setting must die only with the process.
	runtime.LockOSThread()
	if unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0) != nil {
		return false
	}
	if os.Getppid() != ppid {
		return true
	}
	for {
		// Pause returns whenever the runtime's own signals (e.g. SIGURG
		// preemption) interrupt it; re-enter to keep the thread parked.
		_ = unix.Pause()
	}
}
