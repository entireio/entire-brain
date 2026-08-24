package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const fileLockRetryInterval = 10 * time.Millisecond

var errFileLockTimeout = errors.New("file lock timeout")

type fileLock struct {
	path        string
	file        *os.File
	processLock *processFileLock
}

// processFileLocks makes file locks visible to Go goroutines for the full
// acquireFileLock-to-Close lifetime. The OS lock remains necessary for other
// processes.
var processFileLocks processFileLockRegistry

type processFileLockRegistry struct {
	sync.Mutex
	// os.FileInfo has no portable comparable identity, so lookup uses os.SameFile.
	locks []*processFileLock
}

type processFileLock struct {
	parent   os.FileInfo
	basename string
	token    chan struct{}
	refs     int // holders and waiters; protected by processFileLocks
}

func acquireFileLock(path, code string, timeout time.Duration) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	parent, basename, err := fileLockIdentity(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	processLock, ok := acquireProcessFileLock(parent, basename, deadline, timeout)
	if !ok {
		return nil, fileLockTimeoutError(code, path)
	}
	defer func() {
		if processLock != nil {
			processLock.release()
		}
	}()

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
			lock := &fileLock{path: path, file: f, processLock: processLock}
			processLock = nil
			return lock, nil
		}
		_ = f.Close()
		if !isLockBusy(err) {
			return nil, fmt.Errorf("lock file: %w", err)
		}
		if timeout <= 0 || time.Now().Add(fileLockRetryInterval).After(deadline) {
			return nil, fileLockTimeoutError(code, path)
		}
		time.Sleep(fileLockRetryInterval)
	}
}

func fileLockIdentity(path string) (os.FileInfo, string, error) {
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, "", fmt.Errorf("stat lock directory: %w", err)
	}
	if !parent.IsDir() {
		return nil, "", fmt.Errorf("lock directory must be a directory: %s", filepath.Dir(path))
	}
	return parent, filepath.Base(path), nil
}

func acquireProcessFileLock(parent os.FileInfo, basename string, deadline time.Time, timeout time.Duration) (*processFileLock, bool) {
	processFileLocks.Lock()
	lock := processFileLocks.find(parent, basename)
	if lock == nil {
		lock = &processFileLock{parent: parent, basename: basename, token: make(chan struct{}, 1)}
		processFileLocks.locks = append(processFileLocks.locks, lock)
	}
	lock.refs++
	processFileLocks.Unlock()

	select {
	case lock.token <- struct{}{}:
		return lock, true
	default:
	}
	if timeout <= 0 {
		lock.releaseReference()
		return nil, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		lock.releaseReference()
		return nil, false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case lock.token <- struct{}{}:
		return lock, true
	case <-timer.C:
		lock.releaseReference()
		return nil, false
	}
}

func (l *processFileLock) release() {
	<-l.token
	l.releaseReference()
}

func (r *processFileLockRegistry) find(parent os.FileInfo, basename string) *processFileLock {
	for _, lock := range r.locks {
		if lock.basename == basename && os.SameFile(lock.parent, parent) {
			return lock
		}
	}
	return nil
}

func (l *processFileLock) releaseReference() {
	processFileLocks.Lock()
	defer processFileLocks.Unlock()
	l.refs--
	if l.refs != 0 {
		return
	}
	for i, lock := range processFileLocks.locks {
		if lock == l {
			copy(processFileLocks.locks[i:], processFileLocks.locks[i+1:])
			last := len(processFileLocks.locks) - 1
			processFileLocks.locks[last] = nil
			processFileLocks.locks = processFileLocks.locks[:last]
			return
		}
	}
}

func fileLockTimeoutError(code, path string) error {
	return fmt.Errorf("%s: timed out acquiring lock at %s: %w", code, path, errFileLockTimeout)
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
	if l.processLock != nil {
		l.processLock.release()
		l.processLock = nil
	}
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
