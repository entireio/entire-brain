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
		if len(row) < 10 {
			t.Fatalf("short row: %v", row)
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
