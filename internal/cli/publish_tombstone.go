package cli

import (
	"fmt"
	"os"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// newPublishTombstoneCommand reports that `publish` was removed: the hosted
// brain-bundle endpoint it posted to (/brain/artifacts) was never deployed,
// so the command failed for every host, and memories travel the per-fact
// wire instead (`memories sync` on a connected repo). Hidden so help never
// advertises a dead command, but still dispatchable so the old invocation
// fails loud with the replacement instead of cobra's generic
// "unknown command".
//
// DisableFlagParsing is set because the removed command's invocation shapes
// carried flags (--repo-id, --api-url, --token); they must reach this error
// rather than die earlier as cobra's generic "unknown flag".
func newPublishTombstoneCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "publish",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("publish was removed: the hosted brain-bundle endpoint it posted to was never deployed; to push memories to Entire, run '%s' on a connected repo ('%s')",
				base.BrainInvocation(os.LookupEnv, "memories sync"), base.BrainInvocation(os.LookupEnv, "connect"))
		},
	}
}
