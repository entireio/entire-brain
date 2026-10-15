package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newDisconnectCommand(opts Options) *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect",
		Short: "Stop syncing this repository with its hosted brain",
		Long: fmt.Sprintf(`disconnect opts this repository out of hosted-brain syncing: memories stop
publishing and pulling, and the daemon's auto-connect will not re-bind it.

Your local brain keeps working exactly as before, and memories already pulled
from teammates stay in the local store — disconnect is not a purge.

Run '%s' to opt back in.`, brainInvocation(os.LookupEnv, "connect")),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDisconnect(cmd, opts)
		},
	}
}

func runDisconnect(cmd *cobra.Command, opts Options) error {
	ctx := cmd.Context()
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil))
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("disconnect requires a local repository path")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	// Works unconnected too: the opt-out is exactly the "never auto-bind me"
	// record.
	binding, err := setHostedOptOut(storage.BrainDir)
	if err != nil {
		return fmt.Errorf("disconnect: %w", err)
	}

	if binding.RepoID != "" {
		renderHostedBanner(cmd.OutOrStdout(),
			"Disconnected: repo "+binding.RepoID+" no longer syncs memories",
			"Your local brain keeps working exactly as before. Memories already pulled from teammates stay in the local store.",
			"Run `"+brainInvocation(os.LookupEnv, "connect")+"` to resume syncing.")
		return nil
	}
	renderHostedBanner(cmd.OutOrStdout(),
		"Disconnected: this repository will not auto-connect to a hosted brain",
		"Your local brain keeps working exactly as before.",
		"Run `"+brainInvocation(os.LookupEnv, "connect")+"` to join.")
	return nil
}
