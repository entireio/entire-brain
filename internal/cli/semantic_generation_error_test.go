package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerationAllocationStopsOnPermissionError(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0200); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if _, err := os.Lstat(filepath.Join(root, "id")); !os.IsPermission(err) {
		t.Skipf("requires non-root permission enforcement: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := semanticAvailableGenerationID(root, "id", "suffix"); done <- err }()
	select {
	case got := <-done:
		if !errors.Is(got, os.ErrPermission) {
			t.Fatalf("expected permission failure, got %v", got)
		}
	case <-time.After(100 * time.Millisecond):
		os.Chmod(root, 0700)
		select {
		case got := <-done:
			t.Errorf("looped until permissions were repaired; returned %v", got)
		case <-time.After(time.Second):
			t.Error("did not recover")
		}
	}
}

func TestGenerationAllocationSkipsOccupiedNames(t *testing.T) {
	root := t.TempDir()
	for _, want := range []string{"id", "id-suffix", "id-suffix-1", "id-suffix-2"} {
		got, err := semanticAvailableGenerationID(root, "id", "suffix")
		if err != nil || got != want {
			t.Fatalf("got %q %v, want %q", got, err, want)
		}
		if err := os.Mkdir(filepath.Join(root, got), 0700); err != nil {
			t.Fatal(err)
		}
	}
}
