package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// A timestamp git's index cannot already be carrying, so rewriting a file with
// this mtime reliably invalidates the cached stat.
var timeFarFuture = time.Date(2030, 1, 1, 1, 1, 1, 0, time.UTC)

// The flag is spelled out here rather than read back out of gitHardenConfig on
// purpose: deriving the expectation from the thing under test would make this
// file pass no matter where the flag moved to, or whether it survived at all.
const noOptionalLocksFlag = "--no-optional-locks"

// --no-optional-locks is a GIT-LEVEL flag. Placed after the subcommand git
// parses it as a subcommand option and exits non-zero, so position is part of
// the guard, not a stylistic preference.
func TestHardenedGitArgsPutsNoOptionalLocksBeforeTheSubcommand(t *testing.T) {
	t.Parallel()
	invocations := [][]string{
		{"status", "--porcelain"},
		{"status", "--porcelain", "--untracked-files=all"},
		{"diff-index", "-M", "--shortstat", "HEAD"},
		{"diff", "--binary", "HEAD"},
		{"diff", "--cached", "--binary", "HEAD", "--", ".", ":(exclude).env"},
		{"diff", "--name-status", "-M", "-C", "HEAD"},
		{"-c", "core.quotepath=false", "diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z"},
		{"-c", "core.quotepath=false", "log", "--no-merges", "--format=%H"},
		{"ls-files", "--others", "--exclude-standard"},
		{"rev-parse", "--show-toplevel"},
		{"branch", "--show-current"},
		{"config", "-z", "--show-scope", "--name-only", "--get-regexp", "^filter\\."},
		{"clone", "--quiet", "--", "https://example.invalid/r.git", "d"},
		{"fetch", "--quiet", "origin"},
		{"commit", "-qm", "msg"},
	}
	for _, args := range invocations {
		got := hardenedGitArgs(args...)
		at := slices.Index(got, noOptionalLocksFlag)
		if at < 0 {
			t.Errorf("%q: spawned argv has no %s: %q", args, noOptionalLocksFlag, got)
			continue
		}
		if slices.Index(got[at+1:], noOptionalLocksFlag) >= 0 {
			t.Errorf("%q: %s appears more than once: %q", args, noOptionalLocksFlag, got)
		}
		sub := gitSubcommandIndex(got)
		if sub < 0 {
			t.Errorf("%q: no subcommand in %q", args, got)
			continue
		}
		if at >= sub {
			t.Errorf("%q: %s sits at %d, at or after the subcommand %q at %d: %q",
				args, noOptionalLocksFlag, at, got[sub], sub, got)
		}
	}
}

// An empty argument list is not a git invocation, so it must not grow a flag
// that would make it look like one.
func TestHardenedGitArgsAddsNothingToAnEmptyInvocation(t *testing.T) {
	t.Parallel()
	if got := hardenedGitArgs(); got != nil {
		t.Fatalf("hardenedGitArgs() = %q, want nil", got)
	}
}

// The unit test above guards the argv BUILDER. This one guards what the
// process actually receives, through the real ExecRunner, by putting a
// recording stub ahead of git on PATH. Issue #326 is about a lock a spawned
// process takes, so the spawned argv is the thing that has to be right.
func TestExecRunnerSpawnsGitWithNoOptionalLocks(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	record := filepath.Join(bin, "argv")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	// The filter-driver probe spawns git directly and is not the invocation
	// under test; answering it with git's own "no matching keys" result (exit
	// 1, no output) keeps it out of the recording.
	script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = '--get-regexp' ]; then exit 1; fi\ndone\n" +
		"for arg do printf '%s\\n' \"$arg\" >> " + quote(record) + "; done\n" +
		"printf -- '---\\n' >> " + quote(record) + "\nexec " + quote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, args := range [][]string{
		{"status", "--porcelain", "--untracked-files=all"},
		{"diff-index", "-M", "--shortstat", "HEAD"},
	} {
		if err := os.Remove(record); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if _, _, err := (ExecRunner{}).Run(context.Background(), repo, "git", args...); err != nil {
			t.Fatalf("run %q: %v", args, err)
		}
		raw, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("stub recorded nothing for %q: %v", args, err)
		}
		spawned := strings.Split(strings.TrimSuffix(strings.TrimSpace(string(raw)), "\n---"), "\n")
		at := slices.Index(spawned, noOptionalLocksFlag)
		if at < 0 {
			t.Fatalf("%q: spawned git without %s: %q", args, noOptionalLocksFlag, spawned)
		}
		sub := gitSubcommandIndex(spawned)
		if sub < 0 || at >= sub {
			t.Fatalf("%q: %s at %d is not before the subcommand (index %d): %q",
				args, noOptionalLocksFlag, at, sub, spawned)
		}
	}
}

var shortstatNumbers = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

// diffStatTriple parses the three numbers a caller reads out of a --shortstat
// line. Comparing the parsed numbers rather than the sentence is what the
// swap has to preserve.
func diffStatTriple(t *testing.T, line string) [3]string {
	t.Helper()
	m := shortstatNumbers.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("not a --shortstat line: %q", line)
	}
	out := [3]string{m[1], "0", "0"}
	if m[2] != "" {
		out[1] = m[2]
	}
	if m[3] != "" {
		out[2] = m[3]
	}
	return out
}

// dirtyWorktreeForDiffStat builds a worktree holding, deliberately, one of
// every case the swap could get wrong: a RENAME (plumbing does no rename
// detection unless asked, and gets a different count without it), an ordinary
// edit, an untracked file (never part of `diff HEAD` at all) and a file whose
// stat data is stale while its CONTENT IS UNCHANGED -- the case that makes git
// want to refresh the index in the first place, and the case a raw plumbing
// diff reports as modified when a porcelain diff does not.
func dirtyWorktreeForDiffStat(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitHardenRun(t, dir, "init", "-q", "--template=", ".")
	gitHardenRun(t, dir, "config", "user.name", "Entire Brain Test")
	gitHardenRun(t, dir, "config", "user.email", "brain-test@example.invalid")
	gitHardenRun(t, dir, "config", "commit.gpgsign", "false")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("renamed.txt", "a\nb\nc\nd\ne\n")
	write("edited.txt", "x\ny\n")
	write("untouched.txt", "keep\n")
	gitHardenRun(t, dir, "add", "-A")
	gitHardenRun(t, dir, "commit", "-qm", "init")
	gitHardenRun(t, dir, "mv", "renamed.txt", "renamed-elsewhere.txt")
	write("edited.txt", "x\ny\nz\n")
	write("new.txt", "untracked\n")
	makeStatDirty(t, dir, "untouched.txt")
	return dir
}

// makeStatDirty rewrites a file with its own content and a new mtime, so the
// index's cached stat no longer matches while the blob does.
func makeStatDirty(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, timeFarFuture, timeFarFuture); err != nil {
		t.Fatal(err)
	}
}

func indexDigest(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// The replacement must report the SAME NUMBERS as `git diff --shortstat HEAD`,
// which is the command it replaced. The production call runs first, while the
// stat dirt is still there; the reference porcelain runs second because it
// refreshes the index and would otherwise clean the fixture up before the code
// under test ever saw it.
func TestBrainLiveDiffStatMatchesThePorcelainOnADirtyWorktree(t *testing.T) {
	gitHardenSkipUnsupported(t)
	dir := dirtyWorktreeForDiffStat(t)

	live, err := brainLiveStateReport(context.Background(), ExecRunner{}, dir, nil)
	if err != nil {
		t.Fatalf("brainLiveStateReport: %v", err)
	}
	if strings.TrimSpace(live.DiffStat) == "" {
		t.Fatalf("no diff stat produced on a dirty worktree; warnings: %q", live.Warnings)
	}

	cmd := exec.Command("git", "-c", "core.hooksPath="+gitHooksDisabledPath, "diff", "--shortstat", "HEAD")
	cmd.Dir = dir
	reference, err := cmd.Output()
	if err != nil {
		t.Fatalf("reference porcelain: %v", err)
	}

	got := diffStatTriple(t, live.DiffStat)
	want := diffStatTriple(t, string(reference))
	if got != want {
		t.Fatalf("diff stat numbers changed\n got %v from %q\nwant %v from %q",
			got, strings.TrimSpace(live.DiffStat), want, strings.TrimSpace(string(reference)))
	}
	// Pin the fixture's own meaning, so the comparison above cannot pass by
	// both sides degrading together: the rename must be counted as ONE changed
	// file, not as a delete plus an add, and the stat-dirty file must not be
	// counted at all.
	if want != [3]string{"2", "1", "0"} {
		t.Fatalf("fixture no longer exercises rename detection: porcelain said %q", strings.TrimSpace(string(reference)))
	}
}

// The regression itself (issue #326): inspecting a repository must not write
// .git/index, because writing it means holding .git/index.lock, which makes a
// concurrent human or agent git command fail outright.
//
// Scope is honest and narrow: manifest is nil, so this covers the git commands
// brainLiveStateReport itself runs. changedSemanticFiles, reached only with a
// semantic manifest, still runs `git diff --name-status HEAD`, which refreshes
// the index even under --no-optional-locks.
func TestBrainLiveStateReportDoesNotWriteTheIndex(t *testing.T) {
	gitHardenSkipUnsupported(t)
	dir := dirtyWorktreeForDiffStat(t)

	before := indexDigest(t, dir)
	if _, err := brainLiveStateReport(context.Background(), ExecRunner{}, dir, nil); err != nil {
		t.Fatalf("brainLiveStateReport: %v", err)
	}
	if after := indexDigest(t, dir); after != before {
		t.Fatalf("inspecting the repository rewrote .git/index (it took .git/index.lock): %s -> %s", before, after)
	}

	// Control. A stat-dirty worktree is the precondition for the whole test:
	// if the fixture were not stat-dirty, nothing would want the lock and the
	// assertion above would hold for the wrong reason. Plain porcelain git,
	// run with no hardening at all, must rewrite the index here.
	control := exec.Command("git", "-c", "core.hooksPath="+gitHooksDisabledPath, "status", "--porcelain")
	control.Dir = dir
	if err := control.Run(); err != nil {
		t.Fatalf("control git status: %v", err)
	}
	if indexDigest(t, dir) == before {
		t.Fatal("fixture is not stat-dirty: unhardened git status left .git/index alone, so the assertion above proves nothing")
	}
}
