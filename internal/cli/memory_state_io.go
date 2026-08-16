package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// memoryStateInventoryMaxEntries bounds every operational directory scan. It
// is a variable so adversarial tests can exercise truncation without creating
// thousands of files.
var memoryStateInventoryMaxEntries = 10_000

var (
	memoryStateLstat    = os.Lstat
	memoryStateOpenFile = os.OpenFile
)

// readMemoryStateFile is the single reader for untrusted work-record and abstract state. It binds
// the opened descriptor to both lstat observations, rejects aliases and
// non-regular files, and reads at most maxBytes. memoryStateReadOpenFlags adds
// O_NONBLOCK on Unix so a regular-file-to-FIFO swap cannot hang the worker.
func readMemoryStateFile(brainDir, rel, label string, maxBytes int64) ([]byte, bool, error) {
	return readMemoryStateFileExpected(brainDir, rel, label, maxBytes, nil)
}

// errMemoryStateEnumerationStale marks the one failure an UNLOCKED scanner may
// legitimately see: the file's identity changed between directory enumeration
// and open. For a reader holding the Brain write lock that is a genuine
// tampering signal. For an unlocked reader it is also the ordinary outcome of a
// lock-holding writer's writeFileAtomic rename landing mid-scan.
var errMemoryStateEnumerationStale = errors.New("memory state entry changed after directory enumeration")

// readMemoryStateFileRefreshed reads a just-enumerated file, tolerating exactly
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
func readMemoryStateFileRefreshed(brainDir, rel, label string, maxBytes int64, expected os.FileInfo) ([]byte, bool, error) {
	data, present, err := readMemoryStateFileExpected(brainDir, rel, label, maxBytes, expected)
	if err != nil && errors.Is(err, errMemoryStateEnumerationStale) {
		return readMemoryStateFileExpected(brainDir, rel, label, maxBytes, nil)
	}
	return data, present, err
}

func readMemoryStateFileExpected(brainDir, rel, label string, maxBytes int64, expected os.FileInfo) ([]byte, bool, error) {
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return nil, false, fmt.Errorf("%s: %s has an unsafe path %q: %w", memoryErrStateUnsafe, label, rel, err)
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	path := filepath.Join(brainDir, clean)
	before, err := memoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%s: %s is not a regular file: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	if expected != nil && !os.SameFile(expected, before) {
		return nil, true, fmt.Errorf("%s: %s changed after directory enumeration: %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), errMemoryStateEnumerationStale)
	}
	f, err := memoryStateOpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, true, fmt.Errorf("%s: open %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, true, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	after, err := memoryStateLstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return nil, true, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	if err := rejectOpenFileAlias(path, f, label); err != nil {
		return nil, true, fmt.Errorf("%s: validate opened %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	data, err := safeReadAll(f, maxBytes, path)
	if err != nil {
		return nil, true, fmt.Errorf("%s: read %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	return data, true, nil
}

type memoryStateDirectory struct {
	Entries   []os.DirEntry
	Present   bool
	Total     int
	Truncated bool
	Degraded  bool
}

// readMemoryStateDirectory uses the directory descriptor for enumeration and
// reads ceiling+1 entries so callers know when completeness was lost. It never
// silently treats a truncated inventory as complete.
func readMemoryStateDirectory(brainDir, rel, label string, ceiling int) (memoryStateDirectory, error) {
	if ceiling < 1 {
		ceiling = memoryStateInventoryMaxEntries
	}
	clean, err := cleanBrainRelativePath(filepath.ToSlash(strings.TrimSpace(rel)))
	if err != nil {
		return memoryStateDirectory{Degraded: true}, fmt.Errorf("%s: %s has an unsafe path %q: %w", memoryErrStateUnsafe, label, rel, err)
	}
	if err := rejectExistingSymlinkPathComponents(brainDir, clean); err != nil {
		return memoryStateDirectory{Degraded: true}, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	path := filepath.Join(brainDir, clean)
	before, err := memoryStateLstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return memoryStateDirectory{}, nil
		}
		return memoryStateDirectory{Degraded: true}, fmt.Errorf("%s: inspect %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return memoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: %s is not a directory: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	dir, err := memoryStateOpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return memoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: open %s %s: %w", memoryErrStateUnsafe, label, filepath.ToSlash(clean), err)
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		return memoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	after, err := memoryStateLstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return memoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: %s changed or became unsafe while opening: %s", memoryErrStateUnsafe, label, filepath.ToSlash(clean))
	}
	entries, err := dir.ReadDir(ceiling + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return memoryStateDirectory{Present: true, Degraded: true}, fmt.Errorf("%s: read %s %s: %w", memoryErrStateCorrupt, label, filepath.ToSlash(clean), err)
	}
	result := memoryStateDirectory{Entries: entries, Present: true, Total: len(entries)}
	if len(entries) > ceiling {
		result.Entries = entries[:ceiling]
		result.Truncated = true
		result.Degraded = true
	}
	return result, nil
}
