package cli

import (
	"fmt"
	"os"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// newInitAgentsTombstoneCommand reports that `init-agents` was removed:
// its behavior moved into `setup`, which took over `--strict`/`--normal`.
// Hidden so no-argument help never advertises a dead command, but still
// dispatchable so the old invocation fails loud instead of hitting cobra's
// generic "unknown command".
//
// DisableFlagParsing is set because the removed command's own flags
// (--strict, --normal, --repo, --json) must reach this error rather than die
// earlier as cobra's generic "unknown flag" -- those were its main
// invocation shapes, and RunE errors unconditionally regardless of args.
func newInitAgentsTombstoneCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "init-agents",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("init-agents was removed; its behavior moved into `%s` (--strict/--normal now live there)", base.BrainInvocation(os.LookupEnv, "setup"))
		},
	}
	return cmd
}
