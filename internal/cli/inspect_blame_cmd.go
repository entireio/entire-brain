package cli

import (
	"fmt"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

func newInspectBlameCommand(opts Options) *cobra.Command {
	var (
		branch  string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "blame <memory-id>",
		Short: "Show the source anchors a memory was derived from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			factID := args[0]
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			facts, err := factstore.Load(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			i := indexOfFact(facts, factID)
			if i < 0 {
				return fmt.Errorf("no fact %s on %s", factID, resolvedBranch)
			}
			fact := facts[i]
			if jsonOut {
				return cmdutil.WriteJSON(cmd, fact)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s [%s] %s\n", fact.ID, factstore.KindOrInferred(fact), strings.Join(fact.Paths, ","), fact.Text)
			fmt.Fprintf(cmd.OutOrStdout(), "  origin=%s status=%s\n", fact.Origin, fact.Status)
			for _, a := range fact.Provenance {
				verified := "unsigned"
				if a.Verified {
					verified = "verified"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  - session=%s checkpoint=%s commit=%s %s:%d (%s)\n",
					cmdutil.ValueOrUnset(a.SessionID), cmdutil.ValueOrUnset(a.CheckpointID), cmdutil.ValueOrUnset(a.Commit), cmdutil.ValueOrUnset(a.Transcript), a.Line, verified)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch the memory belongs to (default: current branch)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}
