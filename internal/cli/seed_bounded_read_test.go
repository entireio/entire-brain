package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// seedLargeDocBytes is comfortably larger than any legitimate seed input and far
// larger than the per-read bound, so an unbounded slurp shows up unmistakably in the
// allocation counter without making the test slow.
const seedLargeDocBytes = 64 << 20 // 64 MiB

// writeLargeDoc writes a file of n bytes of printable, NUL-free content and returns
// its sha256, so a test can assert the hash is unchanged by a bounded read.
func writeLargeDoc(t *testing.T, path string, n int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	hasher := sha256.New()
	chunk := []byte(strings.Repeat("entire-brain seed doc line\n", 512))
	for written := 0; written < n; {
		block := chunk
		if remaining := n - written; remaining < len(block) {
			block = block[:remaining]
		}
		if _, err := f.Write(block); err != nil {
			t.Fatalf("write: %v", err)
		}
		hasher.Write(block)
		written += len(block)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil))
}

// allocatedBy reports how many bytes fn allocated in total. TotalAlloc is cumulative
// and never decreases, so it measures the peak demand a slurp places on the
// allocator even when the garbage collector reclaims it immediately after.
func allocatedBy(t *testing.T, fn func()) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestInspectSeedFileReadIsBounded pins the memory cost of scanning one seed file.
//
// A high-signal doc larger than --max-file-bytes is deliberately kept (marked
// truncated) rather than skipped, and only its first max-file-bytes are ever copied
// into the seed. Reading the whole thing into memory first is therefore pure waste,
// and a repository with one very large README turns an ordinary `entire brain
// refresh` into an allocation of the file's full size.
func TestInspectSeedFileReadIsBounded(t *testing.T) {
	repoDir := t.TempDir()
	wantHash := writeLargeDoc(t, filepath.Join(repoDir, "README.md"), seedLargeDocBytes)
	opts := seedCommandOptions{maxFileBytes: defaultSeedMaxFileBytes, includeTests: true}

	var entry seedFileIndexEntry
	used := allocatedBy(t, func() { entry = inspectSeedFile(repoDir, "README.md", opts) })

	if !entry.Included {
		t.Fatalf("large high-signal doc was dropped: reason=%q", entry.Reason)
	}
	if !entry.Truncated {
		t.Error("large doc should be marked truncated")
	}
	// Behaviour must not change: the recorded hash still covers the whole file.
	if entry.Hash != wantHash {
		t.Errorf("hash changed: got %q want %q", entry.Hash, wantHash)
	}
	if bound := uint64(8 << 20); used > bound {
		t.Errorf("inspectSeedFile allocated %d bytes for a %d byte file; want < %d (the read is unbounded)", used, seedLargeDocBytes, bound)
	}
}

// TestWriteSeedDocsReadIsBounded pins the copy path. It truncates to
// defaultSeedMaxFileBytes, so it never needs more than that plus a byte in memory.
func TestWriteSeedDocsReadIsBounded(t *testing.T) {
	repoDir := t.TempDir()
	outputDir := t.TempDir()
	writeLargeDoc(t, filepath.Join(repoDir, "README.md"), seedLargeDocBytes)

	scan := seedScanResult{
		RepoDir: repoDir,
		Docs: []seedDocument{{
			Path:      "README.md",
			SeedPath:  filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, "README.md")),
			Bytes:     seedLargeDocBytes,
			Truncated: true,
		}},
	}

	var err error
	used := allocatedBy(t, func() { err = writeSeedDocs(outputDir, scan) })
	if err != nil {
		t.Fatalf("writeSeedDocs: %v", err)
	}

	written, readErr := os.ReadFile(filepath.Join(outputDir, seedDirName, seedDocsDirName, "README.md"))
	if readErr != nil {
		t.Fatalf("read copied doc: %v", readErr)
	}
	if len(written) <= defaultSeedMaxFileBytes {
		t.Errorf("copied doc is %d bytes; expected the truncation marker to follow %d bytes", len(written), defaultSeedMaxFileBytes)
	}
	if !strings.HasSuffix(string(written), "[truncated]\n") {
		t.Error("copied doc is missing the truncation marker")
	}
	if bound := uint64(8 << 20); used > bound {
		t.Errorf("writeSeedDocs allocated %d bytes for a %d byte doc; want < %d (the read is unbounded)", used, seedLargeDocBytes, bound)
	}
}

// TestSeedManifestReadsAreBounded pins the two manifest parsers, which slurped their
// whole file with no ceiling at all before parsing it as JSON/TOML.
func TestSeedManifestReadsAreBounded(t *testing.T) {
	repoDir := t.TempDir()
	pkg := filepath.Join(repoDir, "package.json")
	writeLargeDoc(t, pkg, seedLargeDocBytes)
	mise := filepath.Join(repoDir, "mise.toml")
	writeLargeDoc(t, mise, seedLargeDocBytes)

	// The read ceiling for a manifest is maxManifestBytes (16 MiB), and io.ReadAll
	// grows by doubling, so a bounded read costs at most a small multiple of the
	// ceiling. An unbounded os.ReadFile preallocates the stat size exactly, so it
	// cannot come in under the file's own 64 MiB. That gap is what this asserts.
	bound := uint64(maxManifestBytes) * 5 / 2

	var pkgErr error
	usedPkg := allocatedBy(t, func() { _, pkgErr = packageJSONCommands(pkg, "package.json") })
	if pkgErr == nil {
		t.Error("packageJSONCommands accepted a 64 MiB manifest")
	}
	if usedPkg > bound {
		t.Errorf("packageJSONCommands allocated %d bytes for a %d byte manifest; want < %d (the read is unbounded)", usedPkg, seedLargeDocBytes, bound)
	}

	var miseErr error
	usedMise := allocatedBy(t, func() { _, miseErr = miseCommands(mise, "mise.toml") })
	if miseErr == nil {
		t.Error("miseCommands accepted a 64 MiB manifest")
	}
	if usedMise > bound {
		t.Errorf("miseCommands allocated %d bytes for a %d byte manifest; want < %d (the read is unbounded)", usedMise, seedLargeDocBytes, bound)
	}
}
