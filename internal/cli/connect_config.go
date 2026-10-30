package cli

import (
	"context"
	"io"

	"github.com/entireio/entire-brain/internal/cli/command/connect"
	"github.com/entireio/entire-brain/internal/cli/command/disconnect"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// connectConfig binds the repo identity resolution, push-hook management, and
// the hosted sync engine at the assembly boundary. Command packages never
// import cli, and no mutable hook is copied.
func connectConfig(opts Options) connect.Config {
	return connect.Config{Options: opts, Deps: connect.Deps{
		RepoStoragePaths:   repoStoragePaths,
		AgentSurfaceTarget: agentSurfaceTarget,
		InstallPushHook: func(ctx context.Context, opts base.Options, repoDir string) (bool, string, error) {
			hookState, hookPath, err := installPushHook(ctx, opts, repoDir)
			return hookState == pushHookSharedDir, hookPath, err
		},
		HostedFactsSyncAndPull: func(ctx context.Context, errW io.Writer, opts base.Options, repoDir string, storage base.RepoStorage, branch string) (bool, bool, error) {
			outcome, err := hostedFactsSyncAndPull(ctx, errW, opts, repoDir, storage, branch)
			return outcome.Attempted, outcome.PullSucceeded, err
		},
		HostedSyncBranch: hostedSyncBranch,
	}}
}

// disconnectConfig binds the same boundary for the opt-out: disconnect removes
// exactly the hook connect installs.
func disconnectConfig(opts Options) disconnect.Config {
	return disconnect.Config{Options: opts, Deps: disconnect.Deps{
		RepoStoragePaths:   repoStoragePaths,
		AgentSurfaceTarget: agentSurfaceTarget,
		UninstallPushHook:  uninstallPushHook,
	}}
}
