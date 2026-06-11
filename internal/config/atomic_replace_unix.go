//go:build !windows

package config

import (
	"os"
	"path/filepath"
)

func replaceConfigFileAtomic(tmpName, path string) error {
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil
	}
	defer dir.Close()
	return dir.Sync()
}
