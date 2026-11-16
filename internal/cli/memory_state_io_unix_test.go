//go:build !windows

package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestMemoryStateReaderRejectsFIFOAndSocketWithoutBlocking(t *testing.T) {
	brainDir := t.TempDir()
	dir := filepath.Join(brainDir, filepath.FromSlash(memoryWorkDirRel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fifoRel := filepath.ToSlash(filepath.Join(memoryWorkDirRel, "hostile.json"))
	fifo := filepath.Join(brainDir, filepath.FromSlash(fifoRel))
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, _, err := brainstore.ReadMemoryStateFile(brainDir, fifoRel, "hostile state", base.MaxManifestBytes); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("FIFO read = %v", err)
	}
	socketRel := filepath.ToSlash(filepath.Join(memoryWorkDirRel, "hostile.sock"))
	socket := filepath.Join(brainDir, filepath.FromSlash(socketRel))
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer listener.Close()
	if _, _, err := brainstore.ReadMemoryStateFile(brainDir, socketRel, "hostile state", base.MaxManifestBytes); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("socket read = %v", err)
	}
}

func TestMemoryStateReaderRejectsRegularToFIFOSwap(t *testing.T) {
	brainDir := t.TempDir()
	rel := filepath.ToSlash(filepath.Join(memoryWorkDirRel, "swapped.json"))
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	originalOpen := brainstore.MemoryStateOpenFile
	var once sync.Once
	brainstore.MemoryStateOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == path {
			once.Do(func() {
				_ = os.Rename(path, path+".original")
				_ = syscall.Mkfifo(path, 0o600)
			})
		}
		return originalOpen(name, flag, perm)
	}
	t.Cleanup(func() { brainstore.MemoryStateOpenFile = originalOpen })
	if _, _, err := brainstore.ReadMemoryStateFile(brainDir, rel, "swapped state", base.MaxManifestBytes); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("swapped FIFO read = %v", err)
	}
}
