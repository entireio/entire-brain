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

// semanticIndexVerb is the command path that builds the semantic index, as
// cobra registers it: `index` hangs off `refresh`, not off the root.
//
// Twelve strings used to tell the reader to run "entire brain index" (quoted
// here rather than shown as a command, because it is not one and this package
// holds every printed command to resolving) -- a verb that has never existed — the binary answers `unknown command "index"`. The
// advice was unrunnable in both spellings, so every one of them was a dead end
// handed to a user whose index was already missing. The verb lives here, beside
// the prefix, so the two halves of a printed command cannot drift apart.
const semanticIndexVerb = "refresh index"

// brainInvocation renders a command the reader can actually type: the spelling
// of this program they reached (see setupCommandPrefix) followed by a verb path
// that cobra resolves. Every printed command goes through it rather than
// hardcoding one of the two binary names.
func brainInvocation(lookup tui.EnvLookup, verb string) string {
	command := setupBrainCommand(setupCommandPrefix(lookup))
	if verb = strings.TrimSpace(verb); verb != "" {
		command += " " + verb
	}
	return command
}
