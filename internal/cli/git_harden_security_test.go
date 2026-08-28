package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
