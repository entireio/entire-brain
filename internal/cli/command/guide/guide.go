package guide

import (
	"fmt"

	"github.com/entireio/entire-brain/internal/agentsetup"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// New is the read-only preview of the agent instructions
// `setup` installs. Installation lives in setup's agents phase; saving a
// guidance mode is `setup --strict` / `setup --normal`.
func New(opts base.Options) *cobra.Command {
	var repo string
	var strict, normal bool
	cmd := &cobra.Command{Use: "agent-guide", Aliases: []string{"guide"}, Short: "Preview repository-specific agent instructions", Args: cobra.NoArgs}
	cmd.Long = "Preview repository-specific agent instructions without writing files. Inherits the saved guidance mode; --strict and --normal override this preview only. Save a mode with `setup --strict` or `setup --normal`. Outside a repository, prints standalone Brain guidance (normal unless --strict is supplied)."
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		mode, err := agentsetup.SelectMode(strict, normal)
		if err != nil {
			return err
		}
		root, err := agentsetup.Context(repo, opts.Env.RepoRoot)
		if err != nil {
			return err
		}
		guide, err := agentsetup.Preview(root, "brain", agentsetup.Options{Mode: mode})
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(cmd.OutOrStdout(), guide)
		return err
	}
	cmd.Flags().StringVar(&repo, "repo", "", "Project root (default: host repository or nearest repository)")
	cmd.Flags().BoolVar(&strict, "strict", false, "Use strict guidance (preview only; save with `setup --strict`)")
	cmd.Flags().BoolVar(&normal, "normal", false, "Use normal guidance (preview only; save with `setup --normal`)")
	return cmd
}
