//go:build !windows

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSessionTombstoneLoaderRejectsFIFOBeforeOpen(t *testing.T) {
	brainDir := t.TempDir()
	path := filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, state, err := loadSessionTombstonesChecked(brainDir)
	var typed *sessionTombstoneLoadError
	if !errors.As(err, &typed) || typed.Code != memoryErrStateCorrupt || state.State != sessionTombstoneCorrupt {
		t.Fatalf("FIFO state=%+v err=%v", state, err)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("FIFO error does not explain rejection: %v", err)
	}
}

func TestMemoryMigrationProgressRejectsFIFOBeforeOpen(t *testing.T) {
	brainDir := t.TempDir()
	path := filepath.Join(brainDir, filepath.FromSlash(memoryMigrationProgressRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := loadMemoryMigrationProgress(brainDir); !present || err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("FIFO migration progress present=%v err=%v", present, err)
	}
}

func TestPrivacyCheckedOpenRejectsLeafSwapToSymlink(t *testing.T) {
	brainDir := t.TempDir()
	stones := emptySessionTombstones()
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath))
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"version":1,"excluded":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	original := privacyOpen
	swapped := false
	privacyOpen = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		if path == target && !swapped {
			swapped = true
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
		}
		return original(path, flag, perm)
	}
	t.Cleanup(func() { privacyOpen = original })
	_, state, err := loadSessionTombstonesChecked(brainDir)
	var typed *sessionTombstoneLoadError
	if !swapped || !errors.As(err, &typed) || typed.Code != memoryErrStateCorrupt || state.State != sessionTombstoneCorrupt {
		t.Fatalf("leaf swap state=%+v err=%v swapped=%v", state, err, swapped)
	}
}

func TestPrivacyCheckedOpenRejectsLeafSwapToFIFOWithoutBlocking(t *testing.T) {
	brainDir := t.TempDir()
	stones := emptySessionTombstones()
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath))
	original := privacyOpen
	swapped := false
	privacyOpen = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		if path == target && !swapped {
			swapped = true
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if flag&syscall.O_NONBLOCK == 0 {
			t.Fatalf("privacy read opened without O_NONBLOCK: flags=%#x", flag)
		}
		return original(path, flag, perm)
	}
	t.Cleanup(func() { privacyOpen = original })
	_, state, err := loadSessionTombstonesChecked(brainDir)
	var typed *sessionTombstoneLoadError
	if !swapped || !errors.As(err, &typed) || typed.Code != memoryErrStateCorrupt || state.State != sessionTombstoneCorrupt {
		t.Fatalf("FIFO leaf swap state=%+v err=%v swapped=%v", state, err, swapped)
	}
}

func TestPrivacyCheckedDirectoryOpenRejectsSwapToSymlink(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	target := filepath.Join(brainDir, factsDirName)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	original := privacyOpen
	swapped := false
	privacyOpen = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		if path == target && !swapped {
			swapped = true
			if err := os.Rename(target, target+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
		}
		return original(path, flag, perm)
	}
	t.Cleanup(func() { privacyOpen = original })
	_, err := privacyDerivedStoreArtifacts(brainDir)
	if !swapped || err == nil || (!strings.Contains(err.Error(), memoryErrStateCorrupt) && !strings.Contains(err.Error(), memoryErrStateUnsafe)) {
		t.Fatalf("directory swap err=%v swapped=%v", err, swapped)
	}
}

func TestPrivacyVectorSnapshotRejectsSymlinkAndLeafSwap(t *testing.T) {
	vectorRel := filepath.ToSlash(filepath.Join(historyDirName, embedStoreDirName, historyVecStoreFileNamePortable))
	t.Run("symlink", func(t *testing.T) {
		brainDir := t.TempDir()
		path := filepath.Join(brainDir, filepath.FromSlash(vectorRel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.sqlite")
		if err := os.WriteFile(outside, []byte("external-canary"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, _, err := snapshotPrivacySQLiteStore(brainDir, vectorRel); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
			t.Fatalf("vector symlink snapshot error = %v", err)
		}
	})

	t.Run("regular to symlink", func(t *testing.T) {
		brainDir := t.TempDir()
		path := filepath.Join(brainDir, filepath.FromSlash(vectorRel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.sqlite")
		if err := os.WriteFile(outside, []byte("external-canary"), 0o600); err != nil {
			t.Fatal(err)
		}
		original := privacyOpen
		swapped := false
		privacyOpen = func(name string, flag int, perm os.FileMode) (*os.File, error) {
			if name == path && !swapped {
				swapped = true
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			return original(name, flag, perm)
		}
		t.Cleanup(func() { privacyOpen = original })
		if _, _, err := snapshotPrivacySQLiteStore(brainDir, vectorRel); !swapped || err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
			t.Fatalf("vector swap snapshot error = %v swapped=%v", err, swapped)
		}
	})
}
