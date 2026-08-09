//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

func replaceFileAtomic(tmpName, path string) error {
	return windows.MoveFileEx(
		windows.StringToUTF16Ptr(tmpName),
		windows.StringToUTF16Ptr(path),
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
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
