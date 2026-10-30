//go:build !windows

package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestBrainManifestReaderRejectsFIFOSwap(t *testing.T) {
	dir := writeTestManifest(t, `{"schema_version":3}`)
	path := filepath.Join(dir, FileName)
	originalOpen := brainstore.MemoryStateOpenFile
	brainstore.MemoryStateOpenFile = func(name string, flags int, perm os.FileMode) (*os.File, error) {
		if name == path {
			if err := os.Rename(path, path+".original"); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return originalOpen(name, flags, perm)
	}
	t.Cleanup(func() { brainstore.MemoryStateOpenFile = originalOpen })
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("FIFO swap: %v", err)
	}
}
