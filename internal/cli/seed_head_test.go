package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// Issue #327, the half the code index fix left behind: with someone working in
// the tree, seed refused to run, so the first commit made while the tree stayed
// dirty left the repository context on the old commit, the seed axis `stale`,
// and freshness `unsafe` until the tree was clean again.
//
// These run real git: what is under test is which BYTES the seed read, and a
// scripted runner has no worktree or object store to tell them apart.

func seedHeadTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repoDir := t.TempDir()
	gitAt(t, repoDir, "init", "-q")
	gitAt(t, repoDir, "config", "user.name", "Seed Test")
	gitAt(t, repoDir, "config", "user.email", "seed@example.com")
	gitAt(t, repoDir, "config", "commit.gpgsign", "false")
	// The fixture writes LF files straight into the worktree. With a global
	// core.autocrlf=true (the Windows runner default) a checkout of the same
	// commit writes CRLF, which is correct behaviour and a different hash.
	gitAt(t, repoDir, "config", "core.autocrlf", "false")
	writeSeedHeadFiles(t, repoDir, map[string]string{
		"README.md":      "# committed readme\n",
		"docs/guide.md":  "committed guide\n",
		"package.json":   `{"scripts":{"test":"committed-test"}}`,
		"src/main.go":    "package main\n\nfunc main() {}\n",
		"src/helpers.go": "package main\n",
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "initial")
	return repoDir
}

func writeSeedHeadFiles(t *testing.T, root string, files map[string]string) {
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

func gitOutputAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return string(out)
}

// dirtySeedHeadTree leaves every kind of uncommitted change a seed could
// absorb: an unstaged edit to a doc and to a manifest the scan parses, a
// deleted doc, a staged new doc, and an untracked doc.
func dirtySeedHeadTree(t *testing.T, repoDir string) {
	t.Helper()
	writeSeedHeadFiles(t, repoDir, map[string]string{
		"README.md":    "# UNCOMMITTED readme\n",
		"package.json": `{"scripts":{"test":"UNCOMMITTED-test"}}`,
		"staged.md":    "UNCOMMITTED staged doc\n",
		"untracked.md": "UNCOMMITTED untracked doc\n",
	})
	if err := os.Remove(filepath.Join(repoDir, "docs", "guide.md")); err != nil {
		t.Fatal(err)
	}
	gitAt(t, repoDir, "add", "staged.md")
}

func readSeedManifest(t *testing.T, outputDir string) bmanifest.SeedSource {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, bmanifest.FileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest exportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil {
		t.Fatalf("manifest has no seed source: %s", data)
	}
	return *manifest.Sources.Seed
}

// isolateSeedHeadTemp points the process temp directory at a test directory so
// a leftover checkout is visible. It sets the environment, so the test that
// calls it cannot run in parallel.
func isolateSeedHeadTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	return dir
}

func assertNoSeedHeadLeftovers(t *testing.T, tempDir string) {
	t.Helper()
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), seedHeadTempPrefix) {
			t.Errorf("the committed HEAD checkout was left behind: %s", entry.Name())
		}
	}
}

// TestDirtySeedReadsCommittedHEAD: a dirty tree seeds exactly what a clean
// checkout of the same commit seeds, and the repository it ran in is left as
// it was found.
func TestDirtySeedReadsCommittedHEAD(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	head := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD"))

	cleanDir := filepath.Join(t.TempDir(), "clean")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}
	clean := readSeedManifest(t, cleanDir)

	dirtySeedHeadTree(t, repoDir)
	statusBefore := gitOutputAt(t, repoDir, "status", "--porcelain", "--untracked-files=all")
	indexPath := filepath.Join(repoDir, ".git", "index")
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--output", dirtyDir, repoDir)
	if err != nil {
		t.Fatalf("a dirty tree must seed from committed HEAD, not refuse: %v\n%s", err, out)
	}
	if !strings.Contains(out, "source: committed HEAD "+shortCommitHash(head)) || !strings.Contains(out, "5 uncommitted paths left out") {
		t.Errorf("seed did not say it read committed HEAD and left the changes out:\n%s", out)
	}
	dirty := readSeedManifest(t, dirtyDir)

	if dirty.Commit != head {
		t.Errorf("seed commit = %q, want HEAD %q", dirty.Commit, head)
	}
	if dirty.WorktreeMode != "tracked" || dirty.WorktreeHash != "" {
		t.Errorf("a HEAD seed must read as committed state: mode=%q hash=%q", dirty.WorktreeMode, dirty.WorktreeHash)
	}
	// The fingerprint covers every file's hash, so equality means no byte of
	// the uncommitted tree reached the scan.
	if dirty.FileFingerprint != clean.FileFingerprint {
		t.Errorf("dirty-tree seed fingerprint %s differs from the clean seed of the same commit %s", dirty.FileFingerprint, clean.FileFingerprint)
	}

	readme, err := os.ReadFile(filepath.Join(dirtyDir, seedDirName, seedDocsDirName, "README.md"))
	if err != nil {
		t.Fatalf("read seeded README: %v", err)
	}
	if string(readme) != "# committed readme\n" {
		t.Errorf("seeded README = %q, want the committed bytes", readme)
	}
	if _, err := os.Stat(filepath.Join(dirtyDir, seedDirName, seedDocsDirName, "docs", "guide.md")); err != nil {
		t.Errorf("a doc deleted only in the worktree is still committed and must be seeded: %v", err)
	}
	for _, doc := range dirty.Documents {
		if doc.Path == "staged.md" || doc.Path == "untracked.md" {
			t.Errorf("uncommitted document %s was seeded", doc.Path)
		}
	}
	var testCommand string
	for _, command := range dirty.Commands {
		if command.Name == "npm run test" {
			testCommand = command.Command
		}
	}
	if testCommand != "committed-test" {
		t.Errorf("package.json scripts came from the worktree: npm run test = %q", testCommand)
	}

	// Read the index BEFORE asking git for status again: a plain `git status`
	// may refresh the index itself, which would blame the seed for it.
	indexAfter, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Error("seeding rewrote the repository's real index; the checkout must use a scratch index")
	}
	if statusAfter := gitOutputAt(t, repoDir, "status", "--porcelain", "--untracked-files=all"); statusAfter != statusBefore {
		t.Errorf("seeding changed the worktree status:\nbefore:\n%s\nafter:\n%s", statusBefore, statusAfter)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestCommitOnADirtyTreeKeepsTheSeedOnHEAD is the issue's own sequence: seed,
// start editing, commit something else while the edit is still open, refresh.
// The seed used to stay on the old commit and grade the axis `stale`.
func TestCommitOnADirtyTreeKeepsTheSeedOnHEAD(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", repoDir); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	writeSeedHeadFiles(t, repoDir, map[string]string{"README.md": "# still being edited\n"})
	writeSeedHeadFiles(t, repoDir, map[string]string{"docs/new.md": "committed while dirty\n"})
	gitAt(t, repoDir, "add", "docs/new.md")
	gitAt(t, repoDir, "commit", "-q", "-m", "second")
	head := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD"))

	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--update", repoDir); err != nil {
		t.Fatalf("reseed after a commit on a dirty tree: %v", err)
	}

	ctx := context.Background()
	storage, err := repoStoragePaths(ctx, opts.Runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := bmanifest.Load(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Seed == nil || manifest.Sources.Seed.Commit != head {
		t.Fatalf("the seed did not follow HEAD to %s: %+v", head, manifest)
	}
	live, err := brainLiveStateReport(ctx, opts.Runner, repoDir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	retrieval := buildBrainRetrievalStatus(ctx, opts.Runner, storage.BrainDir, repoDir, manifest, live, false)
	if axis := retrieval.Freshness.Axes["seed"]; axis.State != dirtyUnindexedAxisState {
		t.Errorf("seed axis = %q (%s), want %q: on HEAD, with the open edit excluded", axis.State, axis.Detail, dirtyUnindexedAxisState)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedWithNoCommitStillRefuses: an unborn branch has no committed
// state to fall back on, so "commit or stash them" is still the answer, worded
// exactly as before so setup's hint and watch's deferral keep matching it.
func TestDirtySeedWithNoCommitStillRefuses(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Parallel()
	repoDir := t.TempDir()
	gitAt(t, repoDir, "init", "-q")
	writeSeedHeadFiles(t, repoDir, map[string]string{"README.md": "# never committed\n"})
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: ExecRunner{}}

	_, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--output", filepath.Join(t.TempDir(), "seed"), repoDir)
	assertDirtyRefusalIsActionable(t, "seed on an unborn branch", err, "README.md")
	if strings.Contains(err.Error(), "could not be checked out") {
		t.Errorf("an unborn branch is not a checkout failure and must not be reported as one: %v", err)
	}
}

// TestSeedHeadRefusalKeepsTheMachineCode: when the checkout itself fails, the
// fallback is the old refusal, and it must still carry the prefix setup and
// watch key on, plus the reason the checkout was not used.
func TestSeedHeadRefusalKeepsTheMachineCode(t *testing.T) {
	t.Parallel()
	err := seedHeadRefusal(errors.New("disk full"), []string{"src/a.go"})
	message := err.Error()
	for _, want := range []string{brainstore.DirtyWorktreeCode + ":", "src/a.go", "disk full"} {
		if !strings.Contains(message, want) {
			t.Errorf("fallback refusal lost %q: %s", want, message)
		}
	}
	if !setupIsDirtyWorktree(message) {
		t.Errorf("setup no longer recognises the fallback refusal: %s", message)
	}
}

func TestChunkPathsByBytesBoundsEachCommandLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 40)
	paths := []string{"a", "b", long, "c", "d"}
	batches := chunkPathsByBytes(paths, 10)
	var flattened []string
	for _, batch := range batches {
		size := 0
		for _, path := range batch {
			size += len(path) + 1
		}
		if size > 10 && len(batch) > 1 {
			t.Errorf("batch %v is %d bytes, over the limit", batch, size)
		}
		flattened = append(flattened, batch...)
	}
	if strings.Join(flattened, ",") != strings.Join(paths, ",") {
		t.Errorf("chunking lost or reordered paths: %v", batches)
	}
	if len(chunkPathsByBytes(nil, 10)) != 0 {
		t.Error("no paths must produce no batches")
	}
}
