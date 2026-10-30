package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A long-running daemon outlives the binary it was started from; the probe is
// what lets it notice and hand itself back to the service manager (COR-2174).
func TestWatchBinaryChangeProbeDetectsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entire-brain")
	if err := os.WriteFile(path, []byte("build A"), 0o755); err != nil {
		t.Fatal(err)
	}
	probe := watchBinaryChangeProbe(path)
	if probe == nil {
		t.Fatal("probe must exist for a readable binary")
	}
	if probe() {
		t.Fatal("unchanged binary reported as replaced")
	}

	// In-place rewrite (same inode, new mtime): the nightly `go build` case.
	if err := os.WriteFile(path, []byte("build B"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !probe() {
		t.Fatal("rewritten binary not reported as replaced")
	}

	// Atomic replacement (new inode): the install-by-rename case.
	fresh := watchBinaryChangeProbe(path)
	replacement := filepath.Join(t.TempDir(), "next")
	if err := os.WriteFile(replacement, []byte("build C!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if !fresh() {
		t.Fatal("renamed-over binary not reported as replaced")
	}

	// A vanished binary (mid-replace) must not trigger a restart onto nothing.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if fresh() {
		t.Fatal("a missing binary must defer the decision, not restart")
	}

	if watchBinaryChangeProbe(filepath.Join(t.TempDir(), "absent")) != nil {
		t.Fatal("an unreadable binary must disable the probe")
	}
}
