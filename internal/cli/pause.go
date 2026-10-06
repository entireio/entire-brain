package cli

import (
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

func newStopCommand(opts Options) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Pause background brain work on this machine",
		Long: "Pause background brain work on this machine until `start`.\n\n" +
			"From its next pass every watcher, installed or run by hand, idles, memory\n" +
			"workers exit without working, the session-end hook skips its refresh and\n" +
			"distill, and `setup` skips its backfill. A pass already running, such as a\n" +
			"`setup` backfill (capped at --backfill-budget sessions), finishes first.\n\n" +
			"Nothing is lost: captured sessions stay canonical and are picked up by the\n" +
			"watcher, the next hook, or a re-run `setup` once `start` lifts the pause.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := setBackgroundPaused(opts.Env, true); err != nil {
				return fmt.Errorf("pause background work: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "background brain work paused; passes already running finish first (`%s start` resumes)\n", setupCommandPrefix(os.LookupEnv))
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
