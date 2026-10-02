package agentsetup

import (
	"os"
	"path/filepath"
)

// OuterRepo reports the nearest ancestor of root that is itself a git
// repository, or "" when root is the outermost one.
//
// Context resolves the guide's destination by walking up from the working
// directory to the NEAREST .git, which is right until a repository is nested
// inside another. The documented build-from-source flow produces exactly that
// shape: `git clone entire-brain` inside a project, `cd entire-brain`, install
// — and leaves the shell inside the clone, so the next `init-agents` writes
// AGENTS.md, CLAUDE.md and the guide into the clone instead of the project.
// The clone is usually untracked by the outer repository, so the files are
// invisible to the team the guide was meant for and nothing reports an error.
//
// Returning the outer root lets the caller name both paths and let the person
// decide, rather than silently picking the inner one.
func OuterRepo(root string) string {
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	for p := filepath.Dir(abs); ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return p
		} else if !os.IsNotExist(err) {
			return ""
		}
		if parent := filepath.Dir(p); parent == p {
			return ""
		}
	}
}
