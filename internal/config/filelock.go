package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const fileLockRetryInterval = 10 * time.Millisecond

type fileLock struct {
	file *os.File
}

func acquireFileLock(path, code string, timeout time.Duration) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open lock file: %w", err)
		}
		err = tryLockFile(f)
		if err == nil {
			writeLockMetadata(f)
			return &fileLock{file: f}, nil
		}
		_ = f.Close()
		if !isLockBusy(err) {
			return nil, fmt.Errorf("lock file: %w", err)
		}
		if timeout <= 0 || time.Now().Add(fileLockRetryInterval).After(deadline) {
			return nil, fmt.Errorf("%s: timed out acquiring lock at %s", code, path)
		}
		time.Sleep(fileLockRetryInterval)
	}
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
