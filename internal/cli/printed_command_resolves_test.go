package cli

import (
	"regexp"
	"strings"
	"testing"
)

// Printed advice is only advice if the reader can type it.
//
// `entire memory repair` was printed by three of the memory health/error
// surfaces and resolves in NEITHER dispatch mode. A standalone reader has no
// `entire` on PATH at all -- that is the whole reason setupCommandPrefix falls
// back to the binary's own name. A plugin reader does have `entire`, but
// `memory` is not one of its verbs: it is a subcommand of THIS binary, reached
// as `entire brain memory`. So the string was wrong for everyone, which is why
// standalone and host-dispatched runs printed it identically -- it was never
// reading the signal at all.
//
// This test does not pin the sentence. It extracts every backticked command
// from the advice these surfaces produce, in both dispatch modes, and resolves
// the verbs against the real command tree. A future string that names a brain
// subcommand without the brain's spelling fails here whatever its wording.
var printedCommandPattern = regexp.MustCompile("`(entire[^`]*)`")

// memoryAdviceStrings gathers the advice from every site that names a repair
// command, with the environment already arranged by the caller.
func memoryAdviceStrings(t *testing.T) []string {
	t.Helper()
	advice := []string{
		projectionStateAction(projectionStateAbsent),
		projectionStateAction(projectionStateStale),
		projectionStateAction(projectionStateCorrupt),
	}

	inventory := memoryJobInventory{
		Migrations: []memoryStateIssue{{
			Kind:    "job",
			File:    "job-0001.json",
			Code:    memoryErrMigrationRequired,
			Version: 1,
		}},
		EntriesObserved: 1,
		EntriesScanned:  1,
		ScanComplete:    true,
	}
	health := memoryJobInventoryHealth(inventory)
	action, _ := health["action"].(string)
	if action == "" || action == "none" {
		t.Fatalf("job inventory health produced no migration advice: %v", health)
	}
	advice = append(advice, action)

	issues := memoryHealthIssues([]memoryStateIssue{{
		Kind:    "hint",
		File:    "hint-0001.json",
		Code:    memoryErrMigrationRequired,
		Version: 1,
	}})
	if len(issues) != 1 {
		t.Fatalf("expected one health issue, got %d", len(issues))
	}
	advice = append(advice, issues[0].Action)

	return advice
}

func TestPrintedRepairCommandsResolveInBothDispatchModes(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})

	for _, mode := range []struct {
		name string
		// cliVersion is the host's unconditional plugin marker. Empty means a
		// standalone run: nothing set it, so nothing dispatched us.
		cliVersion string
		wantPrefix string
	}{
		{name: "standalone", cliVersion: "", wantPrefix: "entire-brain "},
		{name: "host-dispatched", cliVersion: "0.3.0", wantPrefix: "entire brain "},
	} {
		t.Run(mode.name, func(t *testing.T) {
			if mode.cliVersion == "" {
				t.Setenv(envCLIVersion, "")
			} else {
				t.Setenv(envCLIVersion, mode.cliVersion)
			}

			checked := 0
			for _, line := range memoryAdviceStrings(t) {
				for _, match := range printedCommandPattern.FindAllStringSubmatch(line, -1) {
					printed := match[1]
					checked++

					if !strings.HasPrefix(printed, mode.wantPrefix) {
						t.Errorf("%s run printed %q; a reader who reached this binary that way types %q",
							mode.name, printed, mode.wantPrefix+"...")
						continue
					}

					// The verbs after the spelling must resolve against the
					// real tree, not merely look plausible.
					args := strings.Fields(strings.TrimPrefix(printed, mode.wantPrefix))
					if len(args) == 0 {
						t.Errorf("%s run printed a bare prefix %q with no subcommand", mode.name, printed)
						continue
					}
					found, rest, err := root.Find(args)
					if err != nil {
						t.Errorf("%s run printed `%s`, which does not resolve: %v", mode.name, printed, err)
						continue
					}
					if len(rest) > 0 {
						t.Errorf("%s run printed `%s`, but %q is not a subcommand of %q",
							mode.name, printed, strings.Join(rest, " "), found.CommandPath())
					}
				}
			}
			if checked == 0 {
				t.Fatalf("no printed commands found in the memory advice -- the test is checking nothing")
			}
		})
	}
}
