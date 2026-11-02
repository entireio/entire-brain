package parentwatch

import "golang.org/x/sys/unix"

// watchParent registers a kqueue NOTE_EXIT watch on the parent and returns a
// wait that blocks until the parent exits.
func watchParent(ppid int) (func() bool, error) {
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
	return func() bool {
		events := make([]unix.Kevent_t, 1)
		return waitReady(func() (int, error) { return unix.Kevent(kq, nil, events, nil) })
	}, nil
}
