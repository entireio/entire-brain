//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceConfigReportsDirectorySyncOpenFailure(t *testing.T) {
	dir := t.TempDir()
	tmp, dst := filepath.Join(dir, "temporary"), filepath.Join(dir, "config.json")
	if err := os.WriteFile(tmp, []byte("new config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if f, err := os.Open(dir); err == nil {
		f.Close()
		t.Skip("host permits reading owner-unreadable directories")
	}
	err := replaceConfigFileAtomic(tmp, dst)
	if err == nil || !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("expected post-replacement durability error, got %v", err)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "new config" {
		t.Fatalf("replacement must be visible: %q, %v", data, err)
	}
}
