package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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

// gitHardenRun runs one of the fixture's OWN git commands. It turns hooks off
// for that invocation only, on argv, because `git init` copies whatever
// init.templateDir holds into .git/hooks and a developer with a global
// core.hooksPath would otherwise have their real pre-commit run inside a
// fixture repository.
//
// The suppression used to be written into the fixture repository's config
// instead (`git config core.hooksPath <dir>/.no-hooks`), which disarmed hooks
// for the CODE UNDER TEST as well. Every hardening test therefore ran against a
// repository where the hook vector was already neutralized by the fixture, so
// the suite was structurally incapable of noticing that nothing in the product
// neutralized it -- and it did not, until gitHardenConfig gained core.hooksPath.
// Keeping the flag on argv covers the test's own scaffolding and nothing else,
// which is what lets the hook tests below drive production code at a live hook.
func gitHardenRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + gitHooksDisabledPath}, args...)...)
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
	// --template= starts .git/hooks empty. Without it a developer whose
	// init.templateDir ships executable hooks would get them copied into every
	// fixture repository, where they are neither this test's payload nor
	// something the assertions can account for.
	gitHardenRun(t, dir, "init", "-q", "--template=", ".")
	gitHardenRun(t, dir, "config", "user.name", "Entire Brain Test")
	gitHardenRun(t, dir, "config", "user.email", "brain-test@example.invalid")
	gitHardenRun(t, dir, "config", "commit.gpgsign", "false")
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

// A FIFTH vector, and the cheapest of the five to arm: git HOOKS.
//
// The four above each need a config key, an in-tree .gitattributes entry, or
// both. A hook needs neither: an executable file at .git/hooks/<name> is the
// entire arming, and core.hooksPath merely relocates the directory for an
// attacker who would rather also set a key. Nothing in the product read
// core.hooksPath before gitHardenConfig did.
//
// post-index-change is the hook a read-only indexer reaches. git runs it
// whenever a command REWRITES the index, and git rewrites the index when it
// meets a tracked file whose cached stat data no longer matches the filesystem
// but whose content still hashes to the indexed blob: it re-reads the file,
// confirms the content, refreshes the stat cache, and writes the index back
// out. `git status` does exactly that, and `git status` is the first thing the
// worktree fingerprint runs.
//
// Measured against the unpatched binary on git 2.54.0, in a repository armed
// with nothing but an executable hook file:
//
//	refresh --worktree --agent none   3 executions
//	refresh --agent none              6
//	status                            1
//	status --verbose                  1
//	overview                          1
//
// -- every one of them while the command exited 0 and reported "+ healthy".
//
// Only post-index-change is reachable while this binary merely reads. The
// assertions below still cover the whole hook directory rather than that one
// name, because the directory is the repository's and the neutralizer is not
// hook-specific; a future call path that checks something out must not quietly
// reopen the vector under a different hook name.

const gitHardenHookName = "post-index-change"

// gitHardenRunWithHooks is gitHardenRun without the hook suppression, for the
// control that proves a test's arming actually fires. Using gitHardenRun here
// would prove nothing: it disables the very hook being checked.
func gitHardenRunWithHooks(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitHardenHookRepo builds a repository carrying a live post-index-change hook
// and returns the file that hook appends a line to on every execution.
//
// The hook is written straight to disk because git has no command that installs
// one, and it writes only to that file: post-index-change inherits the caller's
// stdout, so a chattier payload would corrupt the porcelain under test rather
// than merely proving it ran.
//
// b.txt is the file the arming works on. gitHardenRepo leaves a.txt genuinely
// modified, and git does not refresh the cached stat of a file whose content no
// longer matches the index -- it has real work to report instead. The trigger
// has to be a file that is still clean.
func gitHardenHookRepo(t *testing.T) (repo, log string) {
	t.Helper()
	repo = gitHardenRepo(t)
	log = filepath.Join(t.TempDir(), "EXECUTIONS")

	hooks := filepath.Join(repo, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\necho fired >> " + log + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, gitHardenHookName), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Staging b.txt alone keeps a.txt in the index exactly as HEAD has it, so
	// the commit does not swallow the worktree modification the other tests in
	// this file depend on.
	gitHardenRun(t, repo, "add", "b.txt")
	gitHardenRun(t, repo, "commit", "-qm", "armed")
	return repo, log
}

// gitHardenArmHook leaves b.txt's content alone and makes only its stat data
// disagree with the index, which is the state that costs git an index rewrite.
// The timestamp is pushed into the future because git additionally refuses to
// trust a cached stat whose mtime is not safely in the past, so a future date
// arms the check on any filesystem timestamp granularity.
func gitHardenArmHook(t *testing.T, repo, log string) {
	t.Helper()
	future := time.Now().Add(365 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(repo, "b.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(log); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// gitHardenHookExecutions counts the lines the payload appended, so a failure
// can say how many times the repository ran code rather than just that it did.
func gitHardenHookExecutions(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read hook log: %v", err)
	}
	return len(strings.Fields(string(data)))
}

// gitHardenRequireLiveHook proves the arming fires before anything is asserted
// about it. Without this control a hook test that silently stopped arming
// anything -- a git change, a coarse-grained filesystem -- would keep passing
// while defending nothing.
func gitHardenRequireLiveHook(t *testing.T, repo, log string) {
	t.Helper()
	gitHardenArmHook(t, repo, log)
	gitHardenRunWithHooks(t, repo, "status", "--porcelain")
	if gitHardenHookExecutions(t, log) == 0 {
		t.Skipf("plain `git status` did not run %s here, so the vector cannot be exercised", gitHardenHookName)
	}
}

// gitHardenBrainEnv points the real commands at a throwaway brain store. The
// commands below are driven through NewRootCommand rather than through the
// individual git helpers because the finding is about what a user typing
// `entire-brain status` gets, and because the command layer is where a future
// git spawn that forgot the runner would show up.
func gitHardenBrainEnv(t *testing.T, repo string) EntireEnv {
	t.Helper()
	root := t.TempDir()
	return EntireEnv{
		RepoRoot:        repo,
		PluginDataDir:   filepath.Join(root, "data"),
		PluginCacheDir:  filepath.Join(root, "cache"),
		PluginStateDir:  filepath.Join(root, "state"),
		PluginConfigDir: filepath.Join(root, "config"),
	}
}

// gitHardenAssertCommandsRunNoHook drives every command the vector was measured
// on and asserts the repository never got to run code. A command that fails is
// still evidence -- the hook must not run either way -- so its error is
// reported alongside a violation rather than ending the test early.
func gitHardenAssertCommandsRunNoHook(t *testing.T, repo, log, vector string) {
	t.Helper()
	env := gitHardenBrainEnv(t, repo)
	for _, argv := range [][]string{
		{"refresh", "--worktree", "--agent", "none"},
		{"refresh", "--agent", "none"},
		{"status"},
		{"status", "--verbose"},
		{"overview"},
	} {
		gitHardenArmHook(t, repo, log)
		cmd := NewRootCommand(Options{Version: "test-version", Env: env})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(argv)
		err := cmd.Execute()
		if n := gitHardenHookExecutions(t, log); n != 0 {
			t.Fatalf("SECURITY: `entire-brain %s` ran a repo-local %s hook %d time(s) via %s (command err: %v)\n%s",
				strings.Join(argv, " "), gitHardenHookName, n, vector, err, out.String())
		}
	}
}

// TestIndexingDoesNotRunRepoLocalGitHooks is the cheapest arming of all: a file
// and a chmod, with nothing at all written to .git/config.
func TestIndexingDoesNotRunRepoLocalGitHooks(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, log := gitHardenHookRepo(t)
	gitHardenRequireLiveHook(t, repo, log)

	gitHardenAssertCommandsRunNoHook(t, repo, log, ".git/hooks")

	// The same vector at the two functions every one of those commands reaches
	// it through, so a regression is reported against the code that carries the
	// defect and not only against the command that surfaced it.
	gitHardenArmHook(t, repo, log)
	if _, err := gitStatusPorcelainAll(context.Background(), ExecRunner{}, repo); err != nil {
		t.Fatalf("gitStatusPorcelainAll: %v", err)
	}
	if n := gitHardenHookExecutions(t, log); n != 0 {
		t.Fatalf("SECURITY: gitStatusPorcelainAll ran a repo-local %s hook %d time(s)", gitHardenHookName, n)
	}

	gitHardenArmHook(t, repo, log)
	fingerprint, err := worktreeFingerprint(context.Background(), ExecRunner{}, repo)
	if err != nil {
		t.Fatalf("worktreeFingerprint: %v", err)
	}
	if n := gitHardenHookExecutions(t, log); n != 0 {
		t.Fatalf("SECURITY: worktreeFingerprint ran a repo-local %s hook %d time(s)", gitHardenHookName, n)
	}
	// Suppressing hooks must not cost the read: git still has to refresh the
	// index and report the worktree, it just must not announce it to the
	// repository's own code.
	if fingerprint == "" {
		t.Fatal("empty fingerprint")
	}
}

// TestIndexingDoesNotRunGitHooksRelocatedByHooksPath covers the config-key
// half. core.hooksPath moves the directory, so a neutralizer that only ignored
// .git/hooks -- or that pointed core.hooksPath at a RELATIVE path, which git
// resolves against the repository's own worktree -- would still execute here.
func TestIndexingDoesNotRunGitHooksRelocatedByHooksPath(t *testing.T) {
	gitHardenSkipUnsupported(t)
	repo, log := gitHardenHookRepo(t)

	elsewhere := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".git", "hooks", gitHardenHookName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(elsewhere, gitHardenHookName), body, 0o755); err != nil {
		t.Fatal(err)
	}
	// Remove the original so a pass cannot come from the default directory
	// being the one that was neutralized.
	if err := os.Remove(filepath.Join(repo, ".git", "hooks", gitHardenHookName)); err != nil {
		t.Fatal(err)
	}
	gitHardenRun(t, repo, "config", "core.hooksPath", elsewhere)

	gitHardenRequireLiveHook(t, repo, log)
	gitHardenAssertCommandsRunNoHook(t, repo, log, "core.hooksPath")
}
