package cli

import (
	"fmt"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

func newFactsStorageCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{Use: "storage", Short: "Inspect, migrate, or roll back memory storage", Args: cobra.NoArgs}
	for _, action := range []string{"status", "migrate", "rollback"} {
		var global, jsonOut bool
		sub := &cobra.Command{
			Use: action, Args: cobra.NoArgs,
			Short: map[string]string{"status": "Show memory storage format", "migrate": "Consolidate legacy memories, retaining compatibility", "rollback": "Export current memories for older releases"}[action],
			RunE: func(cmd *cobra.Command, _ []string) error {
				var dir string
				var err error
				if global {
					dir, _, err = factstore.ResolveGlobalTarget(opts.Env)
				} else {
					_, dir, _, err = resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
				}
				if err != nil {
					return err
				}
				if action != "status" {
					err = brainstore.WithBrainWriteLock(dir, func() error {
						if action == "rollback" {
							return factstore.RollbackRepository(dir)
						}
						return factstore.MigrateRepository(dir)
					})
					if err != nil {
						return err
					}
				}
				report, err := factstore.RepositoryStatus(dir)
				if err != nil {
					return err
				}
				if jsonOut {
					return cmdutil.WriteJSON(cmd, report)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "memory storage: %s (legacy compatibility enabled)\n", report.Format)
				return nil
			},
		}
		sub.Flags().BoolVar(&global, "global", false, "Use the global memory store")
		sub.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable output")
		cmd.AddCommand(sub)
	}
	return cmd
}
