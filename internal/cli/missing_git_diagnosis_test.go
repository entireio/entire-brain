package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// gitAbsentRunner is a machine with no `git` on PATH: every git invocation
// fails the way exec does when it cannot find the binary, and nothing else is
// runnable either.
type gitAbsentRunner struct{}

func (gitAbsentRunner) Run(_ context.Context, _, name string, _ ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New("exec: \"" + name + "\": executable file not found in $PATH")
}

// gitPresentNoRepoRunner is a machine WITH git, pointed at a plain directory:
// `git --version` answers, `rev-parse` does not.
type gitPresentNoRepoRunner struct{}

func (gitPresentNoRepoRunner) Run(_ context.Context, _, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" && len(args) > 0 && args[0] == "--version" {
		return []byte("git version 2.48.1\n"), nil, nil
	}
	return nil, []byte("fatal: not a git repository (or any of the parent directories): .git\n"),
		errors.New("exit status 128")
}

// A missing `git` is not a missing repository, and the remedy for one is not
// the remedy for the other.
//
// `git rev-parse --show-toplevel` fails for both reasons and says which only in
// stderr nobody read, so every repo-ness gate reported the same thing: "not a
// git repository: <path>" -- for a path that was a perfectly good repository --
// and told the reader to run `git init`, which needs the program that is
// missing. The advice could not be followed even in principle.
func TestMissingGitIsNotReportedAsMissingRepository(t *testing.T) {
	const repoDir = "/tmp/a-real-repository"

	t.Run("git absent", func(t *testing.T) {
		err := notARepositoryError(context.Background(), gitAbsentRunner{}, repoDir,
			"entire-brain setup needs git.", "entire-brain setup", "entire-brain setup <path>")
		if err == nil {
			t.Fatal("a directory with no usable git must still be an error")
		}
		msg := err.Error()

		if strings.Contains(msg, "not a git repository") {
			t.Errorf("git is missing, not the repository, but the message says otherwise:\n%s", msg)
		}
		if !strings.Contains(msg, "git is not available") {
			t.Errorf("message does not name the real problem:\n%s", msg)
		}
		// The remedy has to be one the reader can carry out. `git init` cannot
		// be run on a machine with no git.
		if strings.Contains(msg, "git init") {
			t.Errorf("remedy needs the missing program:\n%s", msg)
		}
		if !strings.Contains(msg, "install git") {
			t.Errorf("message does not say to install git:\n%s", msg)
		}
	})

	t.Run("git present, directory is not a repository", func(t *testing.T) {
		err := notARepositoryError(context.Background(), gitPresentNoRepoRunner{}, repoDir,
			"entire-brain setup needs git.", "entire-brain setup", "entire-brain setup <path>")
		if err == nil {
			t.Fatal("a plain directory must still be an error")
		}
		msg := err.Error()

		if !strings.Contains(msg, "not a git repository") {
			t.Errorf("git is present, so the directory is the problem:\n%s", msg)
		}
		if strings.Contains(msg, "git is not available") {
			t.Errorf("git answered --version; it is available:\n%s", msg)
		}
		// Here `git init` IS followable, and is the right advice.
		if !strings.Contains(msg, "git init") {
			t.Errorf("message does not offer the remedy that works here:\n%s", msg)
		}
	})
}

// The four gates that refuse a non-repository must all route through the
// diagnosis, or the bug survives in whichever one was missed.
func TestRepoNessGatesDiagnoseMissingGit(t *testing.T) {
	ctx := context.Background()
	opts := Options{Runner: gitAbsentRunner{}, Env: EntireEnv{}}
	repoDir := t.TempDir()

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"path", func() error {
			return checkPathTargetIsAddressable(ctx, opts, repoStorage{}, repoDir)
		}},
		{"refresh", func() error {
			return nameDegenerateRepoFailure(ctx, opts.Runner, repoDir, errors.New("boom"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("expected a failure with no usable git")
			}
			if strings.Contains(err.Error(), "not a git repository") {
				t.Errorf("%s misdiagnoses a missing git as a missing repository:\n%s", tc.name, err)
			}
			if !strings.Contains(err.Error(), "git is not available") {
				t.Errorf("%s does not name the missing git:\n%s", tc.name, err)
			}
		})
	}
}
