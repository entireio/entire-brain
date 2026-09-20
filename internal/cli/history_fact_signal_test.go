package cli

import (
	"encoding/json"
	"testing"
)

// Tool output is where a command reveals a decided value. The signal that
// decides whether such output enters the history index used to be a hardcoded
// list of phrases taken from individual benchmark task names, so a real
// repository got almost none of its command output indexed.
func TestHistoryFactSignalMatchesValuesCommandsPrint(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{
			"flag default from --help",
			"      --confidence float   Minimum agent confidence to auto-apply a merge/supersede; below this it is queued for review (default 0.75)",
		},
		{"go constant", "\tdefaultFactConfidenceThreshold = 0.75"},
		{"go declaration", "func applyFactActions(active []factRecord) {"},
		{"typed declaration", "type factAction struct {"},
		{"python default", "def merge(threshold = 0.75):"},
		{"quoted assignment", `mode = "queued"`},
		{"boolean assignment", "historyIndex = true"},
		{"short assign", "threshold := 0.75"},
		{"file line anchor", "internal/cli/facts_merge.go:17: constant declared here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !historyTextHasCodeFactSignal(tc.text) {
				t.Fatalf("expected a durable fact signal in %q", tc.text)
			}
		})
	}
}

func TestHistoryFactSignalIgnoresRoutineChatter(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"plain prose", "Let me look at how the reconcile engine is wired together."},
		{"directory listing", "total 48"},
		{"progress line", "Scanning 200 checkpoints"},
		{"bare path", "internal/cli/facts_merge.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if historyTextHasCodeFactSignal(tc.text) {
				t.Fatalf("did not expect a fact signal in %q", tc.text)
			}
		})
	}
}

// The pre-decode line filter runs on RAW transcript lines, where embedded
// quotes are JSON-escaped. A line whose only content is a code fact used to be
// dropped before the fragment extractor could see it, so tool output carrying
// a decided value never reached the index unless narrative keywords happened
// to share the line.
func TestPrefilterAdmitsRawCodeFactLines(t *testing.T) {
	lines := []string{
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"defaultFactConfidenceThreshold = 0.75"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"--confidence float   Minimum agent confidence to auto-apply a merge/supersede; below this it is queued for review (default 0.75)"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"timeout = 30\nretries = 5\nmode = \"strict\""}]}}`,
	}
	for _, line := range lines {
		if !historyLineMayContainIndexedContent(line) {
			t.Errorf("raw code-fact line dropped before decode: %q", line)
		}
	}
}

func TestPrefilterStillSkipsIrrelevantLines(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"session_meta"}`,
	}
	for _, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatal(err)
		}
		if historyLineMayContainIndexedContent(line) && len(extractHistoryJSONFragments(obj)) > 0 {
			t.Errorf("irrelevant line produced fragments: %q", line)
		}
	}
}

// The predicate must not depend on vocabulary borrowed from benchmark tasks.
// These strings were the entire allowlist; matching them as such would mean the
// index is once again tuned to the benchmark rather than to code.
func TestHistoryFactSignalIsNotBenchmarkVocabulary(t *testing.T) {
	for _, phrase := range []string{
		"bare auth", "seed agent", "schema contract", "brainignore",
		"attributionbasecommit", "resolve transcript path", "github workflow",
	} {
		if historyTextHasCodeFactSignal(phrase) {
			t.Fatalf("benchmark phrase %q must not by itself signal a code fact", phrase)
		}
	}
}
