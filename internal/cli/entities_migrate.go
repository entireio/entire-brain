package cli

import (
	"github.com/entireio/entire-brain/internal/entityindex"
	"github.com/spf13/cobra"
)

func newEntitiesMigrateCommand(opts Options) *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "migrate [path]",
		Short: "Recompute stored entity history after a Graph parser identity change",
		Long:  "Carry already-indexed commits across all branches into the current Graph parser namespace, skipping commits already present there. Computes diffs without the write lock, then atomically publishes only if git-meta has not changed. Preserves older parser records, source commits, checkpoint/session provenance, and authored memory. Requires source Git commits locally; retry if another writer changes git-meta.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _, store, err := openEntityIndexStore(cmd.Context(), opts, entitiesTarget(opts, args))
			if err != nil {
				return err
			}
			revision, err := entityindex.ProviderIdentity(cmd.Context(), opts.Runner, repo, binary)
			if err != nil {
				return err
			}
			result, err := entityindex.Migrate(cmd.Context(), opts.Runner, store, entityindex.BuildOptions{RepoDir: repo, GraphBinary: binary, IdentityRevision: revision, Now: opts.Now})
			if err != nil {
				return err
			}
			return writeIndentedJSON(cmd.OutOrStdout(), result)
		},
	}
	cmd.Flags().StringVar(&binary, "graph-binary", "entire", "Entire CLI binary that exposes graph commands")
	return cmd
}
