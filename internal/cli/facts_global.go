package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// Facts that are not about one repository.
//
// Every fact a brain held belonged to a repo and a branch. That is right for
// most of them — "compaction runs above eight segments" is a claim about one
// codebase — and wrong for the rest. "I prefer table-driven tests", "we deploy
// on Thursdays", "the staging cluster is in eu-west-1" are true in every repo
// somebody works in, and had nowhere to live: record them in one repo's brain
// and they are invisible from the next, or record them in all of them and every
// correction has to be made N times.
//
// Worse, `remember` outside a repository failed outright. The one moment you
// most want to write something down — you are not in a checkout, you just
// learned something — was the one moment the command refused.
//
// A global brain is the same fact store in a different directory, sitting
// beside the per-repo ones. Reusing the store rather than inventing a second
// format means global facts get retraction, verification, taxonomy, dedupe and
// the write lock for free, and `facts` reads them with the code that already
// exists. Recall merges them in; everything else about them is ordinary.

const (
	// globalStoreDirName sits beside "repos" in the plugin data directory, so a
	// global brain is visible next to the per-repo ones rather than buried
	// inside one of them.
	globalStoreDirName = "global"

	// globalFactBranch is the branch component of the global store's filename.
	// Global facts are not on a branch; this is the storage layer's requirement,
	// not a claim about the fact. Display never takes a fact's scope from this
	// value — see globalFactIDs — because a repository may legitimately have a
	// branch called "global" and a fact on it is not a global fact.
	globalFactBranch = "global"
)

// globalBrainDir returns the global store's directory, creating nothing. The
// caller creates it on first write, so merely reading never materialises a
// directory for somebody who has no global facts.
func globalBrainDir(env EntireEnv) (string, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dirs.Data, globalStoreDirName), nil
}

// loadGlobalFacts reads the global store. A store that does not exist yet is
// not an error: it is the state of every brain before the first global fact,
// and recall must not fail because of it.
func loadGlobalFacts(env EntireEnv) ([]factRecord, error) {
	brainDir, err := globalBrainDir(env)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(brainDir); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, nil
		}
		return nil, statErr
	}
	return loadFacts(brainDir, globalFactBranch)
}

// globalFactIDs is how the rest of the code knows which facts are global.
//
// Taking it from the records that were actually loaded out of the global store
// — rather than from a field on the fact — means the answer stays right no
// matter what a repository's branches are called, and no matter what a fact's
// stored branch says.
func globalFactIDs(facts []factRecord) map[string]bool {
	ids := make(map[string]bool, len(facts))
	for _, fact := range facts {
		ids[fact.ID] = true
	}
	return ids
}

// mergeGlobalFacts appends global facts to a repository's, dropping any whose
// id a repository fact already has.
//
// The repository's copy wins on a collision. Fact ids are content-derived, so a
// collision means the same statement is recorded in both places; the local one
// is the more specific claim and carries that repo's provenance, which is the
// evidence somebody would check.
func mergeGlobalFacts(repoFacts, global []factRecord) ([]factRecord, map[string]bool) {
	// Only a live repository fact shadows. The precedence above is justified by
	// the local copy being the more specific claim carrying checkable
	// provenance; a retracted or superseded fact makes no claim, so it has
	// nothing to shadow with. Letting it shadow anyway would mean retracting a
	// local duplicate silently removed the global statement from this one repo.
	seen := make(map[string]bool, len(repoFacts))
	for _, fact := range repoFacts {
		switch fact.Status {
		case "", factStatusActive:
			seen[fact.ID] = true
		}
	}
	merged := repoFacts
	globalIDs := map[string]bool{}
	for _, fact := range global {
		if seen[fact.ID] {
			continue
		}
		// A retracted or superseded global fact is filtered by the same code
		// that filters repository ones; nothing special happens here.
		merged = append(merged, fact)
		globalIDs[fact.ID] = true
	}
	return merged, globalIDs
}

// globalFactsEnabled reports whether global facts should be merged into a
// repository read.
//
// Default on: a preference somebody recorded once is meant to apply, and a
// memory layer that makes you ask for what you already told it is not one.
// ENTIRE_BRAIN_NO_GLOBAL_FACTS and --no-global turn it off, for a machine where
// repository answers must not be influenced by anything outside the repository.
func globalFactsEnabled(noGlobalFlag bool) bool {
	if noGlobalFlag {
		return false
	}
	return !securityToggleEnabled("ENTIRE_BRAIN_NO_GLOBAL_FACTS")
}

// globalFactsForRead loads the global store for a read surface, honouring the
// off switches. A damaged global store must not take retrieval down with it, so
// the error comes back for the caller to warn about rather than to propagate:
// the repository's own facts are still the answer to most queries.
func globalFactsForRead(env EntireEnv, noGlobal bool) ([]factRecord, error) {
	if !globalFactsEnabled(noGlobal) {
		return nil, nil
	}
	return loadGlobalFacts(env)
}

// resolveGlobalFactsTarget is the global counterpart of resolveFactsTarget, for
// commands operating on the global store directly.
func resolveGlobalFactsTarget(env EntireEnv) (brainDir, branch string, err error) {
	brainDir, err = globalBrainDir(env)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(brainDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create global brain: %w", err)
	}
	return brainDir, globalFactBranch, nil
}

// inARepository reports whether the current directory is actually inside a git
// working tree.
//
// It exists so `remember` outside a checkout can file the fact globally instead
// of refusing. The distinction matters: "you are not in a repository" is a
// reason to choose a different store, not a reason to throw away what somebody
// just typed.
//
// Asking git is the whole point, and the reason this does not simply check
// whether resolveFactsTarget succeeds: that succeeds for ANY directory that
// exists, then defaults the branch to "main" when git cannot name one. A fact
// recorded in ~/Desktop would be filed to a repository that does not exist, on
// a branch nobody is on, and never seen again — which is worse than the refusal
// this was meant to replace, because it looks like it worked.
// It returns an error for the case that is neither: a repository whose identity
// cannot be decided. resolveFactsTarget refuses to guess when state exists under
// two keys, and treating that refusal as "not in a repository" would swallow the
// guard and quietly file a repo-scoped fact globally — defeating the exact
// protection it was raised to provide. "No repository here" and "I cannot tell
// which repository this is" call for opposite responses.
func inARepository(ctx context.Context, opts Options) (bool, error) {
	repoDir, _, _, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), "")
	if err != nil {
		var conflict *localRepoIdentityConflictError
		if errors.As(err, &conflict) {
			return false, err
		}
		return false, nil
	}
	inside, err := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(inside) == "true", nil
}

// globalFactNotice is what the user sees when a fact was filed globally without
// them asking. Silence here would be the worst option: the fact is recorded
// somewhere they did not choose, and they would only find out by missing it.
func globalFactNotice(explicit bool) string {
	if explicit {
		return ""
	}
	return "not in a repository, so this was remembered globally; it will be recalled from every repo"
}

// describeFactScope labels a fact for display.
func describeFactScope(fact factRecord, globalIDs map[string]bool) string {
	if globalIDs[fact.ID] {
		return "global"
	}
	return strings.TrimSpace(fact.Branch)
}

// newFactsGlobalCommand lists what is in the global store.
//
// Global facts surface in every repository, which makes an unreviewable global
// store the worst kind: a wrong fact would follow somebody everywhere and there
// would be no one place to go and look at what they had accumulated. `recall`
// shows the ones matching a query; this shows all of them.
func newFactsGlobalCommand(opts Options) *cobra.Command {
	var (
		jsonOut    bool
		includeAll bool
	)
	cmd := &cobra.Command{
		Use:   "global",
		Short: "List facts recorded globally rather than for one repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			facts, err := loadGlobalFacts(opts.Env)
			if err != nil {
				return err
			}
			active := make([]factRecord, 0, len(facts))
			for _, fact := range facts {
				if !includeAll && fact.Status != "" && fact.Status != factStatusActive {
					continue
				}
				active = append(active, fact)
			}
			brainDir, dirErr := globalBrainDir(opts.Env)
			if dirErr != nil {
				return dirErr
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{
					"store": brainDir,
					"count": len(active),
					"facts": active,
				})
			}
			out := cmd.OutOrStdout()
			if len(active) == 0 {
				// The empty state is where somebody learns the feature exists,
				// so it says how to use it rather than only that there is
				// nothing here.
				fmt.Fprintln(out, "no global facts")
				fmt.Fprintln(out, "  `entire-brain remember \"<fact>\" --global` records one for every repository")
				return nil
			}
			for _, fact := range active {
				printFactLine(out, fact)
			}
			fmt.Fprintf(out, "\n%d global fact(s) in %s\n", len(active), brainDir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the facts as JSON")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include retracted and superseded facts")
	return cmd
}
