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
	var noHooks bool
	cmd := &cobra.Command{Use: "agent-guide", Aliases: []string{"guide"}, Short: "Preview repository-specific agent instructions", Args: cobra.NoArgs}
	cmd.Long = "Preview repository-specific agent instructions without writing files. Inherits the saved guidance mode; --strict and --normal override this preview only. Save a mode with init-agents. Outside a repository, prints standalone Brain guidance (normal unless --strict is supplied)."
	if install {
		cmd.Use = "init-agents [path]"
		cmd.Aliases = nil
		cmd.Short = "Install coordinated agent instructions"
		cmd.Long = "Install coordinated agent instructions, preserving enabled products and the saved guidance mode. --strict saves mandatory tool-use rules for all enabled products; --normal resets to normal guidance. New repositories default to normal.\n\nIt also merges a Claude Code PreToolUse entry into .claude/settings.json so `hook pre-edit` delivers file-anchored facts at the moment an agent edits — the verb was unwired, and the docs admitted it had no effect until a harness called it. Pass --no-agent-hooks to skip that, or --no-agent-hooks=false to re-add it."
		cmd.Args = cobra.MaximumNArgs(1)
		cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit changed file paths as JSON")
		cmd.Flags().BoolVar(&noHooks, "no-agent-hooks", false, "Write the instructions but do not merge the Claude Code PreToolUse entry")
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
			// The PreToolUse merge is deliberately NOT inside agentsetup: that
			// package is mirrored byte-for-byte into entire-graph and its
			// documented write scope is exactly three instruction files. The
			// hook is this product's, so it is wired from this product's
			// command. Unlike setup, init-agents REPORTS a hook failure as an
			// error — the user asked for the wiring by name.
			wireHook := !noHooks
			if jsonOut {
				changed, err := agentsetup.InstallChanged(root, render)
				if err != nil {
					return err
				}
				if wireHook {
					_, hookChanged, err := installPreEditHook(root)
					if err != nil {
						return err
					}
					if hookChanged {
						changed = append(changed, claudeSettingsDir+"/"+claudeSettingsFile)
					}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"changed_files": changed})
			}
			if err := agentsetup.Install(root, render, cmd.OutOrStdout()); err != nil {
				return err
			}
			if !wireHook {
				return nil
			}
			path, hookChanged, err := installPreEditHook(root)
			if err != nil {
				return err
			}
			if hookChanged {
				fmt.Fprintf(cmd.OutOrStdout(), "updated %s\n", path)
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
