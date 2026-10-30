package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

// notifyHostedFactsSync is a test seam over hostedFactsSyncAndPull.
var notifyHostedFactsSync = hostedFactsSyncAndPull

// memoryPushWorkLaunch spawns `memory push-work` detached, the same shape as
// memoryWorkerLaunch: `memory notify` is a fast content-free hint and must
// never carry the upload's network round trips inline — a host adapter may
// invoke it synchronously. The child runs with the worker-origin environment
// so nothing it shells out to re-notifies. Injectable for tests; a launch
// failure never fails the host.
var memoryPushWorkLaunch = func(repoDir, branch string) error {
	args := []string{"memory", "push-work"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	return launchDetachedSelf(repoDir, args...)
}

// newMemoryPushWorkCommand is the detached half of the push hint: notify
// dispatches this verb, and this verb uploads pending fact versions.
// Hidden — it is an internal dispatch target, not a user surface.
func newMemoryPushWorkCommand(opts Options) *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:    "push-work",
		Hidden: true,
		Short:  "Sync pending memories for a commit or push event (internal)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// One repo resolution: resolveSessionsBrain would run the same
			// resolveLocalTargetRepoDir again internally, a redundant git
			// spawn on every push.
			repoDir, local, err := base.ResolveLocalTargetRepoDir(cmd.Context(), opts.Runner, agentSurfaceTarget(opts, nil))
			if err != nil || !local {
				return nil // no local repo: silence, never an error
			}
			storage, err := repoStoragePaths(cmd.Context(), opts.Runner, opts.Env, repoDir)
			if err != nil {
				return err
			}
			syncHostedMemoriesOnEvent(cmd.Context(), cmd.ErrOrStderr(), opts, repoDir, storage, strings.TrimSpace(branch))
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Captured branch when the host knows it")
	return cmd
}

// syncHostedMemoriesOnEvent flushes available memories on an explicit event. There
// is no timer throttle: pushHostedFacts serializes uploads and deduplicates
// acknowledged record versions. Failures remain pending until another event.
func syncHostedMemoriesOnEvent(ctx context.Context, errW io.Writer, opts Options, repoDir string, storage repoStorage, branch string) bool {
	if base.BrainNoEgressMode() {
		return false
	}
	if binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir); err != nil || (present && (binding.OptedOut || binding.NoAccessUntil.After(opts.Now().UTC()))) {
		return false
	}
	if branch == "" {
		branch = hostedSyncBranch(ctx, opts.Runner, repoDir)
	}
	branches := []string{branch}
	seen := map[string]bool{branch: true}
	if !seen[distillDefaultBranch] {
		branches = append(branches, distillDefaultBranch)
		seen[distillDefaultBranch] = true
	}
	// Distillation can finish for historical branches. A later event must also
	// retry their pending records, even after the user changes branches.
	all, err := factstore.LoadAllBranches(storage.BrainDir)
	if err != nil {
		fmt.Fprintf(errW, "memory sync: inventory failed: %v\n", err)
		return false
	}
	extra := []string{}
	for b := range all {
		if b != "" && !seen[b] {
			extra = append(extra, b)
		}
	}
	sort.Strings(extra)
	branches = append(branches, extra...)
	for _, b := range branches {
		if _, err := notifyHostedFactsSync(ctx, errW, opts, repoDir, storage, b); err != nil {
			fmt.Fprintf(errW, "memory sync failed (pending until the next sync): %v\n", err)
		}
	}
	return true
}
