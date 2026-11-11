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

// END-TO-END: the nested-repo warning the command actually prints must say the
// inner repository is a legitimate target, and must carry the quoted outer
// path. The helper being correct is not the same as the call site using it.
func TestNestedRepoWarningSaysStayingPutIsFineAndQuotesThePath(t *testing.T) {
	t.Setenv("ENTIRE_CLI_VERSION", "")
	outer := filepath.Join(t.TempDir(), "My Projects", "outer")
	inner := filepath.Join(outer, "inner")
	for _, d := range []string{filepath.Join(outer, ".git"), filepath.Join(inner, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Non-vacuity: the outer path must actually contain a space, or nothing is
	// being tested.
	if !strings.Contains(outer, " ") {
		t.Fatalf("fixture path has no space: %s", outer)
	}

	var out bytes.Buffer
	cmd := NewRootCommand(Options{Version: "test", Env: EntireEnv{RepoRoot: inner}, Now: time.Now})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	// NO positional arg: the warning fires only when no explicit path is
	// given. The fixture's bare .git directories are not real repositories, so
	// setup refuses later — the warning must already have printed by then.
	cmd.SetArgs([]string{"setup"})
	_ = cmd.Execute()

	got := out.String()
	if !strings.Contains(got, "git repository inside") {
		t.Fatalf("the nested-repo warning did not fire, so this guard measures nothing:\n%s", got)
	}
	if !strings.Contains(got, "If this repository is the one you meant, ignore this warning.") {
		t.Fatalf("the warning must say the inner repository is a legitimate target:\n%s", got)
	}
	if strings.Contains(got, "setup "+outer+"\n") {
		t.Errorf("the suggested command carries the path unquoted:\n%s", got)
	}
	if !strings.Contains(got, "setup '"+outer+"'") {
		t.Errorf("the suggested command must carry the quoted path:\n%s", got)
	}
}
