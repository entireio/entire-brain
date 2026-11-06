package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A reseed used to only ever add to seed/docs: a document that left the
// repository kept its copy, and the doc index kept returning it. These tests
// cover the two ways that happened and the files the prune must not touch.

func seedPruneRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repoDir := t.TempDir()
	gitAt(t, repoDir, "init", "-q")
	gitAt(t, repoDir, "config", "user.name", "Seed Test")
	gitAt(t, repoDir, "config", "user.email", "seed@example.com")
	gitAt(t, repoDir, "config", "commit.gpgsign", "false")
	writeSeedPruneFiles(t, repoDir, files)
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "initial")
	return repoDir
}

func writeSeedPruneFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// seedPruneRun seeds repoDir into outputDir with no agent.
func seedPruneRun(t *testing.T, repoDir, outputDir string, args ...string) {
	t.Helper()
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: ExecRunner{}}
	argv := append([]string{"refresh", "seed", "--agent", "none", "--output", outputDir}, args...)
	if out, err := execute(t, NewRootCommand(opts), append(argv, repoDir)...); err != nil {
		t.Fatalf("seed %v: %v\n%s", args, err, out)
	}
}

func assertSeedDoc(t *testing.T, outputDir, rel string, want bool) {
	t.Helper()
	_, err := os.Lstat(filepath.Join(outputDir, seedDirName, seedDocsDirName, filepath.FromSlash(rel)))
	switch {
	case want && err != nil:
		t.Errorf("seed/docs/%s should be there: %v", rel, err)
	case !want && err == nil:
		t.Errorf("seed/docs/%s is still there", rel)
	}
}

func assertDocIndexLacks(t *testing.T, outputDir, text string) {
	t.Helper()
	records, _, _, err := loadDocRecordsFromSeed(outputDir)
	if err != nil {
		t.Fatalf("load doc records: %v", err)
	}
	for _, record := range records {
		if strings.Contains(record.Text, text) {
			t.Errorf("the doc index still returns %q from %s", text, record.Path)
		}
	}
}

func TestReseedRemovesDocumentsDeletedFromTheRepository(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":          "# readme\n",
		"docs/old.md":        "# Old guide\n\nDeploy with the retired zebra pipeline.\n",
		"docs/keep/guide.md": "# Guide\n",
	})
	outputDir := filepath.Join(t.TempDir(), "brain")
	seedPruneRun(t, repoDir, outputDir)
	assertSeedDoc(t, outputDir, "docs/old.md", true)

	gitAt(t, repoDir, "rm", "-q", "docs/old.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop old doc")
	seedPruneRun(t, repoDir, outputDir, "--update")

	assertSeedDoc(t, outputDir, "docs/old.md", false)
	assertSeedDoc(t, outputDir, "README.md", true)
	assertSeedDoc(t, outputDir, "docs/keep/guide.md", true)
	assertDocIndexLacks(t, outputDir, "zebra pipeline")
}

func TestCleanReseedRemovesADocumentOnlyTheWorktreeHad(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	writeSeedPruneFiles(t, repoDir, map[string]string{
		"docs/drafts/draft.md": "# Draft\n\nAn untracked zebra pipeline note.\n",
	})
	outputDir := filepath.Join(t.TempDir(), "brain")
	seedPruneRun(t, repoDir, outputDir, "--worktree")
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)

	if err := os.RemoveAll(filepath.Join(repoDir, "docs")); err != nil {
		t.Fatal(err)
	}
	seedPruneRun(t, repoDir, outputDir, "--update")

	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", false)
	// The directories it emptied go with it.
	assertSeedDoc(t, outputDir, "docs", false)
	assertSeedDoc(t, outputDir, "README.md", true)
	assertDocIndexLacks(t, outputDir, "zebra pipeline")
}

// TestSeedDocThatCannotBeReadIsDroppedNotSilentlySkipped: a document whose
// source read fails is reported and taken out of the file index and the
// seed's documents, so the prune that removes its earlier copy never leaves it
// recorded as seeded with nothing on disk.
func TestSeedDocThatCannotBeReadIsDroppedNotSilentlySkipped(t *testing.T) {
	repoDir := t.TempDir()
	outputDir := t.TempDir()
	writeSeedPruneFiles(t, outputDir, map[string]string{"seed/docs/docs/gone.md": "# earlier copy\n"})
	scan := &seedScanResult{
		RepoDir: repoDir,
		Files:   []seedFileIndexEntry{{Path: "docs/gone.md", Included: true, Reason: "included"}},
		Docs:    []seedDocument{{Path: "docs/gone.md", SeedPath: "seed/docs/docs/gone.md", Reason: "included"}},
	}
	if err := writeSeedDocs(outputDir, scan); err != nil {
		t.Fatalf("write seed docs: %v", err)
	}
	if scan.Docs[0].SeedPath != "" {
		t.Errorf("an unreadable document kept its seed path %q", scan.Docs[0].SeedPath)
	}
	if scan.Files[0].Included {
		t.Errorf("an unreadable document is still in the file index as included: %+v", scan.Files[0])
	}
	if !strings.Contains(strings.Join(scan.Warnings, "\n"), "document not indexed: docs/gone.md") {
		t.Errorf("the unreadable document is not reported: %q", scan.Warnings)
	}
	assertSeedDoc(t, outputDir, "docs/gone.md", false)
}

// TestPruneSeedDocsKeepsADocumentRenamedOnlyInCase runs where the filesystem
// folds case (macOS and Windows by default): the old copy and the new name are
// one file there, and the prune must not delete the document it just wrote.
func TestPruneSeedDocsKeepsADocumentRenamedOnlyInCase(t *testing.T) {
	probe := t.TempDir()
	if err := os.WriteFile(filepath.Join(probe, "case"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(probe, "CASE")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	repoDir := t.TempDir()
	outputDir := t.TempDir()
	writeSeedPruneFiles(t, repoDir, map[string]string{"docs/guide.md": "# renamed guide\n"})
	writeSeedPruneFiles(t, outputDir, map[string]string{"seed/docs/docs/Guide.md": "# old spelling\n"})
	scan := &seedScanResult{
		RepoDir: repoDir,
		Files:   []seedFileIndexEntry{{Path: "docs/guide.md", Included: true, Reason: "included"}},
		Docs:    []seedDocument{{Path: "docs/guide.md", SeedPath: "seed/docs/docs/guide.md", Reason: "included"}},
	}
	if err := writeSeedDocs(outputDir, scan); err != nil {
		t.Fatalf("write seed docs: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outputDir, "seed", "docs", "docs", "guide.md"))
	if err != nil || string(got) != "# renamed guide\n" {
		t.Errorf("the document written under its new spelling = %q, %v; want the renamed guide", got, err)
	}
}

// TestPruneSeedDocsLeavesWhatItCannotSafelyRemove: a symlink is reported and
// left, never followed; a second name for the file just written (what a
// case-insensitive filesystem gives a document renamed only in case) is kept;
// and a symlinked seed directory is not walked at all.
func TestPruneSeedDocsLeavesWhatItCannotSafelyRemove(t *testing.T) {
	t.Run("entries", func(t *testing.T) {
		outputDir := t.TempDir()
		docs := filepath.Join(outputDir, seedDirName, seedDocsDirName)
		writeSeedPruneFiles(t, docs, map[string]string{
			"guide.md":     "# Guide\n",
			"stale/old.md": "# Old\n",
		})
		aliased := os.Link(filepath.Join(docs, "guide.md"), filepath.Join(docs, "Guide.md")) == nil
		outside := filepath.Join(t.TempDir(), "secret.md")
		writeSeedPruneFiles(t, filepath.Dir(outside), map[string]string{"secret.md": "outside\n"})
		linked := os.Symlink(outside, filepath.Join(docs, "link.md")) == nil

		scan := &seedScanResult{}
		pruneSeedDocs(outputDir, scan, map[string]bool{"seed/docs/guide.md": true})

		assertSeedDoc(t, outputDir, "guide.md", true)
		assertSeedDoc(t, outputDir, "stale/old.md", false)
		assertSeedDoc(t, outputDir, "stale", false)
		if aliased {
			assertSeedDoc(t, outputDir, "Guide.md", true)
		}
		if linked {
			assertSeedDoc(t, outputDir, "link.md", true)
			if !strings.Contains(strings.Join(scan.Warnings, "\n"), "seed/docs/link.md") {
				t.Errorf("the symlink that was left is not reported: %q", scan.Warnings)
			}
		}
		if _, err := os.Stat(outside); err != nil {
			t.Errorf("the symlink's target outside the brain was touched: %v", err)
		}
	})
	t.Run("symlinked seed directory", func(t *testing.T) {
		outputDir := t.TempDir()
		elsewhere := t.TempDir()
		writeSeedPruneFiles(t, elsewhere, map[string]string{"docs/old.md": "# Old\n"})
		if err := os.Symlink(elsewhere, filepath.Join(outputDir, seedDirName)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		scan := &seedScanResult{}
		pruneSeedDocs(outputDir, scan, map[string]bool{})
		if _, err := os.Stat(filepath.Join(elsewhere, "docs", "old.md")); err != nil {
			t.Errorf("a file behind a symlinked seed directory was removed: %v", err)
		}
		if len(scan.Warnings) == 0 {
			t.Error("a symlinked seed directory was skipped without a warning")
		}
	})
}
