package brainstore

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

const (
	BrainLockDirName      = "locks"
	BrainWriteLockName    = "write.lock"
	brainManifestLockName = "manifest.lock"
	BrainDistillLockName  = "distill-pass.lock"
	BrainWriteLockTimeout = 10 * time.Second
)

func BrainWriteLockPath(brainDir string) string {
	return filepath.Join(brainDir, BrainLockDirName, BrainWriteLockName)
}

func AcquireBrainWriteLock(brainDir string) (func(), error) {
	return AcquireBrainWriteLockTimeout(brainDir, BrainWriteLockTimeout)
}

func AcquireBrainWriteLockTimeout(brainDir string, timeout time.Duration) (func(), error) {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, BrainLockDirName); err != nil {
		return nil, err
	}
	lock, err := AcquireFileLock(BrainWriteLockPath(brainDir), "brain_locked", timeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

func WithBrainWriteLock(brainDir string, fn func() error) error {
	unlock, err := AcquireBrainWriteLock(brainDir)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// WithDistillPassLock is the cross-process guard against two distill passes
// spending agent tokens on the same sessions at once. `setup` creates exactly
// that race: it spawns the detached backfill AND installs a watcher whose first
// tick distills immediately. The brain write lock does not help — it only wraps
// the flush at the END of a pass, so both processes sail through the expensive
// agent calls first and only then serialize on writing the identical facts.
//
// Two properties matter, and both differ from the other brain locks:
//
//   - it is held for the WHOLE pass, not just the write, because the token spend
//     is the resource being protected;
//   - it is TRY-only. A second comer must skip cleanly, not queue behind an
//     hours-long backfill and then re-spend on the sessions that backfill just
//     finished. onBusy reports the skip; the pass returns nil, because "someone
//     else is already doing this" is a success for the caller.
func WithDistillPassLock(brainDir string, onBusy func(), fn func() error) error {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, BrainLockDirName); err != nil {
		return err
	}
	// timeout 0 = try once; AcquireFileLock reports contention as a timeout.
	lock, err := AcquireFileLock(filepath.Join(brainDir, BrainLockDirName, BrainDistillLockName), "distill_pass_locked", 0)
	if err != nil {
		if errors.Is(err, ErrFileLockTimeout) {
			if onBusy != nil {
				onBusy()
			}
			return nil
		}
		return err
	}
	defer func() { _ = lock.Close() }()
	return fn()
}

func WithBrainManifestWriteLock(brainDir string, fn func() error) error {
	if err := base.RejectSymlinkedBrainRoot(brainDir); err != nil {
		return err
	}
	if err := base.RejectExistingSymlinkPathComponents(brainDir, BrainLockDirName); err != nil {
		return err
	}
	lock, err := AcquireFileLock(filepath.Join(brainDir, BrainLockDirName, brainManifestLockName), "brain_manifest_locked", BrainWriteLockTimeout)
	if err != nil {
		return err
	}
	defer lock.Close()
	return fn()
}
