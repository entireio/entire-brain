package cli

import (
	"fmt"
	"io"
	"strings"

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
				renderStorageStatus(cmd.OutOrStdout(), report)
				return nil
			},
		}
		sub.Flags().BoolVar(&global, "global", false, "Use the global memory store")
		sub.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable output")
		cmd.AddCommand(sub)
	}
	return cmd
}

// renderStorageStatus is the human form. It is the destination the one-line
// retained-cleanup warning points at, so the retained entries print here in
// full — one per line, the aggregate split at its "; " joins — instead of as
// the single wall the warning used to dump on every write.
func renderStorageStatus(out io.Writer, report factstore.StorageStatus) {
	fmt.Fprintf(out, "memory storage: %s\n", report.Format)
	fmt.Fprintf(out, "records: %d\n", report.Records)
	cleanup := report.Cleanup
	if cleanup == nil || (cleanup.State == "" || cleanup.State == "complete") && cleanup.ReclaimedBytes == 0 {
		return
	}
	fmt.Fprintf(out, "legacy cleanup: %s", cleanup.State)
	if cleanup.PendingDirs > 0 {
		fmt.Fprintf(out, " · %d legacy dir(s) pending", cleanup.PendingDirs)
	}
	if cleanup.ReclaimedBytes > 0 {
		fmt.Fprintf(out, " · %d byte(s) reclaimed", cleanup.ReclaimedBytes)
	}
	fmt.Fprintln(out)
	if cleanup.State == "retained" {
		fmt.Fprintln(out, "recovery data from the pre-flat store is kept until a human decides; nothing was deleted:")
		for _, entry := range cleanup.Retained {
			// The verifier aggregates one parked dir's items into one string,
			// "<.legacy-dir>: <item>; <item>" with errors.Join newlines mixed
			// in; peel the dir onto its own line and give every item a row.
			if dir, rest, ok := strings.Cut(entry, ": "); ok && strings.HasPrefix(dir, ".legacy-") {
				fmt.Fprintf(out, "  %s:\n", dir)
				entry = rest
			}
			normalized := strings.ReplaceAll(entry, "; ", "\n")
			for _, part := range strings.Split(normalized, "\n") {
				if part = strings.TrimSpace(part); part != "" {
					fmt.Fprintf(out, "  %s\n", part)
				}
			}
		}
	}
}
