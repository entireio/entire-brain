package cli

import (
	"encoding/json"
	"fmt"
	"github.com/entireio/entire-brain/internal/agentsetup"
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
	var strict, normal bool
	cmd := &cobra.Command{Use: "agent-guide", Aliases: []string{"guide"}, Short: "Preview repository-specific agent instructions", Args: cobra.NoArgs}
	cmd.Long = "Preview repository-specific agent instructions without writing files. Inherits the saved guidance mode; --strict and --normal override this preview only. Save a mode with init-agents. Outside a repository, prints standalone Brain guidance (normal unless --strict is supplied)."
	if install {
		cmd.Use = "init-agents [path]"
		cmd.Aliases = nil
		cmd.Short = "Install coordinated agent instructions"
		cmd.Long = "Install coordinated agent instructions, preserving enabled products and the saved guidance mode. --strict saves mandatory tool-use rules for all enabled products; --normal resets to normal guidance. New repositories default to normal."
		cmd.Args = cobra.MaximumNArgs(1)
		cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit changed file paths as JSON")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		mode, err := agentsetup.SelectMode(strict, normal)
		if err != nil {
			return err
		}
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
		render := func() (string, error) { return agentsetup.Preview(root, "brain", agentsetup.Options{Mode: mode}) }
		if install {
			if root == "" {
				return fmt.Errorf("outside a repository; supply --repo")
			}
			// Name both roots when this one sits inside another repository.
			// Writing the guide into a nested clone is silent and looks like
			// success, and the files land where the outer repository cannot
			// see them. See agentsetup.OuterRepo.
			if outer := agentsetup.OuterRepo(root); outer != "" && !cmd.Flags().Changed("repo") && len(args) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: %s is a git repository inside %s.\nThe guide, AGENTS.md and CLAUDE.md go to the inner one. If you meant the project, run:\n    entire brain init-agents --repo %s\n\n",
					root, outer, outer)
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
	cmd.Flags().BoolVar(&strict, "strict", false, "Use strict guidance (saved by init-agents; preview only for agent-guide)")
	cmd.Flags().BoolVar(&normal, "normal", false, "Use normal guidance (saved by init-agents; preview only for agent-guide)")
	return cmd
}
