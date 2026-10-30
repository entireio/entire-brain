package facts

import (
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

func TestFilterFactsByTemporalEligibilityFailsClosedAndCountsReasons(t *testing.T) {
	anchor := func(ids ...string) []factmerge.Anchor {
		out := make([]factmerge.Anchor, 0, len(ids))
		for _, id := range ids {
			out = append(out, factmerge.Anchor{SessionID: id})
		}
		return out
	}
	facts := []factmerge.Record{
		{ID: "eligible-active", Status: factmerge.StatusActive, Provenance: anchor("early", "offset-early")},
		{ID: "eligible-superseded", Status: factmerge.StatusSuperseded, Provenance: anchor("early")},
		{ID: "empty"},
		{ID: "missing-anchor-id", Provenance: anchor("")},
		// Exclusion reason precedence is stable even when an unknown anchor appears first.
		{ID: "excluded", Provenance: anchor("not-in-map", "own-fix")},
		{ID: "unknown", Provenance: anchor("not-in-map")},
		{ID: "boundary", Provenance: anchor("at-cutoff")},
		{ID: "future", Provenance: anchor("late")},
	}
	dates := map[string]string{
		"early":        "2026-07-14T23:00:00Z",
		"offset-early": "2026-07-15T00:30:00+02:00",
		"own-fix":      "2026-07-14T20:00:00Z",
		"at-cutoff":    "2026-07-15T00:00:00Z",
		"late":         "2026-07-15T00:00:01Z",
	}
	got, audit, err := filterFactsByTemporalEligibility(facts, dates, "2026-07-15T00:00:00Z", []string{"own-fix"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "eligible-active" || got[1].ID != "eligible-superseded" {
		t.Fatalf("eligible facts = %+v", got)
	}
	if audit.PrefilterCorpusCount != 8 || audit.EligibleCount != 2 {
		t.Fatalf("audit counts = %+v", audit)
	}
	wantReasons := map[string]int{
		eligibilityEmptyProvenance: 2,
		eligibilityExcludedSession: 1,
		eligibilityUnknownSession:  1,
		EligibilityAtOrAfterCutoff: 2,
	}
	for reason, want := range wantReasons {
		if audit.ExcludedCounts[reason] != want {
			t.Errorf("excluded %s = %d, want %d", reason, audit.ExcludedCounts[reason], want)
		}
	}
}
