package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A PreToolUse hook in <repo>/.claude/settings.json is project scoped — unless
// the repository root is the home directory, where that path IS Claude Code's
// global settings file and the hook runs in every session on the machine, for
// every project.
//
// Observed, not imagined: on a Windows machine whose C:\Users\<user> was itself
// a git repository, a run resolved the home directory as the repo root and
// merged the hook into the user's global settings, where it stayed.
func TestInstallPreEditHookRefusesToWriteTheUsersGlobalSettings(t *testing.T) {
	home := redirectHomeDir(t)

	path, changed, err := installPreEditHook(home)
	if err == nil {
		t.Fatal("installing into the home directory must be refused; it just wrote the user's global settings")
	}
	if changed {
		t.Error("a refused install must report no change")
	}
	if want := filepath.Join(home, claudeSettingsDir, claudeSettingsFile); path != want {
		t.Errorf("path = %q, want %q so the message can name the file", path, want)
	}
	// The refusal must be a refusal, not a report after the fact.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("%s exists after a refused install: %v", path, statErr)
	}
}

// The positive control. Without it, a guard that refused everything would look
// identical to a guard that refuses the right thing.
func TestInstallPreEditHookStillWiresAnOrdinaryRepository(t *testing.T) {
	home := redirectHomeDir(t)
	repo := filepath.Join(home, "projects", "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	path, changed, err := installPreEditHook(repo)
	if err != nil {
		t.Fatalf("a repository below the home directory must still be wired: %v", err)
	}
	if !changed {
		t.Error("a fresh repository should have gained the hook")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("settings file not written at %s: %v", path, err)
	}
}

func TestIsUserHomeDir(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		repoDir string
		homeDir string
		want    bool
	}{
		{"identical", "/Users/sam", "/Users/sam", true},
		{"child is not the home", "/Users/sam/code", "/Users/sam", false},
		{"parent is not the home", "/Users", "/Users/sam", false},
		{"trailing separator", "/Users/sam/", "/Users/sam", true},
		// Windows hands back whatever casing the profile was created with, and
		// a repo root can arrive with either separator.
		{"windows casing differs", `C:\Users\Sam`, `c:\users\sam`, true},
		{"windows mixed separators", "C:/Users/Sam", `C:\Users\Sam`, true},
		{"windows sibling", `C:\Users\Sam2`, `C:\Users\Sam`, false},
		{"empty repo dir", "", "/Users/sam", false},
		{"empty home dir", "/Users/sam", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isUserHomeDir(tc.repoDir, tc.homeDir); got != tc.want {
				t.Fatalf("isUserHomeDir(%q, %q) = %v, want %v", tc.repoDir, tc.homeDir, got, tc.want)
			}
		})
	}
}
