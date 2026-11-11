package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
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

// seedPruneBrainStore returns options for seeding repoDir into its brain
// store, and the store's directory.
func seedPruneBrainStore(t *testing.T, repoDir string) (Options, string) {
	t.Helper()
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: ExecRunner{}}
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

// assertSeedWarns checks that the seed manifest in outputDir records a
// warning naming text.
func assertSeedWarns(t *testing.T, outputDir, text string) {
	t.Helper()
	manifest, err := bmanifest.Load(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil || !strings.Contains(strings.Join(manifest.Sources.Seed.Warnings, "\n"), text) {
		t.Errorf("the seed manifest records no warning naming %s: %+v", text, manifest.Sources)
	}
}

// writeAndPruneSeedDoc copies the one document rel from repoDir into
// outputDir's seed, and then prunes as runSeed does once the refresh commits.
func writeAndPruneSeedDoc(t *testing.T, repoDir, outputDir, rel string) *seedScanResult {
	t.Helper()
	scan := &seedScanResult{
		RepoDir: repoDir,
		Files:   []seedFileIndexEntry{{Path: rel, Included: true, Reason: "included"}},
		Docs:    []seedDocument{{Path: rel, SeedPath: "seed/docs/" + rel, Reason: "included"}},
	}
	if err := writeSeedDocs(outputDir, scan); err != nil {
		t.Fatalf("write seed docs: %v", err)
	}
	pruneSeedDocs(outputDir, scan, scan.writtenDocs, seedDocsOwnership{all: true})
	return scan
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
	draft := map[string]string{"docs/drafts/draft.md": "# Draft\n\nAn untracked zebra pipeline note.\n"}
	outputDir, out, err := refreshAfterDeletingASeededDraft(t, draft, func(repoDir, _ string) {
		writeSeedPruneFiles(t, repoDir, draft)
	})
	if err == nil || !strings.Contains(err.Error(), "worktree_changed") {
		t.Fatalf("refresh = %v\n%s; want worktree_changed", err, out)
	}
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)
}

// TestFailedManifestWriteKeepsTheDocumentsItWouldPrune: a refresh whose
// manifest write fails keeps the previous manifest, like a refused one, so it
// must keep the documents that manifest describes too.
func TestFailedManifestWriteKeepsTheDocumentsItWouldPrune(t *testing.T) {
	draft := map[string]string{"docs/drafts/draft.md": "# Draft\n\nAn untracked zebra pipeline note.\n"}
	outputDir, out, err := refreshAfterDeletingASeededDraft(t, draft, func(_, outputDir string) {
		// The final worktree check is the last step before the manifest
		// write, so breaking the manifest here makes that write fail.
		writeSeedPruneFiles(t, outputDir, map[string]string{bmanifest.FileName: "{not json"})
	})
	if err == nil {
		t.Fatalf("refresh succeeded with a broken manifest:\n%s", out)
	}
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)
}

// refreshAfterDeletingASeededDraft seeds a repository with --worktree while
// draft is untracked in it, deletes the draft, and runs a --worktree --update
// refresh that calls act just before its final worktree check. By then the
// refresh has written its artifacts: the file index no longer lists the
// draft.
func refreshAfterDeletingASeededDraft(t *testing.T, draft map[string]string, act func(repoDir, outputDir string)) (string, string, error) {
	t.Helper()
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	writeSeedPruneFiles(t, repoDir, draft)
	outputDir := filepath.Join(t.TempDir(), "brain")
	seedPruneRun(t, repoDir, outputDir, "--worktree")
	assertSeedDoc(t, outputDir, "docs/drafts/draft.md", true)

	if err := os.RemoveAll(filepath.Join(repoDir, "docs")); err != nil {
		t.Fatal(err)
	}
	fileIndex := filepath.Join(outputDir, seedDirName, "file-index.json")
	runner := &seedStatusHookRunner{hook: func() bool {
		data, err := os.ReadFile(fileIndex)
		if err != nil || strings.Contains(string(data), "draft.md") {
			return false
		}
		act(repoDir, outputDir)
		return true
	}}
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: runner}
	out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--output", outputDir, "--worktree", "--update", repoDir)
	if !runner.fired {
		t.Fatalf("the refresh never reached its final worktree check: %v\n%s", err, out)
	}
	return outputDir, out, err
}

// TestReadmeFailureAfterTheManifestLeavesThePrunePending: the README is
// written after the manifest, so a refresh whose README write fails has
// already saved a manifest that counts as current, without pruning. In the
// brain store the pending marker stays, so the old copy is not left behind
// for good, and the next refresh that gets through prunes it and clears the
// marker. That holds as well when an explicit --update --output names the
// brain store under another spelling: the ordinary refresh after it reads the
// same directory.
func TestReadmeFailureAfterTheManifestLeavesThePrunePending(t *testing.T) {
	t.Run("brain store", func(t *testing.T) {
		readmeFailureLeavesThePrunePending(t, false)
	})
	t.Run("explicit output naming the brain store", func(t *testing.T) {
		readmeFailureLeavesThePrunePending(t, true)
	})
}

func readmeFailureLeavesThePrunePending(t *testing.T, explicit bool) {
	t.Helper()
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":   "# readme\n",
		"docs/old.md": "# Old guide\n",
	})
	opts, brainDir := seedPruneBrainStore(t, repoDir)
	ordinary := []string{"refresh", "seed", "--agent", "none"}
	args := ordinary
	if explicit {
		if err := os.MkdirAll(brainDir, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(filepath.Dir(brainDir), alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		args = append(slices.Clone(ordinary), "--update", "--output", filepath.Join(alias, filepath.Base(brainDir)))
	}
	seedWith := func(argv []string) error {
		t.Helper()
		out, err := execute(t, NewRootCommand(opts), append(slices.Clone(argv), repoDir)...)
		if err != nil {
			t.Logf("seed: %v\n%s", err, out)
		}
		return err
	}
	seed := func() error {
		t.Helper()
		return seedWith(args)
	}
	if err := seed(); err != nil {
		t.Fatal(err)
	}
	assertSeedDoc(t, brainDir, "docs/old.md", true)
	if seedPrunePending(brainDir) {
		t.Fatal("a seed that finished left its prune marked pending")
	}
	gitAt(t, repoDir, "rm", "-q", "docs/old.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop old doc")
	readme := filepath.Join(brainDir, exportReadmeFileName)
	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(readme, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := seed(); err == nil {
		t.Fatal("refresh succeeded with a README it cannot write")
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(string(clibase.GitOutput(context.Background(), ExecRunner{}, repoDir, "rev-parse", "HEAD"))); manifest.Sources == nil || manifest.Sources.Seed == nil || manifest.Sources.Seed.Commit != head {
		t.Fatalf("the manifest was not saved before the README failed: %+v", manifest.Sources)
	}
	if !seedPrunePending(brainDir) {
		t.Error("the prune that did not run is not marked pending")
	}

	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	if err := seedWith(ordinary); err != nil {
		t.Fatal(err)
	}
	assertSeedDoc(t, brainDir, "docs/old.md", false)
	if seedPrunePending(brainDir) {
		t.Error("the prune is still marked pending after a refresh finished it")
	}
}

// TestAnExplicitOutputNamingTheBrainStoreWaitsForItsWriteLock: a seed whose
// --output names the brain store prunes the store's documents, so it waits for
// the store's write lock as an ordinary seed does. It used to prune while
// another writer held the lock.
func TestAnExplicitOutputNamingTheBrainStoreWaitsForItsWriteLock(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":   "# readme\n",
		"docs/old.md": "# Old guide\n",
	})
	opts, brainDir := seedPruneBrainStore(t, repoDir)
	seed := func(args ...string) error {
		argv := append([]string{"refresh", "seed", "--agent", "none"}, args...)
		out, err := execute(t, NewRootCommand(opts), append(argv, repoDir)...)
		if err != nil {
			t.Logf("seed: %v\n%s", err, out)
		}
		return err
	}
	if err := seed(); err != nil {
		t.Fatal(err)
	}
	assertSeedDoc(t, brainDir, "docs/old.md", true)
	gitAt(t, repoDir, "rm", "-q", "docs/old.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop old doc")

	unlock, err := brainstore.AcquireBrainWriteLockContext(context.Background(), brainDir)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- seed("--update", "--output", brainDir) }()
	select {
	case err := <-done:
		t.Fatalf("the seed finished while another writer held the brain write lock: %v", err)
	case <-time.After(time.Second):
	}
	assertSeedDoc(t, brainDir, "docs/old.md", true)
	unlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the seed did not finish once the brain write lock was released")
	}
	assertSeedDoc(t, brainDir, "docs/old.md", false)
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
	assertSeedWarns(t, outputDir, "seed/docs/link.md")
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
	repoDir := seedPruneRepo(t, map[string]string{
		"seed/docs/README.md": "# tracked\n",
		seedPrunePendingPath:  "a tracked file that only shares the marker's name\n",
	})
	seedPruneRun(t, repoDir, repoDir, "--update")
	got, err := os.ReadFile(filepath.Join(repoDir, "seed", "docs", "README.md"))
	if err != nil || string(got) != "# tracked\n" {
		t.Errorf("the repository's own seed/docs/README.md = %q, %v; want it untouched", got, err)
	}
	// The pending marker is the brain store's alone.
	got, err = os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(seedPrunePendingPath)))
	if err != nil || string(got) != "a tracked file that only shares the marker's name\n" {
		t.Errorf("the repository's own %s = %q, %v; want it untouched", seedPrunePendingPath, got, err)
	}
}

// TestUpdateIntoAnotherDirectoryRemovesOnlyWhatASeedThereRecorded: --update
// accepts any existing directory, such as another repository with files of
// its own under seed/docs. Only the documents an earlier seed of that output
// recorded in its manifest are a seed's to remove there; the rest, and an
// empty directory no removal emptied, stay, and are reported. That holds on
// every later --update too, not only on the first, for a hard link to a
// document the seed recorded, and for the directory's own file named like the
// pending marker, which the seed neither reads as a list nor replaces.
func TestUpdateIntoAnotherDirectoryRemovesOnlyWhatASeedThereRecorded(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":   "# readme\n",
		"docs/old.md": "# Old guide\n",
	})
	theirs := map[string]string{
		"seed/docs/notes/theirs.md": "# Not a seed's\n",
		seedPrunePendingPath:        "seed/docs/notes/theirs.md is this repository's own\n",
	}
	outputDir := seedPruneRepo(t, theirs)
	if err := os.MkdirAll(filepath.Join(outputDir, seedDirName, seedDocsDirName, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	seedPruneRun(t, repoDir, outputDir, "--update")
	assertSeedDoc(t, outputDir, "docs/old.md", true)
	assertSeedDoc(t, outputDir, "notes/theirs.md", true)
	assertSeedWarns(t, outputDir, "seed/docs/notes/theirs.md")
	docs := filepath.Join(outputDir, seedDirName, seedDocsDirName)
	linked := os.Link(filepath.Join(docs, "docs", "old.md"), filepath.Join(docs, "notes", "linked.md")) == nil

	gitAt(t, repoDir, "rm", "-q", "docs/old.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop old doc")
	seedPruneRun(t, repoDir, outputDir, "--update")
	assertSeedDoc(t, outputDir, "docs/old.md", false)
	assertSeedDoc(t, outputDir, "docs", false)
	assertSeedDoc(t, outputDir, "README.md", true)
	assertSeedDoc(t, outputDir, "empty", true)
	if linked {
		assertSeedDoc(t, outputDir, "notes/linked.md", true)
	}
	for rel, want := range theirs {
		if got, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(rel))); err != nil || string(got) != want {
			t.Errorf("the output's own %s = %q, %v; want it untouched", rel, got, err)
		}
	}
}

// TestAForeignMarkerIsReportedWhenEveryRemovalWorks: an output's own
// seed/prune-pending leaves a seed no marker to record a failed removal in.
// The seed says so in its manifest and seed/risks.md, also when its prune
// has nothing else to report.
func TestAForeignMarkerIsReportedWhenEveryRemovalWorks(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":   "# readme\n",
		"docs/old.md": "# Old guide\n",
	})
	outputDir := seedPruneRepo(t, map[string]string{seedPrunePendingPath: "this repository's own\n"})
	seedPruneRun(t, repoDir, outputDir, "--update")
	gitAt(t, repoDir, "rm", "-q", "docs/old.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop old doc")
	seedPruneRun(t, repoDir, outputDir, "--update")
	assertSeedDoc(t, outputDir, "docs/old.md", false)
	assertSeedWarns(t, outputDir, "not recorded for a retry")
	if risks, err := os.ReadFile(filepath.Join(outputDir, seedDirName, "risks.md")); err != nil || !strings.Contains(string(risks), "not recorded for a retry") {
		t.Errorf("seed/risks.md does not say the marker is not a seed's: %v\n%s", err, risks)
	}
	if got, err := os.ReadFile(filepath.Join(outputDir, seedPrunePendingPath)); err != nil || string(got) != "this repository's own\n" {
		t.Errorf("the output's own %s = %q, %v; want it untouched", seedPrunePendingPath, got, err)
	}
}

// TestAPruneThatCouldNotRemoveADocumentStaysPending: a stale document the
// prune could not remove, for a reason that can pass, keeps the pending
// marker. In the brain store refresh then reseeds
// (TestRefreshReseedsWhileAPruneIsPending) instead of skipping a seed whose
// manifest counts as current; in any other --output the marker keeps the
// document the seed's to remove, although the new manifest no longer lists
// it. Either way the seed that can remove the document clears the marker.
// Root removes files from a read-only directory, and Windows does not stop
// it, so the test cannot fail the removal there.
func TestAPruneThatCouldNotRemoveADocumentStaysPending(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod read-only directory semantics are not portable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only directory")
	}
	t.Run("brain store", func(t *testing.T) {
		removalFailureLeavesThePrunePending(t, false)
	})
	t.Run("another output", func(t *testing.T) {
		removalFailureLeavesThePrunePending(t, true)
	})
}

func removalFailureLeavesThePrunePending(t *testing.T, explicit bool) {
	t.Helper()
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":       "# readme\n",
		"docs/old/old.md": "# Old guide\n\nDeploy with the retired zebra pipeline.\n",
		"docs/gone.md":    "# Gone\n",
	})
	opts, outputDir := seedPruneBrainStore(t, repoDir)
	args := []string{"refresh", "seed", "--agent", "none"}
	pending := func() bool { return seedPrunePending(outputDir) }
	if explicit {
		outputDir = filepath.Join(t.TempDir(), "out")
		args = append(args, "--update", "--output", outputDir)
		// The marker lists just the document the prune left: the one it
		// removed is no longer the seed's.
		pending = func() bool {
			listed, marker := readSeedPruneMarker(outputDir)
			return marker == seedPruneMarkerOurs && slices.Equal(listed, []string{"seed/docs/docs/old/old.md"})
		}
	}
	seed := func() {
		t.Helper()
		if out, err := execute(t, NewRootCommand(opts), append(slices.Clone(args), repoDir)...); err != nil {
			t.Fatalf("seed: %v\n%s", err, out)
		}
	}
	seed()
	assertSeedDoc(t, outputDir, "docs/old/old.md", true)
	gitAt(t, repoDir, "rm", "-q", "docs/old/old.md", "docs/gone.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop old docs")
	locked := filepath.Join(outputDir, seedDirName, seedDocsDirName, "docs", "old")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	seed()
	assertSeedDoc(t, outputDir, "docs/old/old.md", true)
	assertSeedDoc(t, outputDir, "docs/gone.md", false)
	if !pending() {
		listed, marker := readSeedPruneMarker(outputDir)
		t.Fatalf("a prune that could not remove a document did not leave just it pending (%q, %v)", listed, marker)
	}

	if err := os.Chmod(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	seed()
	assertSeedDoc(t, outputDir, "docs/old/old.md", false)
	assertDocIndexLacks(t, outputDir, "zebra pipeline")
	if _, err := os.Lstat(filepath.Join(outputDir, filepath.FromSlash(seedPrunePendingPath))); err == nil {
		t.Error("the prune is still marked pending after a seed finished it")
	}
}

// TestReadmeFailureInAnotherOutputKeepsItsOldDocumentOwned: in an --output
// other than the brain store, a seed whose README write fails has saved a
// manifest that no longer lists the old document, without pruning it. The
// marker lists it, so the next seed there still removes it.
func TestReadmeFailureInAnotherOutputKeepsItsOldDocumentOwned(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":   "# readme\n",
		"docs/old.md": "# Old guide\n",
	})
	outputDir := filepath.Join(t.TempDir(), "out")
	seedPruneRun(t, repoDir, outputDir)
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
		t.Fatalf("seed succeeded with a README it cannot write:\n%s", out)
	}
	if listed, marker := readSeedPruneMarker(outputDir); marker != seedPruneMarkerOurs || !slices.Contains(listed, "seed/docs/docs/old.md") {
		t.Fatalf("the marker = %q, %v; want it to list the old document", listed, marker)
	}

	if err := os.Remove(readme); err != nil {
		t.Fatal(err)
	}
	seedPruneRun(t, repoDir, outputDir, "--update")
	assertSeedDoc(t, outputDir, "docs/old.md", false)
	if _, marker := readSeedPruneMarker(outputDir); marker != seedPruneMarkerAbsent {
		t.Errorf("the marker is still there after a seed removed what it listed (%v)", marker)
	}
}

// TestFailedManifestWriteInAnotherOutputKeepsWhatItWroteOwned: in an
// --output other than the brain store, a seed whose manifest write fails has
// written documents that the manifest there does not list. The marker lists
// them, so a later seed that no longer has one still removes its copy.
func TestFailedManifestWriteInAnotherOutputKeepsWhatItWroteOwned(t *testing.T) {
	repoDir := seedPruneRepo(t, map[string]string{"README.md": "# readme\n"})
	outputDir := filepath.Join(t.TempDir(), "out")
	seedPruneRun(t, repoDir, outputDir, "--worktree")
	writeSeedPruneFiles(t, repoDir, map[string]string{"docs/new.md": "# New\n\nThe zebra pipeline.\n"})

	manifestPath := filepath.Join(outputDir, bmanifest.FileName)
	saved, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	fileIndex := filepath.Join(outputDir, seedDirName, "file-index.json")
	runner := &seedStatusHookRunner{hook: func() bool {
		// The final worktree check is the last step before the manifest
		// write, so breaking the manifest here makes that write fail.
		data, err := os.ReadFile(fileIndex)
		if err != nil || !strings.Contains(string(data), "new.md") {
			return false
		}
		writeSeedPruneFiles(t, outputDir, map[string]string{bmanifest.FileName: "{not json"})
		return true
	}}
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: runner}
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--output", outputDir, "--worktree", "--update", repoDir); err == nil || !runner.fired {
		t.Fatalf("the seed did not fail at its manifest write (fired %v): %v\n%s", runner.fired, err, out)
	}
	assertSeedDoc(t, outputDir, "docs/new.md", true)
	if listed, marker := readSeedPruneMarker(outputDir); marker != seedPruneMarkerOurs || !slices.Contains(listed, "seed/docs/docs/new.md") {
		t.Fatalf("the marker = %q, %v; want it to list the document the failed seed wrote", listed, marker)
	}

	if err := os.WriteFile(manifestPath, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repoDir, "docs", "new.md")); err != nil {
		t.Fatal(err)
	}
	seedPruneRun(t, repoDir, outputDir, "--worktree", "--update")
	assertSeedDoc(t, outputDir, "docs/new.md", false)
	assertDocIndexLacks(t, outputDir, "zebra pipeline")
	if _, marker := readSeedPruneMarker(outputDir); marker != seedPruneMarkerAbsent {
		t.Errorf("the marker is still there after a seed removed what it listed (%v)", marker)
	}
}

// TestReadSeedPruneMarkerTakesOnlyItsOwnList: a file named like the marker is
// a list only in the form a seed writes, naming files under seed/docs.
func TestReadSeedPruneMarkerTakesOnlyItsOwnList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		listed     []string
		want       seedPruneMarker
	}{
		{name: "absent", want: seedPruneMarkerAbsent},
		{name: "list", text: seedPrunePendingListHeader + strconv.Quote("seed/docs/a.md") + "\n" + strconv.Quote("seed/docs/b c/d.md") + "\n", listed: []string{"seed/docs/a.md", "seed/docs/b c/d.md"}, want: seedPruneMarkerOurs},
		{name: "empty list", text: seedPrunePendingListHeader, want: seedPruneMarkerOurs},
		{name: "brain store text", text: seedPrunePendingText, want: seedPruneMarkerForeign},
		{name: "someone else's file", text: "tracked\n", want: seedPruneMarkerForeign},
		{name: "no header", text: strconv.Quote("seed/docs/a.md") + "\n", want: seedPruneMarkerForeign},
		{name: "unquoted path", text: seedPrunePendingListHeader + "seed/docs/a.md\n", want: seedPruneMarkerForeign},
		{name: "outside seed/docs", text: seedPrunePendingListHeader + strconv.Quote("seed/risks.md") + "\n", want: seedPruneMarkerForeign},
		{name: "climbs out", text: seedPrunePendingListHeader + strconv.Quote("seed/docs/../../x.md") + "\n", want: seedPruneMarkerForeign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outputDir := t.TempDir()
			if tc.text != "" {
				writeSeedPruneFiles(t, outputDir, map[string]string{seedPrunePendingPath: tc.text})
			}
			listed, marker := readSeedPruneMarker(outputDir)
			if marker != tc.want || !slices.Equal(listed, tc.listed) {
				t.Errorf("readSeedPruneMarker = %q, %v; want %q, %v", listed, marker, tc.listed, tc.want)
			}
		})
	}
}

// TestPruneRetriesWhenItCannotCheckSeedDocs: a filesystem error while
// checking the seed/docs path, unlike a symlink there, may pass, so the prune
// asks for a retry. Root reads any directory, and Windows does not stop it.
func TestPruneRetriesWhenItCannotCheckSeedDocs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod read-only directory semantics are not portable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permission")
	}
	t.Parallel()
	outputDir := t.TempDir()
	writeSeedPruneFiles(t, outputDir, map[string]string{"seed/docs/old.md": "# Old\n"})
	seedDir := filepath.Join(outputDir, seedDirName)
	if err := os.Chmod(seedDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(seedDir, 0o700) })
	scan := &seedScanResult{}
	if !pruneSeedDocs(outputDir, scan, map[string]bool{}, seedDocsOwnership{all: true}) {
		t.Errorf("a prune that could not check seed/docs did not ask for a retry: %q", scan.Warnings)
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
	scan := writeAndPruneSeedDoc(t, repoDir, outputDir, "docs/gone.md")
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
	scan := writeAndPruneSeedDoc(t, repoDir, outputDir, "docs/guide.md")
	got, err := os.ReadFile(filepath.Join(outputDir, "seed", "docs", "docs", "guide.md"))
	if err != nil || string(got) != "# renamed guide\n" {
		t.Errorf("the document written under its new spelling = %q, %v; want the renamed guide", got, err)
	}
	if len(scan.Warnings) > 0 {
		t.Errorf("warnings = %q, want none", scan.Warnings)
	}
}

// TestRemovalMatchesAnotherSpellingNotAHardLink: a recorded name that is not
// listed, on a case-insensitive filesystem, is the listed entry spelled
// another way. Its identity takes that entry, which folds to the same name,
// and never a hard link to it under another name, which the seed did not
// record. Here hard links stand in for the other spelling. The matcher for
// what a seed wrote, which only ever keeps files, takes identity alone.
func TestRemovalMatchesAnotherSpellingNotAHardLink(t *testing.T) {
	t.Parallel()
	outputDir := t.TempDir()
	docs := filepath.Join(outputDir, seedDirName, seedDocsDirName)
	writeSeedPruneFiles(t, docs, map[string]string{"docs/guide.md": "# Guide\n", "ς/a.md": "# A\n"})
	if err := os.MkdirAll(filepath.Join(docs, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(docs, "Σ"), 0o700); err != nil {
		t.Fatal(err)
	}
	for from, to := range map[string]string{"docs/guide.md": "docs/Guide.md", "ς/a.md": "Σ/a.md"} {
		if err := os.Link(filepath.Join(docs, filepath.FromSlash(from)), filepath.Join(docs, filepath.FromSlash(to))); err != nil {
			t.Skipf("no hard link to stand in for another spelling: %v", err)
		}
	}
	if err := os.Link(filepath.Join(docs, "docs", "guide.md"), filepath.Join(docs, "notes", "link.md")); err != nil {
		t.Fatal(err)
	}
	recorded := map[string]bool{"seed/docs/docs/Guide.md": true, "seed/docs/Σ/a.md": true}
	listed := map[string]bool{"seed/docs/docs/guide.md": true, "seed/docs/notes/link.md": true, "seed/docs/ς/a.md": true}
	removes := seedDocsMatcher(outputDir, recorded, listed)
	keeps := seedDocsMatcher(outputDir, recorded, nil)
	for rel, want := range map[string]bool{"seed/docs/docs/guide.md": true, "seed/docs/ς/a.md": true, "seed/docs/notes/link.md": false} {
		if got := removes(rel); got != want {
			t.Errorf("removal matcher(%s) = %v, want %v", rel, got, want)
		}
		if !keeps(rel) {
			t.Errorf("keep matcher(%s) = false, want true", rel)
		}
	}
}

// TestUpdateIntoAnotherDirectoryRemovesOnlyTheOtherSpelling runs where the
// filesystem folds case. A repository directory renamed only in case is
// written into the old directory, so the next manifest records a spelling
// the directory listing does not have. Once the document is gone, its copy
// under the old spelling is removed, and a hard link to it elsewhere, which
// no seed recorded, is not.
func TestUpdateIntoAnotherDirectoryRemovesOnlyTheOtherSpelling(t *testing.T) {
	probe := t.TempDir()
	if err := os.WriteFile(filepath.Join(probe, "case"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(probe, "CASE")); err != nil {
		t.Skip("filesystem is case-sensitive")
	}
	repoDir := seedPruneRepo(t, map[string]string{
		"README.md":            "# readme\n",
		"docs/Guides/guide.md": "# Guide\n\nThe zebra pipeline.\n",
	})
	outputDir := filepath.Join(t.TempDir(), "out")
	seedPruneRun(t, repoDir, outputDir, "--update")
	gitAt(t, repoDir, "mv", "docs/Guides", "docs/renaming")
	gitAt(t, repoDir, "mv", "docs/renaming", "docs/guides")
	gitAt(t, repoDir, "commit", "-q", "-m", "rename Guides to guides")
	seedPruneRun(t, repoDir, outputDir, "--update")
	docs := filepath.Join(outputDir, seedDirName, seedDocsDirName)
	if err := os.MkdirAll(filepath.Join(docs, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(docs, "docs", "guides", "guide.md"), filepath.Join(docs, "notes", "link.md")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	gitAt(t, repoDir, "rm", "-q", "docs/guides/guide.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "drop the guide")
	seedPruneRun(t, repoDir, outputDir, "--update")
	assertSeedDoc(t, outputDir, "docs/guides/guide.md", false)
	assertSeedDoc(t, outputDir, "notes/link.md", true)
}

// TestRemovalTakesNoOtherSpellingAfterAnIncompleteWalk: a recorded document
// in a directory the walk cannot read is not listed, though it is there. A
// hard link to it in another directory, whose path folds to the same name,
// is not the seed's, so the prune must not take it for the document's other
// spelling. Root reads any directory and Windows does not stop it, and on a
// case-insensitive filesystem the two directories are one.
func TestRemovalTakesNoOtherSpellingAfterAnIncompleteWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod read-only directory semantics are not portable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permission")
	}
	t.Parallel()
	outputDir := t.TempDir()
	docs := filepath.Join(outputDir, seedDirName, seedDocsDirName)
	writeSeedPruneFiles(t, docs, map[string]string{"Docs/guide.md": "# Guide\n"})
	if _, err := os.Stat(filepath.Join(docs, "docs")); err == nil {
		t.Skip("filesystem is case-insensitive")
	}
	if err := os.Mkdir(filepath.Join(docs, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(docs, "Docs", "guide.md"), filepath.Join(docs, "docs", "guide.md")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	unreadable := filepath.Join(docs, "Docs")
	if err := os.Chmod(unreadable, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })

	scan := &seedScanResult{}
	ownership := seedDocsOwnership{recorded: map[string]bool{"seed/docs/Docs/guide.md": true}}
	if !pruneSeedDocs(outputDir, scan, map[string]bool{}, ownership) {
		t.Errorf("a prune that could not read a directory did not ask for a retry: %q", scan.Warnings)
	}
	assertSeedDoc(t, outputDir, "docs/guide.md", true)
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
		if pruneSeedDocs(outputDir, scan, map[string]bool{"seed/docs/guide.md": true}, seedDocsOwnership{all: true}) {
			t.Error("a prune that left only what it refuses by design asked for a retry")
		}

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
		pruneSeedDocs(outputDir, scan, map[string]bool{"seed/docs/ς/guide.md": true}, seedDocsOwnership{all: true})
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
		pruneSeedDocs(outputDir, scan, map[string]bool{}, seedDocsOwnership{all: true})
		if _, err := os.Stat(filepath.Join(elsewhere, "docs", "old.md")); err != nil {
			t.Errorf("a file behind a symlinked seed directory was removed: %v", err)
		}
		if len(scan.Warnings) == 0 {
			t.Error("a symlinked seed directory was skipped without a warning")
		}
	})
}
