//go:build windows

package cli

import "golang.org/x/sys/windows"

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
