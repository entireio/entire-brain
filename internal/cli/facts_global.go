package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"strings"

	factscmd "github.com/entireio/entire-brain/internal/cli/command/facts"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
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
	inside, err := base.GitScalar(ctx, opts.Runner, repoDir, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(inside) == "true", nil
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
		Short: "List memories recorded globally rather than for one repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			facts, err := factstore.LoadGlobal(opts.Env)
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
			brainDir, dirErr := factstore.GlobalBrainDir(opts.Env)
			if dirErr != nil {
				return dirErr
			}
			if jsonOut {
				return cmdutil.WriteJSON(cmd, map[string]any{
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
				fmt.Fprintln(out, "no global memories")
				fmt.Fprintln(out, "  `entire-brain remember \"<memory>\" --global` records one for every repository")
				return nil
			}
			for _, fact := range active {
				factscmd.PrintFactLine(out, fact)
			}
			fmt.Fprintf(out, "\n%d global memory(ies) in %s\n", len(active), brainDir)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the memories as JSON")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include retracted and superseded memories")
	return cmd
}
