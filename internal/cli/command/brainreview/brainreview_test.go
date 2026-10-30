package brainreview

import (
	"strings"
	"testing"
)

func TestBrainReviewMapsAnomalyToFinding(t *testing.T) {
	// Honest severity: nothing the heuristic detector emits is "high" — that tier is reserved for a
	// future high-confidence (locus+recency) signal. changed (0.8) is rename-ambiguous -> medium;
	// deleted (0.65) is noisier/opt-in -> low.
	if regressionSeverity(0.95) != "high" || regressionSeverity(0.8) != "medium" || regressionSeverity(0.65) != "low" {
		t.Fatalf("severity thresholds wrong: %s/%s/%s", regressionSeverity(0.95), regressionSeverity(0.8), regressionSeverity(0.65))
	}
	f := AnomalyToReviewFinding(Anomaly{
		File: "pkg/review_context.go", Line: 412, Kind: "changed", Identifier: "scopebaseref",
		Expected: `scopeBaseRef+"..HEAD"`, Current: `"master..HEAD"`, Confidence: 0.8, Evidence: "sessions/x.jsonl:1",
	})
	// A hedged "could be a rename" changed-finding must NOT surface as HIGH — the machine severity
	// must not contradict the prose hedge.
	if f.Severity != "medium" || f.File != "pkg/review_context.go" || f.Line != 412 {
		t.Fatalf("finding fields wrong: %+v", f)
	}
	if !strings.Contains(f.Title, "Suspected regression") || !strings.Contains(f.Detail, `scopeBaseRef+"..HEAD"`) || !strings.Contains(f.Detail, "master..HEAD") {
		t.Fatalf("finding title/detail wrong: %+v", f)
	}

	// Location-only branch: Expected/Current blanked → generic detail, no expected/current leak.
	loc := AnomalyToReviewFinding(Anomaly{
		File: "pkg/review_context.go", Line: 412, Kind: "changed", Identifier: "scopebaseref",
		Expected: "", Current: "", Confidence: 0.8, Evidence: "sessions/x.jsonl:1",
	})
	if !strings.Contains(loc.Detail, "verify against the brain's history") || strings.Contains(loc.Detail, "history shows") {
		t.Fatalf("location-only finding must use the generic detail (no expected/current), got %q", loc.Detail)
	}
}

// The uncapped detectRegressionAnomalies wrapper discarded ScanCaps,
// so every caller except runRegressionDetect lost the "history scan stopped
// early" signal. `brain review` and the workspace regressions/review surfaces
// could then print an unqualified clean bill of health over a raw-history scan
// that hit a cap and never finished -- the exact bug class the caps were added
// to eliminate, simply not propagated.
//
// regressionNoAnomaliesLine had said this correctly since the caps existed.
// These two summaries could not, because they never received the flag.
func TestReviewSummaryQualifiesATruncatedScan(t *testing.T) {
	t.Parallel()

	clean := ReviewSummary(0, 7, false)
	if !strings.Contains(clean, "no suspected regressions") {
		t.Fatalf("an untruncated clean scan must still read clean: %q", clean)
	}
	partial := ReviewSummary(0, 7, true)
	if strings.Contains(partial, "no suspected regressions") {
		t.Errorf("a truncated scan must not read as a clean bill of health: %q", partial)
	}
	for _, want := range []string{"PARTIAL", "stopped at a cap", "not a clean result"} {
		if !strings.Contains(partial, want) {
			t.Errorf("the partial summary must contain %q: %q", want, partial)
		}
	}
	// Findings outrank the qualifier: there is nothing clean to qualify.
	if got := ReviewSummary(2, 7, true); !strings.Contains(got, "suspected regression(s)") {
		t.Errorf("findings must still be reported first: %q", got)
	}
	// Nothing compared stays INCONCLUSIVE, which is stronger than PARTIAL.
	if got := ReviewSummary(0, 0, true); !strings.Contains(got, "INCONCLUSIVE") {
		t.Errorf("a scan that compared nothing is inconclusive, not partial: %q", got)
	}
}
