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

func TestDeclaredDocIndexReadRejectsFIFOWithoutWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.FromSlash(docIndexPath))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := loadDocIndex(dir); done <- err }()
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

func TestDeclaredDocIndexRejectsAliases(t *testing.T) {
	for _, link := range []struct {
		name   string
		create func(string, string) error
	}{
		{"symlink", os.Symlink}, {"hardlink", os.Link},
	} {
		t.Run(link.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target.json")
			if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, filepath.FromSlash(docIndexPath))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := link.create(target, path); err != nil {
				t.Fatal(err)
			}
			if err := verifyDeclaredDocIndex(dir); err == nil {
				t.Fatal("aliased index accepted")
			}
			if _, err := loadDocIndex(dir); err == nil {
				t.Fatal("aliased index read accepted")
			}
		})
	}
}
