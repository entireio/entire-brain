package cli

import (
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Backlog UX-21. About a hundred remedies hardcoded `entire brain <verb>`. On a
// plugin-style install the runnable spelling is the binary's own name, so each of
// them told the reader to type a command that does not exist for them. The
// guards below hold the fix from two directions: no source line may print the
// hardcoded spelling again (static, with a floor so it cannot match nothing),
// and the remedies that were converted must render in the spelling the
// environment dictates, in both modes (behavioural).

// hardcodedBrainVerb is a literal "entire brain" followed by a verb this binary
// really has. Prose about the product ("entire brain viz — url") is not matched
// because it carries no verb from this list.
// Any verb, not an enumeration. The list used to name fifteen and the guard
// was then blind to every verb nobody remembered to add -- `reset` among them,
// which is how a hardcoded `entire brain reset` reached a printed remedy with
// this test passing. A new subcommand must not silently fall outside the rule.
var hardcodedBrainVerb = regexp.MustCompile(`entire brain [a-z][a-z-]*\b`)

// deliberatelyHardcoded names the lines that keep the literal spelling, and why.
// Each reason is a category, not a per-line excuse.
var deliberatelyHardcoded = []struct{ file, contains, reason string }{
	{"command/add/add.go", "with ` + \"`entire brain dash`\"", "cobra Long help text built at command-construction time, before any lookup exists"},
	{"internal/retrieval/retrieve.go", `"entire brain search`, "matcher over transcript text, not a printed remedy"},
	{"seed.go", "Intake: run `entire brain inspect", "written into the persisted seed artifact; spelling must be deterministic, not reader-dependent"},
	{"internal/retrieval/retrieve.go", `"entire brain query`, "matcher over transcript text, not a printed remedy"},
	{"internal/retrieval/retrieve.go", `"entire brain brief`, "matcher over transcript text, not a printed remedy"},
	{"command/viz/viz.go", `"entire brain viz — %s`, "banner naming the surface being shown, not a command to run"},
	{"doctor.go", "cd '<your repository>' && entire brain doctor", "a shell TEMPLATE with a cd and a placeholder, not a verb the lookup can render"},
}

func TestNoHardcodedBrainSpellingInPrintedRemedies(t *testing.T) {
	scanned, helperSites, allowed := 0, 0, 0
	// "." plus the packages remedies have sunk into (COR-2268): the fact-store
	// remedies, the add command, the stats command, the declared-index repair
	// hint, and the history-store, semantic-store, doc-store, and
	// retrieval-engine read layers must stay under this guard rather than
	// escape it by moving.
	var names []string
	for _, dir := range []string{".", "internal/factstore", "command/add", "command/bench", "command/dash", "command/patternscmd", "command/repoidentity", "command/retrieve", "command/stats", "command/viz", "command/watch", "internal/brainstore", "internal/historystore", "internal/semstore", "internal/docstore", "internal/retrieval", "patterns"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "setup_invocation.go" {
				continue
			}
			if dir != "." {
				name = dir + "/" + name
			}
			names = append(names, name)
		}
	}
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(data), "\n") {
			scanned++
			helperSites += strings.Count(line, "BrainInvocation(")
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !hardcodedBrainVerb.MatchString(line) {
				continue
			}
			ok := false
			for _, a := range deliberatelyHardcoded {
				if a.file == name && strings.Contains(line, a.contains) {
					ok = true
					allowed++
				}
			}
			if !ok {
				t.Errorf("%s:%d hardcodes `entire brain <verb>`; render it with base.BrainInvocation(os.LookupEnv, verb) "+
					"or add a reasoned entry to deliberatelyHardcoded: %s", name, number+1, trimmed)
			}
		}
	}
	// Non-vacuity: the converted sites, and the deliberate exceptions, must
	// actually have been seen.
	if scanned < 10000 {
		t.Fatalf("scanned only %d lines; the walk is broken", scanned)
	}
	if helperSites < 90 {
		t.Fatalf("found only %d BrainInvocation call sites; the scan is matching nothing", helperSites)
	}
	if allowed < len(deliberatelyHardcoded) {
		t.Fatalf("only %d of %d allowlist entries matched a line; stale allowlist or broken matcher", allowed, len(deliberatelyHardcoded))
	}
}

// Rendered remedies, driven both ways. t.Setenv forbids t.Parallel, which is
// why this test does not call it.
func TestConvertedRemediesFollowTheDispatchSpelling(t *testing.T) {
	render := func() map[string]string {
		return map[string]string{
			"brainstore.DeclaredIndexRepairHint": brainstore.DeclaredIndexRepairHint(),
			"factstore.RepairHint":               factstore.RepairHint(),
			"brainstore.DeclaredIndexDefect":     brainstore.DeclaredIndexDefect{Source: brainstore.DeclaredIndexSeed, Absent: true, Path: "seed.json"}.Warning(),
		}
	}
	for _, mode := range []struct{ name, cliVersion, want, forbid string }{
		{"standalone", "", "entire-brain refresh", "entire brain "},
		{"host-dispatched", "0.3.0", "entire brain refresh", "entire-brain "},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(base.EnvCLIVersion, mode.cliVersion)
			rendered := render()
			if len(rendered) < 3 {
				t.Fatalf("rendered only %d remedies", len(rendered))
			}
			for site, text := range rendered {
				if !strings.Contains(text, mode.want) {
					t.Errorf("%s under %s printed %q, want it to contain %q", site, mode.name, text, mode.want)
				}
				if strings.Contains(text, mode.forbid) {
					t.Errorf("%s under %s printed %q, which uses the other spelling (%q)", site, mode.name, text, mode.forbid)
				}
			}
		})
	}
}
