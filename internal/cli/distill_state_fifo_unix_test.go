//go:build !windows

package cli

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDistillStateReadersRejectFIFO(t *testing.T) {
	for _, name := range []string{"cache", "proposals"} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			rel := distillCachePath
			if name == "proposals" {
				rel = factsProposalsRelPath("main")
			}
			path := filepath.Join(d, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if name == "cache" {
					loadDistillCache(d)
					done <- nil
				} else {
					_, err := loadFactProposals(d, "main")
					done <- err
				}
			}()
			select {
			case err := <-done:
				if name == "proposals" && err == nil {
					t.Error("FIFO proposal queue accepted")
				}
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
		})
	}
}
