//go:build windows

package config

import "golang.org/x/sys/windows"

func replaceConfigFileAtomic(tmpName, path string) error {
	return windows.MoveFileEx(
		windows.StringToUTF16Ptr(tmpName),
		windows.StringToUTF16Ptr(path),
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
