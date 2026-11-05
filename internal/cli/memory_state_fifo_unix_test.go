//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// Restored from the former session_privacy_fifo_unix_test.go: the migration
// progress loader's FIFO rejection is independent of the removed layer.
func TestMemoryMigrationProgressRejectsFIFOBeforeOpen(t *testing.T) {
	brainDir := t.TempDir()
	path := filepath.Join(brainDir, filepath.FromSlash(memoryMigrationProgressRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := loadMemoryMigrationProgress(brainDir); !present || err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) {
		t.Fatalf("FIFO migration progress present=%v err=%v", present, err)
	}
}
