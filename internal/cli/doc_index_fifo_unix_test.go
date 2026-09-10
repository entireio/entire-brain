//go:build !windows

package cli

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeclaredDocIndexRejectsFIFOWithoutWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.FromSlash(docIndexPath))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- verifyDeclaredDocIndex(dir) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as a regular index")
		}
	case <-time.After(time.Second):
		// Release a regressed blocking open so the test leaves no goroutine behind.
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if err == nil {
			defer unix.Close(fd)
		}
		t.Fatal("index probe blocked waiting for a FIFO writer")
	}
}
