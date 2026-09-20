package cli

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestSkillReplacementPreservesHardlinkAlias(t *testing.T) {
	d := t.TempDir()
	dest, err := skillDestinations("standard", "repo", "audit", d)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("original"), 0600)
	os.MkdirAll(filepath.Dir(dest[0].Path), 0700)
	if err := os.Link(outside, dest[0].Path); err != nil {
		t.Skip(err)
	}
	if err := writeSkillFile(dest[0], []byte("replacement skill")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "original" {
		t.Fatalf("regression %q %v", got, err)
	}
	t.Log("outside hard-linked file unchanged")
}

func TestSkillReplacementRejectsSwappedDirectory(t *testing.T) {
	base := t.TempDir()
	dests, err := skillDestinations("standard", "repo", "swap", base)
	if err != nil {
		t.Fatal(err)
	}
	d := dests[0]
	parent := filepath.Dir(d.Path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "SKILL.md")
	if err := os.WriteFile(sentinel, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	originalParent, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	swapBlocked := false
	err = writeSkillFileBeforeReplace(d, []byte("replacement"), func() {
		if err := os.Rename(parent, parent+"-moved"); err != nil {
			// Windows refuses to rename the directory held open by os.Root.
			// ERROR_SHARING_VIOLATION is 32; all other fixture errors still fail.
			if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) {
				swapBlocked = true
				return
			}
			t.Fatal(err)
		}
		if err := os.Symlink(outside, parent); err != nil {
			t.Fatal(err)
		}
	})
	if swapBlocked {
		if err != nil {
			t.Fatalf("OS blocked swap, but original destination publication failed: %v", err)
		}
		currentParent, statErr := os.Stat(parent)
		if statErr != nil || !os.SameFile(originalParent, currentParent) {
			t.Fatalf("blocked swap changed original directory: %v", statErr)
		}
		if _, statErr := os.Lstat(parent + "-moved"); !os.IsNotExist(statErr) {
			t.Fatalf("blocked swap created moved directory: %v", statErr)
		}
		got, readErr := os.ReadFile(d.Path)
		if readErr != nil || string(got) != "replacement" {
			t.Fatalf("original destination not published: %q %v", got, readErr)
		}
	} else if err == nil {
		t.Fatal("directory swap accepted")
	}
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(got) != "outside" {
		t.Fatalf("outside changed: %q %v", got, readErr)
	}
	cleanupDir := parent + "-moved"
	if swapBlocked {
		cleanupDir = parent
	}
	entries, err := os.ReadDir(cleanupDir)
	if err != nil {
		t.Fatal(err)
	}
	if swapBlocked {
		if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
			t.Fatalf("unexpected files after valid publication: %v", entries)
		}
	} else if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}
