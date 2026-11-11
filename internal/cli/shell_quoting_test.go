package cli

import (
	"strings"
	"testing"
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
