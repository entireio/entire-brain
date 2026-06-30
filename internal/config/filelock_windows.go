//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func fileLockOpenFlags() int {
	return 0
}

func rejectOpenFileHardlink(path string, f *os.File, _ os.FileInfo, label string) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks > 1 {
		return fmt.Errorf("%s must not be hardlinked: %s", label, path)
	}
	return nil
}

func tryLockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
}

func unlockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}

func isLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
