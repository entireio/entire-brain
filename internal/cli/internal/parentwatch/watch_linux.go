package parentwatch

import "golang.org/x/sys/unix"

// watchParent opens a pidfd on the parent and returns a wait that blocks in
// poll(2); the pidfd becomes readable when the parent exits (kernel >= 5.3).
// On ENOSYS it falls back to PR_SET_PDEATHSIG: the kernel then delivers
// SIGTERM on parent death and the normal signal path exits, so the returned
// wait has nothing to observe and blocks forever. PDEATHSIG caveat: it fires
// when the parent THREAD that forked us dies, not strictly the process —
// acceptable for a fallback. On any unexpected poll failure the wait blocks
// forever, degrading to no watchdog rather than killing a healthy process.
func watchParent(ppid int) (func(), error) {
	fd, err := unix.PidfdOpen(ppid, 0)
	if err == unix.ENOSYS {
		if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
			return nil, err
		}
		return func() { select {} }, nil
	}
	if err != nil {
		return nil, err
	}
	return func() {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		for {
			n, err := unix.Poll(fds, -1)
			if n > 0 {
				return
			}
			if err != nil && err != unix.EINTR {
				select {}
			}
		}
	}, nil
}
