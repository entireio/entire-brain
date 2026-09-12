package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two shared path guards rejected ".", an absolute path, and any rel
// starting "../" — but not rel that IS exactly "..". filepath.Clean("..") is
// "..", which is not "." and does not have the "../" prefix, so it fell through
// the whole prefix test.
//
// The per-component symlink walk then Lstats filepath.Join(root, "..") — the
// PARENT of root, a real directory and not a symlink — and passes it. The only
// thing left standing between "one level up" and the caller is the closing
// containment check, and that check returns nil early when root does not exist
// yet. On a machine where the store has not been created, "" resolves one level
// above the store root and the caller acts on it.
//
// The reachable consequence is brainDirForKey(env, ".."), whose result
// brain_delete_project hands to os.RemoveAll: the whole plugin data root,
// workspaces and every other project's brain included.

func TestRejectExistingSymlinkPathComponentsRejectsBareDotDot(t *testing.T) {
	t.Parallel()
	// root deliberately absent — the first-write case the guard is written to
	// tolerate, and the case where the containment check short-circuits.
	root := filepath.Join(t.TempDir(), "repos")
	if err := rejectExistingSymlinkPathComponents(root, ".."); err == nil {
		t.Fatalf("guard accepted %q, which resolves to the parent of root (%s)", "..", filepath.Dir(root))
	}
}

func TestRejectSymlinkPathComponentsRejectsBareDotDot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "repos")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rejectSymlinkPathComponents(root, ".."); err == nil {
		t.Fatalf("read guard accepted %q, which resolves to the parent of root", "..")
	}
}

// TestBrainDirForKeyRejectsDotDot is the end-to-end shape: the repo key reaches
// brainDirForKey straight from the brain_delete_project MCP argument, and its
// return value is passed to os.RemoveAll.
func TestBrainDirForKeyRejectsDotDot(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	env := EntireEnv{PluginDataDir: dataDir}

	dir, err := brainDirForKey(env, "..")
	if err == nil {
		t.Fatalf("brainDirForKey resolved repo key %q to %s — os.RemoveAll on that wipes the whole brain data root", "..", dir)
	}

	// A legitimate key must still resolve, under the store root.
	good, err := brainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatalf("legitimate repo key rejected: %v", err)
	}
	store := filepath.Join(dataDir, repoStoreDirName)
	if !strings.HasPrefix(good, store+string(filepath.Separator)) {
		t.Fatalf("legitimate key resolved outside the store: %s (want under %s)", good, store)
	}
}
