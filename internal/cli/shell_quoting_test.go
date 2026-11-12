package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shellQuotedRepoDir feeds paths into SUGGESTED COMMANDS (the semantic repo-key
// mismatch remedy, the bundle-import hint). Unquoted, a path with a space --
// ordinary on macOS -- made the remedy silently do the wrong thing when
// copied, and one with $ or a backtick would expand rather than be passed
// through.
func TestShellQuotedRepoDirQuotesPathsForSuggestedCommands(t *testing.T) {
	t.Parallel()

	for name, dir := range map[string]string{
		"space":        "/Users/me/My Projects/outer",
		"dollar":       "/srv/$HOME/outer",
		"backtick":     "/srv/`id`/outer",
		"semicolon":    "/srv/a;rm -rf x/outer",
		"single quote": "/srv/it's/outer",
		"glob":         "/srv/a*/outer",
	} {
		t.Run(name, func(t *testing.T) {
			got := shellQuotedRepoDir(dir)
			if got == dir {
				t.Fatalf("%q was passed through unquoted into a copy-pasteable command", dir)
			}
			if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
				t.Errorf("want a single-quoted token, got %s", got)
			}
			// A bare ' must be carried through as '\'' or the quoting itself
			// breaks the command.
			if strings.Contains(dir, "'") && !strings.Contains(got, `'\''`) {
				t.Errorf("an embedded single quote must be escaped: %s", got)
			}
		})
	}

	// An ordinary path stays bare, so the common case reads naturally.
	if got := shellQuotedRepoDir("/srv/outer"); got != "/srv/outer" {
		t.Errorf("an ordinary path must not be quoted, got %s", got)
	}
	if got := shellQuotedRepoDir("  "); got != "." {
		t.Errorf("an empty path must fall back to the cwd, got %s", got)
	}
}

// A nested repository is NOT warned about: a valid git repository is a valid
// setup target wherever it sits (product decision on trail 2851 — the warning
// fired on normal layouts like repos cloned under a repo-tracked parent; the
// build-from-source caution lives in the README).
func TestSetupDoesNotWarnAboutNestedRepositories(t *testing.T) {
	t.Setenv("ENTIRE_CLI_VERSION", "")
	outer := filepath.Join(t.TempDir(), "outer")
	inner := filepath.Join(outer, "inner")
	for _, d := range []string{filepath.Join(outer, ".git"), filepath.Join(inner, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var out bytes.Buffer
	cmd := NewRootCommand(Options{Version: "test", Env: EntireEnv{RepoRoot: inner}, Now: time.Now})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"setup"})
	_ = cmd.Execute()

	if strings.Contains(out.String(), "git repository inside") {
		t.Fatalf("setup must not warn about a nested repository:\n%s", out.String())
	}
}
