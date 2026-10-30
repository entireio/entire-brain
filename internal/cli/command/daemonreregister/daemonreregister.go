package daemonreregister

import (
	"context"
	"io"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// Config carries the options and the cli-bound re-registration operation. The
// operation stays in cli because its closure — the setup unit-plan builder, the
// registration lock, and the daemon installer — is the machinery `setup` and
// the supervised watch loop share.
type Config struct {
	Options base.Options
	Deps    Deps
}

type Deps struct {
	// Reregister reinstalls the machine's one watcher under the same lock
	// `setup` uses, after rechecking that the unit still exists.
	Reregister func(ctx context.Context, out io.Writer, opts base.Options) error
}

// New is the internal command the exiting daemon spawns (detached, on the
// replaced binary) to re-register the installed watcher.
func New(cfg Config) *cobra.Command {
	return &cobra.Command{
		Use:   "daemon-reregister",
		Short: "Re-register the installed background watcher with the service manager (internal)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cfg.Deps.Reregister(cmd.Context(), cmd.OutOrStdout(), cfg.Options)
		},
	}
}
