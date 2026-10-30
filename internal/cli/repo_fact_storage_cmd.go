package cli

import (
	"fmt"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
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
					dir, _, err = resolveGlobalFactsTarget(opts.Env)
				} else {
					_, dir, _, err = resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
				}
				if err != nil {
					return err
				}
				if action != "status" {
					err = brainstore.WithBrainWriteLock(dir, func() error {
						if action == "rollback" {
							return exportLegacyRepoFacts(dir)
						}
						return migrateRepoFacts(dir)
					})
					if err != nil {
						return err
					}
				}
				s, native, err := readRepoFactStore(dir)
				if err != nil {
					return err
				}
				// Include changes made by an older binary since the last native
				// commit. Status is still read-only.
				s, err = reconcileRepoFactStore(dir)
				if err != nil {
					return err
				}
				if err := s.compact(); err != nil {
					return err
				}
				format := "legacy"
				if native {
					format = "repository"
				}
				report := struct {
					SchemaVersion int    `json:"schema_version"`
					Format        string `json:"format"`
					Compatibility bool   `json:"legacy_compatibility"`
					Records       int    `json:"records"`
					Views         int    `json:"views"`
				}{1, format, true, len(s.Records), len(s.Views)}
				if jsonOut {
					return writeJSON(cmd, report)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "memory storage: %s (legacy compatibility enabled)\n", format)
				return nil
			},
		}
		sub.Flags().BoolVar(&global, "global", false, "Use the global memory store")
		sub.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable output")
		cmd.AddCommand(sub)
	}
	return cmd
}
