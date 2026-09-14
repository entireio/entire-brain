package cli

import (
	"encoding/json"
	"fmt"
	"github.com/ashtom/entire-brain/internal/agentsetup"
	"github.com/spf13/cobra"
)

func newBrainGuideCommand(opts Options) *cobra.Command {
	return newAgentInstructionsCommand(opts, false)
}
func newInitAgentsCommand(opts Options) *cobra.Command {
	return newAgentInstructionsCommand(opts, true)
}
func newAgentInstructionsCommand(opts Options, install bool) *cobra.Command {
	var repo string
	var jsonOut bool
	cmd := &cobra.Command{Use: "agent-guide", Aliases: []string{"guide"}, Short: "Preview repository-specific agent instructions", Args: cobra.NoArgs}
	if install {
		cmd.Use = "init-agents [path]"
		cmd.Aliases = nil
		cmd.Short = "Install coordinated agent instructions"
		cmd.Args = cobra.MaximumNArgs(1)
		cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit changed file paths as JSON")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		target := repo
		if len(args) == 1 {
			if cmd.Flags().Changed("repo") {
				return fmt.Errorf("supply either a path or --repo")
			}
			target = args[0]
		}
		root, err := agentsetup.Context(target, opts.Env.RepoRoot)
		if err != nil {
			return err
		}
		render := func() (string, error) { return agentsetup.Preview(root, "brain", agentsetup.Options{}) }
		if install {
			if root == "" {
				return fmt.Errorf("outside a repository; supply --repo")
			}
			if jsonOut {
				changed, err := agentsetup.InstallChanged(root, render)
				if err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"changed_files": changed})
			}
			return agentsetup.Install(root, render, cmd.OutOrStdout())
		}
		guide, err := render()
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), guide)
		return err
	}
	cmd.Flags().StringVar(&repo, "repo", "", "Project root (default: host repository or nearest repository)")
	return cmd
}
