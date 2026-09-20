//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHistoryAuditScanCacheRefusesFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, historyScanCachePath)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { loadHistoryScanCache(dir); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		// Unblock the old reader so a failed regression leaves no hanging goroutine.
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			_, _ = unix.Write(fd, []byte("invalid cache"))
			_ = unix.Close(fd)
		}
		<-done
		t.Fatal("cache read waited for a FIFO writer")
	}
}
