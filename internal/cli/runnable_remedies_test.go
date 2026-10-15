package cli

import (
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
	{"add.go", "with ` + \"`entire brain dash`\"", "cobra Long help text built at command-construction time, before any lookup exists"},
	{"retrieve.go", `"entire brain search`, "matcher over transcript text, not a printed remedy"},
	{"mcp.go", "entire brain", "MCP tool descriptions are pinned by TestMCPToolSurfaceGolden; env-dependent descriptions would make the golden vary by shell"},
	{"seed.go", "Intake: run `entire brain inspect", "written into the persisted seed artifact; spelling must be deterministic, not reader-dependent"},
	{"capabilities.go", `"entire brain status --details --json"`, "machine-readable capabilities contract pinned by capabilities_report_regression_test.go"},
	{"retrieve.go", `"entire brain query`, "matcher over transcript text, not a printed remedy"},
	{"retrieve.go", `"entire brain brief`, "matcher over transcript text, not a printed remedy"},
	{"viz.go", `"entire brain viz — %s`, "banner naming the surface being shown, not a command to run"},
}

func TestNoHardcodedBrainSpellingInPrintedRemedies(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned, helperSites, allowed := 0, 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "setup_invocation.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(data), "\n") {
			scanned++
			helperSites += strings.Count(line, "brainInvocation(")
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
				t.Errorf("%s:%d hardcodes `entire brain <verb>`; render it with brainInvocation(os.LookupEnv, verb) "+
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
		t.Fatalf("found only %d brainInvocation call sites; the scan is matching nothing", helperSites)
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
			"declaredIndexRepairHint": declaredIndexRepairHint(),
			"factsRepairHint":         factsRepairHint(),
			"declaredIndexDefect":     declaredIndexDefect{Source: declaredIndexSeed, Absent: true, Path: "seed.json"}.Warning(),
		}
	}
	for _, mode := range []struct{ name, cliVersion, want, forbid string }{
		{"standalone", "", "entire-brain refresh", "entire brain "},
		{"host-dispatched", "0.3.0", "entire brain refresh", "entire-brain "},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(envCLIVersion, mode.cliVersion)
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
