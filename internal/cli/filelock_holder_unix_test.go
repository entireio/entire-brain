//go:build !windows

package cli

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Opening a fifo for reading blocks until someone opens the write end, so a
// fifo where the lock file should be would hang the very error path whose job
// is to report that we have already waited long enough. It must be refused
// without being opened.
func TestLockHolderDescriptionNeverBlocksOnAFifo(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "write.lock")
	if err := syscall.Mkfifo(path+fileLockOwnerSuffix, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan string, 1)
	go func() { done <- describeFileLockHolder(path) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("a fifo must describe no holder, got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("describeFileLockHolder blocked on a fifo")
	}
}
