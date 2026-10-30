package base

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func CleanBrainRelativePath(rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path is outside brain: %s", rel)
	}
	return clean, nil
}

func PrepareBrainRelativeSQLiteFile(brainDir, rel string) (string, error) {
	if err := RejectSymlinkedBrainRoot(brainDir); err != nil {
		return "", err
	}
	clean, err := CleanBrainRelativePath(rel)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(clean)
	if dir != "." {
		if err := RejectExistingSymlinkPathComponents(brainDir, dir); err != nil {
			return "", err
		}
	}
	abs := filepath.Join(brainDir, clean)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", err
	}
	if err := RejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return "", err
	}
	if err := RejectUnsafeExistingRegularFile(abs, "sqlite cache"); err != nil {
		return "", err
	}
	return abs, nil
}

// ErrFileHardlinked classifies the nlink>1 refusal so callers can tell an
// externally hardlinked state file (a backup or bench tool's cheap copy, which
// a rebuild clears) from damage that needs different handling. The rendered
// message is unchanged: "<label> must not be hardlinked: <path>".
var ErrFileHardlinked = errors.New("must not be hardlinked")

func RejectUnsafeExistingRegularFile(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink: %s", label, path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file: %s", label, path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|FileLockOpenFlags()|MemoryStateReadOpenFlags(), 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return RejectOpenFileAlias(path, f, label)
}

func RejectOpenFileAlias(path string, f *os.File, label string) error {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink: %s", label, path)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(pathInfo, fileInfo) {
		return fmt.Errorf("%s changed while opening: %s", label, path)
	}
	if !fileInfo.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file: %s", label, path)
	}
	if err := RejectOpenFileHardlink(path, f, fileInfo, label); err != nil {
		return err
	}
	return nil
}

func RejectSymlinkPathComponents(root, rel string) error {
	clean := filepath.Clean(rel)
	// clean == ".." must be listed explicitly: filepath.Clean("..") is "..",
	// which is neither "." nor prefixed with "../", so a bare ".." otherwise
	// falls through this whole test. The per-component walk below then Lstats
	// the PARENT of root — a real directory, not a symlink — and passes it, and
	// the closing containment check returns nil early whenever root does not
	// exist yet. One level up is exactly the escape the guard exists to stop.
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside root: %s", rel)
	}
	current := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component must not be a symlink: %s", rel)
		}
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	pathResolved, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return err
	}
	if !PathInside(rootResolved, pathResolved) {
		return fmt.Errorf("path escapes root through symlinks: %s", rel)
	}
	return nil
}

func RejectExistingSymlinkPathComponents(root, rel string) error {
	clean := filepath.Clean(rel)
	// clean == ".." must be listed explicitly: filepath.Clean("..") is "..",
	// which is neither "." nor prefixed with "../", so a bare ".." otherwise
	// falls through this whole test. The per-component walk below then Lstats
	// the PARENT of root — a real directory, not a symlink — and passes it, and
	// the closing containment check returns nil early whenever root does not
	// exist yet. One level up is exactly the escape the guard exists to stop.
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside root: %s", rel)
	}
	current := root
	deepestExisting := root
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				break // tolerate not-yet-created leaves on the write path
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component must not be a symlink: %s", rel)
		}
		deepestExisting = current
	}
	// Parity with RejectSymlinkPathComponents (the read path): verify the existing
	// prefix still resolves inside root once symlinks in root's own ancestry are
	// normalized. The per-component checks alone miss an ancestor-symlink escape,
	// which is exactly the gap the write path previously had. The root may not be
	// created yet on a first write — there is nothing to escape through then, so a
	// not-yet-existing prefix is treated as safe.
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	existingResolved, err := filepath.EvalSymlinks(deepestExisting)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !PathInside(rootResolved, existingResolved) {
		return fmt.Errorf("path escapes root through symlinks: %s", rel)
	}
	return nil
}

func RejectSymlinkedBrainRoot(brainDir string) error {
	info, err := os.Lstat(brainDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("brain directory must not be a symlink: %s", brainDir)
	}
	return nil
}

func PathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
