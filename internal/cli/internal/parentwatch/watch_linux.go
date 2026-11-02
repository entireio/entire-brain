package parentwatch

import (
	"os"
	"os/signal"
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

// pdeathsigWait arms PR_SET_PDEATHSIG and blocks until the signal proves the
// parent PROCESS is gone. Two PDEATHSIG sharp edges are handled here:
//
//   - The condition is per parent THREAD: the kernel fires when the thread
//     that forked us exits, and a Go dispatcher can retire that thread while
//     the process lives. So the signal is SIGUSR1, caught, and each delivery
//     only triggers a getppid check: a dead parent process shows a new ppid
//     (reparented to pid 1 or a subreaper), while a dead parent thread leaves
//     it unchanged — the child reparents inside the thread group and the
//     still-armed PDEATHSIG fires again on the next death. A false fire can
//     therefore never kill a healthy plugin.
//   - The armed setting is also per OUR thread: if the runtime destroyed the
//     thread that called prctl, the fallback would silently disarm, so that
//     thread is locked and never released. Parking on the channel keeps this
//     goroutine alive for the process lifetime.
//
// Notify runs before prctl so the fire can never hit SIGUSR1's default
// terminating disposition.
func pdeathsigWait(ppid int) bool {
	runtime.LockOSThread()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGUSR1)
	if unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGUSR1), 0, 0, 0) != nil {
		return false
	}
	if os.Getppid() != ppid {
		return true
	}
	for range sigs {
		if os.Getppid() != ppid {
			return true
		}
	}
	return false
}
