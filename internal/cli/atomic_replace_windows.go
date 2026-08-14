//go:build windows

package cli

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsAtomicReplaceAttempts = 100
	windowsAtomicReplaceDelay    = 5 * time.Millisecond
)

func replaceFileAtomic(tmpName, path string) error {
	tmpNamePtr := windows.StringToUTF16Ptr(tmpName)
	pathPtr := windows.StringToUTF16Ptr(path)
	var err error
	for attempt := 0; attempt < windowsAtomicReplaceAttempts; attempt++ {
		err = windows.MoveFileEx(
			tmpNamePtr,
			pathPtr,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
		)
		if err == nil || !windowsAtomicReplaceRetryable(err) {
			return err
		}
		time.Sleep(windowsAtomicReplaceDelay)
	}
	return err
}

func windowsAtomicReplaceRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func syncParentDir(path string) error {
	_ = path
	return nil
}

// Windows has no portable directory-fsync equivalent exposed by os. File data
// and marker data are flushed before each rename/remove; startup recovery keeps
// a surviving marker fail-closed if directory metadata is replayed after a
// power loss.
func syncPinnedDirectory(root *os.Root) error {
	_ = root
	return nil
}
