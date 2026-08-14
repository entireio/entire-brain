//go:build !windows

package cli

import (
	"os"
	"path/filepath"
)

// atomicReplaceBeforeParentSync is a deterministic durability-failure seam.
// Production leaves it as a no-op; Unix tests use it to prove callers handle
// the rename-succeeded/sync-failed outcome rather than assuming no mutation.
var atomicReplaceBeforeParentSync = func(string) error { return nil }

func replaceFileAtomic(tmpName, path string) error {
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := atomicReplaceBeforeParentSync(path); err != nil {
		return &atomicReplaceDurabilityError{Path: path, Err: err}
	}
	if err := syncParentDir(path); err != nil {
		return &atomicReplaceDurabilityError{Path: path, Err: err}
	}
	return nil
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// syncPinnedDirectory persists directory-entry operations through the exact
// descriptor-rooted directory used for publication, even if its pathname is
// concurrently replaced.
func syncPinnedDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
