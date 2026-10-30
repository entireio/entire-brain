package cli

import (
	"errors"
	"fmt"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

// errFactStoreIncomplete is the exit verdict for a read surface that rendered
// a fact view over a store the manifest says is short. The view itself is
// printed — hiding the survivors helps nobody — but the command exits nonzero
// so a script cannot read the output as complete, matching how a corrupt
// history index already behaves.
var errFactStoreIncomplete = errors.New("memory store cannot produce every memory the manifest declares")

// reportFactStoreIntegrity prints the cross-check verdict for a fact read
// surface and returns the error that should end the command, or nil when the
// store is intact.
//
// In text mode the warning goes to stdout, ahead of the view it qualifies, so
// a reader who pipes stdout sees it beside the numbers it corrects. In JSON
// mode stdout belongs to the document and the warning goes to stderr instead;
// the nonzero exit is what a JSON consumer must key on.
func reportFactStoreIntegrity(cmd *cobra.Command, brainDir string, jsonOut bool) error {
	integrity := factstore.InspectBrain(brainDir)
	if integrity.OK() {
		return nil
	}
	out := cmd.OutOrStdout()
	if jsonOut {
		out = cmd.ErrOrStderr()
	}
	fmt.Fprintf(out, "warning: %s\n", integrity.Warning())
	return renderedCommandError{Err: errFactStoreIncomplete}
}
