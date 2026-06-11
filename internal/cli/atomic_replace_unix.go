//go:build !windows

package cli

import (
	"os"
	"path/filepath"
)

func replaceFileAtomic(tmpName, path string) error {
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncParentDir(path)
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
