package parentwatch

import "golang.org/x/sys/unix"

// watchParent registers a kqueue NOTE_EXIT watch on the parent and returns a
// blocking wait. The wait returns only when the parent has exited; on any
// unexpected kqueue failure it blocks forever, degrading to no watchdog
// rather than killing a process whose parent is still alive.
func watchParent(ppid int) (func(), error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	change := unix.Kevent_t{
		Ident:  uint64(ppid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD,
		Fflags: unix.NOTE_EXIT,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	return func() {
		events := make([]unix.Kevent_t, 1)
		for {
			n, err := unix.Kevent(kq, nil, events, nil)
			if n > 0 {
				return
			}
			if err != nil && err != unix.EINTR {
				select {}
			}
		}
	}, nil
}
