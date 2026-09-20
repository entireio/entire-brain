//go:build !windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

func replaceConfigFileAtomic(tmpName, path string) error {
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("config replaced but open parent for sync failed: %w", err)
	}
	defer dir.Close()
	return dir.Sync()
}
