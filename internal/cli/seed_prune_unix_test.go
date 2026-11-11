//go:build !windows

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// TestACopyLiveButNotSyncedIsRecorded: a document write that renamed its copy
// into place and then failed the directory sync reports an error, but the
// copy is there. The pending marker of an --output other than the brain
// store lists it, so a later seed that no longer has the document removes
// it. Not parallel: the sync seam is process-wide.
func TestACopyLiveButNotSyncedIsRecorded(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	outputDir := filepath.Join(t.TempDir(), "out")
	seedPruneRun(t, repoDir, outputDir, "--worktree")
	writeSeedPruneFiles(t, repoDir, map[string]string{"docs/new.md": "# New\n\nThe zebra pipeline.\n"})

	copyPath := filepath.Join(outputDir, seedDirName, seedDocsDirName, "docs", "new.md")
	saved := brainstore.AtomicReplaceBeforeParentSync
	brainstore.AtomicReplaceBeforeParentSync = func(path string) error {
		if path == copyPath {
			return errors.New("sync failed")
		}
		return nil
	}
	t.Cleanup(func() { brainstore.AtomicReplaceBeforeParentSync = saved })

	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: ExecRunner{}}
	out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--output", outputDir, "--worktree", "--update", repoDir)
	brainstore.AtomicReplaceBeforeParentSync = saved
	if err == nil || !strings.Contains(err.Error(), "not durably synced") {
		t.Fatalf("refresh = %v\n%s; want the copy's sync failure", err, out)
	}
	if _, statErr := os.Lstat(copyPath); statErr != nil {
		t.Fatalf("the copy is not live: %v", statErr)
	}
	if listed, marker := readSeedPruneMarker(outputDir); marker != seedPruneMarkerOurs || !slices.Contains(listed, "seed/docs/docs/new.md") {
		t.Fatalf("the marker = %q, %v; want it to list the live copy", listed, marker)
	}

	if err := os.Remove(filepath.Join(repoDir, "docs", "new.md")); err != nil {
		t.Fatal(err)
	}
	seedPruneRun(t, repoDir, outputDir, "--worktree", "--update")
	assertSeedDoc(t, outputDir, "docs/new.md", false)
	assertDocIndexLacks(t, outputDir, "zebra pipeline")
}

// TestACopyIsClaimedBeforeItIsLive: in an --output other than the brain
// store, the pending marker claims a new document's path before its copy is
// renamed into place, so a seed killed while copying, which no error path
// cleans up after, still leaves the copy the next seed's to remove. Not
// parallel: the sync seam is process-wide.
func TestACopyIsClaimedBeforeItIsLive(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	outputDir := filepath.Join(t.TempDir(), "out")
	seedPruneRun(t, repoDir, outputDir, "--worktree")
	writeSeedPruneFiles(t, repoDir, map[string]string{"docs/new.md": "# New\n"})

	copyPath := filepath.Join(outputDir, seedDirName, seedDocsDirName, "docs", "new.md")
	var claimed, seen bool
	saved := brainstore.AtomicReplaceBeforeParentSync
	brainstore.AtomicReplaceBeforeParentSync = func(path string) error {
		if path == copyPath && !seen {
			seen = true
			listed, marker := readSeedPruneMarker(outputDir)
			claimed = marker == seedPruneMarkerOurs && slices.Contains(listed, "seed/docs/docs/new.md")
		}
		return nil
	}
	t.Cleanup(func() { brainstore.AtomicReplaceBeforeParentSync = saved })
	seedPruneRun(t, repoDir, outputDir, "--worktree", "--update")
	brainstore.AtomicReplaceBeforeParentSync = saved
	if !seen || !claimed {
		t.Fatalf("copy written %v, claimed by the marker when it went live %v; want both", seen, claimed)
	}
	if _, marker := readSeedPruneMarker(outputDir); marker != seedPruneMarkerAbsent {
		t.Errorf("the marker is still there after a seed that finished (%v)", marker)
	}
}
