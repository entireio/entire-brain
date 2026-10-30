package factstore

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

const (
	// globalStoreDirName sits beside "repos" in the plugin data directory, so a
	// global brain is visible next to the per-repo ones rather than buried
	// inside one of them.
	globalStoreDirName = "global"

	// GlobalBranch is the branch component of the global store's filename.
	// Global facts are not on a branch; this is the storage layer's requirement,
	// not a claim about the fact. Display never takes a fact's scope from this
	// value — see globalFactIDs — because a repository may legitimately have a
	// branch called "global" and a fact on it is not a global fact.
	GlobalBranch = "global"
)

// GlobalBrainDir returns the global store's directory, creating nothing. The
// caller creates it on first write, so merely reading never materialises a
// directory for somebody who has no global memories.
func GlobalBrainDir(env base.EntireEnv) (string, error) {
	dirs, err := base.ResolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dirs.Data, globalStoreDirName), nil
}

// GlobalFactsEnabled reports whether global facts should be merged into a
// repository read.
//
// Default on: a preference somebody recorded once is meant to apply, and a
// memory layer that makes you ask for what you already told it is not one.
// ENTIRE_BRAIN_NO_GLOBAL_FACTS and --no-global turn it off, for a machine where
// repository answers must not be influenced by anything outside the repository.
func GlobalFactsEnabled(noGlobalFlag bool) bool {
	if noGlobalFlag {
		return false
	}
	return !base.SecurityToggleEnabled("ENTIRE_BRAIN_NO_GLOBAL_FACTS")
}

// LoadGlobal reads the global store. A store that does not exist yet is
// not an error: it is the state of every brain before the first global fact,
// and recall must not fail because of it.
func LoadGlobal(env base.EntireEnv) ([]factmerge.Record, error) {
	brainDir, err := GlobalBrainDir(env)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(brainDir); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, nil
		}
		return nil, statErr
	}
	return Load(brainDir, GlobalBranch)
}

// GlobalFactsForRead loads the global store for a read surface, honouring the
// off switches. A damaged global store must not take retrieval down with it, so
// the error comes back for the caller to warn about rather than to propagate:
// the repository's own facts are still the answer to most queries.
func GlobalFactsForRead(env base.EntireEnv, noGlobal bool) ([]factmerge.Record, error) {
	if !GlobalFactsEnabled(noGlobal) {
		return nil, nil
	}
	return LoadGlobal(env)
}

// ResolveGlobalTarget is the global counterpart of the per-repo facts-target
// resolution, for commands operating on the global store directly.
func ResolveGlobalTarget(env base.EntireEnv) (brainDir, branch string, err error) {
	brainDir, err = GlobalBrainDir(env)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(brainDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create global brain: %w", err)
	}
	return brainDir, GlobalBranch, nil
}
