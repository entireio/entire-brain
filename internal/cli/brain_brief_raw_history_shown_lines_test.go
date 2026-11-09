package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeRawTopUpFixture writes one transcript whose three lines all match
// ALPHA_IDENTIFIER and returns the Brain directory and the transcript's
// Brain-relative path.
func writeRawTopUpFixture(t *testing.T) (brainDir, rel string) {
	t.Helper()
	brainDir = t.TempDir()
	sessionDir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(&body, `{"type":"agent_message","message":"ALPHA_IDENTIFIER evidence %d"}`+"\n", i)
	}
	name := "20260716T010203Z_0001.jsonl"
	if err := os.WriteFile(filepath.Join(sessionDir, name), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return brainDir, "sessions/main/" + name
}

type rawHistoryMatcherFunc func(brainDir, task string, existing []brainTextMatch, limit int) ([]brainTextMatch, error)

func rawHistoryMatcherImplementations() map[string]rawHistoryMatcherFunc {
	return map[string]rawHistoryMatcherFunc{
		"single": brainBriefRawHistoryMatchesSingleScan,
		"multi": func(brainDir, task string, existing []brainTextMatch, limit int) ([]brainTextMatch, error) {
			return brainBriefRawHistoryMatchesMultiScan(brainDir, task, existing, limit, nil)
		},
	}
}

func rawMatchLines(matches []brainTextMatch) []int {
	lines := make([]int, 0, len(matches))
	for _, match := range matches {
		lines = append(lines, match.Line)
	}
	return lines
}

// An indexed record's summary never equals the raw line's excerpt, so the
// path:line:excerpt key alone let the top-up repeat a line the index already
// showed, as raw JSON, in a slot another line could have had.
func TestRawHistoryTopUpSkipsLinesTheIndexShows(t *testing.T) {
	t.Parallel()
	brainDir, rel := writeRawTopUpFixture(t)
	indexed := []brainTextMatch{{Path: rel, Line: 1, Excerpt: "ALPHA_IDENTIFIER evidence 1"}}
	for name, matcher := range rawHistoryMatcherImplementations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := matcher(brainDir, "ALPHA_IDENTIFIER", indexed, 2)
			if err != nil {
				t.Fatal(err)
			}
			if lines := rawMatchLines(got); !reflect.DeepEqual(lines, []int{2, 3}) {
				t.Fatalf("raw top-up lines = %v, want [2 3] (line 1 is already shown): %+v", lines, got)
			}
			for _, match := range got {
				if match.Path != rel {
					t.Fatalf("raw top-up path = %q, want %q", match.Path, rel)
				}
			}
		})
	}
}

// A hit that the existing matches already hold must not count toward a
// query's cap: with one slot left, the top-up returned nothing although
// line 2 matched.
func TestRawHistoryTopUpDuplicateDoesNotTakeTheLastSlot(t *testing.T) {
	t.Parallel()
	brainDir, _ := writeRawTopUpFixture(t)
	first, err := brainBriefRawHistoryMatchesMultiScan(brainDir, "ALPHA_IDENTIFIER", nil, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Line != 1 {
		t.Fatalf("first raw hit = %+v, want line 1", first)
	}
	for name, matcher := range rawHistoryMatcherImplementations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := matcher(brainDir, "ALPHA_IDENTIFIER", first, 1)
			if err != nil {
				t.Fatal(err)
			}
			if lines := rawMatchLines(got); !reflect.DeepEqual(lines, []int{2}) {
				t.Fatalf("raw top-up lines = %v, want [2]: %+v", lines, got)
			}
		})
	}
}

func TestBrainBriefRawTopUpDoesNotRepeatIndexedLines(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	report := runBrainBriefHistoryMerge(t, fixture.opts, 6, brainBriefRawHistoryMatchesObserved)
	matches := report.History.Matches
	if len(matches) == 0 {
		t.Fatal("brief returned no history matches")
	}
	seen := map[string]bool{}
	for _, match := range matches {
		location := fmt.Sprintf("%s:%d", match.Path, match.Line)
		if seen[location] {
			t.Fatalf("history shows %s twice: %+v", location, matches)
		}
		seen[location] = true
	}
}
