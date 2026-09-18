//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestSafeReadFileFollowsASymlinkToARegularFile.
//
// safeOpenRegularFile exists to stop a read PARKING or RUNNING FOREVER: open(2)
// on a FIFO with no writer blocks inside the syscall, and a character device
// yields bytes without end. A symlink pointing at an ordinary file does neither.
// Refusing every symlink was collateral damage, and it lands on all 13
// safeReadFile call sites at once -- including the ones whose paths the user
// supplies precisely because they are somewhere else on disk
// (brain_ingest_traces, loadEvalTasks).
//
// A symlinked file is completely ordinary on a developer machine: a checkout
// reached through a symlinked parent, a dotfile linked out of a dotfiles repo,
// a fixture linked into a test tree, anything under a symlinked /home or the
// macOS /tmp -> /private/tmp link. Every one of those became an unexplained
// "must not be a symlink".
func TestSafeReadFileFollowsASymlinkToARegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	want := []byte(`{"ok":true}`)
	if err := os.WriteFile(target, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	got, err := safeReadFile(link, maxManifestBytes)
	if err != nil {
		t.Fatalf("safeReadFile through a symlink to a regular file: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("safeReadFile = %q, want %q", got, want)
	}
}

// TestSafeReadFileRefusesASymlinkToAFIFO: following symlinks does not mean
// following them anywhere. What matters is what the descriptor turns out to be,
// and that is decided by fstat on the descriptor itself -- so a symlink cannot
// be used as a wrapper to smuggle a FIFO past the type check.
func TestSafeReadFileRefusesASymlinkToAFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	link := filepath.Join(dir, "link")
	mkfifoForTest(t, fifo)
	if err := os.Symlink(fifo, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := safeReadFile(link, maxManifestBytes)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink-to-FIFO error = %v, want it to name the regular-file requirement", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("safeReadFile parked on a symlink to a FIFO: O_NONBLOCK is not reaching the open")
	}
}

// TestSafeReadFileRefusesADirectory keeps the other half of the type check
// honest: a directory opens fine and then fails on read, which is a confusing
// error a long way from the cause.
func TestSafeReadFileRefusesADirectory(t *testing.T) {
	if _, err := safeReadFile(t.TempDir(), maxManifestBytes); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v, want it to name the regular-file requirement", err)
	}
}

// TestIngestTracesAcceptsASymlinkedTraceFile is the user-visible consequence at
// the call site the review named: brain_ingest_traces takes an arbitrary path,
// so a symlinked trace file is a normal way to pass one.
func TestIngestTracesAcceptsASymlinkedTraceFile(t *testing.T) {
	opts, _ := indexedTraceIngestFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "traces.ndjson")
	link := filepath.Join(dir, "link.ndjson")
	if err := os.WriteFile(target, []byte(`{"from":"caller","to":"auth.ValidateToken","type":"CALLS"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if err := runSemanticIngestTraces(&cobra.Command{Use: "ingest"}, opts, semanticTraceIngestOptions{json: true}, link); err != nil {
		t.Fatalf("ingest a symlinked trace file: %v", err)
	}
}

// TestSafeReadFileRefusesACharacterDevice: /dev/zero opens instantly and yields
// bytes forever, so only the type check stops it. It would exhaust the size
// ceiling first and report the wrong reason.
func TestSafeReadFileRefusesACharacterDevice(t *testing.T) {
	info, err := os.Stat(os.DevNull)
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Skip("no character device available")
	}
	if _, err := safeReadFile(os.DevNull, maxManifestBytes); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("character device error = %v, want it to name the regular-file requirement", err)
	}
}
