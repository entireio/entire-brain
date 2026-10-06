package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// backgroundPausedFile is machine-level, beside the watch plan, because the
// thing it pauses — the one watcher and every worker it and the hooks launch —
// is machine-level too.
const backgroundPausedFile = "paused"

func backgroundPausedPath(env EntireEnv) (string, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", fmt.Errorf("resolve plugin state dir: %w", err)
	}
	return filepath.Join(dirs.State, backgroundPausedFile), nil
}

// backgroundPaused fails open: a state dir that cannot be resolved or read
// means "not paused", because a brain that silently stops updating is worse
// than one that keeps going after a pause it could not see.
func backgroundPaused(env EntireEnv) bool {
	path, err := backgroundPausedPath(env)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func setBackgroundPaused(env EntireEnv, paused bool) error {
	path, err := backgroundPausedPath(env)
	if err != nil {
		return err
	}
	if !paused {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}

// stopRepoBackfill stops the setup backfill recorded for the current repository,
// returning its pid, or 0 when there is no repository or nothing running.
// Only this repository's: backfills are recorded per repo and `setup` starts
// one at a time, so that is where an in-flight one is.
func stopRepoBackfill(ctx context.Context, opts Options, stop func(pid int) error) (int, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil))
	if err != nil || !local {
		return 0, nil
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return 0, nil
	}
	state, ok := readSetupBackfillState(filepath.Dir(storage.HeadPath))
	if !ok || !backfillRunning(storage.BrainDir, state.PID) {
		return 0, nil
	}
	if err := stop(state.PID); err != nil {
		return 0, err
	}
	return state.PID, nil
}

func newStopCommand(opts Options) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Pause all background brain work on this machine",
		Long: "Pause all background brain work on this machine until `start`.\n\n" +
			"The installed watcher stays loaded but idles, memory workers launched by\n" +
			"agent hooks exit without working, and `setup` skips its backfill. Work\n" +
			"recorded while paused is not lost; it runs after `start`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := setBackgroundPaused(opts.Env, true); err != nil {
				return fmt.Errorf("pause background work: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "background brain work paused (`entire brain start` resumes)")
			pid, err := stopRepoBackfill(cmd.Context(), opts, stopDetachedProcess)
			if err != nil {
				return fmt.Errorf("stop this repository's backfill: %w", err)
			}
			if pid != 0 {
				fmt.Fprintf(out, "stopped this repository's fact backfill (pid %d)\n", pid)
			}
			return nil
		},
	}
}

func newStartCommand(opts Options) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Resume background brain work paused by `stop`",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := setBackgroundPaused(opts.Env, false); err != nil {
				return fmt.Errorf("resume background work: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "background brain work resumed")
			return nil
		},
	}
}
