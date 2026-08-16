//go:build !windows

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAbstractPrivacyCleanupRejectsRegularToFIFOSwapWithoutBlocking(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	digest := sessionViewDigest(view)
	artifact := validAbstractForView(view, digest)
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	writeAbstractBytesForTest(t, brainDir, digest, append(data, '\n'))
	target := filepath.Join(brainDir, filepath.FromSlash(abstractRel(digest)))
	originalOpen := memoryStateOpenFile
	swapped := false
	memoryStateOpenFile = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		if path == target && !swapped {
			swapped = true
			if err := os.Remove(target); err != nil {
				return nil, err
			}
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				return nil, err
			}
		}
		return originalOpen(path, flag, perm)
	}
	t.Cleanup(func() { memoryStateOpenFile = originalOpen })
	done := make(chan error, 1)
	go func() { done <- purgeSessionAbstracts(brainDir, manifest, "nav-sess") }()
	select {
	case err := <-done:
		if !swapped || err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
			t.Fatalf("swapped=%v err=%v", swapped, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("privacy cleanup blocked on a FIFO leaf swap")
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO canary changed: info=%v err=%v", info, err)
	}
}
