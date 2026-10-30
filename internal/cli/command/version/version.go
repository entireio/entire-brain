package version

import (
	"fmt"

	"github.com/spf13/cobra"
)

func New(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print plugin version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version)
			return nil
		},
	}
}
