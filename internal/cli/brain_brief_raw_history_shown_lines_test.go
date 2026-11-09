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

// The raw hit is a repeat only when each value of its line that has the query
// is exactly an excerpt the brief shows and the line mentions the query
// nowhere else. Anything less keeps the hit with its raw excerpt.
func TestRawHistoryTopUpComparesTheLinesFragments(t *testing.T) {
	t.Parallel()
	padding := strings.Repeat("padding words before the term ", 30)
	long := padding + "ALPHA_IDENTIFIER past the cut"
	if len(cleanHistorySummary(long)) <= 700 || strings.Contains(historyFragmentSummary(long), "ALPHA_IDENTIFIER") {
		t.Fatalf("fixture text must put ALPHA_IDENTIFIER past the 700-character summary cut")
	}
	twice := "ALPHA_IDENTIFIER first mention " + padding + padding + "ALPHA_IDENTIFIER failed with exit 1"
	if !strings.Contains(historyFragmentSummary(twice), "ALPHA_IDENTIFIER") {
		t.Fatalf("fixture text must mention ALPHA_IDENTIFIER in its summary and again past the cut")
	}
	// A mention written with punctuation, which matches the query only
	// normalized, across the summary's 697-byte content cut.
	head := "ALPHA_IDENTIFIER first mention "
	straddle := head + strings.Repeat("p", 680-len(head)-1) + " ALPHA / IDENTIFIER failed later"
	if summary := historyFragmentSummary(straddle); !strings.HasSuffix(summary, "ALPHA / IDENTIFIE...") {
		t.Fatalf("fixture text must cut ALPHA / IDENTIFIER at the summary limit, got %q", summary[len(summary)-30:])
	}
	textBlock := func(text string) string { return `{"type":"text","text":"` + text + `"}` }
	cases := []struct {
		name     string
		blocks   []string
		shown    []string
		query    string // ALPHA_IDENTIFIER when empty
		wantKept bool
		wantIn   string // text the kept hit's excerpt must show, when set
	}{
		{
			name:     "another block has the query too",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER was discussed"), textBlock("ALPHA_IDENTIFIER failed with exit 1")},
			shown:    []string{"ALPHA_IDENTIFIER was discussed"},
			wantKept: true,
			wantIn:   "failed with exit 1",
		},
		{
			name:     "an unshown block that normalizes like the shown one",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER = 1"), textBlock("ALPHA_IDENTIFIER = -1")},
			shown:    []string{"ALPHA_IDENTIFIER = 1"},
			wantKept: true,
			wantIn:   "-1",
		},
		{
			name:     "a shown excerpt that ends in an ellipsis",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER was fixed..."), textBlock("ALPHA_IDENTIFIER was fixed but broke later")},
			shown:    []string{"ALPHA_IDENTIFIER was fixed..."},
			wantKept: true,
			wantIn:   "broke later",
		},
		{
			name:     "a key that has the query",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER already fixed"), `{"type":"tool_use","id":"c1","name":"Write","input":{"ALPHA_IDENTIFIER":"new value"}}`},
			shown:    []string{"ALPHA_IDENTIFIER already fixed"},
			wantKept: true,
			wantIn:   "new value",
		},
		{
			name:     "a normalized mention across the summary cut",
			blocks:   []string{textBlock(straddle)},
			shown:    []string{historyFragmentSummary(straddle)},
			wantKept: true,
		},
		{
			name:     "an unshown block far after a shown one",
			blocks:   []string{textBlock("ALPHA_IDENTIFIER was discussed"), textBlock(padding + padding), textBlock("ALPHA_IDENTIFIER failed with exit 1")},
			shown:    []string{"ALPHA_IDENTIFIER was discussed"},
			wantKept: true,
		},
		{
			name:     "the query again past the summary cut",
			blocks:   []string{textBlock(twice)},
			shown:    []string{historyFragmentSummary(twice)},
			wantKept: true,
		},
		{
			// Normalized, the line reads "... text test test test failed":
			// the key's mention overlaps the shown value's.
			name:     "a key that overlaps a shown mention",
			blocks:   []string{`{"type":"text","text":"test_test","test":"failed"}`},
			shown:    []string{"test_test"},
			query:    "test_test",
			wantKept: true,
			wantIn:   "failed",
		},
		{
			// Case-folded, the key reads "foobar/baz", which normalizes to the
			// query's "foobar baz"; in its own case it would read "foo bar baz".
			name:     "a camel-case key that matches once case-folded",
			blocks:   []string{textBlock("FOOBAR_BAZ discussed"), `{"type":"tool_use","id":"c1","name":"Write","input":{"fooBar/baz":"failed"}}`},
			shown:    []string{"FOOBAR_BAZ discussed"},
			query:    "FOOBAR_BAZ",
			wantKept: true,
			wantIn:   "failed",
		},
		{
			// Decoded, the shown value has the line's only mention apart from
			// the key; read raw, the escape hides it and the counts balance.
			name:     "an escaped shown value and a key that has the query",
			blocks:   []string{`{"type":"text","text":"\u0041LPHA_IDENTIFIER discussed","ALPHA_IDENTIFIER":"failed"}`},
			shown:    []string{"ALPHA_IDENTIFIER discussed"},
			wantKept: true,
			wantIn:   "failed",
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
			query := tc.query
			if query == "" {
				query = "ALPHA_IDENTIFIER"
			}
			for impl, matcher := range rawHistoryMatcherImplementations() {
				got, err := matcher(brainDir, query, indexed, 4)
				if err != nil {
					t.Fatal(err)
				}
				if kept := len(got) == 1 && got[0].Line == 1; kept != tc.wantKept || len(got) > 1 {
					t.Fatalf("%s: raw top-up = %+v, want kept=%v", impl, got, tc.wantKept)
				}
				if tc.wantKept && got[0].Excerpt != historyRawLineExcerpt(line, query) {
					t.Fatalf("%s: kept excerpt %q is not the raw line's excerpt", impl, got[0].Excerpt)
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

// The index keeps only code facts from a tool's output, so a line can mention
// the query in text no history record holds. Showing every record of the line
// does not show that text, and the kept hit's excerpt shows it.
func TestRawHistoryTopUpKeepsToolOutputTheIndexLeavesOut(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	sessionDir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "20260716T010203Z_0001.jsonl"
	line := `{"v":1,"type":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"grep \"ALPHA_IDENTIFIER\" -n"},"result":{"output":"ALPHA_IDENTIFIER stopped halfway and never answered","status":"error"}}]}`
	if err := os.WriteFile(filepath.Join(sessionDir, name), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record, ok := decodeTranscriptRecord(line)
	if !ok {
		t.Fatal("fixture line does not decode as a transcript record")
	}
	rel := "sessions/main/" + name
	var indexed []brainTextMatch
	for _, fragment := range extractHistoryJSONFragments(record) {
		summary := historyFragmentSummary(fragment.Text)
		if strings.Contains(summary, "halfway") {
			t.Fatalf("fixture output must not be indexed, got fragment %q", summary)
		}
		indexed = append(indexed, brainTextMatch{Path: rel, Line: 1, Excerpt: summary})
	}
	if len(indexed) == 0 {
		t.Fatal("fixture line has no indexed fragments")
	}
	for impl, matcher := range rawHistoryMatcherImplementations() {
		got, err := matcher(brainDir, "ALPHA_IDENTIFIER", indexed, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Line != 1 || !strings.Contains(got[0].Excerpt, "stopped halfway") {
			t.Fatalf("%s: raw top-up = %+v, want the line kept with its tool output", impl, got)
		}
	}
}

func TestRawLineTextsReadsStringValuesInOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line        string
		wantValues  []string
		wantDecoded string
	}{
		{
			line:        `{"type":"assistant","content":[{"type":"text","text":"first"},{"input":{"command":"second"},"n":3,"ok":true,"none":null}],"text":"third"}`,
			wantValues:  []string{"assistant", "text", "first", "second", "third"},
			wantDecoded: "type assistant content type text text first input command second n ok none text third",
		},
		{line: `["a",["b",{"k":"c"}]]`, wantValues: []string{"a", "b", "c"}, wantDecoded: "a b k c"},
		{line: `{"\u0041":"\u0042"}`, wantValues: []string{"B"}, wantDecoded: "A B"},
		{line: `plain text line`, wantValues: []string{"plain text line"}, wantDecoded: "plain text line"},
		{line: `{"type": "broken", "text"`, wantValues: []string{`{"type": "broken", "text"`}, wantDecoded: `{"type": "broken", "text"`},
		{line: `{"a":"x"} trailing`, wantValues: []string{`{"a":"x"} trailing`}, wantDecoded: `{"a":"x"} trailing`},
	}
	for _, tc := range cases {
		values, decoded := rawLineTexts(tc.line)
		if !reflect.DeepEqual(values, tc.wantValues) || decoded != tc.wantDecoded {
			t.Errorf("rawLineTexts(%q) = %q, %q; want %q, %q", tc.line, values, decoded, tc.wantValues, tc.wantDecoded)
		}
	}
}

func TestCountHistoryMentionsCountsOverlaps(t *testing.T) {
	t.Parallel()
	if got := countHistoryMentions("test_test_test failed", "test_test"); got != (historyMentionCount{literal: 2, normalized: 2}) {
		t.Fatalf("countHistoryMentions = %+v, want 2 literal and 2 normalized", got)
	}
	if got := countHistoryMentions("ALPHA / IDENTIFIER", "ALPHA_IDENTIFIER"); got != (historyMentionCount{literal: 0, normalized: 1}) {
		t.Fatalf("countHistoryMentions = %+v, want 1 normalized", got)
	}
	if got := countHistoryMentions("fooBar/baz", "FOOBAR_BAZ"); got != (historyMentionCount{literal: 0, normalized: 1}) {
		t.Fatalf("countHistoryMentions = %+v, want the case-folded mention", got)
	}
}
