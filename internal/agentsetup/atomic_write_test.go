package agentsetup

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestContainedReplacementPreservesOriginalOnWriteFailure(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "AGENTS.md")
	const original = "# User instructions\nDo not lose this text.\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	failure := errors.New("simulated disk full")
	err = replaceContainedFile(root, "AGENTS.md", 0644, func(f *os.File) error {
		if _, err := f.WriteString("partial replacement"); err != nil {
			t.Fatal(err)
		}
		if got := readFileForTest(t, path); got != original {
			t.Fatalf("original changed before commit: %q", got)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("got %v, want write failure", err)
	}
	if got := readFileForTest(t, path); got != original {
		t.Fatalf("lost user text: %q", got)
	}
	entries, err := os.ReadDir(repo)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary output leaked: %v, %v", entries, err)
	}
	if err := writeContainedFile(root, "AGENTS.md", []byte("complete replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := readFileForTest(t, path); got != "complete replacement" {
		t.Fatal(got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode changed: %v", info.Mode())
	}
}

func TestReadRecordRejectsUnnormalizedPaths(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "brain.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "./brain.json", "sub/../brain.json", "sub/./brain.json", "sub//brain.json", "../brain.json", filepath.Join(repo, "brain.json")} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := readRecord(repo, name); err == nil {
				t.Fatalf("accepted %q", name)
			}
		})
	}
	if _, present, err := readRecord(repo, "brain.json"); err != nil || !present {
		t.Fatalf("valid record: %v, %v", present, err)
	}
}

func TestSafeKeyBeforePlatformConversion(t *testing.T) {
	for _, key := range []string{"gh/team/repo", "gh/team/sub/repo", "local/repo-1234"} {
		if !safeKey(key) {
			t.Fatalf("valid key rejected: %q", key)
		}
	}
	for _, key := range []string{"", "/gh/team/repo", "gh/team/../repo", "gh/./repo", "gh//repo", `gh\team\repo`, "C:/repo", "gh/team/repo/", "workspaces/repo"} {
		if safeKey(key) {
			t.Fatalf("unsafe raw key accepted: %q", key)
		}
	}
}
