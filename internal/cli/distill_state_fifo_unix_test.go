//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDistillStateReadersRejectFIFO(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, filepath.FromSlash(distillCachePath))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		loadDistillCache(d)
		done <- nil
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		// Release the old blocking reader so a regression cannot hang the suite.
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			_, _ = unix.Write(fd, []byte("{}"))
			_ = unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("reader did not unblock")
		}
		t.Error("state reader blocked on FIFO")
	}
}
