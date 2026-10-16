package cli

import (
	"strings"

	"github.com/entireio/entire-brain/internal/providerdetail"
	"github.com/entireio/entire-brain/internal/tui"
)

const (
	// setupBrainBinaryName is this binary's own name: what a reader who runs it
	// directly types.
	setupBrainBinaryName = "entire-brain"
	// setupEntireBinaryName is the host CLI that dispatches this binary as an
	// external command, and setupBrainPluginVerb is the verb it dispatches on.
	setupEntireBinaryName = "entire"
	setupBrainPluginVerb  = "brain"
	// setupPluginCommand is the plugin spelling: what the reader types when they
	// reach this binary through the Entire CLI.
	setupPluginCommand = setupEntireBinaryName + " " + setupBrainPluginVerb
)

// setupCommandPrefix answers WHICH of the two spellings of this program the
// reader actually typed. The resolution lives in internal/providerdetail so
// that packages outside internal/cli -- the entity index, which must name the
// same re-run command the semantic index names -- can reach it; see
// providerdetail.SetupCommand for the signal and why it is the only one.
func setupCommandPrefix(lookup tui.EnvLookup) string {
	return providerdetail.SetupCommand(providerdetail.EnvLookup(lookup))
}

// setupBrainCommand is the guard every printer goes through, so that a caller
// that forgot to resolve a prefix prints the standalone name rather than a
// command starting with a space.
func setupBrainCommand(brainCmd string) string {
	if trimmed := strings.TrimSpace(brainCmd); trimmed != "" {
		return trimmed
	}
	return setupBrainBinaryName
}
