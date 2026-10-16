package cli

import (
	"strings"
	"testing"
)

// providerConditions are the two ways the semantic-provider pair can be out of
// agreement, in the words the product actually prints. Each row records which
// component the MESSAGE says is behind, read off the message's own text rather
// than off the classifier under test.
var providerConditions = []struct {
	name   string
	detail string
	behind string // "graph" | "brain"
}{
	{
		name:   "provider absent",
		detail: "provider=degraded (read graph identity revision: Invalid usage: unknown command \"graph\" for \"entire\"; the entire-graph semantic provider is not installed (entire has no `graph` command))",
		behind: "graph",
	},
	{
		name:   "provider not on PATH",
		detail: `provider=degraded (read graph identity revision: exec: "entire": executable file not found in $PATH)`,
		behind: "graph",
	},
	{
		name:   "provider ahead of this build",
		detail: `provider=degraded (semantic provider emitted schema_version "1.3" but this build reads 1.1; minors are additive, so the snapshot is usable, but upgrade entire-brain to ingest everything "1.3" carries)`,
		behind: "brain",
	},
}

// remedyTargets answers which component a remedy acts on, from the remedy text
// alone.
func remedyTargets(remedy string) string {
	switch {
	case strings.Contains(remedy, "plugin install graph"):
		return "graph"
	case strings.Contains(remedy, "get-brain.sh"):
		return "brain"
	default:
		return "unknown(" + remedy + ")"
	}
}

// TestSemanticProviderRemedyNamesTheComponentThatIsBehind is the property, not
// the string: a remedy must act on the side the message says is behind.
//
// The old remedy was a static entry in a name-keyed table, so it could not
// vary by condition -- it said "install graph" even on a skew where Graph is
// the side that is AHEAD, offering the reader an action that cannot help and
// that would widen the gap. Any future attempt to answer this finding with one
// fixed command fails here, because no single command satisfies both rows.
func TestSemanticProviderRemedyNamesTheComponentThatIsBehind(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, tc := range providerConditions {
		check := doctorCheckResult{
			Name: doctorSemanticProviderFinding, State: "warn",
			Detail: tc.detail, Scope: doctorScopeEnvironment,
		}
		remedy := doctorRemedy(check)
		if got := remedyTargets(remedy); got != tc.behind {
			t.Errorf("%s: message says %s is behind, remedy acts on %s (%q)\n detail: %s",
				tc.name, tc.behind, got, remedy, tc.detail)
		}
		seen[tc.behind] = true
	}
	// Non-vacuity: a table that only ever exercised one side would pass
	// against the static map this replaced.
	if len(seen) < 2 {
		t.Fatalf("fixture is vacuous: it only covers %v, so a constant remedy would pass", seen)
	}
}

// TestSemanticProviderRemedyClassifiesTheFullDetail: the doctor's short report
// truncates, and it truncates the skew message mid-word -- "upgrade
// entire-b...". Choosing the remedy from what is PRINTED would send the reader
// to Graph exactly on the long lines that most need the help, so the
// classification reads the full detail.
func TestSemanticProviderRemedyClassifiesTheFullDetail(t *testing.T) {
	t.Parallel()
	var skew string
	for _, tc := range providerConditions {
		if tc.behind == "brain" {
			skew = tc.detail
		}
	}
	if skew == "" {
		t.Fatal("fixture is vacuous: no skew condition in the table")
	}
	check := doctorCheckResult{
		Name: doctorSemanticProviderFinding, State: "warn",
		Detail: skew, Scope: doctorScopeEnvironment,
	}
	printed := doctorActionableLine{finding: check, remedy: doctorRemedy(check)}.detail()
	if strings.Contains(printed, "upgrade entire-brain") {
		t.Skipf("fixture no longer truncates the marker; printed: %s", printed)
	}
	if remedyTargets(doctorRemedy(check)) != "brain" {
		t.Fatalf("remedy flipped to the wrong component once the marker was truncated away:\n%s", printed)
	}
	if !strings.Contains(printed, "get-brain.sh") {
		t.Fatalf("the rendered line does not carry the brain-upgrade remedy:\n%s", printed)
	}
}

// TestSemanticProviderHasNoStaticRemedyEntry keeps the structural fix from
// being undone by a well-meaning table addition: an entry here would be
// unreachable for this finding, and would read as the authoritative answer.
func TestSemanticProviderHasNoStaticRemedyEntry(t *testing.T) {
	t.Parallel()
	if remedy, ok := doctorRemedies[doctorSemanticProviderFinding]; ok {
		t.Fatalf("a condition-dependent finding has a static remedy entry %q", remedy)
	}
}
