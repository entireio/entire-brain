package parentwatch

import "golang.org/x/sys/unix"

// watchParent opens a pidfd on the parent and returns a wait that blocks in
// poll(2); the pidfd becomes readable when the parent exits (kernel >= 5.3).
func watchParent(ppid int) (func() bool, error) {
	fd, err := unix.PidfdOpen(ppid, 0)
	if err != nil {
		// pidfd_open can be missing (ENOSYS, kernel < 5.3) or blocked (EPERM
		// under seccomp profiles that still allow prctl), so any failure falls
		// back to PR_SET_PDEATHSIG. The kernel then delivers SIGTERM on parent
		// death and the normal signal path exits — there is nothing to watch,
		// which a nil wait tells Arm. Caveat: PDEATHSIG tracks the parent
		// thread that spawned us, not strictly the process.
		if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return func() bool {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		return waitReady(func() (int, error) { return unix.Poll(fds, -1) })
	}, nil
}
