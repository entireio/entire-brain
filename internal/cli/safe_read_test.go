package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeReadFileRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(path, make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := safeReadFile(path, 512); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("expected oversize rejection, got %v", err)
	}
	got, err := safeReadFile(path, 4096)
	if err != nil || len(got) != 1024 {
		t.Fatalf("within-limit read failed: len=%d err=%v", len(got), err)
	}
}

func TestSafeReadAllExactBoundary(t *testing.T) {
	// A file exactly at the limit is accepted; one byte over is rejected.
	if _, err := safeReadAll(strings.NewReader("abcd"), 4, "x"); err != nil {
		t.Fatalf("exact boundary should pass: %v", err)
	}
	if _, err := safeReadAll(strings.NewReader("abcde"), 4, "x"); err == nil {
		t.Fatalf("one over should fail")
	}
}

func TestSemanticSnapshotMaxBytesHonorsOverride(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES", "123")
	if got := semanticSnapshotMaxBytes(); got != 123 {
		t.Fatalf("override = %d, want 123", got)
	}
	t.Setenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES", "not-a-number")
	if got := semanticSnapshotMaxBytes(); got != defaultMaxSemanticSnapshotBytes {
		t.Fatalf("invalid override should fall back to default, got %d", got)
	}
}
