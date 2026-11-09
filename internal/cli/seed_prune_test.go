package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
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

// TestRefusedWorktreeRefreshKeepsTheDocumentsItWouldPrune: a --worktree
// refresh refused because the tree changed while it ran keeps the previous
// manifest, and with it the previous worktree hash. The prune used to run
// before that check, so a document the earlier seed copied, deleted when the
// refresh started and restored while it ran, was removed although the
// restored tree matched the saved hash: the next refresh skipped the reseed
// and the document stayed missing from search.
func TestRefusedWorktreeRefreshKeepsTheDocumentsItWouldPrune(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	draft := map[string]string{"docs/drafts/draft.md": "# Draft\n\nAn untracked zebra pipeline note.\n"}
	writeSeedPruneFiles(t, repoDir, draft)
	outputDir := filepath.Join(t.TempDir(), "brain")
	seedPruneRun(t, repoDir, outputDir, "--worktree")
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)

	if err := os.RemoveAll(filepath.Join(repoDir, "docs")); err != nil {
		t.Fatal(err)
	}
	fileIndex := filepath.Join(outputDir, seedDirName, "file-index.json")
	runner := &seedStatusHookRunner{hook: func() bool {
		// This refresh has written its artifacts once the file index no
		// longer lists the draft. The next git status is the final worktree
		// check, so the draft comes back just before it.
		data, err := os.ReadFile(fileIndex)
		if err != nil || strings.Contains(string(data), "draft.md") {
			return false
		}
		writeSeedPruneFiles(t, repoDir, draft)
		return true
	}}
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: runner}
	out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--output", outputDir, "--worktree", "--update", repoDir)
	if !runner.fired {
		t.Fatalf("the draft was never restored during the refresh: %v\n%s", err, out)
	}
	if err == nil || !strings.Contains(err.Error(), "worktree_changed") {
		t.Fatalf("refresh = %v\n%s; want worktree_changed", err, out)
	}
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)
}

// TestFailedManifestWriteKeepsTheDocumentsItWouldPrune: a refresh whose
// manifest write fails keeps the previous manifest, like a refused one, so it
// must keep the documents that manifest describes too.
func TestFailedManifestWriteKeepsTheDocumentsItWouldPrune(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	writeSeedPruneFiles(t, repoDir, map[string]string{"docs/drafts/draft.md": "# Draft\n\nAn untracked zebra pipeline note.\n"})
	outputDir := filepath.Join(t.TempDir(), "brain")
	seedPruneRun(t, repoDir, outputDir, "--worktree")
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)

	if err := os.RemoveAll(filepath.Join(repoDir, "docs")); err != nil {
		t.Fatal(err)
	}
	fileIndex := filepath.Join(outputDir, seedDirName, "file-index.json")
	runner := &seedStatusHookRunner{hook: func() bool {
		// Once this refresh has written its artifacts, the next git status
		// is the final worktree check, the last step before the manifest
		// write. Breaking the manifest there makes that write fail.
		data, err := os.ReadFile(fileIndex)
		if err != nil || strings.Contains(string(data), "draft.md") {
			return false
		}
		writeSeedPruneFiles(t, outputDir, map[string]string{bmanifest.FileName: "{not json"})
		return true
	}}
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: runner}
	out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--output", outputDir, "--worktree", "--update", repoDir)
	if !runner.fired {
		t.Fatalf("the manifest was never broken during the refresh: %v\n%s", err, out)
	}
	if err == nil {
		t.Fatalf("refresh succeeded with a broken manifest:\n%s", out)
	}
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)
}

// TestReadmeFailureAfterTheManifestLeavesThePrunePending: the README is
// written after the manifest, so a refresh whose README write fails has
// already saved a manifest that counts as current, without pruning. The
// pending marker stays, so the old copy is not left behind for good, and the
// next refresh that gets through prunes it and clears the marker.
func TestReadmeFailureAfterTheManifestLeavesThePrunePending(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":   "# readme\n",
		"docs/old.md": "# Old guide\n",
	})
	outputDir := filepath.Join(t.TempDir(), "brain")
	seedPruneRun(t, repoDir, outputDir)
	if seedPrunePending(outputDir) {
		t.Fatal("a seed that finished left its prune marked pending")
	}
	gitAt(t, repoDir, "rm", "-q", "docs/old.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop old doc")
	readme := filepath.Join(outputDir, exportReadmeFileName)
	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(readme, 0o700); err != nil {
		t.Fatal(err)
	}

	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: ExecRunner{}}
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--output", outputDir, "--update", repoDir); err == nil {
		t.Fatalf("refresh succeeded with a README it cannot write:\n%s", out)
	}
	manifest, err := bmanifest.Load(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(string(clibase.GitOutput(context.Background(), ExecRunner{}, repoDir, "rev-parse", "HEAD"))); manifest.Sources == nil || manifest.Sources.Seed == nil || manifest.Sources.Seed.Commit != head {
		t.Fatalf("the manifest was not saved before the README failed: %+v", manifest.Sources)
	}
	if !seedPrunePending(outputDir) {
		t.Error("the prune that did not run is not marked pending")
	}

	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	seedPruneRun(t, repoDir, outputDir, "--update")
	assertSeedDoc(t, outputDir, "docs/old.md", false)
	if seedPrunePending(outputDir) {
		t.Error("the prune is still marked pending after a refresh finished it")
	}
}

// TestRefreshReseedsWhileAPruneIsPending: a current manifest is no reason to
// skip the seed while its prune is marked pending.
func TestRefreshReseedsWhileAPruneIsPending(t *testing.T) {
	opts, brainDir := refreshFailureFixture(t)
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(seedPrunePendingPath)), []byte(seedPrunePendingText), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := runRefresh(context.Background(), cmd, opts, refreshCommandOptions{
		outputDir: defaultExportDir, checkpointLimit: defaultCheckpointLimit,
		entireBinary: "entire-test", graphBinary: "entire", scope: exportScopeAll,
		skipSessions: true,
		seed:         seedCommandOptions{agent: "none", agentTimeoutAction: "keep-quick", format: "markdown+json", maxFileBytes: defaultSeedMaxFileBytes, maxFiles: defaultSeedMaxFiles},
	}); err != nil {
		t.Fatalf("refresh: %v\n%s", err, out.String())
	}
	if seedPrunePending(brainDir) {
		t.Errorf("refresh skipped the seed although its prune was marked pending:\n%s", out.String())
	}
}

// TestPruneWarningsReachTheManifestAndRisks: the prune runs after the
// manifest and seed/risks.md are written, so what it could not remove is
// recorded by writing both again.
func TestPruneWarningsReachTheManifestAndRisks(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	outputDir := filepath.Join(t.TempDir(), "brain")
	seedPruneRun(t, repoDir, outputDir)
	outside := filepath.Join(t.TempDir(), "secret.md")
	writeSeedPruneFiles(t, filepath.Dir(outside), map[string]string{"secret.md": "outside\n"})
	if err := os.Symlink(outside, filepath.Join(outputDir, seedDirName, seedDocsDirName, "link.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	seedPruneRun(t, repoDir, outputDir, "--update")
	manifest, err := bmanifest.Load(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil || !strings.Contains(strings.Join(manifest.Sources.Seed.Warnings, "\n"), "seed/docs/link.md") {
		t.Errorf("the symlink the prune left is not in the manifest: %+v", manifest.Sources)
	}
	risks, err := os.ReadFile(filepath.Join(outputDir, seedDirName, "risks.md"))
	if err != nil || !strings.Contains(string(risks), "seed/docs/link.md") {
		t.Errorf("the symlink the prune left is not in seed/risks.md: %v\n%s", err, risks)
	}
}

// seedStatusHookRunner runs git for real, calling hook before each git status
// until it reports that it acted.
type seedStatusHookRunner struct {
	ExecRunner
	hook  func() bool
	fired bool
}

func (r *seedStatusHookRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if !r.fired && name == "git" && len(args) > 0 && args[0] == "status" {
		r.fired = r.hook()
	}
	return r.ExecRunner.Run(ctx, dir, name, args...)
}

// TestUpdateIntoTheRepositoryKeepsItsOwnSeedDocs: --update skips the output
// checks a fresh or --force seed makes, so it can seed into the repository
// itself. Its own files under seed/docs are not this seed's to prune.
func TestUpdateIntoTheRepositoryKeepsItsOwnSeedDocs(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"seed/docs/README.md": "# tracked\n"})
	seedPruneRun(t, repoDir, repoDir, "--update")
	got, err := os.ReadFile(filepath.Join(repoDir, "seed", "docs", "README.md"))
	if err != nil || string(got) != "# tracked\n" {
		t.Errorf("the repository's own seed/docs/README.md = %q, %v; want it untouched", got, err)
	}
}

// TestSeedDocsOverlapRepositoryComparesDirectoriesNotSpellings: the overlap
// check holds for a seed/docs reached through a symlink into the repository,
// and, where the filesystem folds case, for a repository spelled in another
// case.
func TestSeedDocsOverlapRepositoryComparesDirectoriesNotSpellings(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repoDir := filepath.Join(base, "Repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "brain")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	docs := func(outputDir string) string { return filepath.Join(outputDir, seedDirName, seedDocsDirName) }
	if !seedDocsOverlapRepository(docs(repoDir), repoDir) {
		t.Error("seed/docs inside the repository is not an overlap")
	}
	if seedDocsOverlapRepository(docs(elsewhere), repoDir) {
		t.Error("seed/docs outside the repository is an overlap")
	}
	if seedDocsOverlapRepository(docs(repoDir), "") {
		t.Error("no repository is an overlap")
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(repoDir, "sub"), link); err == nil {
		if !seedDocsOverlapRepository(docs(link), repoDir) {
			t.Error("seed/docs reached through a symlink into the repository is not an overlap")
		}
	}
	lower := filepath.Join(base, "repo")
	if _, err := os.Stat(lower); err != nil {
		t.Log("filesystem is case-sensitive; the other-case spelling is not checked")
		return
	}
	if !seedDocsOverlapRepository(docs(lower), repoDir) {
		t.Error("seed/docs under the repository spelled in another case is not an overlap")
	}
}

// TestSeedDocThatCannotBeReadIsDroppedNotSilentlySkipped: a document whose
// source read fails is reported and taken out of the file index and the
// seed's documents, so the prune that removes its earlier copy never leaves it
// recorded as seeded with nothing on disk. The source is a directory: it
// passes the path checks before the read, as a missing file would not, and
// the regular-file read then refuses it.
func TestSeedDocThatCannotBeReadIsDroppedNotSilentlySkipped(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docs", "gone.md"), 0o700); err != nil {
		t.Fatal(err)
	}
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
	// As runSeed does once the refresh commits.
	pruneSeedDocs(outputDir, scan, scan.writtenDocs)
	if scan.Docs[0].SeedPath != "" {
		t.Errorf("an unreadable document kept its seed path %q", scan.Docs[0].SeedPath)
	}
	if scan.Files[0].Included {
		t.Errorf("an unreadable document is still in the file index as included: %+v", scan.Files[0])
	}
	if !strings.Contains(strings.Join(scan.Warnings, "\n"), "document not indexed: docs/gone.md: not read:") {
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
	t.Run("second name that lowercasing does not match", func(t *testing.T) {
		// What APFS gives a directory renamed from Σ to ς: one directory,
		// listed under its old spelling, while strings.ToLower maps Σ to σ.
		outputDir := t.TempDir()
		docs := filepath.Join(outputDir, seedDirName, seedDocsDirName)
		writeSeedPruneFiles(t, docs, map[string]string{"ς/guide.md": "# Guide\n"})
		if err := os.MkdirAll(filepath.Join(docs, "Σ"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(filepath.Join(docs, "ς", "guide.md"), filepath.Join(docs, "Σ", "guide.md")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		scan := &seedScanResult{}
		pruneSeedDocs(outputDir, scan, map[string]bool{"seed/docs/ς/guide.md": true})
		assertSeedDoc(t, outputDir, "ς/guide.md", true)
		assertSeedDoc(t, outputDir, "Σ/guide.md", true)
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
