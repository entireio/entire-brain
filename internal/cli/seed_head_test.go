package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// TestDirtySeedIgnoresUncommittedGitattributes: checkout-index reads the
// WORKTREE's .gitattributes unless told otherwise, so an uncommitted
// working-tree-encoding rule re-encoded the committed README into UTF-16, which
// the scan then dropped as binary content.
func TestDirtySeedIgnoresUncommittedGitattributes(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	cleanDir := filepath.Join(t.TempDir(), "clean")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}
	clean := readSeedManifest(t, cleanDir)

	// An untracked root .gitattributes, and an edit to a committed nested one.
	writeSeedHeadFiles(t, repoDir, map[string]string{"docs/.gitattributes": "*.md text\n"})
	gitAt(t, repoDir, "add", "docs/.gitattributes")
	gitAt(t, repoDir, "commit", "-q", "-m", "nested attributes")
	cleanDir = filepath.Join(t.TempDir(), "clean-nested")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}
	clean = readSeedManifest(t, cleanDir)
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes":      "*.md working-tree-encoding=UTF-16LE\n",
		"docs/.gitattributes": "*.md working-tree-encoding=UTF-16LE\n",
	})
	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed with an untracked .gitattributes: %v\n%s", err, out)
	}
	readme, err := os.ReadFile(filepath.Join(dirtyDir, seedDirName, seedDocsDirName, "README.md"))
	if err != nil {
		t.Fatalf("the committed README was not seeded: %v", err)
	}
	if string(readme) != "# committed readme\n" {
		t.Errorf("seeded README = %q, want the committed bytes, not ones converted by uncommitted attributes", readme)
	}
	guide, err := os.ReadFile(filepath.Join(dirtyDir, seedDirName, seedDocsDirName, "docs", "guide.md"))
	if err != nil || string(guide) != "committed guide\n" {
		t.Errorf("seeded docs/guide.md = %q, %v; an edited nested .gitattributes must not convert it", guide, err)
	}
	if dirty := readSeedManifest(t, dirtyDir); dirty.FileFingerprint != clean.FileFingerprint {
		t.Errorf("uncommitted attributes changed what the seed read: fingerprint %s, clean %s", dirty.FileFingerprint, clean.FileFingerprint)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedChecksOutOnlyWhatTheScanNeeds: a file whose path or blob size
// settles its entry -- a denied path, an excluded test, a blob over
// --max-file-bytes that no conversion can shrink -- is never written into the
// checkout, and the scan still records it exactly as a scan of the clean tree
// does. Everything else is checked out, media included: the size of the
// checked-out file decides between "too large" and "binary or media", and a
// conversion can make it differ from the blob's.
func TestDirtySeedChecksOutOnlyWhatTheScanNeeds(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	statOnly := map[string]string{
		"vendor/lib.js": "module.exports = 1\n",
		".env":          "SECRET=committed\n",
		"data/big.txt":  strings.Repeat("big ", 40),
		// High-signal by name, but media: the scan only stats it.
		"README.png":       strings.Repeat("\x89PNG", 20),
		"src/main_test.go": "package main\n",
	}
	checkedOut := map[string]string{
		"docs/long.md": strings.Repeat("a long committed doc ", 10),
		"src/small.go": "package main\n",
		"logo.png":     "\x89PNG\r\n\x1a\n\x00\x00",
		// 50 bytes of LF lines as a blob, 75 once eol=crlf checks them out:
		// too large, not media, as in a clean checkout.
		"crlf.png": strings.Repeat("a\n", 25),
		// 40 Latin-1 bytes in the worktree, an 80-byte UTF-8 blob: over
		// --max-file-bytes as a blob, under it once checked out.
		"data/latin.txt": strings.Repeat("\xe9", 40),
	}
	writeSeedHeadFiles(t, repoDir, statOnly)
	writeSeedHeadFiles(t, repoDir, checkedOut)
	// A 40-byte blob checked out as 80 bytes of UTF-16: too large once
	// converted, not media, which is what its blob size alone says. It is
	// checked out with the other converted files and dropped there, at the
	// size it came out.
	writeSeedHeadFiles(t, repoDir, map[string]string{"wide.png": utf16LE(strings.Repeat("w", 40))})
	// Over --max-file-bytes itself, so in neither list.
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes": "data/latin.txt working-tree-encoding=ISO-8859-1\nwide.png working-tree-encoding=UTF-16LE\ncrlf.png text eol=crlf\n",
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "stat-only files")
	// Give the clean tree the CRLF file a checkout of the commit writes.
	if err := os.Remove(filepath.Join(repoDir, "crlf.png")); err != nil {
		t.Fatal(err)
	}
	gitAt(t, repoDir, "checkout", "--", "crlf.png")
	if info, err := os.Stat(filepath.Join(repoDir, "crlf.png")); err != nil || info.Size() != 75 {
		t.Fatalf("crlf.png was not checked out with CRLF line ends: %v, %v", info, err)
	}
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	cleanDir := filepath.Join(t.TempDir(), "clean")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}

	dirtySeedHeadTree(t, repoDir)
	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	for rel, content := range statOnly {
		if _, err := os.Lstat(filepath.Join(head.dir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s was written into the checkout, but the scan only stats it", rel)
		}
		if size, ok := head.unreadSize(rel); !ok || size != int64(len(content)) {
			t.Errorf("%s: unread size = %d, %v; want its committed size %d", rel, size, ok, len(content))
		}
	}
	for rel := range checkedOut {
		if _, err := os.Stat(filepath.Join(head.dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s must be checked out: %v", rel, err)
		}
	}
	if size, ok := head.unreadSize("wide.png"); !ok || size != 80 {
		t.Errorf("wide.png: unread size = %d, %v; want its checked-out size 80", size, ok)
	}
	head.remove()

	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed the dirty tree: %v\n%s", err, out)
	}
	cleanIndex, err := os.ReadFile(filepath.Join(cleanDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	dirtyIndex, err := os.ReadFile(filepath.Join(dirtyDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cleanIndex, dirtyIndex) {
		t.Errorf("the dirty-tree file index differs from the clean one of the same commit:\nclean:\n%s\ndirty:\n%s", cleanIndex, dirtyIndex)
	}
	if entry := seedIndexEntry(t, dirtyDir, "crlf.png"); entry.Reason != "too large" {
		t.Errorf("crlf.png = %+v; want too large at its checked-out size", entry)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedBoundsConvertedCheckouts: a file whose checkout converts is
// checked out so the scan sees its converted size, but one whose blob alone is
// more than seedHeadDriverMaxShrink times --max-file-bytes is past the cap, so
// it is never written out, and is reported too large at its blob size, with a
// warning. Here re-encoding only grows it, so the scan gives it the same
// reason, and so the same fingerprint, as a clean seed of the commit.
func TestDirtySeedBoundsConvertedCheckouts(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	huge := strings.Repeat("h", seedHeadDriverMaxShrink*64+1)
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes": "data/*.txt working-tree-encoding=UTF-16LE\n",
		"data/huge.txt":  utf16LE(huge),
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "converted files")
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	cleanDir := filepath.Join(t.TempDir(), "clean")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}

	dirtySeedHeadTree(t, repoDir)
	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	if _, err := os.Lstat(filepath.Join(head.dir, "data", "huge.txt")); err == nil {
		t.Error("a converted file past the bound was written into the checkout")
	}
	if size, ok := head.unreadSize("data/huge.txt"); !ok || size != int64(len(huge)) {
		t.Errorf("data/huge.txt: unread size = %d, %v; want its blob size %d", size, ok, len(huge))
	}
	if len(head.warnings) != 1 || !strings.Contains(head.warnings[0], "working-tree-encoding") || !strings.Contains(head.warnings[0], "data/huge.txt") {
		t.Errorf("warnings = %q; want the one for a converted file past the cap", head.warnings)
	}
	head.remove()

	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed the dirty tree: %v\n%s", err, out)
	}
	for name, dir := range map[string]string{"clean": cleanDir, "dirty": dirtyDir} {
		entry := seedIndexEntry(t, dir, "data/huge.txt")
		if entry.Included || entry.Reason != "too large" {
			t.Errorf("%s seed: data/huge.txt = %+v; want too large", name, entry)
		}
	}
	if clean, dirty := readSeedManifest(t, cleanDir), readSeedManifest(t, dirtyDir); dirty.FileFingerprint != clean.FileFingerprint {
		t.Errorf("dirty-tree seed fingerprint %s differs from the clean seed of the same commit %s", dirty.FileFingerprint, clean.FileFingerprint)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedChecksOutWhatAFilterDriverRuns: a filter driver's output can
// be smaller than its blob, so a file one converts is checked out up to
// seedHeadDriverMaxShrink times --max-file-bytes; a global driver that shrinks
// a large blob to a small file leaves it included, as in a clean seed. A
// filter whose only driver is repository scoped is blanked by the hardened
// runner, writes the blob unchanged, and so keeps the ordinary size rule: a
// large one is never checked out.
func TestDirtySeedChecksOutWhatAFilterDriverRuns(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	// clean inverts smudge, so git status keeps calling the squeezed
	// checkout clean even when it compares contents (a racily clean entry).
	config := "[filter \"squeeze\"]\n\tsmudge = tr -s x\n\tclean = \"cat >/dev/null; printf %0257d 0 | tr 0 x\"\n"
	if err := os.WriteFile(global, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	repoDir := seedHeadTestRepo(t)
	gitAt(t, repoDir, "config", "filter.local.smudge", "tr -s y")
	big := 4*64 + 1
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes":        "filtered/squeezed.txt filter=squeeze\nfiltered/local.txt filter=local\n",
		"filtered/squeezed.txt": strings.Repeat("x", big),
		"filtered/local.txt":    strings.Repeat("y", big),
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "filtered files")
	// A clean checkout of the commit runs the global driver. The repository
	// one is left out of it, as the hardened runner leaves it out of seed.
	if err := os.Remove(filepath.Join(repoDir, "filtered", "squeezed.txt")); err != nil {
		t.Fatal(err)
	}
	gitAt(t, repoDir, "checkout", "--", "filtered/squeezed.txt")
	if got, err := os.ReadFile(filepath.Join(repoDir, "filtered", "squeezed.txt")); err != nil || string(got) != "x" {
		t.Fatalf("the global driver did not run on checkout: %q, %v", got, err)
	}
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	cleanDir := filepath.Join(t.TempDir(), "clean")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}

	dirtySeedHeadTree(t, repoDir)
	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	if got, err := os.ReadFile(filepath.Join(head.dir, "filtered", "squeezed.txt")); err != nil || string(got) != "x" {
		t.Errorf("filtered/squeezed.txt = %q, %v; want the global driver's output", got, err)
	}
	if _, err := os.Lstat(filepath.Join(head.dir, "filtered", "local.txt")); err == nil {
		t.Error("a large file whose only driver is repository scoped was written into the checkout")
	}
	if size, ok := head.unreadSize("filtered/local.txt"); !ok || size != int64(big) {
		t.Errorf("filtered/local.txt: unread size = %d, %v; want its blob size %d", size, ok, big)
	}
	head.remove()

	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed the dirty tree: %v\n%s", err, out)
	}
	if entry := seedIndexEntry(t, dirtyDir, "filtered/squeezed.txt"); !entry.Included {
		t.Errorf("filtered/squeezed.txt = %+v; want it included, as in a clean seed", entry)
	}
	cleanIndex, err := os.ReadFile(filepath.Join(cleanDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	dirtyIndex, err := os.ReadFile(filepath.Join(dirtyDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cleanIndex, dirtyIndex) {
		t.Errorf("the dirty-tree file index differs from the clean one of the same commit:\nclean:\n%s\ndirty:\n%s", cleanIndex, dirtyIndex)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedCapsWhatAFilterDriverWrites: a blob more than
// seedHeadDriverMaxShrink times --max-file-bytes is never handed to its
// driver, so one checkout writes a bounded amount whatever the driver does.
// It is reported too large from the blob's size, with a warning: a clean seed
// would see the driver's output instead, and past the cap that is the price
// of the bound.
func TestDirtySeedCapsWhatAFilterDriverWrites(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[filter \"squeeze\"]\n\tsmudge = tr -s x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	repoDir := seedHeadTestRepo(t)
	vast := seedHeadDriverMaxShrink*64 + 1
	near := seedHeadDriverMaxShrink * 64
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes":    "filtered/*.txt filter=squeeze\n",
		"filtered/vast.txt": strings.Repeat("x", vast),
		"filtered/near.txt": strings.Repeat("x", near),
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "filtered files")
	dirtySeedHeadTree(t, repoDir)

	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	if _, err := os.Lstat(filepath.Join(head.dir, "filtered", "vast.txt")); err == nil {
		t.Error("a blob past the driver cap was written into the checkout")
	}
	if size, ok := head.unreadSize("filtered/vast.txt"); !ok || size != int64(vast) {
		t.Errorf("filtered/vast.txt: unread size = %d, %v; want its blob size %d", size, ok, vast)
	}
	if got, err := os.ReadFile(filepath.Join(head.dir, "filtered", "near.txt")); err != nil || string(got) != "x" {
		t.Errorf("filtered/near.txt = %q, %v; want the driver's output", got, err)
	}
	if len(head.warnings) != 1 || !strings.Contains(head.warnings[0], "1 committed files") || !strings.Contains(head.warnings[0], "filtered/vast.txt") {
		t.Errorf("warnings = %q; want one naming the file not run through its driver", head.warnings)
	}
	head.remove()

	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed the dirty tree: %v\n%s", err, out)
	}
	if entry := seedIndexEntry(t, dirtyDir, "filtered/vast.txt"); entry.Included || entry.Reason != "too large" || entry.Bytes != int64(vast) {
		t.Errorf("filtered/vast.txt = %+v; want too large at its blob size", entry)
	}
	if entry := seedIndexEntry(t, dirtyDir, "filtered/near.txt"); !entry.Included {
		t.Errorf("filtered/near.txt = %+v; want it included", entry)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedDropsWhatAFilterDriverGrows: a driver can write far more than
// its blob, so the checkout cannot bound a driver file by its blob size. Each
// one is checked out alone and removed at once when its real size puts it over
// --max-file-bytes, and the scan records that real size, exactly as a clean
// seed of the worktree the same driver wrote.
func TestDirtySeedDropsWhatAFilterDriverGrows(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	// clean undoes smudge, so git status calls the grown checkout clean.
	config := "[filter \"grow\"]\n\tsmudge = \"cat >/dev/null; printf %04096d 0\"\n\tclean = \"cat >/dev/null; printf x\"\n"
	if err := os.WriteFile(global, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	repoDir := seedHeadTestRepo(t)
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes": "grown/*.txt filter=grow\n",
		"grown/one.txt":  "x",
		"grown/two.txt":  "x",
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "grown files")
	for _, rel := range []string{"grown/one.txt", "grown/two.txt"} {
		if err := os.Remove(filepath.Join(repoDir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
		gitAt(t, repoDir, "checkout", "--", rel)
	}
	if info, err := os.Stat(filepath.Join(repoDir, "grown", "one.txt")); err != nil || info.Size() != 4096 {
		t.Fatalf("the driver did not grow the worktree file: %v, %v", info, err)
	}
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	cleanDir := filepath.Join(t.TempDir(), "clean")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}

	dirtySeedHeadTree(t, repoDir)
	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	for _, rel := range []string{"grown/one.txt", "grown/two.txt"} {
		if _, err := os.Lstat(filepath.Join(head.dir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s grew past --max-file-bytes and was left in the checkout", rel)
		}
		if size, ok := head.unreadSize(rel); !ok || size != 4096 {
			t.Errorf("%s: unread size = %d, %v; want the driver's output size 4096", rel, size, ok)
		}
	}
	head.remove()

	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed the dirty tree: %v\n%s", err, out)
	}
	cleanIndex, err := os.ReadFile(filepath.Join(cleanDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	dirtyIndex, err := os.ReadFile(filepath.Join(dirtyDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cleanIndex, dirtyIndex) {
		t.Errorf("the dirty-tree file index differs from the clean one of the same commit:\nclean:\n%s\ndirty:\n%s", cleanIndex, dirtyIndex)
	}
	if entry := seedIndexEntry(t, dirtyDir, "grown/one.txt"); entry.Reason != "too large" || entry.Bytes != 4096 {
		t.Errorf("grown/one.txt = %+v; want too large at the driver's output size", entry)
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedReportsAFilterDriverThatFails: the checkout runs drivers in an
// empty work tree so the worktree's attributes stay out, which breaks a
// global driver whose command names a repository-relative helper. git then
// writes the blob unconverted, and the seed says so instead of passing the
// unconverted file off silently. A README the scan reads at any size goes
// through the same driver checkout. A driver that converts the file and only
// prints progress is reported as output, not as a failure.
func TestDirtySeedReportsAFilterDriverThatFails(t *testing.T) {
	for _, tc := range []struct {
		name, path, smudge, want, warning string
	}{
		{name: "failing driver", path: "data/note.enc", smudge: "sh scripts/decode.sh", want: "aaa\n", warning: "a filter driver failed"},
		{name: "failing driver on a README", path: "README.enc", smudge: "sh scripts/decode.sh", want: "aaa\n", warning: "a filter driver failed"},
		{name: "driver printing progress", path: "data/note.enc", smudge: "echo decoding >&2; tr a b", want: "bbb\n", warning: "a filter driver printed to stderr checking out committed HEAD (1 of its checkout calls), last: decoding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateSeedHeadTemp(t)
			global := filepath.Join(t.TempDir(), "gitconfig")
			if err := os.WriteFile(global, []byte("[filter \"decode\"]\n\tsmudge = \""+tc.smudge+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			repoDir := seedHeadTestRepo(t)
			writeSeedHeadFiles(t, repoDir, map[string]string{
				".gitattributes":    tc.path + " filter=decode\n",
				"scripts/decode.sh": "tr a b\n",
				tc.path:             "aaa\n",
			})
			gitAt(t, repoDir, "add", "-A")
			gitAt(t, repoDir, "commit", "-q", "-m", "encoded file")
			dirtySeedHeadTree(t, repoDir)

			head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
			if err != nil {
				t.Fatalf("check out HEAD: %v", err)
			}
			defer head.remove()
			if got, err := os.ReadFile(filepath.Join(head.dir, filepath.FromSlash(tc.path))); err != nil || string(got) != tc.want {
				t.Fatalf("%s = %q, %v; want %q", tc.path, got, err, tc.want)
			}
			if len(head.warnings) != 1 || !strings.HasPrefix(head.warnings[0], tc.warning) {
				t.Errorf("warnings = %q; want one starting %q", head.warnings, tc.warning)
			}
		})
	}
}

// TestSeedHeadDriverStderrTellsAFailureFromOutput: only git's own failure
// line makes the warning say files are unconverted, wherever it falls.
func TestSeedHeadDriverStderrTellsAFailureFromOutput(t *testing.T) {
	t.Parallel()
	var quiet seedHeadDriverStderr
	quiet.add([]byte("  \n"))
	if got := quiet.warning(); got != "" {
		t.Errorf("blank stderr warning = %q, want none", got)
	}
	var failed seedHeadDriverStderr
	failed.add([]byte("progress 1/2\n"))
	failed.add([]byte("sh: cannot open decode.sh\nerror: external filter 'sh decode.sh' failed 2\nerror: external filter 'sh decode.sh' failed\n"))
	want := "a filter driver failed checking out committed HEAD (2 of its checkout calls printed to stderr), so some of its files are seeded unconverted: error: external filter 'sh decode.sh' failed 2"
	if got := failed.warning(); got != want {
		t.Errorf("warning = %q\nwant      %q", got, want)
	}
	// An unrecognized failure line still reaches the warning after progress.
	var localized seedHeadDriverStderr
	localized.add([]byte("progress 1/2\nerreur : le filtre externe 'sh decode.sh' a échoué\n"))
	want = "a filter driver printed to stderr checking out committed HEAD (1 of its checkout calls), last: erreur : le filtre externe 'sh decode.sh' a échoué"
	if got := localized.warning(); got != want {
		t.Errorf("warning = %q\nwant      %q", got, want)
	}
}

// gitConfigCountingRunner runs commands for real and records each git config
// and checkout-index call the checkout makes.
type gitConfigCountingRunner struct {
	ExecRunner
	configCalls   [][]string
	checkoutCalls [][]string
	checkoutEnvs  []map[string]string
}

func (r *gitConfigCountingRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" && len(args) > 0 {
		switch args[0] {
		case "config":
			r.configCalls = append(r.configCalls, args)
		case "checkout-index":
			r.checkoutCalls = append(r.checkoutCalls, args)
			r.checkoutEnvs = append(r.checkoutEnvs, env)
		}
	}
	return r.ExecRunner.RunWithEnv(ctx, dir, env, name, args...)
}

// TestDirtySeedReadsFilterDriversInOneCall: however many filters the commit
// names, the checkout reads the driver configuration once, and still tells a
// configured driver from a filter with none. Files a driver converts are
// checked out a batch at a time, not one call each.
func TestDirtySeedReadsFilterDriversInOneCall(t *testing.T) {
	isolateSeedHeadTemp(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[filter \"real\"]\n\tsmudge = tr -s z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	repoDir := seedHeadTestRepo(t)
	big := strings.Repeat("z", 4*64+1)
	files := map[string]string{".gitattributes": "real/* filter=real\n"}
	for i := range 12 {
		name := fmt.Sprintf("none%d", i)
		files[".gitattributes"] += name + "/* filter=" + name + "\n"
		files[name+"/big.txt"] = big
	}
	files["real/big.txt"] = big
	const driven = 2*seedHeadDriverBatchFiles + 1
	for i := range driven - 1 {
		files[fmt.Sprintf("real/more%02d.txt", i)] = big
	}
	writeSeedHeadFiles(t, repoDir, files)
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "filtered files")
	dirtySeedHeadTree(t, repoDir)

	runner := &gitConfigCountingRunner{}
	head, err := checkoutSeedHead(context.Background(), runner, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	if len(runner.configCalls) != 1 {
		t.Errorf("git config ran %d times for 13 filters; want once: %q", len(runner.configCalls), runner.configCalls)
	}
	if got, err := os.ReadFile(filepath.Join(head.dir, "real", "big.txt")); err != nil || string(got) != "z" {
		t.Errorf("real/big.txt = %q, %v; want its configured driver's output", got, err)
	}
	if _, ok := head.unreadSize("none0/big.txt"); !ok {
		t.Error("a large file whose filter has no driver was checked out")
	}
	drivenCalls := 0
	for i, args := range runner.checkoutCalls {
		if strings.HasPrefix(args[len(args)-1], "real/") {
			drivenCalls++
			// git's failure line has to be in English to be recognized.
			if got := runner.checkoutEnvs[i]["LANGUAGE"]; got != "en" {
				t.Errorf("driver checkout LANGUAGE = %q, want en", got)
			}
		}
	}
	if want := (driven + seedHeadDriverBatchFiles - 1) / seedHeadDriverBatchFiles; drivenCalls != want {
		t.Errorf("checkout-index ran %d times for %d driver files; want %d batches", drivenCalls, driven, want)
	}
}

// TestDropCheckedOutRecordsEveryPathThatReadsTheFile: on a case-insensitive
// filesystem A.txt and a.txt are one file, checked out once. When the scan
// would only stat it, it is removed, and both paths are recorded unread at its
// size, so neither is reported as a file that could not be stat'd.
func TestDropCheckedOutRecordsEveryPathThatReadsTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &seedHeadCheckout{dir: dir, unread: map[string]int64{}, readers: map[string][]string{"a.txt": {"A.txt", "a.txt"}}}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c.dropCheckedOut("a.txt", filepath.Join(dir, "a.txt"), 4)
	c.dropCheckedOut("b.txt", filepath.Join(dir, "b.txt"), 4)
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was not removed", name)
		}
	}
	for _, path := range []string{"A.txt", "a.txt", "b.txt"} {
		if size, ok := c.unread[path]; !ok || size != 4 {
			t.Errorf("%s: unread = %d, %v; want 4", path, size, ok)
		}
	}
}

// fixedConfigRunner answers git config with fixed output and error.
type fixedConfigRunner struct {
	out []byte
	err error
}

func (r fixedConfigRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	return r.out, nil, r.err
}

func TestSeedHeadFilterDriversParsesTheEffectiveConfiguration(t *testing.T) {
	t.Parallel()
	// git config's own "not set" exit, with no shell needed to produce it.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	empty := filepath.Join(t.TempDir(), "empty-config")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exitOne := exec.Command("git", "config", "--file", empty, "--get", "filter.none.smudge").Run()
	if exitErr := (*exec.ExitError)(nil); !errors.As(exitOne, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("git config on an unset key: %v; want exit status 1", exitOne)
	}
	cases := []struct {
		name   string
		runner fixedConfigRunner
		runs   []string
		skips  []string
	}{
		{name: "none configured", runner: fixedConfigRunner{err: exitOne}, skips: []string{"a", "lfs", ""}},
		{
			name: "the last value of a key wins",
			runner: fixedConfigRunner{out: []byte("filter.a.smudge\ntr x y\x00filter.b.process\nrun it\x00" +
				"filter.a.smudge\n\x00filter.c.d.smudge\ncat\x00filter.lfs.process\ngit-lfs filter-process\x00")},
			runs:  []string{"b", "c.d"},
			skips: []string{"a", "lfs", "unset", "unspecified"},
		},
		{name: "unreadable", runner: fixedConfigRunner{err: errors.New("signal: killed")}, runs: []string{"a", "anything"}, skips: []string{"lfs", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			drivers := seedHeadFilterDrivers(context.Background(), tc.runner, t.TempDir(), nil)
			for _, name := range tc.runs {
				if !drivers.runs(name) {
					t.Errorf("filter %q: no driver; want one", name)
				}
			}
			for _, name := range tc.skips {
				if drivers.runs(name) {
					t.Errorf("filter %q: a driver; want none", name)
				}
			}
		})
	}
}

// utf16LE encodes ASCII text as UTF-16LE, the bytes a working tree holds for
// a path with working-tree-encoding=UTF-16LE.
func utf16LE(ascii string) string {
	var b strings.Builder
	for i := 0; i < len(ascii); i++ {
		b.WriteByte(ascii[i])
		b.WriteByte(0)
	}
	return b.String()
}

// seedIndexEntry reads one path's entry from a seed's file-index.json.
func seedIndexEntry(t *testing.T, outputDir, rel string) seedFileIndexEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []seedFileIndexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse file-index.json: %v", err)
	}
	for _, entry := range entries {
		if entry.Path == rel {
			return entry
		}
	}
	t.Fatalf("file-index.json has no entry for %s", rel)
	return seedFileIndexEntry{}
}

// TestDirtySeedResolvesLocalLFSObjects: the checkout skips the LFS smudge
// filter so a background seed never downloads, which on its own seeded every
// LFS document as pointer text. A pointer the commit tracks with filter=lfs
// and whose object is already in the local store is now seeded as its content;
// one whose object is not stored stays the pointer a worktree without it would
// show; one the scan would only stat is never copied; a stored object that
// does not match its pointer is reported; and a file that only looks like a
// pointer, with no filter=lfs, is left exactly as committed.
func TestDirtySeedResolvesLocalLFSObjects(t *testing.T) {
	isolateSeedHeadTemp(t)
	// No git-lfs from the user's or the system configuration: its --skip
	// smudge rewrites a pointer with LF line ends. A CRLF pointer is what a
	// checkout writes with no LFS driver in force, as when git-lfs is set up
	// only in the repository's own configuration, which seed blanks.
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repoDir := seedHeadTestRepo(t)
	stored := "# manual kept in LFS\n"
	large := strings.Repeat("large LFS payload ", 20)
	missing := "# never fetched\n"
	corrupt := "# corrupt object\n"
	notLFS := "# a doc that quotes a pointer\n"
	crlf := "# checked out with CRLF line ends\n"
	extended := "# stored through an LFS extension\n"
	extendedPointer := strings.Replace(lfsPointer(extended), "oid ", "ext-0-foo sha256:"+strings.Repeat("0", 64)+"\noid ", 1)
	annotated := "# a pointer with a line after its fields\n"
	annotatedText := lfsPointer(annotated) + "note this is how the manual is stored\n"
	writeLFSObject(t, repoDir, annotated, annotated)
	writeLFSObject(t, repoDir, crlf, crlf)
	writeLFSObject(t, repoDir, extended, extended)
	writeLFSObject(t, repoDir, stored, stored)
	writeLFSObject(t, repoDir, large, large)
	writeLFSObject(t, repoDir, corrupt, strings.Repeat("x", len(corrupt)))
	writeLFSObject(t, repoDir, notLFS, notLFS)
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes":     "docs/manual.md filter=lfs -text\ndocs/missing.md filter=lfs -text\ndocs/corrupt.md filter=lfs -text\ndata/large.txt filter=lfs -text\ndocs/crlf.md filter=lfs text eol=crlf\ndocs/extended.md filter=lfs -text\ndocs/annotated.md filter=lfs -text\n",
		"docs/annotated.md":  annotatedText,
		"docs/crlf.md":       lfsPointer(crlf),
		"docs/extended.md":   extendedPointer,
		"docs/manual.md":     lfsPointer(stored),
		"docs/missing.md":    lfsPointer(missing),
		"docs/corrupt.md":    lfsPointer(corrupt),
		"docs/pointerish.md": lfsPointer(notLFS),
		"data/large.txt":     lfsPointer(large),
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "lfs pointers")
	useLFSSmudge(t, repoDir)
	dirtySeedHeadTree(t, repoDir)

	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	for rel, want := range map[string]string{
		"docs/annotated.md":  annotatedText,
		"docs/crlf.md":       crlf,
		"docs/extended.md":   extendedPointer,
		"docs/manual.md":     stored,
		"docs/missing.md":    lfsPointer(missing),
		"docs/corrupt.md":    lfsPointer(corrupt),
		"docs/pointerish.md": lfsPointer(notLFS),
	} {
		if got, err := os.ReadFile(filepath.Join(head.dir, filepath.FromSlash(rel))); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", rel, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(head.dir, "data", "large.txt")); err == nil {
		t.Error("an LFS object over --max-file-bytes was copied into the checkout")
	}
	if size, ok := head.unreadSize("data/large.txt"); !ok || size != int64(len(large)) {
		t.Errorf("data/large.txt: unread size = %d, %v; want the object's size %d", size, ok, len(large))
	}
	if warnings := strings.Join(head.warnings, "\n"); len(head.warnings) != 2 || !strings.Contains(warnings, "docs/corrupt.md") || !strings.Contains(warnings, "docs/extended.md") {
		t.Errorf("a stored object that fails its pointer's check, and one an extension transforms, must be reported, and only those: %q", warnings)
	}
}

// TestParseLFSPointerReadsWhatGitLFSReads: a filter=lfs file is replaced with
// its stored object only when git-lfs's smudge would read it as a pointer, so
// the parser accepts what git-lfs's non-strict decoder accepts. Each case's
// expectation matches `git lfs pointer --check --stdin --no-strict` (git-lfs
// 3.4.1). The one that mattered: a file with the pointer's fields and then
// another line is a document, and smudge leaves it as committed.
func TestParseLFSPointerReadsWhatGitLFSReads(t *testing.T) {
	t.Parallel()
	oid := strings.Repeat("a", 64)
	ext := "sha256:" + strings.Repeat("b", 64)
	v1 := "version https://git-lfs.github.com/spec/v1\n"
	fields := v1 + "oid sha256:" + oid + "\nsize 12\n"
	for _, tc := range []struct {
		name     string
		data     string
		ok       bool
		extended bool
	}{
		{"pointer", fields, true, false},
		{"no final newline", strings.TrimSuffix(fields, "\n"), true, false},
		{"CRLF line ends", strings.ReplaceAll(fields, "\n", "\r\n"), true, false},
		{"blank lines", "\n" + v1 + "\noid sha256:" + oid + "\n\nsize 12\n\n", true, false},
		{"white space around it", " \t" + fields + "   \n\u00a0", true, false},
		{"a space after the size at the end", strings.TrimSuffix(fields, "\n") + " \n", true, false},
		{"older version line", "version https://hawser.github.com/spec/v1\noid sha256:" + oid + "\nsize 12\n", true, false},
		{"size with a plus sign", v1 + "oid sha256:" + oid + "\nsize +12\n", true, false},
		{"extension before oid", v1 + "ext-0-foo " + ext + "\noid sha256:" + oid + "\nsize 12\n", true, true},
		{"extension before version", "ext-0-foo " + ext + "\n" + fields, true, true},
		{"extension between oid and size", v1 + "oid sha256:" + oid + "\next-0-foo-bar " + ext + "\nsize 12\n", true, true},
		{"two extensions", v1 + "ext-0-foo " + ext + "\next-1-bar " + ext + "\noid sha256:" + oid + "\nsize 12\n", true, true},
		{"a line after size", fields + "note see the manual\n", false, false},
		{"an extension after size", fields + "ext-0-foo " + ext + "\n", false, false},
		{"another key between oid and size", v1 + "oid sha256:" + oid + "\nnote x\nsize 12\n", false, false},
		{"a line without a space", fields + "trailer\n", false, false},
		{"a line of spaces inside it", v1 + "   \noid sha256:" + oid + "\nsize 12\n", false, false},
		{"a space after the oid", v1 + "oid sha256:" + oid + " \nsize 12\n", false, false},
		{"a tab for the space", v1 + "oid\tsha256:" + oid + "\nsize 12\n", false, false},
		{"two spaces", v1 + "oid  sha256:" + oid + "\nsize 12\n", false, false},
		{"a CR left inside a line", v1 + "oid sha256:" + oid + "\r\r\nsize 12\n", false, false},
		{"a version with a trailing space", strings.Replace(fields, "v1\n", "v1 \n", 1), false, false},
		{"no size", v1 + "oid sha256:" + oid + "\n", false, false},
		{"an empty size", v1 + "oid sha256:" + oid + "\nsize \n", false, false},
		{"a negative size", v1 + "oid sha256:" + oid + "\nsize -1\n", false, false},
		{"an uppercase oid", v1 + "oid sha256:" + strings.ToUpper(oid) + "\nsize 12\n", false, false},
		{"a long oid", v1 + "oid sha256:" + oid + "a\nsize 12\n", false, false},
		{"a two-digit extension priority", v1 + "ext-10-foo " + ext + "\noid sha256:" + oid + "\nsize 12\n", false, false},
		{"an extension without an oid", v1 + "ext-0-foo nothing\noid sha256:" + oid + "\nsize 12\n", false, false},
		{"two extensions with one priority", v1 + "ext-0-foo " + ext + "\next-0-bar " + ext + "\noid sha256:" + oid + "\nsize 12\n", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotOID, size, extended, ok := parseLFSPointer([]byte(tc.data))
			if ok != tc.ok || extended != tc.extended {
				t.Fatalf("parseLFSPointer(%q) ok = %v, extended = %v; want %v, %v", tc.data, ok, extended, tc.ok, tc.extended)
			}
			if ok && (gotOID != oid || size != 12) {
				t.Fatalf("parseLFSPointer(%q) = %s, %d; want %s, 12", tc.data, gotOID, size, oid)
			}
		})
	}
}

// TestDirtySeedKeepsLFSPointersWhenFetchFiltersAreSet: Git LFS keeps the
// pointer of a path lfs.fetchinclude or lfs.fetchexclude leaves out, even
// with its object stored locally (git-lfs 3.4.1 does). Seed does not match
// paths the way Git LFS does, so while either is set, in the configuration or
// the commit's .lfsconfig, it keeps every pointer and says so. A key with no
// pattern filters nothing.
func TestDirtySeedKeepsLFSPointersWhenFetchFiltersAreSet(t *testing.T) {
	stored := "# manual kept in LFS\n"
	for _, tc := range []struct {
		name        string
		configure   func(t *testing.T, repoDir string)
		lfsconfig   string
		wantPointer bool
	}{
		{name: "configuration", wantPointer: true, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "lfs.fetchexclude", "docs/other.md")
		}},
		{name: "committed .lfsconfig", wantPointer: true, lfsconfig: "[lfs]\n\tfetchinclude = assets/**\n"},
		{name: "no pattern", configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "lfs.fetchexclude", " , ")
		}},
		// Git LFS takes the last value of a key, and the git configuration
		// comes after the commit's .lfsconfig (checked against git-lfs 3.4.1).
		{name: "configuration emptying the committed .lfsconfig", lfsconfig: "[lfs]\n\tfetchinclude = assets/**\n", configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "lfs.fetchinclude", "")
		}},
		{name: "local configuration emptying the global one", configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "--global", "lfs.fetchexclude", "docs/other.md")
			gitAt(t, repoDir, "config", "lfs.fetchexclude", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateSeedHeadTemp(t)
			global := filepath.Join(t.TempDir(), "gitconfig")
			if err := os.WriteFile(global, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			repoDir := seedHeadTestRepo(t)
			writeLFSObject(t, repoDir, stored, stored)
			files := map[string]string{
				".gitattributes": "docs/manual.md filter=lfs -text\n",
				"docs/manual.md": lfsPointer(stored),
			}
			if tc.lfsconfig != "" {
				files[".lfsconfig"] = tc.lfsconfig
			}
			writeSeedHeadFiles(t, repoDir, files)
			gitAt(t, repoDir, "add", "-A")
			gitAt(t, repoDir, "commit", "-q", "-m", "lfs pointer")
			useLFSSmudge(t, repoDir)
			if tc.configure != nil {
				tc.configure(t, repoDir)
			}
			dirtySeedHeadTree(t, repoDir)

			head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
			if err != nil {
				t.Fatalf("check out HEAD: %v", err)
			}
			defer head.remove()
			want := stored
			if tc.wantPointer {
				want = lfsPointer(stored)
			}
			if got, err := os.ReadFile(filepath.Join(head.dir, "docs", "manual.md")); err != nil || string(got) != want {
				t.Errorf("docs/manual.md = %q, %v; want %q", got, err, want)
			}
			warned := strings.Contains(strings.Join(head.warnings, "\n"), "fetch include/exclude")
			if warned != tc.wantPointer {
				t.Errorf("fetch filter warning = %v, want %v: %q", warned, tc.wantPointer, head.warnings)
			}
		})
	}
}

// TestDirtySeedKeepsLFSPointersWhenTheSmudgeIsSkipped: when the caller's Git
// LFS settings make a clean checkout skip the smudge, it holds the pointer in
// git-lfs's own encoding although the object is stored locally, and so does
// seed; with no lfs filter set up it holds the pointer as committed. Neither
// is warned about, since it is what the checkout holds. Cases checked against
// git-lfs 3.4.1. The committed pointer has an older version line, so the
// three outcomes differ. The repository's own configuration counts although
// seed's git commands blank its filters, and so does the caller's command
// scope, with only those blanks left out.
func TestDirtySeedKeepsLFSPointersWhenTheSmudgeIsSkipped(t *testing.T) {
	stored := "# manual kept in LFS\n"
	committed := strings.Replace(lfsPointer(stored), "git-lfs.github.com", "hawser.github.com", 1)
	const (
		content = iota
		encoded
		asCommitted
	)
	for _, tc := range []struct {
		name      string
		skipEnv   string
		configure func(t *testing.T, repoDir string)
		// runsGitLFS: seed's own checkout runs the filter, which needs git-lfs.
		runsGitLFS bool
		want       int
	}{
		{name: "GIT_LFS_SKIP_SMUDGE=1", skipEnv: "1", configure: useLFSSmudge, want: encoded},
		{name: "GIT_LFS_SKIP_SMUDGE=Yes", skipEnv: "Yes", configure: useLFSSmudge, want: encoded},
		{name: "GIT_LFS_SKIP_SMUDGE=2", skipEnv: "2", configure: useLFSSmudge, want: content},
		{name: "GIT_LFS_SKIP_SMUDGE=1 with no lfs filter", skipEnv: "1", want: asCommitted},
		{name: "no lfs filter", want: asCommitted},
		{name: "repository filter-process --skip", want: encoded, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.process", "git-lfs filter-process --skip")
		}},
		{name: "repository filter-process with a quoted --skip", want: encoded, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.process", "git-lfs filter-process '--skip'")
		}},
		{name: "global filter-process --skip", runsGitLFS: true, want: encoded, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "--global", "filter.lfs.process", "git-lfs filter-process --skip")
		}},
		{name: "smudge --skip without a process", runsGitLFS: true, want: encoded, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "--global", "filter.lfs.smudge", "git-lfs smudge --skip -- %f")
		}},
		{name: "smudge --skip behind a process that does not skip", want: content, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.process", "git-lfs filter-process")
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "git-lfs smudge --skip -- %f")
		}},
		// An empty process is no process: git runs the smudge.
		{name: "repository emptying the global filter-process --skip", want: content, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "--global", "filter.lfs.process", "git-lfs filter-process --skip")
			gitAt(t, repoDir, "config", "filter.lfs.process", "")
			useLFSSmudge(t, repoDir)
		}},
		{name: "caller's GIT_CONFIG_COUNT filter-process over a repository --skip", want: content, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.process", "git-lfs filter-process --skip")
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "Filter.lfs.Process")
			t.Setenv("GIT_CONFIG_VALUE_0", "git-lfs filter-process")
		}},
		{name: "caller's GIT_CONFIG_COUNT filter-process alone", runsGitLFS: true, want: content, configure: func(t *testing.T, repoDir string) {
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "filter.lfs.process")
			t.Setenv("GIT_CONFIG_VALUE_0", "git-lfs filter-process")
		}},
		{name: "caller's git -c filter-process --skip over a repository smudge", runsGitLFS: true, want: encoded, configure: func(t *testing.T, repoDir string) {
			useLFSSmudge(t, repoDir)
			t.Setenv("GIT_CONFIG_PARAMETERS", "'filter.lfs.process=git-lfs filter-process --skip'")
		}},
		// A value from a file the caller includes is in the command scope too,
		// and is not the hardened runner's blank.
		{name: "caller's GIT_CONFIG_COUNT include over a repository --skip", want: content, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.process", "git-lfs filter-process --skip")
			included := filepath.Join(t.TempDir(), "included")
			if err := os.WriteFile(included, []byte("[filter \"lfs\"]\n\tprocess = git-lfs filter-process\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "include.path")
			t.Setenv("GIT_CONFIG_VALUE_0", included)
		}},
		{name: "repository git-lfs at a quoted path with --skip", want: encoded, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", `"/opt/Git LFS/git-lfs.exe" smudge --skip -- %f`)
		}},
		{name: "repository git lfs smudge --skip", want: encoded, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "git lfs smudge --skip -- %f")
		}},
		// Seed does not know what another program writes, so what the checkout
		// wrote stays: the global filter's output, the committed text where
		// the filter is the repository's.
		{name: "global filter that is not git-lfs with GIT_LFS_SKIP_SMUDGE=1", skipEnv: "1", want: asCommitted, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "--global", "filter.lfs.smudge", "cat")
		}},
		{name: "repository wrapper that runs git-lfs", want: content, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "env LC_ALL=C git-lfs smudge -- %f")
		}},
		{name: "repository filter that is not git-lfs", want: asCommitted, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "my-lfs-wrapper smudge %f")
		}},
		{name: "repository filter that names git-lfs as an argument", want: asCommitted, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "cat git-lfs")
		}},
		{name: "repository compound filter with a quoted --skip", want: asCommitted, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "git-lfs smudge -- %f; echo '--skip'")
		}},
		// The command line's own GIT_LFS_SKIP_SMUDGE is what git-lfs reads.
		{name: "repository smudge setting GIT_LFS_SKIP_SMUDGE=1", want: encoded, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "GIT_LFS_SKIP_SMUDGE=1 git-lfs smudge -- %f")
		}},
		{name: "repository smudge setting GIT_LFS_SKIP_SMUDGE=0 over the caller's 1", skipEnv: "1", want: content, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "env GIT_LFS_SKIP_SMUDGE=0 git-lfs smudge -- %f")
		}},
		{name: "repository smudge with --skip after --", want: content, configure: func(t *testing.T, repoDir string) {
			gitAt(t, repoDir, "config", "filter.lfs.smudge", "git-lfs smudge -- --skip")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath("git-lfs"); tc.runsGitLFS && err != nil {
				t.Skip("git-lfs not installed")
			}
			isolateSeedHeadTemp(t)
			global := filepath.Join(t.TempDir(), "gitconfig")
			if err := os.WriteFile(global, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_LFS_SKIP_SMUDGE", tc.skipEnv)
			for _, key := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			repoDir := seedHeadTestRepo(t)
			writeLFSObject(t, repoDir, stored, stored)
			writeSeedHeadFiles(t, repoDir, map[string]string{
				".gitattributes": "docs/manual.md filter=lfs -text\n",
				"docs/manual.md": committed,
			})
			gitAt(t, repoDir, "add", "-A")
			gitAt(t, repoDir, "commit", "-q", "-m", "lfs pointer")
			if tc.configure != nil {
				tc.configure(t, repoDir)
			}
			dirtySeedHeadTree(t, repoDir)

			head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
			if err != nil {
				t.Fatalf("check out HEAD: %v", err)
			}
			defer head.remove()
			want := map[int]string{content: stored, encoded: lfsPointer(stored), asCommitted: committed}[tc.want]
			if got, err := os.ReadFile(filepath.Join(head.dir, "docs", "manual.md")); err != nil || string(got) != want {
				t.Errorf("docs/manual.md = %q, %v; want %q", got, err, want)
			}
			if len(head.warnings) > 0 {
				t.Errorf("warnings = %q, want none", head.warnings)
			}
		})
	}
}

func TestSeedHeadGitLFSCommandReadsTheFilterAsShDoes(t *testing.T) {
	t.Parallel()
	notGitLFS := []string{
		"", "cat", "cat -- %f", "git status", "git", "git -c x=y lfs smudge %f",
		"my-git-lfs-wrapper smudge %f", "git-lfs-wrapper smudge %f",
		"cat git-lfs", "cat git lfs", "env -i git-lfs smudge %f",
		"git-lfs smudge %f; echo '--skip'", "git-lfs smudge %f | tee log",
		"cd /tmp && git-lfs smudge %f", "git-lfs smudge %f > out",
		"$HOME/bin/git-lfs smudge %f", `"$HOME/bin/git-lfs" smudge %f`,
		"`which git-lfs` smudge %f", "git-l* smudge %f", "'git-lfs smudge %f",
		`"git-lfs smudge %f`, `git-lfs smudge %f \`, "git-lfs smudge \\\n%f",
		`"LC_ALL"=C git-lfs smudge %f`, "sh -c", "sh -c 'cat' git-lfs", "# git-lfs smudge %f",
	}
	for _, command := range notGitLFS {
		if args, assignments, ok := seedHeadGitLFSCommand(command); ok {
			t.Errorf("seedHeadGitLFSCommand(%q) = %q, %q, true; want not git-lfs", command, args, assignments)
		}
	}
	for _, tc := range []struct{ command, args, assignments string }{
		{"git-lfs filter-process", "filter-process", ""},
		{"git-lfs smudge --skip -- %f", "smudge|--skip|--|%f", ""},
		{"/usr/local/bin/git-lfs smudge -- %f", "smudge|--|%f", ""},
		{`"C:\Program Files\Git LFS\git-lfs.exe" filter-process`, "filter-process", ""},
		{"'/opt/git lfs/git-lfs' smudge %f", "smudge|%f", ""},
		{`/opt/git\ lfs/Git-LFS.EXE smudge %f`, "smudge|%f", ""},
		{"git lfs smudge %f", "smudge|%f", ""},
		{"git\tlfs smudge %f", "smudge|%f", ""},
		{`git-lfs smudge "--skip" %f`, "smudge|--skip|%f", ""},
		{`git-lfs smudge "a\"b\\c\d" ''`, `smudge|a"b\c\d|`, ""},
		{"git-lfs smudge -- %f # a note", "smudge|--|%f", ""},
		{"git-lfs smudge -- %f#1", "smudge|--|%f#1", ""},
		{"LC_ALL=C git-lfs smudge -- %f", "smudge|--|%f", "LC_ALL=C"},
		{"env LC_ALL=C git-lfs smudge -- %f", "smudge|--|%f", "LC_ALL=C"},
		{`/usr/bin/env "GIT_LFS_SKIP_SMUDGE=1" git-lfs filter-process`, "filter-process", "GIT_LFS_SKIP_SMUDGE=1"},
		{"A=1 env B=2 git lfs smudge %f", "smudge|%f", "A=1|B=2"},
		{"sh -c 'git-lfs smudge -- %f'", "smudge|--|%f", ""},
		{"GIT_LFS_SKIP_SMUDGE=0 bash -c 'GIT_LFS_SKIP_SMUDGE=1 git-lfs smudge %f'", "smudge|%f", "GIT_LFS_SKIP_SMUDGE=0|GIT_LFS_SKIP_SMUDGE=1"},
	} {
		args, assignments, ok := seedHeadGitLFSCommand(tc.command)
		if !ok || strings.Join(args, "|") != tc.args || strings.Join(assignments, "|") != tc.assignments {
			t.Errorf("seedHeadGitLFSCommand(%q) = %q, %q, %v; want %q, %q, true", tc.command, args, assignments, ok, tc.args, tc.assignments)
		}
	}
}

// useLFSSmudge sets git-lfs's smudge up in the repository's own configuration,
// as `git lfs install --local` does, so a clean checkout by the caller fetches
// LFS content. Seed's git commands blank it and so never run it.
func useLFSSmudge(t *testing.T, repoDir string) {
	t.Helper()
	gitAt(t, repoDir, "config", "filter.lfs.smudge", "git-lfs smudge -- %f")
}

func lfsPointer(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", hex.EncodeToString(sum[:]), len(content))
}

// writeLFSObject stores stored in the local LFS store under content's oid;
// passing different bytes of the same length makes a corrupt object.
func writeLFSObject(t *testing.T, repoDir, content, stored string) {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	writeSeedHeadFiles(t, filepath.Join(repoDir, ".git", "lfs", "objects", oid[0:2], oid[2:4]), map[string]string{oid: stored})
}

// TestDirtySeedAgentReadsCommittedHEAD: an agent used to be told the
// repository root was the real repository, so on a dirty tree it could fold
// uncommitted content into a seed recorded as committed HEAD. The packet now
// names the checkout. A command agent still runs in the repository, because
// its command line may name repository-relative files.
func TestDirtySeedAgentReadsCommittedHEAD(t *testing.T) {
	if os.Getenv("GO_SEED_HEAD_AGENT_FIXTURE") == "1" {
		var request struct {
			Phase string            `json:"phase"`
			Repo  map[string]string `json:"repo"`
		}
		if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
			os.Exit(2)
		}
		cwd, _ := os.Getwd()
		root, _ := os.ReadFile(filepath.Join(request.Repo["root"], "README.md"))
		seen, _ := json.Marshal(map[string]string{"cwd": cwd, "root_readme": string(root)})
		artifacts := map[string]string{"quick-overview.md": string(seen)}
		if request.Phase == "deep" {
			artifacts = map[string]string{
				"overview.md": string(seen), "architecture.md": "arch", "risks.md": "risks",
				"maintenance-guide.md": "maint", "open-questions.md": "questions",
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
			"schema_version": 1, "status": "success", "model": "fake", "artifacts": artifacts,
		}); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	t.Setenv("GO_SEED_HEAD_AGENT_FIXTURE", "1")
	isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	dirtySeedHeadTree(t, repoDir)
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: ExecRunner{}}
	outputDir := filepath.Join(t.TempDir(), "seed")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--output", outputDir, "--agent", "command",
		"--agent-command", os.Args[0], "--agent-command=-test.run=^TestDirtySeedAgentReadsCommittedHEAD$", "--require-agent", repoDir); err != nil {
		t.Fatalf("seed with an agent on a dirty tree: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(outputDir, seedDirName, seedAgentDirName, "quick-overview.md"))
	if err != nil {
		t.Fatalf("quick artifact missing: %v", err)
	}
	var seen map[string]string
	if err := json.Unmarshal(data, &seen); err != nil {
		t.Fatalf("agent artifact %q: %v", data, err)
	}
	if seen["root_readme"] != "# committed readme\n" {
		t.Errorf("README under the packet's repo.root = %q, want the committed bytes", seen["root_readme"])
	}
	wantCwd, _ := filepath.EvalSymlinks(repoDir)
	if gotCwd, _ := filepath.EvalSymlinks(seen["cwd"]); gotCwd != wantCwd {
		t.Errorf("command agent ran in %q, want the repository %q so its relative command line still resolves", seen["cwd"], repoDir)
	}
}

func TestSeedAgentWorkDirKeepsCommandAgentsInTheRepository(t *testing.T) {
	t.Parallel()
	for agent, want := range map[string]string{"command": "repo", "codex": "checkout", "claude-code": "checkout"} {
		if got := seedAgentWorkDir(agent, "repo", "checkout"); got != want {
			t.Errorf("seedAgentWorkDir(%q) = %q, want %q", agent, got, want)
		}
	}
}

// TestDirtySeedLeavesOutWhatAPartialCloneHasNotFetched: a sparse checkout of
// a partial clone fetches only the blobs it writes, and `ls-tree -l` dies on
// the first one missing rather than fetch it, which made a dirty seed refuse.
// The dirty seed now skips the blobs the clone does not have, as its worktree
// does, reads the rest at their committed sizes, and fetches nothing.
func TestDirtySeedLeavesOutWhatAPartialCloneHasNotFetched(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	source := seedHeadTestRepo(t)
	writeSeedHeadFiles(t, source, map[string]string{
		"src/large.txt":  strings.Repeat("l", 4*64+1),
		"other/notes.md": "committed notes outside the sparse checkout\n",
	})
	gitAt(t, source, "add", "-A")
	gitAt(t, source, "commit", "-q", "-m", "files in and out of the sparse checkout")
	gitAt(t, source, "config", "uploadpack.allowfilter", "true")
	gitAt(t, source, "config", "uploadpack.allowanysha1inwant", "true")
	sourceURL := filepath.ToSlash(source)
	if !strings.HasPrefix(sourceURL, "/") {
		sourceURL = "/" + sourceURL // file:///C:/... on Windows
	}
	repoDir := filepath.Join(t.TempDir(), "partial")
	gitAt(t, t.TempDir(), "clone", "-q", "--filter=blob:none", "--sparse", "file://"+sourceURL, repoDir)
	gitAt(t, repoDir, "config", "core.autocrlf", "false")
	gitAt(t, repoDir, "sparse-checkout", "set", "src")
	missingBlob := func(rel string) bool {
		t.Helper()
		object := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD:"+rel))
		cmd := exec.Command("git", "cat-file", "-e", object)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), gitNoLazyFetchEnv+"=1")
		return cmd.Run() != nil
	}
	for _, rel := range []string{"docs/guide.md", "other/notes.md"} {
		if !missingBlob(rel) {
			t.Fatalf("the partial clone has %s; the fixture needs it missing", rel)
		}
	}
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	cleanDir := filepath.Join(t.TempDir(), "clean")
	if _, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", cleanDir, repoDir); err != nil {
		t.Fatalf("seed a clean tree: %v", err)
	}

	writeSeedHeadFiles(t, repoDir, map[string]string{
		"README.md":    "# UNCOMMITTED readme\n",
		"untracked.md": "UNCOMMITTED untracked doc\n",
	})
	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD of a sparse partial clone: %v", err)
	}
	defer head.remove()
	if got, err := os.ReadFile(filepath.Join(head.dir, "README.md")); err != nil || string(got) != "# committed readme\n" {
		t.Errorf("README.md = %q, %v; want the committed bytes", got, err)
	}
	if size, ok := head.unreadSize("src/large.txt"); !ok || size != 4*64+1 {
		t.Errorf("src/large.txt: unread size = %d, %v; want its blob size %d", size, ok, 4*64+1)
	}
	for _, rel := range []string{"docs/guide.md", "other/notes.md"} {
		if _, err := os.Lstat(filepath.Join(head.dir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s, which the clone has not fetched, was written into the checkout", rel)
		}
		if _, ok := head.unreadSize(rel); ok {
			t.Errorf("%s was recorded unread at a size the clone does not know", rel)
		}
	}
	if len(head.warnings) != 1 || !strings.Contains(head.warnings[0], "2 committed files") || !strings.Contains(head.warnings[0], "docs/guide.md") {
		t.Errorf("warnings = %q; want one naming the files the clone has not fetched", head.warnings)
	}
	head.remove()

	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", "64", "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed the dirty tree: %v\n%s", err, out)
	}
	cleanIndex, err := os.ReadFile(filepath.Join(cleanDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	dirtyIndex, err := os.ReadFile(filepath.Join(dirtyDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cleanIndex, dirtyIndex) {
		t.Errorf("the dirty-tree file index differs from the clean one of the same commit:\nclean:\n%s\ndirty:\n%s", cleanIndex, dirtyIndex)
	}
	for _, rel := range []string{"docs/guide.md", "other/notes.md"} {
		if !missingBlob(rel) {
			t.Errorf("seeding fetched %s into the partial clone", rel)
		}
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedChecksOutWhatIdentShrinks: with the ident attribute, checkout
// rewrites an expanded "$Id: ... $" committed as it is to "$Id: <object> $",
// so a blob over --max-file-bytes can check out under it, and a clean seed
// includes it. Each "$Id$" also grows by an object id, so a small blob can
// check out over it. Every ident file is checked out in the bounded batches,
// stat'd, and dropped when it comes out too large; one past the cap is never
// written. The limit leaves room for a SHA-256 object id.
func TestDirtySeedChecksOutWhatIdentShrinks(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	const limit = 128
	expanded := "$Id: " + strings.Repeat("a", 4*limit) + " $\n"
	vast := "$Id: " + strings.Repeat("v", seedHeadDriverMaxShrink*limit) + " $\n"
	plain := strings.Repeat("p", 4*limit+1)
	keywords := strings.Repeat("$Id$\n", 20)
	// Committed before the attribute, so git add keeps the expansion.
	writeSeedHeadFiles(t, repoDir, map[string]string{
		"notes/id.txt":       expanded,
		"notes/vast.txt":     vast,
		"notes/plain.txt":    plain,
		"notes/keywords.txt": keywords,
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "expanded ids")
	// Only the attributes file: git add -A would re-clean a racily clean
	// notes file through ident and commit the collapsed keyword instead.
	writeSeedHeadFiles(t, repoDir, map[string]string{".gitattributes": "notes/*.txt ident\n"})
	gitAt(t, repoDir, "add", ".gitattributes")
	gitAt(t, repoDir, "commit", "-q", "-m", "ident")
	for rel, content := range map[string]string{"notes/id.txt": expanded, "notes/vast.txt": vast} {
		if size := strings.TrimSpace(gitOutputAt(t, repoDir, "cat-file", "-s", "HEAD:"+rel)); size != fmt.Sprint(len(content)) {
			t.Fatalf("%s was committed at %s bytes; the fixture needs the expanded %d", rel, size, len(content))
		}
	}
	keywordObject := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD:notes/keywords.txt"))
	grown := 20 * len("$Id: "+keywordObject+" $\n")
	dirtySeedHeadTree(t, repoDir)

	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: limit})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	object := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD:notes/id.txt"))
	if got, err := os.ReadFile(filepath.Join(head.dir, "notes", "id.txt")); err != nil || string(got) != "$Id: "+object+" $\n" {
		t.Errorf("notes/id.txt = %q, %v; want the keyword rewritten to its object", got, err)
	}
	for rel, size := range map[string]int{"notes/plain.txt": len(plain), "notes/keywords.txt": grown} {
		if got, ok := head.unreadSize(rel); !ok || got != int64(size) {
			t.Errorf("%s: unread size = %d, %v; want its checked-out size %d", rel, got, ok, size)
		}
		if _, err := os.Lstat(filepath.Join(head.dir, filepath.FromSlash(rel))); err == nil {
			t.Errorf("%s, which checks out too large, was left in the checkout", rel)
		}
	}
	if _, err := os.Lstat(filepath.Join(head.dir, "notes", "vast.txt")); err == nil {
		t.Error("a blob past the cap was written into the checkout")
	}
	if len(head.warnings) != 1 || !strings.Contains(head.warnings[0], "1 committed files") || !strings.Contains(head.warnings[0], "notes/vast.txt") {
		t.Errorf("warnings = %q; want one naming the file past the cap", head.warnings)
	}
	head.remove()

	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}
	dirtyDir := filepath.Join(t.TempDir(), "dirty")
	if out, err := execute(t, NewRootCommand(opts), "refresh", "seed", "--max-file-bytes", fmt.Sprint(limit), "--output", dirtyDir, repoDir); err != nil {
		t.Fatalf("seed the dirty tree: %v\n%s", err, out)
	}
	if entry := seedIndexEntry(t, dirtyDir, "notes/id.txt"); !entry.Included {
		t.Errorf("notes/id.txt = %+v; want it included, as in a clean seed", entry)
	}
	for rel, size := range map[string]int{"notes/vast.txt": len(vast), "notes/plain.txt": len(plain), "notes/keywords.txt": grown} {
		if entry := seedIndexEntry(t, dirtyDir, rel); entry.Included || entry.Reason != "too large" || entry.Bytes != int64(size) {
			t.Errorf("%s = %+v; want too large at %d bytes", rel, entry, size)
		}
	}
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestDirtySeedChecksOutAnEncodedFileWhateverItsBlobSize: a checkout in a
// working-tree-encoding can be far smaller than the UTF-8 blob, so a file is
// checked out and stat'd however large its blob, up to the driver cap, and its
// entry is the one a checkout of the commit gives, not "too large" at the
// blob's size. TSCII writes the four characters of Tamil "sri"
// (U+0BB8 U+0BCD U+0BB0 U+0BC0), 12 bytes of UTF-8, as one byte, so a 600-byte blob checks out at 50 bytes, under
// --max-file-bytes 64. In a UTF-8 locale ASCII//TRANSLIT writes a combining
// mark as nothing, so a 306-byte blob checks out at 6 bytes; where the locale
// transliterates the marks to "?" instead, the checkout is 156 bytes and the
// same comparison holds. An encoding the platform's iconv lacks is skipped.
func TestDirtySeedChecksOutAnEncodedFileWhateverItsBlobSize(t *testing.T) {
	for _, tc := range []struct{ encoding, blob string }{
		{"TSCII", strings.Repeat("\u0bb8\u0bcd\u0bb0\u0bc0", 50)},
		{"ASCII//TRANSLIT", "abcdef" + strings.Repeat("\u0301", 150)},
	} {
		t.Run(tc.encoding, func(t *testing.T) {
			isolateSeedHeadTemp(t)
			t.Setenv("LC_ALL", "C.UTF-8")
			repoDir := seedHeadTestRepo(t)
			const rel = "data/encoded.txt"
			writeSeedHeadFiles(t, repoDir, map[string]string{rel: tc.blob})
			gitAt(t, repoDir, "add", "-A")
			gitAt(t, repoDir, "commit", "-q", "-m", "UTF-8 blob")
			// Committed after the blob, so the blob stays the UTF-8 bytes.
			writeSeedHeadFiles(t, repoDir, map[string]string{".gitattributes": rel + " working-tree-encoding=" + tc.encoding + "\n"})
			gitAt(t, repoDir, "add", ".gitattributes")
			gitAt(t, repoDir, "commit", "-q", "-m", "encoding")
			if err := os.Remove(filepath.Join(repoDir, filepath.FromSlash(rel))); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("git", "-C", repoDir, "checkout-index", "-f", "--", rel).CombinedOutput(); err != nil {
				t.Skipf("git cannot check out %s here: %v\n%s", tc.encoding, err, out)
			}
			clean, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(rel)))
			if err != nil {
				t.Skipf("git cannot check out %s here: %v", tc.encoding, err)
			}
			// 4x is the most a character-for-character conversion could shrink
			// UTF-8.
			if clean.Size() >= int64(len(tc.blob)) || int64(len(tc.blob)) <= 4*64 {
				t.Fatalf("fixture: a %d-byte blob checked out at %d bytes; want a shrink past 4x 64 bytes", len(tc.blob), clean.Size())
			}

			dirtySeedHeadTree(t, repoDir)
			head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
			if err != nil {
				t.Fatalf("check out HEAD: %v", err)
			}
			defer head.remove()
			if size, unread := head.unreadSize(rel); unread {
				if size != clean.Size() {
					t.Errorf("%s recorded unread at %d bytes; a checkout of the commit writes %d", rel, size, clean.Size())
				}
			} else if got, err := os.Stat(filepath.Join(head.dir, filepath.FromSlash(rel))); err != nil || got.Size() != clean.Size() {
				t.Errorf("%s checked out as %v, %v; a checkout of the commit writes %d bytes", rel, got, err, clean.Size())
			}
		})
	}
}

// skipWhereCaseFolds skips a test whose fixture commits paths that differ
// only in case: a case-insensitive filesystem holds them as one file, so the
// commit would not have them all.
func skipWhereCaseFolds(t *testing.T) {
	t.Helper()
	if seedHeadFoldsCase(t.TempDir()) {
		t.Skip("case-insensitive filesystem: paths that differ only in case would be one file")
	}
}

// TestDirtySeedSettlesAnLFSPointerWithACaseAlias: on a case-sensitive
// filesystem, paths that differ only in case are files of their own. A.txt,
// an LFS pointer larger than --max-file-bytes whose object is not, must not be
// moved in with its ident alias a.txt and recorded too large at the pointer's
// size before settleLFSPointers runs. B.txt, an ident file that checks out too
// large, is dropped from the checkout like any other although b.txt is its
// alias. The fixture needs a case-sensitive filesystem to hold both paths.
func TestDirtySeedSettlesAnLFSPointerWithACaseAlias(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	skipWhereCaseFolds(t)
	repoDir := seedHeadTestRepo(t)
	object := "tiny LFS object\n"
	writeLFSObject(t, repoDir, object, object)
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes": "A.txt filter=lfs -text\na.txt ident\nB.txt ident\n",
		"A.txt":          lfsPointer(object),
		"a.txt":          "plain alias\n",
		"B.txt":          strings.Repeat("$Id$\n", 20),
		"b.txt":          "plain alias\n",
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "case aliases")
	useLFSSmudge(t, repoDir)
	dirtySeedHeadTree(t, repoDir)

	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	if size, ok := head.unreadSize("A.txt"); ok {
		t.Errorf("A.txt was recorded unread at %d bytes, the pointer's size, before its object was settled", size)
	}
	if got, err := os.ReadFile(filepath.Join(head.dir, "A.txt")); err != nil || string(got) != object {
		t.Errorf("A.txt = %q, %v; want its LFS object", got, err)
	}
	if _, err := os.Lstat(filepath.Join(head.dir, "B.txt")); err == nil {
		t.Error("B.txt checks out too large and was left in the checkout because b.txt folds to it")
	}
	if _, ok := head.unreadSize("B.txt"); !ok {
		t.Error("B.txt was not recorded unread at its checked-out size")
	}
	head.remove()
	assertNoSeedHeadLeftovers(t, tempDir)
}

// TestSeedHeadFoldsCaseMatchesTheFilesystem: the probe answers what the
// filesystem does, and leaves nothing behind.
func TestSeedHeadFoldsCaseMatchesTheFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "PROBE"))
	if got, want := seedHeadFoldsCase(dir), err == nil; got != want {
		t.Errorf("seedHeadFoldsCase = %v, want %v", got, want)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("the probe was left behind: %v, %v", entries, err)
	}
	if !seedHeadFoldsCase(filepath.Join(dir, "missing")) {
		t.Error("a directory the probe cannot be written to must count as folding")
	}
}

// TestSeedRepoFoldsCaseLooksAnEntryUpUnderAnotherCase: the repository's case
// rule comes from looking an entry it already has up under another case, so
// nothing is written there. A second name for the same file is what a
// case-insensitive filesystem shows, and two files are what a case-sensitive
// one keeps.
func TestSeedRepoFoldsCaseLooksAnEntryUpUnderAnotherCase(t *testing.T) {
	t.Parallel()
	skipWhereCaseFolds(t)
	dirWith := func(files ...string) string {
		dir := t.TempDir()
		for _, name := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	check := func(name, dir string, wantFolds, wantOK bool) {
		t.Helper()
		before, _ := os.ReadDir(dir)
		if folds, ok := seedRepoFoldsCase(dir); folds != wantFolds || ok != wantOK {
			t.Errorf("%s: seedRepoFoldsCase = %v, %v; want %v, %v", name, folds, ok, wantFolds, wantOK)
		}
		if after, _ := os.ReadDir(dir); len(after) != len(before) {
			t.Errorf("%s: the probe changed the directory: %v -> %v", name, before, after)
		}
	}
	check("one spelling", dirWith("Readme"), false, true)
	check("two files", dirWith("Readme", "rEADME"), false, true)
	check("nothing cased", dirWith("123"), false, false)
	check("missing directory", filepath.Join(t.TempDir(), "missing"), false, false)
	// Two spellings listed side by side are two entries, so case is kept even
	// when they are hard links to one file.
	linked := dirWith("Readme")
	if err := os.Link(filepath.Join(linked, "Readme"), filepath.Join(linked, "rEADME")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	check("two spellings linked to one file", linked, false, true)

	// A folding filesystem lists one spelling and finds the file under the
	// other too. This container cannot mount one, so the lookup stands in.
	folding := dirWith("Readme")
	info, err := os.Lstat(filepath.Join(folding, "Readme"))
	if err != nil {
		t.Fatal(err)
	}
	foldingLstat := func(name string) (fs.FileInfo, error) {
		if strings.EqualFold(name, "Readme") {
			return info, nil
		}
		return nil, fs.ErrNotExist
	}
	if folds, ok := seedFoldsCaseAmong([]string{"Readme"}, foldingLstat); !folds || !ok {
		t.Errorf("one file under two spellings: seedFoldsCaseAmong = %v, %v; want true, true", folds, ok)
	}
}

// TestSeedHeadCaseMismatchRefusesWhatTheRulesReadDifferently: a temporary
// filesystem that treats case unlike the repository's reads a selected path
// that differs only in case from another committed path, including one past
// --max-files, differently from a clean checkout, so the checkout is refused.
// Paths with no such twin read the same either way.
func TestSeedHeadCaseMismatchRefusesWhatTheRulesReadDifferently(t *testing.T) {
	t.Parallel()
	paths := []string{"README.md", "docs/Guide.md", "docs/guide.md", "src/main.go"}
	for _, tc := range []struct {
		name            string
		repo, temp      bool
		selected        []string
		wantRefusalPath string
	}{
		{name: "both case-sensitive", selected: paths},
		{name: "both fold case", repo: true, temp: true, selected: paths},
		{name: "case-sensitive repository, folding temporary directory", temp: true, selected: paths, wantRefusalPath: "docs/Guide.md"},
		{name: "folding repository, case-sensitive temporary directory", repo: true, selected: paths, wantRefusalPath: "docs/Guide.md"},
		{name: "twin past --max-files", temp: true, selected: paths[:2], wantRefusalPath: "docs/Guide.md"},
		{name: "no selected path has a twin", temp: true, selected: paths[:1]},
	} {
		err := seedHeadCaseMismatch(tc.repo, tc.temp, tc.selected, paths)
		switch {
		case tc.wantRefusalPath == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.wantRefusalPath != "" && (err == nil || !strings.Contains(err.Error(), tc.wantRefusalPath)):
			t.Errorf("%s: err = %v; want a refusal naming %s", tc.name, err, tc.wantRefusalPath)
		}
	}
}

// TestSeedHeadCaseAliasesReadTheFileACleanCheckoutLeaves: on a
// case-insensitive filesystem a clean checkout writes every path in tree
// order, so paths that differ only in case read the content of the last one
// it writes. A blob a partial clone has not fetched is not written, and the
// last path can lie past --max-files.
func TestSeedHeadCaseAliasesReadTheFileACleanCheckoutLeaves(t *testing.T) {
	t.Parallel()
	paths := []string{"A.txt", "B.txt", "C.txt", "Z.txt", "a.txt", "b.txt", "c.txt", "d.txt", "z.txt"}
	entries := map[string]seedHeadEntry{"b.txt": {missing: true}, "C.txt": {missing: true}, "c.txt": {missing: true}}
	aliases := seedHeadCaseAliases(paths, entries)
	if got, want := fmt.Sprint(aliases), "map[A.txt:a.txt Z.txt:z.txt b.txt:B.txt]"; got != want {
		t.Errorf("aliases = %s; want %s", got, want)
	}
	selected := paths[:len(paths)-1] // z.txt is past --max-files
	work, readers := seedHeadCaseWork(selected, aliases)
	if got, want := fmt.Sprint(work), "[B.txt C.txt a.txt c.txt d.txt z.txt]"; got != want {
		t.Errorf("work = %s; want %s", got, want)
	}
	if got, want := fmt.Sprint(readers), "map[B.txt:[B.txt b.txt] a.txt:[a.txt A.txt] z.txt:[Z.txt]]"; got != want {
		t.Errorf("readers = %s; want %s", got, want)
	}
}

// TestDirtySeedSizesCaseAliasesOfAPartialClone: a partial clone's listing
// used to mark missing blobs and read sizes only for the first --max-files
// paths, but the alias a clean checkout keeps can lie past them. A missing one
// was taken for the kept alias, and the checkout failed trying to fetch it; a
// present one had no size and was written however large. Missing blobs are now
// marked across the tree, and a kept alias past --max-files is sized.
func TestDirtySeedSizesCaseAliasesOfAPartialClone(t *testing.T) {
	t.Parallel()
	skipWhereCaseFolds(t)
	source := seedHeadTestRepo(t)
	big := strings.Repeat("x", 200)
	writeSeedHeadFiles(t, source, map[string]string{
		"Y/y.md": "# fetched\n",
		"y/y.md": "# never fetched\n",
		"Z/x.md": "# selected\n",
		"z/x.md": big,
	})
	gitAt(t, source, "add", "-A")
	gitAt(t, source, "commit", "-q", "-m", "case aliases in a partial clone")
	gitAt(t, source, "config", "uploadpack.allowfilter", "true")
	gitAt(t, source, "config", "uploadpack.allowanysha1inwant", "true")
	sourceURL := filepath.ToSlash(source)
	if !strings.HasPrefix(sourceURL, "/") {
		sourceURL = "/" + sourceURL
	}
	repoDir := filepath.Join(t.TempDir(), "partial")
	gitAt(t, t.TempDir(), "clone", "-q", "--filter=blob:none", "--sparse", "file://"+sourceURL, repoDir)
	gitAt(t, repoDir, "config", "core.autocrlf", "false")
	gitAt(t, repoDir, "sparse-checkout", "set", "Y", "Z", "z", "src")
	missingBlob := func(rel string) bool {
		t.Helper()
		object := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD:"+rel))
		cmd := exec.Command("git", "cat-file", "-e", object)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), gitNoLazyFetchEnv+"=1")
		return cmd.Run() != nil
	}
	if !missingBlob("y/y.md") || missingBlob("z/x.md") {
		t.Fatal("the fixture needs y/y.md missing and z/x.md fetched")
	}
	commit := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD"))
	// README.md Y/y.md Z/x.md docs/guide.md package.json src/helpers.go
	// src/main.go | y/y.md z/x.md
	opts := seedCommandOptions{maxFiles: 7, maxFileBytes: 64}
	ctx := context.Background()
	paths, entries, err := listSeedHeadTree(ctx, ExecRunner{}, repoDir, commit, opts.maxFiles)
	if err != nil {
		t.Fatal(err)
	}
	if !entries["y/y.md"].missing {
		t.Error("y/y.md, past --max-files and never fetched, is not marked missing")
	}
	tempRoot := t.TempDir()
	c := &seedHeadCheckout{commit: commit, dir: filepath.Join(tempRoot, "tree"), paths: paths, unread: map[string]int64{}, foldsCase: true, tempRoot: tempRoot}
	if err := c.populate(ctx, ExecRunner{}, repoDir, entries, opts); err != nil {
		t.Fatalf("check out with a missing case alias past --max-files: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(c.dir, "Y", "y.md")); err != nil || string(got) != "# fetched\n" {
		t.Errorf("Y/y.md = %q, %v; want it checked out, as the alias a clean checkout keeps", got, err)
	}
	if _, err := os.Lstat(filepath.Join(c.dir, "z", "x.md")); err == nil {
		t.Error("z/x.md, too large however it converts, was written into the checkout")
	}
	if size, ok := c.unreadSize("Z/x.md"); !ok || size != int64(len(big)) {
		t.Errorf("Z/x.md: unread = %d, %v; want z/x.md's size %d", size, ok, len(big))
	}
	if !missingBlob("y/y.md") {
		t.Error("the checkout fetched y/y.md")
	}
}

// TestDirtySeedChecksOutTheCaseAliasACleanCheckoutKeeps: where the
// filesystem folds case, src/A.go and src/a.go are one file holding src/a.go,
// the last in tree order. The checkout used to write src/A.go and skip an
// oversized src/a.go, so the scan read the small blob for both paths. Now only
// the path a clean checkout keeps is checked out, and its aliases follow it:
// too large with it, or read through its file. On a case-sensitive filesystem
// the aliases' own names are missing from the checkout, which is what shows
// they were never written.
func TestDirtySeedChecksOutTheCaseAliasACleanCheckoutKeeps(t *testing.T) {
	t.Parallel()
	skipWhereCaseFolds(t)
	repoDir := seedHeadTestRepo(t)
	big := strings.Repeat("x", 200)
	writeSeedHeadFiles(t, repoDir, map[string]string{
		"src/A.go": "package src\n",
		"src/a.go": big,
		"x/B.txt":  big,
		"x/b.txt":  "small\n",
		"z/D.md":   "# selected\n",
		"z/d.md":   "# past --max-files\n",
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "case aliases")
	commit := strings.TrimSpace(gitOutputAt(t, repoDir, "rev-parse", "HEAD"))
	// 11 paths; z/d.md, the last, is past --max-files while z/D.md is not.
	opts := seedCommandOptions{maxFiles: 10, maxFileBytes: 64}
	ctx := context.Background()
	paths, entries, err := listSeedHeadTree(ctx, ExecRunner{}, repoDir, commit, opts.maxFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 11 || paths[10] != "z/d.md" {
		t.Fatalf("inventory = %q; want 11 paths ending in z/d.md", paths)
	}
	tempRoot := t.TempDir()
	c := &seedHeadCheckout{commit: commit, dir: filepath.Join(tempRoot, "tree"), paths: paths, unread: map[string]int64{}, foldsCase: true, tempRoot: tempRoot}
	if err := c.populate(ctx, ExecRunner{}, repoDir, entries, opts); err != nil {
		t.Fatal(err)
	}
	for path, written := range map[string]bool{
		"src/A.go":  false, // an alias of src/a.go
		"src/a.go":  false, // too large however it converts
		"x/B.txt":   false, // an alias of x/b.txt
		"x/b.txt":   true,
		"z/D.md":    false, // an alias of z/d.md
		"z/d.md":    true,  // checked out for z/D.md, though past --max-files
		"README.md": true,
	} {
		if _, err := os.Lstat(filepath.Join(c.dir, filepath.FromSlash(path))); (err == nil) != written {
			t.Errorf("%s: written = %v; want %v", path, err == nil, written)
		}
	}
	for path, want := range map[string]int64{"src/A.go": int64(len(big)), "src/a.go": int64(len(big))} {
		if size, ok := c.unreadSize(path); !ok || size != want {
			t.Errorf("%s: unread = %d, %v; want %d, src/a.go's size", path, size, ok, want)
		}
	}
	for _, path := range []string{"x/B.txt", "x/b.txt", "z/D.md", "z/d.md"} {
		if size, ok := c.unreadSize(path); ok {
			t.Errorf("%s: recorded unread at %d; the scan reads it through the file checked out", path, size)
		}
	}
}
