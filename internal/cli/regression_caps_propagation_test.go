package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The uncapped detectRegressionAnomalies wrapper discarded regressionScanCaps,
// so every caller except runRegressionDetect lost the "history scan stopped
// early" signal. The workspace regressions/review surfaces could then print an
// unqualified clean bill of health over a raw-history scan that hit a cap and
// never finished -- the exact bug class the caps were added to eliminate,
// simply not propagated. (`brain review`'s summary is covered by
// TestReviewSummaryQualifiesATruncatedScan in command/brainreview.)
func TestWorkspaceReviewSummaryQualifiesATruncatedScan(t *testing.T) {
	t.Parallel()

	clean := workspaceReviewSummary(true, 3, false)
	if !strings.Contains(clean, "no suspected regressions") {
		t.Fatalf("an untruncated clean scan must still read clean: %q", clean)
	}
	partial := workspaceReviewSummary(true, 3, true)
	if strings.Contains(partial, "no suspected regressions") {
		t.Errorf("a truncated scan must not read as a clean bill of health: %q", partial)
	}
	for _, want := range []string{"PARTIAL", "stopped at a cap"} {
		if !strings.Contains(partial, want) {
			t.Errorf("the partial summary must contain %q: %q", want, partial)
		}
	}
	// Nothing compared stays INCONCLUSIVE even when truncated.
	if got := workspaceReviewSummary(true, 0, true); !strings.Contains(got, "INCONCLUSIVE") {
		t.Errorf("a scan that compared nothing is inconclusive: %q", got)
	}
}

// NON-VACUITY / structural: the uncapped wrapper is gone, so no future caller
// can silently discard the caps again. A test asserting the summaries handle a
// flag means nothing if the production path never passes a true one.
func TestNoUncappedRegressionDetectorRemains(t *testing.T) {
	t.Parallel()

	for _, file := range []string{"regression.go", "workspace.go"} {
		src := readGoSourceForTest(t, file)
		// The capped detector must be what production calls.
		if !strings.Contains(src, "detectRegressionAnomaliesCapped(") {
			t.Errorf("%s does not call the capped detector", file)
		}
		// And the uncapped wrapper must not exist to be called.
		if strings.Contains(src, "func detectRegressionAnomalies(") {
			t.Errorf("%s still defines the uncapped wrapper, which is how the caps got discarded", file)
		}
	}
	// The review command moved to command/brainreview (COR-2268); its wiring
	// must still hand it the capped detector, not an uncapped wrapper.
	cfgSrc := readGoSourceForTest(t, "brainreview_config.go")
	if !strings.Contains(cfgSrc, "detectRegressionAnomaliesCapped,") {
		t.Error("brain review is not wired to the capped detector")
	}
	// Both summaries must receive a real flag, not a literal false.
	src := readGoSourceForTest(t, filepath.Join("command", "brainreview", "brainreview.go"))
	if !strings.Contains(src, "ReviewSummary(len(findings), scanned, caps.Truncated())") {
		t.Error("brain review does not pass the truncation flag into its summary")
	}
	ws := readGoSourceForTest(t, "workspace.go")
	if !strings.Contains(ws, "workspaceReviewSummary(result.Checked, result.FilesScanned, result.HistoryTruncated)") {
		t.Error("workspace review does not pass the truncation flag into its summary")
	}
	if !strings.Contains(ws, "result.HistoryTruncated = truncated") {
		t.Error("the workspace results never record the truncation flag, so the summary always sees false")
	}
}

// readGoSourceForTest reads a file in this package for a structural assertion,
// failing loudly when the file is missing or empty so the guard cannot pass
// vacuously.
func readGoSourceForTest(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty; this guard would be vacuous", name)
	}
	return string(data)
}

// Measuring the input in runes and the ellipsis in BYTES is the same
// rune-versus-byte confusion truncateStatusCause exists to fix, on the other
// operand. It agrees for ASCII "..." and over-reserves the moment the marker
// becomes "…" -- one const edit away, in a function whose whole comment is
// about this class of bug.
//
// Driven by temporarily treating a multi-byte marker the way the code would:
// the assertion is that the reserve is a RUNE count, so a 1-rune 3-byte marker
// reserves 1, not 3.
func TestCauseTruncationReservesTheEllipsisInRunes(t *testing.T) {
	t.Parallel()

	// A cause longer than the width must come back exactly statusCauseWidth
	// runes: limit + ellipsis, with the ellipsis charged at its rune width.
	long := strings.Repeat("a", statusCauseWidth*2)
	got := truncateStatusCause(long)
	if n := len([]rune(got)); n != statusCauseWidth {
		t.Errorf("truncated cause is %d runes, want exactly %d (the advertised width)", n, statusCauseWidth)
	}
	if !strings.HasSuffix(got, statusCauseEllipsis) {
		t.Errorf("a truncated cause must carry the marker: %q", got)
	}

	// Non-ASCII input is charged per RUNE, not per byte -- the original bug.
	wide := strings.Repeat("é", statusCauseWidth*2)
	if n := len([]rune(truncateStatusCause(wide))); n != statusCauseWidth {
		t.Errorf("multi-byte cause truncated to %d runes, want %d", n, statusCauseWidth)
	}

	// And a cause that fits is returned untouched, so the reserve is not
	// applied where it does not belong.
	short := strings.Repeat("é", 3)
	if got := truncateStatusCause(short); got != short {
		t.Errorf("a cause that fits must be unchanged: %q != %q", got, short)
	}

	// The structural half: the reserve must be a rune count in the source, or
	// the assertions above pass today and break on the first "…".
	src := readGoSourceForTest(t, "status_render.go")
	if strings.Contains(src, "statusCauseWidth - len(statusCauseEllipsis)") {
		t.Error("the ellipsis reserve is still a byte count, so a multi-byte marker would over-reserve")
	}
	if !strings.Contains(src, "utf8.RuneCountInString(statusCauseEllipsis)") {
		t.Error("the ellipsis reserve is no longer counted in runes")
	}
}
