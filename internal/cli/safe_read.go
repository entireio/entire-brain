package cli

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"time"
)

// Size ceilings for reading untrusted inputs fully into memory. A file larger
// than its ceiling is overwhelmingly more likely to be corrupt or hostile than a
// legitimate input, so we fail loudly instead of letting a single read exhaust
// process memory. Callers needing a different bound pass it explicitly.

// maxDocumentTranscriptBytes bounds a single document-form session transcript
// slurped into memory. Long sessions are large but never approach this. A var
// so the oversized-fixture test can exercise the bound without a 256 MiB
// file.
var maxDocumentTranscriptBytes int64 = 256 << 20 // 256 MiB

const (
	// defaultMaxReadBytes is the fallback ceiling when a caller passes max <= 0.
	defaultMaxReadBytes = 64 << 20 // 64 MiB

	// maxSeedDocBytes bounds a single seed/markdown document read for chunking.
	maxSeedDocBytes = 32 << 20 // 32 MiB

	// maxManifestBytes bounds JSON manifest/index/cursor files, which are small.
	maxManifestBytes = 16 << 20 // 16 MiB

	// defaultMaxSemanticSnapshotBytes bounds a full entire-graph snapshot read. It is
	// generous because a large monorepo's snapshot is legitimately big; operators
	// with even larger repos can raise it via ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES.
	defaultMaxSemanticSnapshotBytes = 2 << 30 // 2 GiB
)

// semanticSnapshotMaxBytes returns the ceiling for reading a full semantic
// snapshot, honoring an ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES override so unusually
// large repositories are not artificially blocked.
func semanticSnapshotMaxBytes() int64 {
	if v := os.Getenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxSemanticSnapshotBytes
}

// safeReadOpenFile is a variable so adversarial tests can drive the
// lstat-to-open TOCTOU window deterministically, matching the existing
// memoryStateOpenFile and privacyOpen seams.
var safeReadOpenFile = os.OpenFile

// safeReadDeadline bounds a read on a descriptor that supports deadlines, so a
// pollable file that never yields EOF cannot park the caller forever. Regular
// files do not support deadlines (see safeOpenRegularFile) and are unaffected.
const safeReadDeadline = 30 * time.Second

// safeOpenRegularFile opens path for reading and refuses anything that is not a
// regular file.
//
// A size ceiling alone does not bound a read: open(2) on a FIFO with no writer
// blocks inside the syscall before a single byte is read, and a character device
// can yield bytes forever. Both are reachable wherever the path is caller
// supplied. So:
//
//   - os.Lstat, never os.Stat, so a symlink pointing at a FIFO cannot slip
//     through the type check the way it would through a resolving stat;
//   - O_NOFOLLOW (fileLockOpenFlags) so the open itself refuses a symlink
//     swapped in after the lstat;
//   - O_NONBLOCK on Unix (memoryStateReadOpenFlags) so even if a FIFO is swapped
//     in during that window the open returns instead of parking;
//   - a post-open SameFile/IsRegular recheck, which closes the remaining TOCTOU
//     window by rejecting a descriptor that is no longer the file we inspected.
//
// This is the same house pattern used for privacy artifacts and lock files
// (rejectOpenFileAlias); the hardlink clause of that helper is deliberately not
// applied here, because a legitimately hardlinked snapshot or document is not a
// denial-of-service vector and callers of safeReadFile only read.
func safeOpenRegularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	f, err := safeReadOpenFile(path, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	openedInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while opening; must be a regular file", path)
	}
	// Best effort: a regular file reports os.ErrNoDeadline and needs no deadline
	// (its reads complete or fail, they do not park in the netpoller). Anything
	// pollable that reached this point despite the checks above is bounded.
	if err := f.SetReadDeadline(time.Now().Add(safeReadDeadline)); err != nil && !errors.Is(err, os.ErrNoDeadline) {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// safeReadFile reads up to max bytes from path. If the file is larger than max it
// returns an error naming the path rather than allocating unbounded memory. A
// max <= 0 falls back to defaultMaxReadBytes. The path must be a regular file:
// see safeOpenRegularFile for why the size ceiling alone is not a bound.
func safeReadFile(path string, max int64) ([]byte, error) {
	f, err := safeOpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return safeReadAll(f, max, path)
}

// safeReadAll reads up to max bytes from r. If r yields more than max bytes it
// returns an error naming source, so an oversized untrusted stream fails loudly
// instead of being silently truncated or driving the process out of memory.
// readBoundExceededError marks the ONE failure that is genuinely about size,
// so a caller can tell it apart from an I/O error or a cancelled context that
// io.ReadAll surfaces through the same return. The message is unchanged from
// the plain error it replaces.
type readBoundExceededError struct {
	source string
	max    int64
}

func (e *readBoundExceededError) Error() string {
	return fmt.Sprintf("%s exceeds maximum size of %d bytes", e.source, e.max)
}

func safeReadAll(r io.Reader, max int64, source string) ([]byte, error) {
	if max <= 0 {
		max = defaultMaxReadBytes
	}
	// Read one byte past the limit to detect overflow, guarding against max+1
	// wrapping negative when max is near MaxInt64 (which would make LimitReader
	// read nothing and silently return empty).
	limit := max
	if max < math.MaxInt64 {
		limit = max + 1
	}
	data, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, &readBoundExceededError{source: source, max: max}
	}
	return data, nil
}
