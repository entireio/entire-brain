// Package providerdetail renders the one failure a first run is most likely to
// hit: the entire-graph provider is not installed, so every component that
// shells out to `entire graph ...` fails with the host CLI's full usage dump.
//
// It lives in its own package because TWO components fail from that single
// cause -- the semantic index (internal/cli) and the entity index
// (internal/entityindex) -- and for a long while only one of them had been
// taught to summarise it. The semantic index reported one readable line naming
// the install step; the entity index embedded 60 lines of `entire --help` and a
// "Did you mean this? grant" that implied the reader had mistyped a command
// they never typed. Two different messages for one cause is the defect, and a
// private copy in each package is how it comes back, so the summary rule, the
// detection markers and the remedy sentence have exactly one home here.
package providerdetail

import (
	"fmt"
	"strings"
)

const (
	// BrainBinaryName is this program's own name: what a reader who runs it
	// directly types.
	BrainBinaryName = "entire-brain"
	// EntireBinaryName is the host CLI that dispatches the brain as an external
	// command, and brainPluginVerb is the verb it dispatches on.
	EntireBinaryName = "entire"
	brainPluginVerb  = "brain"
	// PluginCommand is the plugin spelling: what the reader types when they
	// reach this binary through the Entire CLI.
	PluginCommand = EntireBinaryName + " " + brainPluginVerb

	// envCLIVersion is set by the Entire CLI's plugin dispatcher on every
	// plugin launch, and is the only signal that distinguishes the two
	// spellings. See SetupCommand.
	envCLIVersion = "ENTIRE_CLI_VERSION"

	// InstallGraphCommand is the one runnable way to get the provider: `graph`
	// is in the Entire CLI's official plugin index (`entire plugin search
	// graph`), so this works for a reader who has only the CLI.
	InstallGraphCommand = EntireBinaryName + " plugin install graph"

	// MaxSummary caps a summarised detail so one failure cannot own the
	// terminal even when the subprocess writes a single enormous line.
	MaxSummary = 200
)

// EnvLookup is os.LookupEnv, taken as an argument so the resolution is testable
// without mutating the process environment.
type EnvLookup func(string) (string, bool)

// Summary reduces a subprocess failure to one printable line.
//
// A CLI answers an unknown command by dumping its ENTIRE usage text -- sixty
// lines of command groups and flags -- to stderr, and that text arrives here as
// the error detail. Printed whole it buries the one sentence that matters
// inside setup's failure line, its summary block, `status` and `doctor` alike.
// The line the CLI marks with "Error:" is that sentence; the first line is the
// fallback for tools that do not mark one.
//
// Detection still runs against the FULL detail, so shortening what is shown
// never changes what is recognised.
func Summary(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	lines := strings.Split(detail, "\n")
	summary := strings.TrimSpace(lines[0])
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "Error:"); ok {
			summary = strings.TrimSpace(after)
			break
		}
	}
	summary = strings.TrimSuffix(strings.TrimSpace(summary), "Usage:")
	summary = strings.TrimSpace(summary)
	if runes := []rune(summary); len(runes) > MaxSummary {
		summary = strings.TrimSpace(string(runes[:MaxSummary])) + "..."
	}
	return summary
}

// Flatten is the last net before a detail reaches a terminal: it guarantees the
// invariant every one-line report depends on, that a reported detail occupies
// ONE line, WITHOUT shortening a detail that a producer already summarised.
//
// A producer that already composed a single line -- summary plus the remedy
// sentence, which is deliberately longer than MaxSummary -- passes through
// untouched. Only a detail that still carries a newline, i.e. one nobody
// summarised, is reduced. That asymmetry is the point: the net exists for the
// component nobody has fixed yet, not to truncate the ones already correct.
func Flatten(detail string) string {
	if !strings.ContainsAny(detail, "\n\r") {
		return strings.TrimSpace(detail)
	}
	return Summary(detail)
}

// Missing recognises the provider-absent shape of a failure -- the host CLI
// rejecting an unknown `graph` command, or the binary not being on PATH at all
// -- as opposed to a provider that ran and reported something. Matching on text
// is unavoidable here: the failure is another process's exit status plus its
// stderr, and there is no typed error to switch on across a process boundary.
func Missing(detail string) bool {
	text := strings.ToLower(detail)
	if text == "" {
		return false
	}
	for _, marker := range []string{
		`unknown command "graph"`,
		"unknown command 'graph'",
		"unknown command graph",
		"executable file not found",
		"no such file or directory",
		"command not found",
		// Newer host CLIs have a `graph` dispatcher that reports the absent
		// plugin by name instead of rejecting an unknown command. Added on
		// main while this package was being extracted; folding the two
		// implementations together must not drop it.
		"plugin is not installed",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// SetupCommand answers the question every printed command depends on: WHICH of
// the two spellings of this program did the reader actually type?
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
// on every plugin launch. It is the only unconditional one -- ENTIRE_REPO_ROOT
// is set only inside a worktree, and ENTIRE_PLUGIN_DATA_DIR is dropped when the
// data dir cannot be resolved. os.Args[0] is NOT a signal: the dispatcher execs
// the resolved binary path, so argv[0] is the same absolute path a standalone
// run of that file would carry.
//
// The fallback is the standalone name, which is the honest answer when nothing
// says otherwise: a reader with no ENTIRE_CLI_VERSION reached this binary
// directly, and may not have the Entire CLI at all.
func SetupCommand(lookup EnvLookup) string {
	if lookup == nil {
		return BrainBinaryName
	}
	// A marker present but EMPTY does not count: exporting NAME= to clear it is
	// how a reader turns a signal off.
	if value, ok := lookup(envCLIVersion); ok && strings.TrimSpace(value) != "" {
		return PluginCommand
	}
	return BrainBinaryName
}

// MissingRemedy is the sentence both the semantic index and the entity index
// append when Missing says the provider simply is not installed: what is
// missing, how the reader can tell, the command that fixes it, and the command
// to run afterwards. One home, so the two components cannot disagree about the
// remedy for the same cause.
//
// The command is InstallGraphCommand, NOT the `scripts/install.sh` form this
// sentence used to carry. That form is not runnable for the reader who hits
// this: it presupposes an entire-brain source checkout, which someone who
// installed the brain as a plugin does not have. `graph` is in the official
// plugin index, so one command installs it from anywhere.
func MissingRemedy(graphBinary string, lookup EnvLookup) string {
	binary := strings.TrimSpace(graphBinary)
	if binary == "" {
		binary = EntireBinaryName
	}
	return fmt.Sprintf(
		"the entire-graph semantic provider is not installed (%s has no `graph` command)"+
			" -- install it with `%s`, then re-run `%s setup`",
		binary, InstallGraphCommand, SetupCommand(lookup))
}
