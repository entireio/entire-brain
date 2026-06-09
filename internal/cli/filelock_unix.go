//go:build !windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func fileLockOpenFlags() int {
	return unix.O_NOFOLLOW
}

func rejectOpenFileHardlink(path string, _ *os.File, info os.FileInfo, label string) error {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Errorf("%s must not be hardlinked: %s", label, path)
	}
	return nil
}

func tryLockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

func isLockBusy(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}
