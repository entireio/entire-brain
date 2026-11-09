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
// showed for the query, as raw JSON, in a slot another line could have had.
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

// A line can hold several blocks, and the index keeps one record per
// fragment. An indexed excerpt for the line that lacks the query (another
// block, or text past the 700-character summary cut) does not make the raw
// hit a repeat.
func TestRawHistoryTopUpKeepsAHitTheLinesExcerptLacks(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	sessionDir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "20260716T010203Z_0001.jsonl"
	line := `{"v":1,"type":"assistant","content":[{"type":"text","text":"narrative the index ranked"},{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"go test -run BETA_IDENTIFIER ./..."}}]}`
	if err := os.WriteFile(filepath.Join(sessionDir, name), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel := "sessions/main/" + name
	indexed := []brainTextMatch{{Path: rel, Line: 1, Excerpt: "narrative the index ranked"}}
	for name, matcher := range rawHistoryMatcherImplementations() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := matcher(brainDir, "BETA_IDENTIFIER", indexed, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Path != rel || got[0].Line != 1 {
				t.Fatalf("raw top-up = %+v, want the BETA_IDENTIFIER hit on %s:1", got, rel)
			}
		})
	}
}

// The raw hit is a repeat only when every occurrence of the query in its
// line's fragments is inside a summary the brief shows. Sharing the query with
// one shown fragment is not enough, and a kept hit's excerpt shows the
// occurrence the brief does not.
func TestRawHistoryTopUpComparesTheLinesFragments(t *testing.T) {
	t.Parallel()
	padding := strings.Repeat("padding words before the term ", 30)
	long := padding + "ALPHA_IDENTIFIER past the cut"
	if len(cleanHistorySummary(long)) <= 700 || strings.Contains(historyFragmentSummary(long), "ALPHA_IDENTIFIER") {
		t.Fatalf("fixture text must put ALPHA_IDENTIFIER past the 700-character summary cut")
	}
	twice := "ALPHA_IDENTIFIER first mention " + padding + padding + "ALPHA_IDENTIFIER failed with exit 1"
	if !strings.Contains(historyFragmentSummary(twice), "ALPHA_IDENTIFIER") || strings.Contains(historyRawLineExcerpt(twice, "ALPHA_IDENTIFIER"), "exit 1") {
		t.Fatalf("fixture text must mention ALPHA_IDENTIFIER in its summary and again out of the first mention's excerpt")
	}
	textBlock := func(text string) string { return `{"type":"text","text":"` + text + `"}` }
	cases := []struct {
		name     string
		blocks   []string
		shown    []string
		wantKept bool
		wantIn   string // text the kept hit's excerpt must show
	}{
		{
			name:     "another block has the query too",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER was discussed"), textBlock("ALPHA_IDENTIFIER failed with exit 1")},
			shown:    []string{"ALPHA_IDENTIFIER was discussed"},
			wantKept: true,
			wantIn:   "failed with exit 1",
		},
		{
			name:     "an unshown block far after a shown one",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER was discussed"), textBlock(padding + padding), textBlock("ALPHA_IDENTIFIER failed with exit 1")},
			shown:    []string{"ALPHA_IDENTIFIER was discussed"},
			wantKept: true,
			wantIn:   "failed with exit 1",
		},
		{
			name:     "the query again past the summary cut",
			blocks:   []string{textBlock(twice)},
			shown:    []string{historyFragmentSummary(twice)},
			wantKept: true,
			wantIn:   "failed with exit 1",
		},
		{
			name:     "every block with the query is shown",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER was discussed"), textBlock("ALPHA_IDENTIFIER failed with exit 1")},
			shown:    []string{"ALPHA_IDENTIFIER was discussed", "ALPHA_IDENTIFIER failed with exit 1"},
			wantKept: false,
		},
		{
			name:     "only the shown block has the query",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER was discussed"), textBlock("unrelated narrative")},
			shown:    []string{"ALPHA_IDENTIFIER was discussed"},
			wantKept: false,
		},
		{
			name:     "the query is past the summary cut",
			blocks:   []string{textBlock(long)},
			shown:    []string{historyFragmentSummary(long)},
			wantKept: true,
			wantIn:   "past the cut",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			brainDir := t.TempDir()
			sessionDir := filepath.Join(brainDir, "sessions", "main")
			if err := os.MkdirAll(sessionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			name := "20260716T010203Z_0001.jsonl"
			line := `{"v":1,"type":"assistant","content":[` + strings.Join(tc.blocks, ",") + `]}`
			if err := os.WriteFile(filepath.Join(sessionDir, name), []byte(line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			rel := "sessions/main/" + name
			var indexed []brainTextMatch
			for _, excerpt := range tc.shown {
				indexed = append(indexed, brainTextMatch{Path: rel, Line: 1, Excerpt: excerpt})
			}
			for impl, matcher := range rawHistoryMatcherImplementations() {
				got, err := matcher(brainDir, "ALPHA_IDENTIFIER", indexed, 4)
				if err != nil {
					t.Fatal(err)
				}
				if kept := len(got) == 1 && got[0].Line == 1; kept != tc.wantKept || len(got) > 1 {
					t.Fatalf("%s: raw top-up = %+v, want kept=%v", impl, got, tc.wantKept)
				}
				if tc.wantKept && !strings.Contains(got[0].Excerpt, tc.wantIn) {
					t.Fatalf("%s: kept excerpt %q does not show %q", impl, got[0].Excerpt, tc.wantIn)
				}
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
