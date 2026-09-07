package cli

import (
	"github.com/ashtom/entire-brain/internal/entityindex"
	"github.com/spf13/cobra"
)

func newEntitiesMigrateCommand(opts Options) *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "migrate [path]",
		Short: "Recompute stored entity history after a Graph parser identity change",
		Long:  "Recompute every already-indexed commit across all branches with the current Graph provider. Publishes one atomic git-meta update only after all diffs succeed. Preserves indexed windows, source commits, checkpoint/session provenance, and authored memory. Requires all indexed Git commits to be available locally.",
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
