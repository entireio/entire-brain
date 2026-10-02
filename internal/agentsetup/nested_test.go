package agentsetup

import (
	"os"
	"path/filepath"
	"testing"
)

func mkrepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The reported shape: entire-brain cloned INSIDE a project, so the clone
// carries its own .git. Context walks to the nearest one, so init-agents run
// from the clone writes the guide there — invisible to the project, with no
// error. OuterRepo is what lets the command say so.
func TestOuterRepoFindsTheProjectAroundANestedClone(t *testing.T) {
	project := mkrepo(t, t.TempDir())
	clone := mkrepo(t, filepath.Join(project, "entire-brain"))

	if got := OuterRepo(clone); got != project {
		t.Fatalf("OuterRepo(%s) = %q, want the project root %q", clone, got, project)
	}
}

// The ordinary case must stay silent, or the warning becomes noise everyone
// learns to ignore.
func TestOuterRepoIsEmptyForATopLevelRepo(t *testing.T) {
	// A repo whose ancestors hold no .git.
	parent := t.TempDir()
	repo := mkrepo(t, filepath.Join(parent, "project"))

	if got := OuterRepo(repo); got != "" {
		t.Fatalf("a top-level repository must report no outer repo, got %q", got)
	}
	if got := OuterRepo(""); got != "" {
		t.Fatalf("an empty root must report no outer repo, got %q", got)
	}
}

// A worktree or submodule stores .git as a FILE, not a directory. Those are
// still repositories and still nest, so the check uses Lstat and must not care
// which it is.
func TestOuterRepoDetectsAGitFileNotJustADirectory(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := mkrepo(t, filepath.Join(project, "nested"))

	if got := OuterRepo(inner); got != project {
		t.Fatalf("a .git FILE marks a repository too; OuterRepo = %q, want %q", got, project)
	}
}
