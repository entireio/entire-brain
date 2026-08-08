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

// safeReadFile reads up to max bytes from path. If the file is larger than max it
// returns an error naming the path rather than allocating unbounded memory. A
// max <= 0 falls back to defaultMaxReadBytes.
func safeReadFile(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return safeReadAll(f, max, path)
}

// safeReadAll reads up to max bytes from r. If r yields more than max bytes it
// returns an error naming source, so an oversized untrusted stream fails loudly
// instead of being silently truncated or driving the process out of memory.
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
		return nil, fmt.Errorf("%s exceeds maximum size of %d bytes", source, max)
	}
	return data, nil
}
