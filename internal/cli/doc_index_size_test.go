package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDocIndexSizeBoundPreservesExistingIndex(t *testing.T) {
	previous := maxDocIndexBytes
	maxDocIndexBytes = 32
	t.Cleanup(func() { maxDocIndexBytes = previous })
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.FromSlash(docIndexPath))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 33)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() error{
		func() error { return verifyDeclaredDocIndex(dir) },
		func() error { _, err := loadDocIndex(dir); return err },
	} {
		var bound *readBoundExceededError
		if err := read(); !errors.As(err, &bound) {
			t.Fatalf("expected size error, got %v", err)
		}
	}
	original := "{}\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var bound *readBoundExceededError
	if _, err := writeDocIndexAndSourceLocked(dir, time.Now()); !errors.As(err, &bound) {
		t.Fatalf("oversized write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatal("rejected write replaced the readable index")
	}
	if _, err := loadDocIndex(dir); err != nil {
		t.Fatal(err)
	}
}
