package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
		// A 40-byte blob checked out as 80 bytes of UTF-16: too large once
		// converted, not media, which is what its blob size alone says.
		"wide.png": utf16LE(strings.Repeat("w", 40)),
	}
	writeSeedHeadFiles(t, repoDir, statOnly)
	writeSeedHeadFiles(t, repoDir, checkedOut)
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
// more than seedHeadEncodingMaxShrink times --max-file-bytes is too large
// however it re-encodes, so it is never written out; the scan gives it the
// same reason, and so the same fingerprint, as a clean seed of the commit.
func TestDirtySeedBoundsConvertedCheckouts(t *testing.T) {
	tempDir := isolateSeedHeadTemp(t)
	repoDir := seedHeadTestRepo(t)
	huge := strings.Repeat("h", 4*64+1)
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

// gitConfigCountingRunner runs commands for real and records each git config
// call the checkout makes.
type gitConfigCountingRunner struct {
	ExecRunner
	configCalls [][]string
}

func (r *gitConfigCountingRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" && len(args) > 0 && args[0] == "config" {
		r.configCalls = append(r.configCalls, args)
	}
	return r.ExecRunner.RunWithEnv(ctx, dir, env, name, args...)
}

// TestDirtySeedReadsFilterDriversInOneCall: however many filters the commit
// names, the checkout reads the driver configuration once, and still tells a
// configured driver from a filter with none.
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
	repoDir := seedHeadTestRepo(t)
	stored := "# manual kept in LFS\n"
	large := strings.Repeat("large LFS payload ", 20)
	missing := "# never fetched\n"
	corrupt := "# corrupt object\n"
	notLFS := "# a doc that quotes a pointer\n"
	writeLFSObject(t, repoDir, stored, stored)
	writeLFSObject(t, repoDir, large, large)
	writeLFSObject(t, repoDir, corrupt, strings.Repeat("x", len(corrupt)))
	writeLFSObject(t, repoDir, notLFS, notLFS)
	writeSeedHeadFiles(t, repoDir, map[string]string{
		".gitattributes":     "docs/manual.md filter=lfs -text\ndocs/missing.md filter=lfs -text\ndocs/corrupt.md filter=lfs -text\ndata/large.txt filter=lfs -text\n",
		"docs/manual.md":     lfsPointer(stored),
		"docs/missing.md":    lfsPointer(missing),
		"docs/corrupt.md":    lfsPointer(corrupt),
		"docs/pointerish.md": lfsPointer(notLFS),
		"data/large.txt":     lfsPointer(large),
	})
	gitAt(t, repoDir, "add", "-A")
	gitAt(t, repoDir, "commit", "-q", "-m", "lfs pointers")
	dirtySeedHeadTree(t, repoDir)

	head, err := checkoutSeedHead(context.Background(), ExecRunner{}, repoDir, seedCommandOptions{maxFiles: defaultSeedMaxFiles, maxFileBytes: 64})
	if err != nil {
		t.Fatalf("check out HEAD: %v", err)
	}
	defer head.remove()
	for rel, want := range map[string]string{
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
	if warnings := strings.Join(head.warnings, "\n"); !strings.Contains(warnings, "docs/corrupt.md") || strings.Contains(warnings, "docs/missing.md") {
		t.Errorf("a stored object that fails its pointer's check must be reported, and only that one: %q", warnings)
	}
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
