package disconnect

import (
	"context"
	"fmt"
	"os"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/spf13/cobra"
)

// Config supplies the shared command options and the assembly package's
// storage and push-hook operations. Dependencies are per invocation, never
// global hooks.
type Config struct {
	base.Options
	Deps Deps
}

// Deps keeps the disconnect command independent of command assembly, repo
// identity resolution, and push-hook management. The assembly package supplies
// these operations so disconnect removes the same hook connect installs.
type Deps struct {
	RepoStoragePaths   func(context.Context, base.CommandRunner, base.EntireEnv, string) (base.RepoStorage, error)
	AgentSurfaceTarget func(base.Options, []string) string
	UninstallPushHook  func(ctx context.Context, opts base.Options, repoDir string) error
}

func New(cfg Config) *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect",
		Short: "Stop syncing this repository with its hosted brain",
		Long: `disconnect opts this repository out of hosted-brain syncing: memories stop
publishing and pulling. Nothing reconnects it except an explicit
'entire brain connect'.

Your local brain keeps working exactly as before, and memories already pulled
from teammates stay in the local store — disconnect is not a purge.

Run 'entire brain connect' to opt back in.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDisconnect(cmd, cfg)
		},
	}
}

func runDisconnect(cmd *cobra.Command, cfg Config) error {
	ctx := cmd.Context()
	opts := cfg.Options
	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, cfg.Deps.AgentSurfaceTarget(opts, nil))
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("disconnect requires a local repository path")
	}
	storage, err := cfg.Deps.RepoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	// Works unconnected too: the opt-out is exactly the "never auto-bind me"
	// record.
	binding, err := brainstore.SetHostedOptOut(storage.BrainDir)
	if err != nil {
		return fmt.Errorf("disconnect: %w", err)
	}
	// Remove our pre-push hook (restoring any hook we had chained to). The
	// opt-out already makes the hook a no-op, so a removal failure costs an
	// empty spawn per push, never a sync — report and continue.
	if hookErr := cfg.Deps.UninstallPushHook(ctx, opts, repoDir); hookErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "disconnect: push hook not removed: %v\n", hookErr)
	}

	if binding.RepoID != "" {
		cmdutil.RenderHostedBanner(cmd.OutOrStdout(),
			"Disconnected: "+storage.Key+" no longer syncs memories",
			"Your local brain keeps working exactly as before. Memories already pulled from teammates stay in the local store.",
			"Run `"+base.BrainInvocation(os.LookupEnv, "connect")+"` to resume syncing.")
		return nil
	}
	cmdutil.RenderHostedBanner(cmd.OutOrStdout(),
		"Disconnected: this repository does not sync with a hosted brain",
		"Your local brain keeps working exactly as before.",
		"Run `"+base.BrainInvocation(os.LookupEnv, "connect")+"` to join.")
	return nil
}
