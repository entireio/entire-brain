//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `docs extract` takes a path straight from the caller. os.Stat follows
// symlinks and says nothing about FIFOs, and a plain ReadFile on a FIFO with no
// writer blocks the process forever -- which is why safe_read.go exists and why
// the seed path already reads caller-supplied documents through it.
func TestDocsExtractRefusesAFIFOInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "spec.pdf")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		cmd := newDocsExtractCommand()
		cmd.SetOut(&strings.Builder{})
		cmd.SetErr(&strings.Builder{})
		cmd.SetArgs([]string{fifo})
		done <- cmd.Execute()
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was accepted as a document")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("docs extract blocked on a FIFO with no writer")
	}
}

func TestDocsExtractRefusesAnOversizedFileOnItsStatedSize(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "huge.pdf")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: the file states a size past the ceiling without costing the disk.
	if err := f.Truncate(maxExtractDocumentBytes + 1); err != nil {
		f.Close()
		t.Skipf("sparse file unavailable: %v", err)
	}
	f.Close()

	cmd := newDocsExtractCommand()
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs([]string{big})
	if err := cmd.Execute(); err == nil {
		t.Fatal("a file past the extraction ceiling was read anyway")
	}
}
