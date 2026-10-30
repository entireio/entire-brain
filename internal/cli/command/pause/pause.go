package pause

import (
	"fmt"
	"os"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

func NewStop(opts base.Options) *cobra.Command {
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
			if err := base.SetBackgroundPaused(opts.Env, true); err != nil {
				return fmt.Errorf("pause background work: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "background brain work paused; passes already running finish first (`%s start` resumes)\n", base.SetupCommandPrefix(os.LookupEnv))
			return nil
		},
	}
}

func NewStart(opts base.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Resume background brain work paused by `stop`",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := base.SetBackgroundPaused(opts.Env, false); err != nil {
				return fmt.Errorf("resume background work: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "background brain work resumed")
			return nil
		},
	}
}
