package cli

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// docs/feature-matrix.md states counts that docs/feature-matrix.csv determines.
// Prose and data drift apart the moment one is edited without the other, and a
// comparison table whose own summary is wrong is worse than no table — this is
// the document that invites people to check our claims.

// featureMatrixColumns is the matrix's shape: Category, Feature, the seven
// products, Notes, Source. encoding/csv already rejects a row whose field count
// differs from the header's, so this guards the header itself from drifting —
// which is what would silently move the column a test reads by index.
const featureMatrixColumns = 11

func featureMatrixRows(t *testing.T) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "docs", "feature-matrix.csv"))
	if err != nil {
		t.Fatalf("open feature-matrix.csv: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse feature-matrix.csv: %v", err)
	}
	if len(rows) < 2 {
		t.Fatal("the matrix has no rows")
	}
	if len(rows[0]) != featureMatrixColumns {
		t.Fatalf("the matrix header has %d columns, want %d: %v", len(rows[0]), featureMatrixColumns, rows[0])
	}
	return rows[1:]
}

func featureMatrixProse(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "feature-matrix.md"))
	if err != nil {
		t.Fatalf("read feature-matrix.md: %v", err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func TestFeatureMatrixProseMatchesTheData(t *testing.T) {
	rows := featureMatrixRows(t)
	prose := featureMatrixProse(t)

	var graphYes, brainYes, viaGraph, neither int
	for _, row := range rows {
		if len(row) != featureMatrixColumns {
			t.Fatalf("row has %d columns, want %d: %v", len(row), featureMatrixColumns, row)
		}
		if row[2] == "Yes" {
			graphYes++
		}
		switch row[3] {
		case "Yes":
			brainYes++
		case "Via Graph":
			viaGraph++
		}
		if row[2] != "Yes" && row[3] != "Yes" && row[3] != "Via Graph" {
			neither++
		}
	}

	for what, want := range map[string]int{
		"capability count":            len(rows),
		"Graph Yes":                   graphYes,
		"Brain Yes":                   brainYes,
		"rows delegated to Graph":     viaGraph,
		"rows where neither says Yes": neither,
	} {
		if !strings.Contains(prose, strconv.Itoa(want)) {
			t.Fatalf("feature-matrix.md never states the %s (%d); the prose and the data have drifted", what, want)
		}
	}
}

// The label counts in the prose table are derived from the Notes column.
func TestFeatureMatrixLabelCountsAreCorrect(t *testing.T) {
	rows := featureMatrixRows(t)
	prose := featureMatrixProse(t)

	counts := map[string]int{}
	for _, row := range rows {
		for _, label := range []string{"REAL GAP", "TRADE-OFF", "PARTLY REAL", "THEY DO IT BETTER"} {
			if strings.HasPrefix(row[9], label) || strings.Contains(row[9], "— "+label) {
				counts[label]++
				break
			}
		}
	}
	// Every gap the campaign closed is built, so none may remain.
	if counts["REAL GAP"] != 0 {
		t.Fatalf("%d row(s) still labelled REAL GAP; every one was meant to be closed or re-scored", counts["REAL GAP"])
	}
	for _, label := range []string{"TRADE-OFF", "PARTLY REAL", "THEY DO IT BETTER"} {
		row := regexp.MustCompile(`(?m)^\| \*\*` + regexp.QuoteMeta(label) + `\*\* \|[^|]*\| ([0-9]+)`)
		m := row.FindStringSubmatch(prose)
		if m == nil {
			t.Fatalf("feature-matrix.md has no count for %s", label)
		}
		stated, _ := strconv.Atoi(m[1])
		if stated != counts[label] {
			t.Fatalf("feature-matrix.md says %d %s row(s); the CSV has %d", stated, label, counts[label])
		}
	}
}

// A row that changed has to say what it is now, not only that it used to be a
// gap. "Was REAL GAP" with nothing after it is a changelog entry, not a claim
// somebody can evaluate.
func TestClosedGapRowsSayWhatShipped(t *testing.T) {
	for _, row := range featureMatrixRows(t) {
		if !strings.HasPrefix(row[9], "Was REAL GAP") {
			continue
		}
		if len(strings.Fields(row[9])) < 12 {
			t.Fatalf("row %q says it was a gap and little else: %q", row[1], row[9])
		}
		if row[2] == "No" && row[3] == "No" {
			t.Fatalf("row %q is marked as closed but both our columns still say No", row[1])
		}
	}
}

// The prose claims a count of gap cells that "moved to Yes" and then enumerates
// them. Nothing checked that claim against the data, and it was wrong: a cell
// that moved to Partial was listed among the ones that moved to Yes, and the
// stated total counted it. A number stated in prose beside a list is the kind of
// claim that rots first, so it is derived here rather than trusted.
func TestFeatureMatrixMovedToYesCountMatchesTheData(t *testing.T) {
	rows := featureMatrixRows(t)
	prose := featureMatrixProse(t)

	movedToYes, wasGap := 0, 0
	for _, row := range rows {
		if !strings.Contains(row[9], "Was REAL GAP") {
			continue
		}
		wasGap++
		// Either product closing the gap counts: edge provenance was closed in
		// Graph, everything else in Brain.
		if row[2] == "Yes" || row[3] == "Yes" {
			movedToYes++
		}
	}
	if wasGap == 0 {
		t.Fatal("no rows are labelled Was REAL GAP; this test would pass vacuously")
	}

	m := regexp.MustCompile(`(?i)\b([A-Za-z]+) of those cells moved\s+to Yes`).FindStringSubmatch(prose)
	if m == nil {
		t.Fatal("feature-matrix.md no longer states how many cells moved to Yes")
	}
	words := map[string]int{
		"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6,
		"seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12,
	}
	stated, ok := words[strings.ToLower(m[1])]
	if !ok {
		t.Fatalf("cannot read the stated count %q", m[1])
	}
	if stated != movedToYes {
		t.Fatalf("feature-matrix.md says %d cell(s) moved to Yes; the CSV has %d", stated, movedToYes)
	}

	// And a cell that did not reach Yes must not be sitting inside that list.
	sentence := regexp.MustCompile(`(?s)of those cells moved\s+to Yes:(.*?)\.\s`).FindStringSubmatch(prose)
	if sentence == nil {
		t.Fatal("the moved-to-Yes sentence no longer enumerates the cells")
	}
	if offender := partialGapNamedInYesList(rows, sentence[1]); offender != "" {
		t.Fatalf("%q is Partial but appears in the moved-to-Yes list: %q", offender, sentence[1])
	}
}

// partialGapNamedInYesList returns the first former-gap feature that reached
// only Partial yet is named inside the moved-to-Yes enumeration, or "".
//
// Either product column counts: edge provenance was closed in Graph, so a gap
// that lands on Partial there is just as wrongly placed in that list as one
// that lands on Partial in Brain.
func partialGapNamedInYesList(rows [][]string, sentence string) string {
	lower := strings.ToLower(sentence)
	for _, row := range rows {
		if !strings.Contains(row[9], "Was REAL GAP") {
			continue
		}
		if row[2] != "Partial" && row[3] != "Partial" {
			continue
		}
		for _, word := range strings.Fields(strings.ToLower(row[1])) {
			if len(word) < 8 {
				continue
			}
			if strings.Contains(lower, strings.Trim(word, "(),")) {
				return row[1]
			}
		}
	}
	return ""
}

// The real CSV has no Graph-Partial gap row, so widening the check to that
// column would otherwise be a branch no data exercises.
func TestPartialGapDetectionCoversBothProductColumns(t *testing.T) {
	const sentence = " document ingest, edge provenance, and mem0 import."
	for _, tc := range []struct {
		name  string
		graph string
		brain string
		want  string
	}{
		{"partial in Brain", "No", "Partial", "Edge provenance (extracted vs inferred)"},
		{"partial in Graph", "Partial", "No", "Edge provenance (extracted vs inferred)"},
		{"yes in both", "Yes", "Yes", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := [][]string{{
				"Retrieval", "Edge provenance (extracted vs inferred)", tc.graph, tc.brain,
				"No", "No", "No", "No", "No", "Was REAL GAP. Closed.", "src",
			}}
			if got := partialGapDetectionFixture(rows, sentence); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// A Partial row that is NOT named in the sentence must not be reported.
	rows := [][]string{{
		"Retrieval", "Scales to 100M-line codebases", "Partial", "No",
		"No", "No", "No", "No", "No", "Was REAL GAP. Measured loss.", "src",
	}}
	if got := partialGapDetectionFixture(rows, sentence); got != "" {
		t.Fatalf("a Partial row absent from the list was reported: %q", got)
	}
}

func partialGapDetectionFixture(rows [][]string, sentence string) string {
	return partialGapNamedInYesList(rows, sentence)
}
