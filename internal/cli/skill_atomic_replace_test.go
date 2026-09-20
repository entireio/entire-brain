package cli

import (
	"os"
	"path/filepath"
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
	err = writeSkillFileBeforeReplace(d, []byte("replacement"), func() {
		if err := os.Rename(parent, parent+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, parent); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("directory swap accepted")
	}
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(got) != "outside" {
		t.Fatalf("outside changed: %q %v", got, readErr)
	}
	entries, err := os.ReadDir(parent + "-moved")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
}
