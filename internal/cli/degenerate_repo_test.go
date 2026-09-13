package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// A degenerate repository already exited 1, which was right. What it printed was
// the first git subprocess that happened to notice, verbatim:
//
//	resolve HEAD for semantic index: git [-c core.fsmonitor=false rev-parse HEAD]:
//	exit status 128: fatal: ambiguous argument 'HEAD': unknown revision or path
//	not in the working tree.
//	Use '--' to separate paths from revisions, like this:
//	'git <command> [<revision>...] -- [<file>...]'
//
// Three things are wrong with that. It does not name the condition ("no commits
// yet"), it names no way out, and `-c core.fsmonitor=false` is an argument this
// binary adds for hardening and the reader never typed -- so the one concrete
// detail in the message is about our internals.

// gitAt runs real git, because these conditions are properties of an actual
// repository on disk and a scripted runner cannot express them: the fake keys on
// (name, args) and never on the directory the command runs in.
func gitAt(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
}

func TestDegenerateRepositoriesAreNamedRatherThanLeakingGitText(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	raw := errors.New("resolve HEAD for semantic index: git [-c core.fsmonitor=false rev-parse HEAD]: exit status 128: fatal: ambiguous argument 'HEAD': unknown revision or path not in the working tree")

	for _, tc := range []struct {
		name  string
		build func(t *testing.T) string
		want  string
	}{
		{
			name: "no commits yet",
			build: func(t *testing.T) string {
				dir := t.TempDir()
				gitAt(t, dir, "init", "-q")
				return dir
			},
			want: "no commits yet",
		},
		{
			name: "bare repository",
			build: func(t *testing.T) string {
				dir := t.TempDir()
				gitAt(t, dir, "init", "-q", "--bare")
				return dir
			},
			want: "bare repository has no working tree",
		},
		{
			name:  "not a repository",
			build: func(t *testing.T) string { return t.TempDir() },
			want:  "not a git repository",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.build(t)
			named := nameDegenerateRepoFailure(context.Background(), ExecRunner{}, dir, raw)
			if named == nil {
				t.Fatal("a failure was swallowed entirely")
			}
			text := named.Error()
			if !strings.Contains(text, tc.want) {
				t.Fatalf("the condition is not named:\n%s", text)
			}
			if !strings.Contains(text, dir) {
				t.Fatalf("the message does not say which repository:\n%s", text)
			}
			// The whole point: git's own text, and the argv this binary added to
			// it, must be gone.
			for _, leak := range []string{"fsmonitor", "fatal:", "exit status 128", "rev-parse"} {
				if strings.Contains(text, leak) {
					t.Fatalf("raw git detail %q survived into the message:\n%s", leak, text)
				}
			}
		})
	}

	// A DETACHED HEAD is deliberately not classified. It builds a brain, the
	// surfaces that care already say branch_tip=stale, and it is also the shape
	// that proves the unborn check needs positive evidence: an unborn HEAD is
	// confirmed by symbolic-ref SUCCEEDING and its branch then failing to
	// resolve, never by `rev-parse HEAD` merely failing -- which is equally what
	// an unavailable git looks like.
	detached := t.TempDir()
	gitAt(t, detached, "init", "-q")
	gitAt(t, detached, "config", "user.email", "t@example.com")
	gitAt(t, detached, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(detached, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, detached, "add", "-A")
	gitAt(t, detached, "commit", "-qm", "init")
	gitAt(t, detached, "checkout", "-q", "--detach")
	if got := nameDegenerateRepoFailure(context.Background(), ExecRunner{}, detached, raw); !errors.Is(got, raw) {
		t.Fatalf("a detached HEAD was classified as a degenerate repository: %v", got)
	}

	// The control, and the reason this is a classifier rather than a blanket
	// rewrite: in a repository with none of these conditions, the original error
	// is the best thing available and is returned untouched.
	healthy := t.TempDir()
	gitAt(t, healthy, "init", "-q")
	gitAt(t, healthy, "config", "user.email", "t@example.com")
	gitAt(t, healthy, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(healthy, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitAt(t, healthy, "add", "-A")
	gitAt(t, healthy, "commit", "-qm", "init")
	if got := nameDegenerateRepoFailure(context.Background(), ExecRunner{}, healthy, raw); !errors.Is(got, raw) {
		t.Fatalf("a healthy repository had an unrelated failure rewritten: %v", got)
	}
}

// TestTheSemanticIndexNamesAnEmptyRepository drives the classifier through the
// call path that actually produced the reported message, so the wiring is
// covered and not just the helper.
func TestTheSemanticIndexNamesAnEmptyRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repoDir := t.TempDir()
	gitAt(t, repoDir, "init", "-q")
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}

	err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil {
		t.Fatal("indexing a repository with no commits must fail")
	}
	text := err.Error()
	if !strings.Contains(text, "no commits yet") {
		t.Fatalf("the semantic index did not name the condition:\n%s", text)
	}
	if strings.Contains(text, "fsmonitor") || strings.Contains(text, "fatal:") {
		t.Fatalf("raw git text survived into the semantic index failure:\n%s", text)
	}
}

// TestWorktreeRefreshNamesADegenerateRepository covers the route around that
// classifier.
//
// #238 applied nameDegenerateRepoFailure at the four sites a default run
// reaches. --worktree reaches none of them: it skips the wrapped worktreeDirty
// check in runSeed entirely, and the FIRST git command it then runs is the
// worktree fingerprint, which was wrapped raw. So the same two repositories
// answered well without the flag and leaked with it:
//
//	fingerprint worktree for seed: git [-c core.fsmonitor=false
//	-c core.hooksPath=/dev/null/entire-brain-hooks-disabled status --porcelain
//	--untracked-files=all]: exit status 128: fatal: this operation must be run
//	in a work tree
//
// Note what leaks past git's own text: core.hooksPath, a flag this binary adds
// and no reader ever typed.
func TestWorktreeRefreshNamesADegenerateRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, tc := range []struct {
		name  string
		init  []string
		want  string
		verbs [][]string
	}{
		{
			name:  "bare repository",
			init:  []string{"init", "-q", "--bare"},
			want:  "bare repository has no working tree",
			verbs: [][]string{{"refresh", "seed", "--worktree"}, {"refresh", "--worktree"}},
		},
		{
			name:  "no commits yet",
			init:  []string{"init", "-q"},
			want:  "no commits yet",
			verbs: [][]string{{"refresh", "seed", "--worktree"}, {"refresh", "--worktree"}},
		},
	} {
		for _, verb := range tc.verbs {
			t.Run(tc.name+" "+strings.Join(verb, " "), func(t *testing.T) {
				repoDir := t.TempDir()
				gitAt(t, repoDir, tc.init...)
				env := semanticTestEnv(t, repoDir)
				env.RepoRoot = repoDir
				opts := Options{Version: "test", Env: env, Runner: ExecRunner{}}

				out, err := execute(t, NewRootCommand(opts), verb...)
				if err == nil {
					t.Fatalf("a degenerate repository must still fail:\n%s", out)
				}
				text := err.Error()
				if !strings.Contains(text, tc.want) {
					t.Fatalf("the condition is not named:\n%s", text)
				}
				// The hardened argv is the part that is ours, not git's, and it
				// is the part a reader can do least with.
				for _, leak := range []string{"fsmonitor", "hooksPath", "fatal:", "exit status 128"} {
					if strings.Contains(text, leak) {
						t.Fatalf("raw git detail %q survived into the message:\n%s", leak, text)
					}
				}
			})
		}
	}
}
