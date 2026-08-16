package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const fileLockRetryInterval = 10 * time.Millisecond

var errFileLockTimeout = errors.New("file lock timeout")

type fileLock struct {
	path string
	file *os.File
}

func acquireFileLock(path, code string, timeout time.Duration) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := rejectUnsafeExistingRegularFile(path, "lock file"); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|fileLockOpenFlags(), 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock file: %w", err)
		}
		if err := rejectOpenFileAlias(path, f, "lock file"); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = tryLockFile(f)
		if err == nil {
			writeLockMetadata(f)
			return &fileLock{path: path, file: f}, nil
		}
		_ = f.Close()
		if !isLockBusy(err) {
			return nil, fmt.Errorf("lock file: %w", err)
		}
		if timeout <= 0 || time.Now().Add(fileLockRetryInterval).After(deadline) {
			return nil, fmt.Errorf("%s: timed out acquiring lock at %s: %w", code, path, errFileLockTimeout)
		}
		time.Sleep(fileLockRetryInterval)
	}
}

func rejectUnsafeExistingRegularFile(path, label string) error {
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
	f, err := os.OpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return rejectOpenFileAlias(path, f, label)
}

func rejectOpenFileAlias(path string, f *os.File, label string) error {
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
	if err := rejectOpenFileHardlink(path, f, fileInfo, label); err != nil {
		return err
	}
	return nil
}

func writeLockMetadata(f *os.File) {
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "pid=%d\ncreated_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	_ = f.Sync()
}

func (l *fileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
