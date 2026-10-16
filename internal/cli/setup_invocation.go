package cli

import (
	"strings"

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

// setupCommandPrefix answers the question every printed command depends on:
// WHICH of the two spellings of this program did the reader actually type?
//
// The brain ships as a kubectl-style external command of the Entire CLI. A
// reader who types `entire brain setup` never types the binary's own name, and
// on a managed install (`entire plugin install`) they cannot: the binary lives
// in <xdg_data>/entire/plugins/bin, a directory the Entire CLI prepends to PATH
// inside its OWN process and nowhere else. Printing `entire-brain status` to
// that reader hands them a command their shell cannot resolve.
//
// THE SIGNAL, and it is the host's, not ours: the Entire CLI's plugin
// dispatcher sets ENTIRE_CLI_VERSION in the child's environment unconditionally
// on every plugin launch. It is the only unconditional one — ENTIRE_REPO_ROOT is
// set only inside a worktree, and ENTIRE_PLUGIN_DATA_DIR is dropped when the
// data dir cannot be resolved. os.Args[0] is NOT a signal: the dispatcher execs
// the resolved binary path, so argv[0] is the same absolute path a standalone
// run of that file would carry.
//
// The fallback is the standalone name, which is the honest answer when nothing
// says otherwise: a reader with no ENTIRE_CLI_VERSION reached this binary
// directly, and may not have the Entire CLI at all.
func setupCommandPrefix(lookup tui.EnvLookup) string {
	if lookup == nil {
		return setupBrainBinaryName
	}
	// A marker present but EMPTY does not count: exporting NAME= to clear it is
	// how a reader turns a signal off.
	if value, ok := lookup(envCLIVersion); ok && strings.TrimSpace(value) != "" {
		return setupPluginCommand
	}
	return setupBrainBinaryName
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
