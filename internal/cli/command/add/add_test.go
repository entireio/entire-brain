package add

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// recordingRunner captures the git commands runBrainAdd issues.
type recordingRunner struct{ calls [][]string }

func (r *recordingRunner) Run(_ context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, append([]string{dir, name}, args...))
	return nil, nil, nil
}

func newAddTestCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	c.SetContext(context.Background())
	return c
}

func TestRunBrainAddRejectsUnsafeNames(t *testing.T) {
	for _, url := range []string{"https://h/..", "https://h/.", "git@h:..", "-/oops", "--upload-pack=x"} {
		rr := &recordingRunner{}
		opts := base.Options{Runner: rr}
		err := runBrainAdd(newAddTestCmd(), opts, addFlags{dir: t.TempDir(), build: false}, url)
		if err == nil {
			t.Errorf("runBrainAdd(%q) should reject the unsafe URL", url)
		}
		if len(rr.calls) != 0 {
			t.Errorf("runBrainAdd(%q) must not run git before validating; ran %v", url, rr.calls)
		}
	}
}

func TestRunBrainAddCloneUsesEndOfOptions(t *testing.T) {
	rr := &recordingRunner{}
	opts := base.Options{Runner: rr}
	dir := t.TempDir()
	if err := runBrainAdd(newAddTestCmd(), opts, addFlags{dir: dir, build: false}, "https://github.com/acme/widget"); err != nil {
		t.Fatal(err)
	}
	if len(rr.calls) == 0 {
		t.Fatal("expected a git clone call")
	}
	clone := rr.calls[0]
	joined := strings.Join(clone, " ")
	if !strings.Contains(joined, "clone") || !strings.Contains(joined, "--") {
		t.Errorf("clone should pass `--` before the URL: %v", clone)
	}
	// The `--` must come immediately before the URL positional.
	var sawSep bool
	for i, a := range clone {
		if a == "--" && i+1 < len(clone) && clone[i+1] == "https://github.com/acme/widget" {
			sawSep = true
		}
	}
	if !sawSep {
		t.Errorf("`--` must directly precede the repo URL: %v", clone)
	}
}

func TestRepoDirName(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widget":     "acme_widget",
		"https://github.com/acme/widget.git": "acme_widget",
		"https://github.com/acme/widget/":    "acme_widget",
		"git@github.com:acme/widget.git":     "acme_widget",
		"https://example.com/widget":         "widget",
		"":                                   "",
	}
	for in, want := range cases {
		if got := repoDirName(in); got != want {
			t.Errorf("repoDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsGitCheckout(t *testing.T) {
	dir := t.TempDir()
	if isGitCheckout(dir) {
		t.Errorf("a bare temp dir is not a checkout")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isGitCheckout(dir) {
		t.Errorf("a dir with a .git directory is a checkout")
	}
}

func TestRunBrainAddHonorsNoEgress(t *testing.T) {
	for _, toggle := range []string{"ENTIRE_BRAIN_NO_EGRESS", "ENTIRE_BRAIN_LOCAL_ONLY"} {
		for _, existing := range []bool{false, true} {
			t.Run(toggle+map[bool]string{false: "/clone", true: "/fetch"}[existing], func(t *testing.T) {
				t.Setenv(toggle, "1")
				dir := t.TempDir()
				if existing {
					if err := os.MkdirAll(filepath.Join(dir, "acme_widget", ".git"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				rr := &recordingRunner{}
				err := runBrainAdd(newAddTestCmd(), base.Options{Runner: rr}, addFlags{dir: dir}, "https://example.invalid/acme/widget")
				if err == nil || !strings.Contains(err.Error(), "no_egress") {
					t.Errorf("expected no-egress error: %v", err)
				}
				if len(rr.calls) != 0 {
					t.Errorf("no-egress invoked git: %v", rr.calls)
				}
			})
		}
	}
}

// cloneFailRunner mimics ExecRunner's error shape: argv and stderr embedded in
// the error, stderr also returned separately.
type cloneFailRunner struct{ stderr string }

func (r *cloneFailRunner) Run(_ context.Context, _, name string, args ...string) ([]byte, []byte, error) {
	return nil, []byte(r.stderr), fmt.Errorf("%s %v: %w: %s", name, args, errors.New("exit status 128"), r.stderr)
}

func TestRunBrainAddCloneFailureIsOneCleanLine(t *testing.T) {
	const repo = "https://github.com/sandbox-audit/nonexistent-repo.git"
	for _, tc := range []struct {
		name, stderr, want string
	}{
		{
			name:   "github-not-found",
			stderr: "remote: Repository not found.\nfatal: repository 'https://github.com/sandbox-audit/nonexistent-repo.git/' not found\n",
			want:   "clone_failed: " + repo + ": repository not found (or no access)",
		},
		{
			name:   "git-fatal-only",
			stderr: "fatal: repository 'https://github.com/sandbox-audit/nonexistent-repo.git/' not found\n",
			want:   "clone_failed: " + repo + ": repository not found (or no access)",
		},
		{
			name:   "ssh-no-access",
			stderr: "Permission denied (publickey).\nfatal: Could not read from remote repository.\n",
			want:   "clone_failed: " + repo + ": repository not found (or no access)",
		},
		{
			name:   "host-key-verification-shows-real-cause",
			stderr: "Host key verification failed.\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n",
			want:   "clone_failed: " + repo + ": Host key verification failed.",
		},
		{
			name:   "connection-refused-shows-real-cause",
			stderr: "ssh: connect to host github.com port 22: Connection refused\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n",
			want:   "clone_failed: " + repo + ": ssh: connect to host github.com port 22: Connection refused",
		},
		{
			name:   "local-permission-denied-is-not-no-access",
			stderr: "fatal: could not create work tree dir 'sandbox-audit_nonexistent-repo': Permission denied\n",
			want:   "clone_failed: " + repo + ": fatal: could not create work tree dir 'sandbox-audit_nonexistent-repo': Permission denied",
		},
		{
			name:   "upload-pack-missing-is-not-not-found",
			stderr: "bash: git-upload-pack: command not found\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n",
			want:   "clone_failed: " + repo + ": bash: git-upload-pack: command not found",
		},
		{
			name:   "footer-only-survives",
			stderr: "fatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.\n",
			want:   "clone_failed: " + repo + ": fatal: Could not read from remote repository.; Please make sure you have the correct access rights; and the repository exists.",
		},
		{
			name:   "other-failure-deduplicated",
			stderr: "fatal: unable to access: Could not resolve host: github.com\nfatal: unable to access: Could not resolve host: github.com\n",
			want:   "clone_failed: " + repo + ": fatal: unable to access: Could not resolve host: github.com",
		},
		{
			name:   "empty-stderr-falls-back-to-root-cause",
			stderr: "",
			want:   "clone_failed: " + repo + ": exit status 128",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runBrainAdd(newAddTestCmd(), base.Options{Runner: &cloneFailRunner{stderr: tc.stderr}}, addFlags{dir: t.TempDir(), build: false}, repo)
			if err == nil {
				t.Fatal("expected a clone failure")
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "[") {
				t.Errorf("error must be one line without git argv: %q", err)
			}
		})
	}
}

type addOriginRunner struct {
	recordingRunner
	origin string
}

func (r *addOriginRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	r.recordingRunner.Run(ctx, dir, name, args...)
	if strings.Join(args, " ") == "remote get-url origin" {
		return []byte(r.origin), nil, nil
	}
	return nil, nil, nil
}
func TestRunBrainAddChecksExistingOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		wantError    bool
	}{
		{"different-host", "https://first.invalid/acme/widget", true},
		{"different-parent", "https://second.invalid/other/acme/widget", true},
		{"missing-origin", "", true},
		{"matching", "https://second.invalid/acme/widget.git", false},
		{"matching-ssh", "git@second.invalid:acme/widget.git", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
			t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "acme_widget", ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			rr := &addOriginRunner{origin: tc.origin}
			err := runBrainAdd(newAddTestCmd(), base.Options{Runner: rr}, addFlags{dir: dir}, "https://second.invalid/acme/widget")
			if (err != nil) != tc.wantError {
				t.Errorf("add error=%v, wantError=%v", err, tc.wantError)
			}
			for _, call := range rr.calls {
				if tc.wantError && strings.Contains(strings.Join(call, " "), " fetch ") {
					t.Errorf("fetched colliding checkout: %v", call)
				}
			}
		})
	}
}
