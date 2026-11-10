package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// COR-2316: a dirty worktree seeds from the committed HEAD snapshot instead of
// refusing, so permanently-dirty worktrees (every Conductor workspace) still
// get background refreshes. Real git: dirtiness and archives are properties of
// an actual repository.
func TestDirtyWorktreeSeedsFromCommittedHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitAt(t, repo, "init", "-q")
	gitAt(t, repo, "config", "user.email", "t@invalid")
	gitAt(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("committed content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// export-ignore must not shape the snapshot (PR #631 Bugbot): git archive
	// would drop docs/guide.md entirely; the checkout-index snapshot keeps it.
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("docs/ export-ignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "guide.md"), []byte("export-ignored doc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A denied directory must consume the cap identically in both modes: the
	// clean path lists it via ls-files and denies in inspectSeedFile.
	if err := os.MkdirAll(filepath.Join(repo, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "build", "tool.txt"), []byte("generated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, repo, "add", "README.md", ".gitattributes", "docs/guide.md", "build/tool.txt")
	gitAt(t, repo, "commit", "-qm", "initial")
	// Dirty the tracked file and add an untracked one.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("UNCOMMITTED edits\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := ExecRunner{}
	dir, commit, cleanup, err := materializeHeadSnapshot(context.Background(), runner, repo)
	if err != nil {
		t.Fatalf("materializeHeadSnapshot: %v", err)
	}
	defer cleanup()
	if len(commit) < 7 {
		t.Fatalf("snapshot commit not resolved: %q", commit)
	}
	body, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("snapshot README: %v", err)
	}
	if string(body) != "committed content\n" {
		t.Fatalf("snapshot must hold committed content, got %q", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked file must not reach the snapshot, stat err=%v", err)
	}
	doc, err := os.ReadFile(filepath.Join(dir, "docs", "guide.md"))
	if err != nil || string(doc) != "export-ignored doc\n" {
		t.Fatalf("export-ignored committed file must reach the snapshot verbatim, err=%v body=%q", err, doc)
	}

	// The scan in snapshot mode reads committed bytes and stamps the snapshot
	// commit, while RepoDir stays the real repository.
	scan, err := scanSeedRepository(context.Background(), runner, repo, dir, commit, "test", seedCommandOptions{maxFiles: 100, maxFileBytes: 1 << 20})
	if err != nil {
		t.Fatalf("scanSeedRepository: %v", err)
	}
	if scan.Commit != commit {
		t.Fatalf("scan must stamp the snapshot commit: got %q want %q", scan.Commit, commit)
	}
	if scan.RepoDir != repo || scan.contentRoot() != dir {
		t.Fatalf("scan roots wrong: RepoDir=%q contentRoot=%q", scan.RepoDir, scan.contentRoot())
	}
	var sawReadme, sawScratch, sawDoc bool
	var denied *seedFileIndexEntry
	for i, f := range scan.Files {
		switch f.Path {
		case "README.md":
			sawReadme = true
		case "scratch.txt":
			sawScratch = true
		case "docs/guide.md":
			sawDoc = true
		case "build/tool.txt":
			denied = &scan.Files[i]
		}
	}
	if !sawReadme || sawScratch {
		t.Fatalf("snapshot scan must list committed files only: readme=%v scratch=%v", sawReadme, sawScratch)
	}
	if !sawDoc {
		t.Fatal("export-ignored committed file must be scanned like any other committed file")
	}
	// Denied paths are enumerated and rejected in inspectSeedFile, exactly as
	// on the clean path, so the --max-files cap sees the same list.
	if denied == nil {
		t.Fatal("a committed file in a denied directory must still be enumerated")
	}
	if denied.Included || denied.Reason != "denied path" {
		t.Fatalf("denied directory file must be excluded as 'denied path', got %+v", *denied)
	}
	if strings.Contains(strings.Join(scan.Warnings, " "), "refusing") {
		t.Fatalf("snapshot mode must not carry a refusal, got %v", scan.Warnings)
	}
}
