package brainstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

type backgroundWriteKey struct{}

// BackgroundWrites marks unattended work. Only acquisition waits are cancellable:
// once acquired, the caller finishes its atomic mutation before releasing the lock.
func BackgroundWrites(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, backgroundWriteKey{}, true)
}

// AcquireBrainWriteLockContext gives foreground waiters priority over unattended
// work. Background work waits until cancelled, without spending its job retries
// on ordinary writer contention. Foreground waits are bounded for a stalled
// owner, but allow an already-running migration to finish atomically.
func AcquireBrainWriteLockContext(ctx context.Context, brainDir string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	background, _ := ctx.Value(backgroundWriteKey{}).(bool)
	timeout := 2 * time.Minute
	if background {
		timeout = -1
	}
	return acquirePriorityBrainWriteLock(ctx, brainDir, timeout, background)
}

func WithBrainWriteLockContext(ctx context.Context, brainDir string, fn func() error) error {
	unlock, err := AcquireBrainWriteLockContext(ctx, brainDir)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// WithBrainWriteLockSettlement finishes already-started work after cancellation.
// Normal background settlement retains its unbounded cancellable wait. Only
// shutdown gets a separate deadline. Retry acquisition, never the mutation:
// a callback may have committed before returning an error.
func WithBrainWriteLockSettlement(ctx context.Context, brainDir string, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	unlock, err := AcquireBrainWriteLockContext(ctx, brainDir)
	if err != nil && ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		unlock, err = AcquireBrainWriteLockContext(cleanupCtx, brainDir)
	}
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// An admission lock serializes intent registration and nonblocking write-lock
// attempts. Each foreground waiter owns a separate OS-locked intent file. The OS
// releases that lease on exit; stale files never confer priority. This supports
// multiple waiters, process crashes, and goroutines without PID/age heuristics.
// The original write.lock still excludes older binaries. All lock ordering is
// admission -> write, and admission is never held while waiting for write.
func acquirePriorityBrainWriteLock(ctx context.Context, brainDir string, timeout time.Duration, background bool) (func(), error) {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, BrainLockDirName); err != nil {
		return nil, err
	}
	dir := filepath.Join(brainDir, BrainLockDirName)
	gatePath := filepath.Join(dir, "write-admission.lock")
	deadline := time.Now().Add(timeout)
	var intent *FileLock
	defer func() {
		if intent != nil {
			path := intent.path
			_ = intent.Close()
			_ = os.Remove(path)
			_ = os.Remove(path + FileLockOwnerSuffix)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gate, err := AcquireFileLock(gatePath, "brain_locked", 0)
		if err == nil {
			var writer *FileLock
			writer, err = func() (*FileLock, error) {
				defer gate.Close()
				if !background && intent == nil {
					f, err := os.CreateTemp(dir, "write-foreground-*.lock")
					if err != nil {
						return nil, err
					}
					path := f.Name()
					if err := f.Close(); err != nil {
						_ = os.Remove(path)
						return nil, err
					}
					intent, err = AcquireFileLock(path, "brain_locked", 0)
					if err != nil {
						_ = os.Remove(path)
						return nil, err
					}
				}
				if background {
					busy, err := foregroundWriterWaiting(dir)
					if err != nil {
						return nil, err
					}
					if busy {
						return nil, ErrFileLockTimeout
					}
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return AcquireFileLock(BrainWriteLockPath(brainDir), "brain_locked", 0)
			}()
			if err == nil {
				return func() { _ = writer.Close() }, nil
			}
		}
		if !errors.Is(err, ErrFileLockTimeout) {
			return nil, err
		}
		if timeout == 0 || (timeout > 0 && !time.Now().Before(deadline)) {
			return nil, fileLockTimeoutError("brain_locked", BrainWriteLockPath(brainDir))
		}
		timer := time.NewTimer(FileLockRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Called only under admission. Unknown/unsafe matching entries fail closed;
// only an unlocked regular intent file can be discarded as a stale waiter.
func foregroundWriterWaiting(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "write-foreground-") || !strings.HasSuffix(name, ".lock") {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := entry.Info()
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("invalid foreground writer intent: %s", path)
		}
		lease, err := AcquireFileLock(path, "brain_locked", 0)
		if errors.Is(err, ErrFileLockTimeout) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		_ = lease.Close()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return false, err
		}
		_ = os.Remove(path + FileLockOwnerSuffix)
	}
	return false, nil
}
