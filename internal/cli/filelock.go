package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
			writeLockOwnerMetadata(path)
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
	return fmt.Errorf("%s: timed out acquiring lock at %s%s: %w", code, path, describeFileLockHolder(path), errFileLockTimeout)
}

// fileLockMetadataMaxBytes bounds the read of an owner file.
// writeLockOwnerMetadata writes two short lines; anything larger is not ours
// and is ignored.
const fileLockMetadataMaxBytes = 512

// fileLockOwnerSuffix names the sidecar carrying a lock's owner metadata.
//
// The metadata lives BESIDE the lock rather than inside it because Windows
// byte-range locks are MANDATORY. tryLockFile takes an exclusive LockFileEx
// over byte 0, and a read overlapping a locked range fails with
// ERROR_LOCK_VIOLATION — so metadata stored at offset 0 of the lock file is
// unreadable by any other process exactly when a holder exists, which is the
// only moment it is worth reading. POSIX flock is advisory and hides this
// completely: the same code read the metadata fine on Linux and macOS and came
// back empty-handed on Windows shard 3.
//
// Storing it at a non-zero offset inside the lock file would also work today,
// but that ties this diagnostic to the exact byte range tryLockFile happens to
// lock, so a later change there would silently break the message on Windows
// alone. A separate file nothing ever locks is the version that cannot rot.
const fileLockOwnerSuffix = ".owner"

// describeFileLockHolder turns the metadata writeLockOwnerMetadata stores
// beside every lock into the one fact a blocked caller actually needs: WHICH
// process is holding this lock, and for how long.
//
// The pid and timestamp have been written on every acquisition since the lock
// package existed and were read by nothing, so a contended brain produced
// "brain_locked: timed out acquiring lock at .../locks/write.lock" and stopped
// there — naming the file the user already knew about and withholding the only
// thing that identifies the culprit. On a machine running a background watcher,
// a detached backfill, an MCP server and a terminal, that is the difference
// between a diagnosable stall and a mystery.
//
// It is advisory and fails silently to the empty string: the message must never
// be worse than the one it replaces. A pid whose process is gone is NOT
// reported — the OS releases the lock when its holder dies, so stale metadata
// names an innocent bystander, and naming one would be worse than naming
// nobody.
//
// The sidecar is written by the holder immediately after it wins the lock, so
// the lock itself serializes the writers and there is exactly one. It is read
// only on a TIMEOUT, which is proof somebody holds the lock right now, so the
// name it gives is the current holder — except in the instant between another
// process acquiring and stamping its own name, where it is the one just before.
// Either way it names a process that was holding this lock, which is what the
// message claims. It is never treated as proof a holder is alive: the lock
// remains the only authority on that.
func describeFileLockHolder(lockPath string) string {
	path := lockPath + fileLockOwnerSuffix
	// Lstat first, and demand a plain file. O_NOFOLLOW rejects a symlink but
	// NOT a fifo, and opening a fifo for reading blocks until someone opens the
	// write end -- which would hang the very error path whose whole job is to
	// report that we already waited long enough.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	f, err := os.OpenFile(path, os.O_RDONLY|fileLockOpenFlags(), 0)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	// Re-check through the descriptor: the path could have been swapped between
	// the Lstat and the open.
	if opened, statErr := f.Stat(); statErr != nil || !opened.Mode().IsRegular() {
		return ""
	}
	buf := make([]byte, fileLockMetadataMaxBytes)
	n, err := f.Read(buf)
	if n <= 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return ""
	}
	pid := 0
	var since time.Time
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "pid":
			parsed, convErr := strconv.Atoi(value)
			if convErr == nil {
				pid = parsed
			}
		case "created_at":
			if parsed, convErr := time.Parse(time.RFC3339Nano, value); convErr == nil {
				since = parsed
			}
		}
	}
	if pid <= 0 || !processAlive(pid) {
		return ""
	}
	// A holder that is US is the most valuable answer of all: it means this
	// process is blocked on a lock it already holds, which no amount of waiting
	// will resolve. Naming it as self stops the reader hunting for another
	// process.
	who := fmt.Sprintf("pid %d", pid)
	if pid == os.Getpid() {
		who = fmt.Sprintf("this process, pid %d", pid)
	}
	if since.IsZero() {
		return fmt.Sprintf(" (held by %s)", who)
	}
	held := time.Since(since)
	if held < 0 {
		held = 0
	}
	return fmt.Sprintf(" (held by %s for %s)", who, held.Round(time.Second))
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

// writeLockOwnerMetadata records who holds lockPath, in the sidecar beside it.
// Called by the winner immediately after the lock is taken, so the lock itself
// guarantees a single writer.
//
// Best-effort throughout: a lock must never fail to be acquired because its
// diagnostic could not be written. There is deliberately no fsync — the point
// is for another PROCESS to read this within seconds, which the page cache
// already provides, and crash durability is worthless for a file whose whole
// subject is a process that no longer exists.
func writeLockOwnerMetadata(lockPath string) {
	path := lockPath + fileLockOwnerSuffix
	if err := rejectUnsafeExistingRegularFile(path, "lock owner file"); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|fileLockOpenFlags(), 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "pid=%d\ncreated_at=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
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
