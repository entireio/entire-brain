package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The tests below encode a code-execution finding: git resolves several
// configuration keys to commands it executes, and it honours them from the
// per-repository .git/config (and in-tree .gitattributes) of whatever
// repository it is pointed at. Indexing an untrusted repository must never run
// code that repository carries.
//
// Every case builds a real repository, arms one vector with a script that
// touches a sentinel file, drives a production code path with the real
// ExecRunner, and asserts both that the sentinel was NOT created and that the
// command still produced correct output (a neutralizer that breaks the output
// silently corrupts the worktree fingerprint, which is its own bug).

func gitHardenSkipUnsupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("sentinel payloads are POSIX shell scripts")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

func gitHardenRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitHardenRepo builds a repository with one committed file that has been
// modified in the worktree without changing its size. Equal sizes force git to
// re-read and re-hash the file rather than shortcutting on stat data, which is
// what makes the clean-filter and fsmonitor paths reachable.
func gitHardenRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitHardenRun(t, dir, "init", "-q", ".")
	gitHardenRun(t, dir, "config", "user.name", "Entire Brain Test")
	gitHardenRun(t, dir, "config", "user.email", "brain-test@example.invalid")
	gitHardenRun(t, dir, "config", "commit.gpgsign", "false")
	gitHardenRun(t, dir, "config", "core.hooksPath", filepath.Join(dir, ".no-hooks"))
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHardenRun(t, dir, "add", "-A")
	gitHardenRun(t, dir, "commit", "-qm", "init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("HELLO\nWORLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// gitHardenPayload writes an executable script that records the fact it ran by
// creating sentinel, then behaves like a well-mannered filter so the arming
// itself does not break the command under test.
func gitHardenPayload(t *testing.T) (script, sentinel string) {
	t.Helper()
	dir := t.TempDir()
	sentinel = filepath.Join(dir, "PWNED")
	script = filepath.Join(dir, "payload.sh")
	body := "#!/bin/sh\ntouch " + sentinel + "\nif [ -f \"$1\" ]; then cat \"$1\"; fi\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, sentinel
}

func gitHardenAssertClean(t *testing.T, sentinel, vector string) {
	t.Helper()
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("SECURITY: repo-local %s executed an attacker-controlled program during indexing", vector)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
	}
}

func TestWorktreeFingerprintDoesNotRunRepoLocalDiffExternal(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	gitHardenRun(t, repo, "config", "diff.external", payload)

	ctx := context.Background()
	fingerprint, err := worktreeFingerprint(ctx, ExecRunner{}, repo)
	if err != nil {
		t.Fatalf("worktreeFingerprint: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "diff.external")
	if fingerprint == "" {
		t.Fatal("empty fingerprint")
	}

	// diff.external also silently empties `git diff` output, so the fingerprint
	// stops seeing worktree content. Prove the diff survived neutralization.
	ignore, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	diff, err := gitDiffBinary(ctx, ExecRunner{}, repo, false, ignore)
	if err != nil {
		t.Fatalf("gitDiffBinary: %v", err)
	}
	if !strings.Contains(string(diff), "a.txt") || !strings.Contains(string(diff), "HELLO") {
		t.Fatalf("diff.external degraded the worktree diff; got %q", string(diff))
	}
}

func TestGitDiffBinaryDoesNotRunAttributeTextconv(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("a.txt diff=pwn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHardenRun(t, repo, "add", ".gitattributes")
	gitHardenRun(t, repo, "config", "diff.pwn.textconv", payload)

	ctx := context.Background()
	ignore, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	diff, err := gitDiffBinary(ctx, ExecRunner{}, repo, false, ignore)
	if err != nil {
		t.Fatalf("gitDiffBinary: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "diff.<driver>.textconv")
	if !strings.Contains(string(diff), "a.txt") {
		t.Fatalf("diff lost its content; got %q", string(diff))
	}
}

func TestChangedSemanticRangesDoesNotRunAttributeTextconv(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("a.txt diff=pwn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHardenRun(t, repo, "add", ".gitattributes")
	gitHardenRun(t, repo, "config", "diff.pwn.textconv", payload)

	if _, err := changedSemanticRanges(context.Background(), ExecRunner{}, repo, true); err != nil {
		t.Fatalf("changedSemanticRanges: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "diff.<driver>.textconv")
}

func TestGitStatusPorcelainAllDoesNotRunCoreFsmonitor(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	gitHardenRun(t, repo, "config", "core.fsmonitor", payload)

	status, err := gitStatusPorcelainAll(context.Background(), ExecRunner{}, repo)
	if err != nil {
		t.Fatalf("gitStatusPorcelainAll: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "core.fsmonitor")
	if !strings.Contains(string(status), "a.txt") {
		t.Fatalf("status lost its content; got %q", string(status))
	}
}

func TestChangedSemanticFilesDoesNotRunCoreFsmonitor(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	gitHardenRun(t, repo, "config", "core.fsmonitor", payload)

	files, err := changedSemanticFiles(context.Background(), ExecRunner{}, repo)
	if err != nil {
		t.Fatalf("changedSemanticFiles: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "core.fsmonitor")
	if len(files) == 0 {
		t.Fatal("changed-file list came back empty")
	}
}

func TestSeedWorktreeFilesDoNotRunCoreFsmonitor(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	gitHardenRun(t, repo, "config", "core.fsmonitor", payload)

	if _, _, err := (ExecRunner{}).Run(context.Background(), repo, "git", "ls-files"); err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "core.fsmonitor")
}

// gitHardenFilterRepo arms a filter driver on every file of a fresh repository.
// filterKey is "clean", "smudge" or "process"; driver is the .gitattributes
// driver name, which is attacker-chosen and need not be a tidy identifier.
func gitHardenFilterRepo(t *testing.T, driver, filterKey string) (repo, sentinel string) {
	t.Helper()
	repo = gitHardenRepo(t)
	payload, sentinel := gitHardenPayload(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("a.txt filter="+driver+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHardenRun(t, repo, "add", ".gitattributes")
	gitHardenRun(t, repo, "config", "filter."+driver+"."+filterKey, payload)
	return repo, sentinel
}

// TestGitDiffBinaryDoesNotRunRepoLocalCleanFilter is the fourth vector.
//
// `git diff HEAD` compares the index/HEAD blob against the WORKTREE file, and
// to do that git must run the content through the clean filter selected by
// .gitattributes. So filter.<driver>.clean executes under the exact hardened
// argv the other three vectors are neutralized by:
//
//	git -c core.fsmonitor=false diff --no-ext-diff --no-textconv --binary HEAD --
//
// Confirmed against git 2.54.0 before the fix. Neither --no-ext-diff nor
// --no-textconv touches the filter machinery: they neutralize the diff drivers,
// which are a different mechanism entirely.
func TestGitDiffBinaryDoesNotRunRepoLocalCleanFilter(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, sentinel := gitHardenFilterRepo(t, "pwn", "clean")

	ctx := context.Background()
	ignore, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	diff, err := gitDiffBinary(ctx, ExecRunner{}, repo, false, ignore)
	if err != nil {
		t.Fatalf("gitDiffBinary: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "filter.<driver>.clean")
	// The neutralizer must not cost the output either: a blanked clean filter
	// still has to produce the real worktree diff.
	if !strings.Contains(string(diff), "a.txt") || !strings.Contains(string(diff), "HELLO") {
		t.Fatalf("blanking the clean filter degraded the worktree diff; got %q", string(diff))
	}
}

// TestWorktreeFingerprintDoesNotRunRepoLocalCleanFilter drives the same vector
// through the fingerprint path, which is what actually runs during indexing.
func TestWorktreeFingerprintDoesNotRunRepoLocalCleanFilter(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, sentinel := gitHardenFilterRepo(t, "pwn", "clean")

	fingerprint, err := worktreeFingerprint(context.Background(), ExecRunner{}, repo)
	if err != nil {
		t.Fatalf("worktreeFingerprint: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "filter.<driver>.clean")
	if fingerprint == "" {
		t.Fatal("empty fingerprint")
	}
}

// TestGitDiffBinaryDoesNotRunRepoLocalProcessFilter covers the sibling key.
// filter.<driver>.process is a long-running protocol filter that SUPERSEDES
// clean and smudge, so blanking only .clean would leave this arming live —
// verified: with .clean blanked and .process armed, the payload still ran.
func TestGitDiffBinaryDoesNotRunRepoLocalProcessFilter(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, sentinel := gitHardenFilterRepo(t, "pwn", "process")

	ctx := context.Background()
	ignore, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	if _, err := gitDiffBinary(ctx, ExecRunner{}, repo, false, ignore); err != nil {
		t.Fatalf("gitDiffBinary: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "filter.<driver>.process")
}

// TestGitDiffBinaryDoesNotRunSmudgeFilter covers the third key. Smudge runs on
// the checkout side rather than the diff side, so it is not reachable from
// gitDiffBinary today; the assertion is that arming it changes nothing, which
// keeps the neutralizer honest if a future call path does check content out.
func TestGitDiffBinaryDoesNotRunSmudgeFilter(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, sentinel := gitHardenFilterRepo(t, "pwn", "smudge")

	ctx := context.Background()
	ignore, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	if _, err := gitDiffBinary(ctx, ExecRunner{}, repo, false, ignore); err != nil {
		t.Fatalf("gitDiffBinary: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "filter.<driver>.smudge")
}

// TestRequiredCleanFilterDoesNotBreakTheRead: filter.<driver>.required=true
// makes git ABORT ("fatal: ... clean filter 'x' failed") when the filter
// command is blanked. A neutralizer that turns a code-execution bug into a
// hard failure to index the repository has only traded one denial for another,
// so required must be forced false alongside the command.
func TestRequiredCleanFilterDoesNotBreakTheRead(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, sentinel := gitHardenFilterRepo(t, "pwn", "clean")
	gitHardenRun(t, repo, "config", "filter.pwn.required", "true")

	ctx := context.Background()
	ignore, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	diff, err := gitDiffBinary(ctx, ExecRunner{}, repo, false, ignore)
	if err != nil {
		t.Fatalf("a required filter broke the read instead of being neutralized: %v", err)
	}
	gitHardenAssertClean(t, sentinel, "filter.<driver>.clean (required)")
	if !strings.Contains(string(diff), "a.txt") {
		t.Fatalf("diff lost its content; got %q", string(diff))
	}
}

// TestCleanFilterWithAnEqualsInItsDriverNameIsNeutralized is why the
// neutralizer is delivered through GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n rather
// than `-c`.
//
// A config subsection may contain "=", and .gitattributes may select it:
// `[filter "na=me"]` armed by `* filter=na=me` executes. But `-c` splits its
// argument at the FIRST "=", so `-c filter.na=me.clean=` sets the key
// "filter.na" to the value "me.clean=" and the real driver is untouched.
// Verified against git 2.54.0: the `-c` form fails to neutralize this and the
// payload runs. The environment form keeps key and value in separate variables,
// so no name can escape it.
func TestCleanFilterWithAnEqualsInItsDriverNameIsNeutralized(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, sentinel := gitHardenFilterRepo(t, "na=me", "clean")

	ctx := context.Background()
	ignore, err := loadBrainIgnore(repo)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	if _, err := gitDiffBinary(ctx, ExecRunner{}, repo, false, ignore); err != nil {
		t.Fatalf("gitDiffBinary: %v", err)
	}
	gitHardenAssertClean(t, sentinel, `filter."na=me".clean`)
}

// TestGlobalFilterDriversAreLeftAlone draws the trust boundary.
//
// The finding is that a REPOSITORY must not be able to make git run code. A
// driver configured in the user's own ~/.gitconfig is not the repository's
// code, it is the user's — `git lfs install` puts filter.lfs.clean there — and
// blanking it would silently corrupt every diff of an LFS repository (git would
// compare a real worktree file against a stored pointer). So only local and
// worktree scope, the scopes the repository itself carries, are neutralized.
func TestGlobalFilterDriversAreLeftAlone(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo := gitHardenRepo(t)
	overrides := repoFilterDriverOverrides(context.Background(), repo)
	if len(overrides) != 0 {
		t.Fatalf("a repository with no local filter driver produced overrides: %v", overrides)
	}

	gitHardenRun(t, repo, "config", "--local", "filter.local-one.clean", "cat")
	overrides = repoFilterDriverOverrides(context.Background(), repo)
	var keys []string
	for _, o := range overrides {
		keys = append(keys, o.Key)
	}
	for _, want := range []string{
		"filter.local-one.clean", "filter.local-one.smudge",
		"filter.local-one.process", "filter.local-one.required",
	} {
		if !slices.Contains(keys, want) {
			t.Fatalf("local driver not fully neutralized: %q missing from %v", want, keys)
		}
	}
}
