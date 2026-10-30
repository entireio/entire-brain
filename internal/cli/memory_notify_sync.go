package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/spf13/cobra"
)

// memoryNotifyHostedSyncMinInterval throttles the push-hint hosted fact sync
// across detached notify processes: a push burst (a rebase + force-push loop)
// must not become one sync per push. A minute keeps the trail fresh on the
// push beat; the daemon tick remains the reconciliation floor.
const memoryNotifyHostedSyncMinInterval = time.Minute

// memoryNotifySyncMarkerName is the brain-relative marker recording the last
// push-hint sync. Shared by every notify process; read and claimed under the
// brain write lock so concurrent hints race safely.
const memoryNotifySyncMarkerName = "hosted_sync_notify.json"

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
// dispatches this verb, and this verb uploads the fact store, throttled.
// Hidden — it is an internal dispatch target, not a user surface.
func newMemoryPushWorkCommand(opts Options) *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:    "push-work",
		Hidden: true,
		Short:  "Upload the memory store for a push hint (internal)",
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
			hostedSyncOnPushHint(cmd.Context(), cmd.ErrOrStderr(), opts, repoDir, storage, strings.TrimSpace(branch))
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Captured branch when the host knows it")
	return cmd
}

type memoryNotifySyncMarker struct {
	LastSyncAt time.Time `json:"last_sync_at"`
}

// hostedSyncOnPushHint uploads the fact store on a push hint: the hinted
// branch, then the default branch, throttled by a shared marker. It
// deliberately does NOT distill — the hint is an upload beat for facts that
// already exist. Fact creation stays on the session-end hook and the daemon
// tick until it moves into the memory worker with the session-end deprecation.
// The marker is claimed BEFORE the work so concurrent notify processes skip
// instead of stampeding; a failed run after the claim simply waits out the
// interval, which the never-fails contract already tolerates.
//
// Same contract as hostedFactsSyncAndPull: this runs behind a host lifecycle
// hint, so nothing here may fail the caller — every outcome is reported on
// errW and swallowed. The return value says whether the work ran, for tests
// and for the caller's log line only.
func hostedSyncOnPushHint(ctx context.Context, errW io.Writer, opts Options, repoDir string, storage repoStorage, branch string) bool {
	// An explicit disconnect — or a recorded no-access pause (COR-2279) —
	// must cost nothing per push: bail before claiming the throttle or
	// spawning sync work. An UNBOUND repo proceeds — the
	// sync's auto-connect is the designed first-contact path, with its own
	// gates.
	if binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir); err == nil && present &&
		(binding.OptedOut || binding.NoAccessUntil.After(opts.Now().UTC())) {
		return false
	}
	claimed := false
	if err := brainstore.WithBrainWriteLock(storage.BrainDir, func() error {
		now := opts.Now().UTC()
		marker := readMemoryNotifySyncMarker(storage.BrainDir)
		if now.Sub(marker.LastSyncAt) < memoryNotifyHostedSyncMinInterval {
			return nil
		}
		if err := writeMemoryNotifySyncMarker(storage.BrainDir, memoryNotifySyncMarker{LastSyncAt: now}); err != nil {
			return err
		}
		claimed = true
		return nil
	}); err != nil {
		fmt.Fprintf(errW, "memory notify: hosted sync throttle: %v\n", err)
		return false
	}
	if !claimed {
		return false
	}

	if branch == "" {
		branch = hostedSyncBranch(ctx, opts.Runner, repoDir)
	}
	syncBranches := []string{branch}
	if branch != distillDefaultBranch {
		syncBranches = append(syncBranches, distillDefaultBranch)
	}
	for _, syncBranch := range syncBranches {
		if _, err := notifyHostedFactsSync(ctx, errW, opts, repoDir, storage, syncBranch); err != nil {
			fmt.Fprintf(errW, "memory notify: hosted memory sync failed (local brain unaffected): %v\n", err)
		}
	}
	return true
}

func memoryNotifySyncMarkerPath(brainDir string) string {
	return filepath.Join(brainDir, memoryNotifySyncMarkerName)
}

// readMemoryNotifySyncMarker treats a missing or unreadable marker as "never
// synced": the worst outcome of a corrupt marker is one extra sync.
func readMemoryNotifySyncMarker(brainDir string) memoryNotifySyncMarker {
	var marker memoryNotifySyncMarker
	data, err := os.ReadFile(memoryNotifySyncMarkerPath(brainDir))
	if err != nil {
		return memoryNotifySyncMarker{}
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return memoryNotifySyncMarker{}
	}
	return marker
}

func writeMemoryNotifySyncMarker(brainDir string, marker memoryNotifySyncMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return os.WriteFile(memoryNotifySyncMarkerPath(brainDir), data, 0o600)
}
