//go:build !windows

package brainstore

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrivacyReadersReportFIFOPresentAndUnsafe(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, present, err := PrivacyArtifactInfo(root, "fifo", "fixture")
	if !present || err == nil || !strings.Contains(err.Error(), StateUnsafeCode) {
		t.Fatalf("FIFO info: present=%v err=%v", present, err)
	}
	_, present, err = ReadPrivacyArtifact(root, "fifo", "fixture", 100)
	if !present || err == nil || !strings.Contains(err.Error(), StateUnsafeCode) {
		t.Fatalf("FIFO artifact: present=%v err=%v", present, err)
	}
}
