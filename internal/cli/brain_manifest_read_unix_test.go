//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestBrainManifestReaderRejectsFIFOSwap(t *testing.T) {
	dir := writeTestManifest(t, `{"schema_version":3}`)
	path := filepath.Join(dir, exportManifestFileName)
	originalOpen := memoryStateOpenFile
	memoryStateOpenFile = func(name string, flags int, perm os.FileMode) (*os.File, error) {
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
	t.Cleanup(func() { memoryStateOpenFile = originalOpen })
	if _, err := loadBrainManifest(dir); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("FIFO swap: %v", err)
	}
}
