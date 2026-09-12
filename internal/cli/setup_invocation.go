package cli

import (
	"strings"

	"github.com/ashtom/entire-brain/internal/tui"
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
