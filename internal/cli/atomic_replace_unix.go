//go:build !windows

package cli

import "os"

func replaceFileAtomic(tmpName, path string) error {
	return os.Rename(tmpName, path)
}
