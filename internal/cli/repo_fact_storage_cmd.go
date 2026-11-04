package cli

import (
	"fmt"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

// The rollback verb left with the per-branch stores: migration is a one-way
// door (released binaries refuse the newer schema rather than reading a
// projection), and the parked facts/.legacy-* dirs are the recovery artifact.
func newFactsStorageCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{Use: "storage", Short: "Inspect or migrate memory storage", Args: cobra.NoArgs}
	for _, action := range []string{"status", "migrate"} {
		var global, jsonOut bool
		sub := &cobra.Command{
			Use: action, Args: cobra.NoArgs,
			Short: map[string]string{"status": "Show memory storage format", "migrate": "Fold legacy branch stores into the repository store"}[action],
			RunE: func(cmd *cobra.Command, _ []string) error {
				var dir string
				var err error
				if global {
					dir, err = factstore.ResolveGlobalTarget(opts.Env)
				} else {
					_, dir, _, err = resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
				}
				if err != nil {
					return err
				}
				var summary factstore.MigrationSummary
				if action == "migrate" {
					err = brainstore.WithBrainWriteLockContext(cmd.Context(), dir, func() error {
						var migrateErr error
						summary, migrateErr = factstore.MigrateRepository(dir)
						if migrateErr != nil || global || !summary.Changed() {
							return migrateErr
						}
						// The fold collapses duplicate ids across legacy files, so the
						// declared count must be rebuilt or every later read warns of
						// loss that never happened. Repo brains only: the global store
						// has no manifest. This manifest write is also what closes the
						// one-way door (it stamps the current schema).
						return updateFactSourceManifestLocked(dir, opts.Now())
					})
					if err != nil {
						return err
					}
					if summary.StragglerFiles > 0 {
						fmt.Fprintln(cmd.ErrOrStderr(), "warning: an older entire-brain wrote memories after migration; they were upgraded — update that machine")
					}
				}
				report, err := factstore.RepositoryStatus(dir)
				if err != nil {
					return err
				}
				if jsonOut {
					if action == "migrate" {
						return cmdutil.WriteJSON(cmd, map[string]any{
							"storage":         report,
							"folded":          summary.Folded,
							"straggler_files": summary.StragglerFiles,
							"moved_dirs":      summary.MovedDirs,
							"cleanup":         summary.Cleanup,
						})
					}
					return cmdutil.WriteJSON(cmd, report)
				}
				if action == "migrate" && summary.Changed() {
					fmt.Fprintf(cmd.OutOrStdout(), "migrated %d record(s) from %d legacy dir(s)\n", summary.Folded, len(summary.MovedDirs))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "memory storage: %s\n", report.Format)
				return nil
			},
		}
		sub.Flags().BoolVar(&global, "global", false, "Use the global memory store")
		sub.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable output")
		cmd.AddCommand(sub)
	}
	return cmd
}
