package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// host_cli.go names one condition: the host Entire CLI this plugin runs under
// is not installed on this machine.
//
// The brain is an Entire CLI plugin, but a built brain is a directory of JSON
// and SQLite that the README promises stays "fully readable and queryable with
// no `entire` on PATH". Readable it was; refreshable it was not. Session
// capture is the one source that reads THROUGH the host CLI, and when that
// subprocess could not be launched the failure travelled up as the raw
//
//	complete routed checkpoint discovery failed: list checkpoints: entire
//	[checkpoint explain --json --search-all]: exec: "entire": executable file
//	not found in $PATH
//
// — a Go error string naming neither the condition nor a way out.
//
// The distinction this file draws is the load-bearing one. A host CLI that is
// ABSENT is an environmental fact about the machine: nothing in the brain is
// broken, no repair applies to it, and it reads exactly the same on the first
// refresh and the hundredth. A host CLI that is PRESENT and whose export
// failed is a fault with a cause and a repair, and it must stay fatal — the
// alternative is answering real data loss with "unavailable, using seed
// baseline", which is the masking this codebase has already had to undo once
// (see discardManifestThisBuildCannotRewrite in refresh.go).

// hostCLIMissingCode prefixes the degraded message so a reader — and
// newSetupComponent's detail matching — keys on a code rather than prose.
const hostCLIMissingCode = "host_cli_missing"

// hostCLIMissing reports whether err is the host CLI failing to LAUNCH because
// the binary does not exist, as opposed to a command that ran and failed.
//
// The evidence is positive and structural, never a substring of the rendered
// message: exec.Command records LookPath's verdict as *exec.Error{Name, Err},
// and every other outcome — a nonzero exit, a parse failure, a timeout —
// arrives as some other type. errors.As walks the wrapping this package adds
// (runExecCommand, listRoutedCheckpoints, discoverCheckpoints), so the signal
// survives the chain intact. The Name check is what keeps a MISSING GIT, which
// produces the same exec.ErrNotFound from the same call site, from being
// reported as an absent Entire CLI.
//
// A binary named by an explicit path that does not exist (--entire-binary
// /nope/entire) is deliberately NOT covered: exec skips LookPath for a path
// with a separator, so there is no ErrNotFound to match, and an operator who
// named a binary by hand asked for that name to be honoured or refused.
func hostCLIMissing(err error, binary string) bool {
	if err == nil {
		return false
	}
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		return false
	}
	if !errors.Is(execErr.Err, exec.ErrNotFound) {
		return false
	}
	return execErr.Name == hostCLIBinaryName(binary)
}

// hostCLIBinaryName resolves the configured --entire-binary, falling back to
// the default the flag itself defaults to.
func hostCLIBinaryName(binary string) string {
	if trimmed := strings.TrimSpace(binary); trimmed != "" {
		return trimmed
	}
	return entireBinaryName
}

// hostCLIMissingExportError restates a session-export launch failure as a
// named condition with a remedy, in the shape `setup` already uses for the
// missing semantic provider:
//
//	<code>: <what needs the tool and why it could not be used>: <raw detail>
//	-- <the step that fixes it>
//
// The original error is wrapped, not replaced, so the raw text a bug report
// needs survives and hostCLIMissing still recognises the result.
func hostCLIMissingExportError(err error, binary string) error {
	name := hostCLIBinaryName(binary)
	return fmt.Errorf(
		"%s: session capture reads agent checkpoints through the Entire CLI and `%s` is not on PATH: %w"+
			" -- install the Entire CLI, then run `%s enable` in this repo;"+
			" every other brain source is built without it",
		hostCLIMissingCode, name, err, entireBinaryName)
}

// hostCLISemanticProviderAbsent reports that the semantic index cannot be built
// because its provider is reached THROUGH the host Entire CLI (`entire graph`)
// and that binary is not installed.
//
// Session capture is not the only stage that runs the host CLI, so fixing only
// the export moved the same fatal error one stage down the refresh: the export
// degraded and `semantic index: verifying provider failed` then aborted the run
// for the identical reason. This is the same environmental condition and it
// degrades the same way.
//
// The probe is what draws the line the brief cares about. A host CLI that is
// PRESENT but has no `graph` subcommand is a different situation entirely: the
// entire-graph plugin is installable right there, the error already says so by
// name ("install it with `scripts/install.sh`"), and the user has the CLI
// needed to run it. That one stays fatal.
func hostCLISemanticProviderAbsent(graphBinary string) bool {
	return hostCLIBinaryName(graphBinary) == entireBinaryName && !hostCLIOnPath()
}

// hostCLIOnPath reports whether the host Entire CLI can be launched at all.
//
// This is a PROBE, not an inference from a failure: `status` uses it to decide
// whether to name the host CLI as the blocker in its verdict, and a verdict
// that guessed would be the same class of defect it exists to report.
// inspectSessionEndHook already asks exactly this question the same way.
func hostCLIOnPath() bool {
	_, err := exec.LookPath(entireBinaryName)
	return err == nil
}
