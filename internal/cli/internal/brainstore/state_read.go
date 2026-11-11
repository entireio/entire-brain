package brainstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
)

// MemoryStateInventoryMaxEntries bounds every operational directory scan. It
// is a variable so adversarial tests can exercise truncation without creating
// thousands of files.
var MemoryStateInventoryMaxEntries = 10_000

var (
	MemoryStateLstat    = os.Lstat
	MemoryStateOpenFile = os.OpenFile
)

// ReadMemoryStateFile is the single reader for untrusted work-record state. It binds
// the opened descriptor to both lstat observations, rejects aliases and
// non-regular files, and reads at most maxBytes. memoryStateReadOpenFlags adds
// O_NONBLOCK on Unix so a regular-file-to-FIFO swap cannot hang the worker.
func ReadMemoryStateFile(brainDir, rel, label string, maxBytes int64) ([]byte, bool, error) {
	return ReadMemoryStateFileExpected(brainDir, rel, label, maxBytes, nil)
}

// ErrMemoryStateEnumerationStale marks the one failure an UNLOCKED scanner may
// legitimately see: the file's identity changed between directory enumeration
// and open. For a reader holding the Brain write lock that is a genuine
// tampering signal. For an unlocked reader it is also the ordinary outcome of a
// lock-holding writer's writeFileAtomic rename landing mid-scan.
var ErrMemoryStateEnumerationStale = errors.New("memory state entry changed after directory enumeration")

// ReadMemoryStateFileRefreshed reads a just-enumerated file, tolerating exactly
// ONE benign identity change by re-reading against the current identity.
//
// The unlocked job/hint scanners (memory jobs, the job selectors, the worker
// relaunch delay) race the worker's own transitionMemoryJob -> writeFileAtomic
// rename. Treating that fresh inode as corruption reported memory_state_unsafe
// for a perfectly healthy file, which aborted whole --session-ref commands and
// silently dropped runnable jobs from relaunch scheduling. The retry drops only
// the enumeration binding: every path-safety, regular-file, descriptor-identity,
// and open-alias check still applies, which is the strongest promise an unlocked
// reader can honestly make. A second mismatch propagates.
func ReadMemoryStateFileRefreshed(brainDir, rel, label string, maxBytes int64, expected os.FileInfo) ([]byte, bool, error) {
	data, present, err := ReadMemoryStateFileExpected(brainDir, rel, label, maxBytes, expected)
	if err != nil && errors.Is(err, ErrMemoryStateEnumerationStale) {
		return ReadMemoryStateFileExpected(brainDir, rel, label, maxBytes, nil)
	}
	return data, present, err
}

func ReadMemoryStateFileExpected(brainDir, rel, label string, maxBytes int64, expected os.FileInfo) ([]byte, bool, error) {
	f, present, err := OpenMemoryStateFileExpected(brainDir, rel, label, expected)
	if err != nil || !present {
		return nil, present, err
	}
	defer f.Close()
	data, err := clibase.SafeReadAll(f, maxBytes, filepath.Join(brainDir, rel))
	if err != nil {
		return nil, true, fmt.Errorf("%s: read %s %s: %w", StateCorruptCode, label, filepath.ToSlash(rel), err)
	}
	return data, true, nil
}

// OpenMemoryStateFileExpected shares the identity and alias protections with
// readers that decode directly from the descriptor. The caller closes the file.
func OpenMemoryStateFileExpected(brainDir, rel, label string, expected os.FileInfo) (*os.File, bool, error) {
	clean, err := clibase.CleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %s has an unsafe path %q: %w", StateUnsafeCode, label, rel, err)
	}
	if err := clibase.RejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", StateUnsafeCode, label, filepath.ToSlash(clean), err)
	}
	path := filepath.Join(brainDir, clean)
	before, err := MemoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", StateCorruptCode, label, filepath.ToSlash(clean), err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%s: %s is not a regular file: %s", StateUnsafeCode, label, filepath.ToSlash(clean))
	}
	if expected != nil && !os.SameFile(expected, before) {
		return nil, true, fmt.Errorf("%s: %s changed after directory enumeration: %s: %w", StateUnsafeCode, label, filepath.ToSlash(clean), ErrMemoryStateEnumerationStale)
	}
	f, err := MemoryStateOpenFile(path, os.O_RDONLY|clibase.FileLockOpenFlags()|clibase.MemoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, true, fmt.Errorf("%s: open %s %s: %w", StateUnsafeCode, label, filepath.ToSlash(clean), err)
	}
	success := false
	defer func() {
		if !success {
			_ = f.Close()
		}
	}()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, true, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", StateUnsafeCode, label, filepath.ToSlash(clean))
	}
	after, err := MemoryStateLstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return nil, true, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", StateUnsafeCode, label, filepath.ToSlash(clean))
	}
	if err := clibase.RejectOpenFileAlias(path, f, label); err != nil {
		return nil, true, fmt.Errorf("%s: validate opened %s %s: %w", StateUnsafeCode, label, filepath.ToSlash(clean), err)
	}
	success = true
	return f, true, nil
}

type MemoryStateDirectory struct {
	Entries   []os.DirEntry
	Present   bool
	Total     int
	Truncated bool
	Degraded  bool
}

// ReadMemoryStateDirectory uses the directory descriptor for enumeration and
// reads ceiling+1 entries so callers know when completeness was lost. It never
// silently treats a truncated inventory as complete.
func ReadMemoryStateDirectory(brainDir, rel, label string, ceiling int) (MemoryStateDirectory, error) {
	if ceiling < 1 {
		ceiling = MemoryStateInventoryMaxEntries
	}
	clean, err := clibase.CleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return MemoryStateDirectory{Degraded: true}, fmt.Errorf("%s: %s has an unsafe path %q: %w", StateUnsafeCode, label, rel, err)
	}
	if err := clibase.RejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return MemoryStateDirectory{Degraded: true}, fmt.Errorf("%s: inspect %s %s: %w", StateUnsafeCode, label, filepath.ToSlash(clean), err)
	}
	path := filepath.Join(brainDir, clean)
	before, err := MemoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return MemoryStateDirectory{}, nil
		}
		return MemoryStateDirectory{Degraded: true}, fmt.Errorf("%s: inspect %s %s: %w", StateCorruptCode, label, filepath.ToSlash(clean), err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return MemoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: %s is not a directory: %s", StateUnsafeCode, label, filepath.ToSlash(clean))
	}
	dir, err := MemoryStateOpenFile(path, os.O_RDONLY|clibase.FileLockOpenFlags()|clibase.MemoryStateReadOpenFlags(), 0)
	if err != nil {
		return MemoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: open %s %s: %w", StateUnsafeCode, label, filepath.ToSlash(clean), err)
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		return MemoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", StateUnsafeCode, label, filepath.ToSlash(clean))
	}
	after, err := MemoryStateLstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return MemoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", StateUnsafeCode, label, filepath.ToSlash(clean))
	}
	entries, err := dir.ReadDir(ceiling + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return MemoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: read %s %s: %w", StateCorruptCode, label, filepath.ToSlash(clean), err)
	}
	result := MemoryStateDirectory{Entries: entries, Present: true, Total: len(entries)}
	if len(entries) > ceiling {
		result.Entries = entries[:ceiling]
		result.Truncated = true
		result.Degraded = true
	}
	return result, nil
}
