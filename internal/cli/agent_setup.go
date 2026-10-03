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
			// The pre-edit hook is the only PUSH path brain has; everything
			// else waits to be asked. A merge failure must not fail the whole
			// install -- the guide is the primary artifact and lands either
			// way -- but it must be SAID, because a hook silently not wired is
			// how this one sat unused since PR #24.
			hookChanged, hookErr := agentsetup.MergePreEditHook(root, "entire brain")
			if jsonOut {
				changed, err := agentsetup.InstallChanged(root, render)
				if err != nil {
					return err
				}
				payload := map[string]any{"changed_files": changed, "pre_edit_hook_installed": hookChanged}
				if hookErr != nil {
					payload["pre_edit_hook_error"] = hookErr.Error()
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(payload)
			}
			if err := agentsetup.Install(root, render, cmd.OutOrStdout()); err != nil {
				return err
			}
			switch {
			case hookErr != nil:
				fmt.Fprintf(cmd.OutOrStdout(), "pre-edit hook not wired: %v\n", hookErr)
			case hookChanged:
				fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (pre-edit hook)\n", ".claude/settings.json")
			}
			return nil
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
