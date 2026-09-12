package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
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

// safeOpenRegularFile opens path for reading and refuses anything that is not a
// regular file.
//
// A size ceiling alone does not bound a read. open(2) on a FIFO with no writer
// blocks inside the syscall before a single byte is read, and a character device
// yields bytes forever, exhausting the ceiling and then reporting the wrong
// reason. Both are reachable wherever the path is caller supplied, and several
// safeReadFile call sites take an arbitrary path by design
// (brain_ingest_traces, loadEvalTasks).
//
// Two mechanisms, and only two:
//
//   - O_NONBLOCK on Unix (memoryStateReadOpenFlags) so opening a FIFO returns
//     immediately instead of parking the calling thread until a writer appears;
//   - fstat on the RESULTING DESCRIPTOR, which must be a regular file.
//
// Checking the descriptor rather than the path is what makes this airtight:
// there is no window between the check and the use, because the thing checked
// IS the thing opened. A path-based lstat would need a post-open recheck to
// close its own TOCTOU gap; this has no gap to close.
//
// It deliberately does NOT refuse symlinks. An earlier revision paired an lstat
// with O_NOFOLLOW, which rejected every symlink at all 13 call sites. That is
// collateral damage, not a control: this helper defends against a read that
// never ends, and a symlink to an ordinary file ends exactly like the ordinary
// file does. Symlinked files are unremarkable on a developer machine -- a
// checkout under a symlinked parent, a fixture linked into a test tree, macOS's
// own /tmp -> /private/tmp -- and refusing them broke user-supplied paths with
// an error that named a property the user had no reason to think mattered. A
// symlink cannot smuggle a FIFO through either, because the fstat above
// describes what was actually opened, not what the path looked like.
//
// Write-side symlink escapes are a different problem with a different control:
// see rejectOpenFileAlias for lock and privacy artifacts, which brain owns and
// writes, and where following a link really would be an escape. Callers of
// safeReadFile only read.
func safeOpenRegularFile(path string) (*os.File, error) {
	// O_NONBLOCK must be on the open itself; there is no way to add it later.
	f, err := safeReadOpenFile(path, os.O_RDONLY|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	// This is a file-type and allocation boundary, not a wall-clock deadline:
	// regular-file reads can still wait on the backing filesystem. Trace ingestion
	// therefore reads before acquiring the semantic index lock.
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
