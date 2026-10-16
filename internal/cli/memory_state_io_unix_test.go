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
	if _, _, err := readMemoryStateFile(brainDir, fifoRel, "hostile state", maxManifestBytes); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("FIFO read = %v", err)
	}
	socketRel := filepath.ToSlash(filepath.Join(memoryWorkDirRel, "hostile.sock"))
	socket := filepath.Join(brainDir, filepath.FromSlash(socketRel))
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer listener.Close()
	if _, _, err := readMemoryStateFile(brainDir, socketRel, "hostile state", maxManifestBytes); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
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
	originalOpen := memoryStateOpenFile
	var once sync.Once
	memoryStateOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if name == path {
			once.Do(func() {
				_ = os.Rename(path, path+".original")
				_ = syscall.Mkfifo(path, 0o600)
			})
		}
		return originalOpen(name, flag, perm)
	}
	t.Cleanup(func() { memoryStateOpenFile = originalOpen })
	if _, _, err := readMemoryStateFile(brainDir, rel, "swapped state", maxManifestBytes); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("swapped FIFO read = %v", err)
	}
}

func TestHistoryProjectionOverlayIdentityRejectsAliasesAndFIFOSwap(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		brainDir := t.TempDir()
		path := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := historyProjectionFileIdentity(brainDir, historyShortTermPath, maxManifestBytes); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
			t.Fatalf("overlay alias identity error = %v", err)
		}
	})

	t.Run("regular to fifo", func(t *testing.T) {
		brainDir := t.TempDir()
		path := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		originalOpen := memoryStateOpenFile
		var once sync.Once
		memoryStateOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
			if name == path {
				once.Do(func() {
					_ = os.Rename(path, path+".original")
					_ = syscall.Mkfifo(path, 0o600)
				})
			}
			return originalOpen(name, flag, perm)
		}
		t.Cleanup(func() { memoryStateOpenFile = originalOpen })
		if _, err := historyProjectionFileIdentity(brainDir, historyShortTermPath, maxManifestBytes); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
			t.Fatalf("overlay swap identity error = %v", err)
		}
	})
}
