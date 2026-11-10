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
	gitAt(t, repo, "add", "README.md")
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
	var sawReadme, sawScratch bool
	for _, f := range scan.Files {
		if f.Path == "README.md" {
			sawReadme = true
		}
		if f.Path == "scratch.txt" {
			sawScratch = true
		}
	}
	if !sawReadme || sawScratch {
		t.Fatalf("snapshot scan must list committed files only: readme=%v scratch=%v", sawReadme, sawScratch)
	}
	if strings.Contains(strings.Join(scan.Warnings, " "), "refusing") {
		t.Fatalf("snapshot mode must not carry a refusal, got %v", scan.Warnings)
	}
}
