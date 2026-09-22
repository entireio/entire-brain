package cli

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRetrievalRecencyPromotesCandidateBelowCutoff(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	facts := []factRecord{
		{ID: "fact:old", Text: "alpha beta retrieval", Status: factStatusActive, UpdatedAt: now.Add(-3 * 365 * 24 * time.Hour)},
		{ID: "fact:fresh", Text: "alpha retrieval", Status: factStatusActive, UpdatedAt: now},
	}
	if err := writeFacts(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		opts := retrievalOptions{Source: retrievalSourceFact, Recency: enabled, Now: now}
		full, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha beta", 2, modeLexical, opts)
		if err != nil {
			t.Fatal(err)
		}
		want := "fact:old"
		if enabled {
			want = "fact:fresh"
		}
		if len(full) != 2 || full[0].ID != want {
			t.Fatalf("recency=%v: unexpected full ranking: %+v", enabled, full)
		}
		page, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha beta", 1, modeLexical, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 || page[0].ID != want || page[0].Score != full[0].Score {
			t.Fatalf("recency=%v: limit=1 must return the top weighted candidate %s, got %+v", enabled, want, page)
		}
	}
}

func TestRetrievalRecencyReportsUndatedResults(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		enabled    bool
		dated      bool
		empty      bool
		wantCaveat bool
	}{
		{name: "undated", enabled: true, wantCaveat: true},
		{name: "disabled"},
		{name: "mixed", enabled: true, dated: true},
		{name: "empty", enabled: true, empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			facts := []factRecord{
				{ID: "fact:a", Text: "alpha retrieval", Status: factStatusActive},
				{ID: "fact:b", Text: "alpha retrieval", Status: factStatusActive},
			}
			if tc.dated {
				facts[0].UpdatedAt = now
			}
			if tc.empty {
				facts = nil
			}
			if err := writeFacts(brainDir, "main", facts); err != nil {
				t.Fatal(err)
			}
			results, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha", 10, modeLexical, retrievalOptions{Source: retrievalSourceFact, Recency: tc.enabled, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != len(facts) {
				t.Fatalf("got %d results, want %d", len(results), len(facts))
			}
			for _, result := range results {
				found := false
				for _, caveat := range result.Caveats {
					if caveat.Kind == "recency_unavailable" {
						found = true
					}
				}
				if found != tc.wantCaveat {
					t.Fatalf("unexpected caveats: %+v", result.Caveats)
				}
			}
			if tc.wantCaveat {
				baseline, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha", 10, modeLexical, retrievalOptions{Source: retrievalSourceFact})
				if err != nil {
					t.Fatal(err)
				}
				for i := range results {
					if results[i].ID != baseline[i].ID || results[i].Score != baseline[i].Score {
						t.Fatal("undated scores/order changed")
					}
				}
				encoded, err := json.Marshal(compactUnifiedResults(results, "alpha"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(encoded, []byte("recency_unavailable")) {
					t.Fatalf("JSON lost caveat: %s", encoded)
				}
				var out bytes.Buffer
				printRetrievalCaveats(&out, results[0])
				if !strings.Contains(out.String(), "none of the returned results has a usable timestamp") {
					t.Fatalf("text lost caveat: %s", out.String())
				}
			}
		})
	}
}

// A brain accumulates, so relevance alone keeps surfacing the oldest matching
// answer forever. These tests pin the three properties that make time-weighting
// safe to add to ranking: it only ever reorders, it never invents an age it does
// not have, and it costs nothing to compute.

func TestRecencyMultiplierHalvesTowardTheFloor(t *testing.T) {
	half := 90 * 24 * time.Hour
	cases := []struct {
		name string
		age  time.Duration
		want float64
	}{
		{"brand new", 0, recencyCeiling},
		{"one half-life", half, recencyFloor + (recencyCeiling-recencyFloor)/2},
		{"two half-lives", 2 * half, recencyFloor + (recencyCeiling-recencyFloor)/4},
		{"ten half-lives", 10 * half, recencyFloor + (recencyCeiling-recencyFloor)/1024},
	}
	for _, tc := range cases {
		got := recencyMultiplier(tc.age, half)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("%s: multiplier = %v, want %v", tc.name, got, tc.want)
		}
		if got < recencyFloor || got > recencyCeiling {
			t.Fatalf("%s: multiplier %v escaped [%v, %v]", tc.name, got, recencyFloor, recencyCeiling)
		}
	}
}

func TestRecencyNeverInventsAnAgeItDoesNotHave(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	half := 90 * 24 * time.Hour

	// Classic history records carry no timestamp. Treating "undated" as "old"
	// would silently demote a whole corpus for a property it never claimed.
	for _, undated := range []string{"", "not-a-timestamp", "2026/09/21"} {
		got, dated := recencyMultiplierFor(undated, now, half)
		if dated {
			t.Fatalf("%q should not count as dated", undated)
		}
		if got != recencyNeutralMultiplier {
			t.Fatalf("%q: multiplier = %v, want the neutral %v", undated, got, recencyNeutralMultiplier)
		}
	}

	// A future timestamp is a broken clock, not a very fresh record, and must
	// not mint a multiplier above the ceiling.
	future := now.Add(365 * 24 * time.Hour).Format(time.RFC3339)
	got, dated := recencyMultiplierFor(future, now, half)
	if !dated {
		t.Fatal("a parseable future timestamp is still a timestamp")
	}
	if got > recencyCeiling {
		t.Fatalf("future timestamp produced %v, above the ceiling %v", got, recencyCeiling)
	}
}

func TestRecencyReordersButNeverDrops(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	half := 90 * 24 * time.Hour
	stamp := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	results := []unifiedResult{
		{ID: "fact:old", Score: 0.050, CreatedAt: stamp(3 * 365 * 24 * time.Hour)},
		{ID: "fact:fresh", Score: 0.049, CreatedAt: stamp(24 * time.Hour)},
		{ID: "history:undated", Score: 0.048},
	}
	dated := applyRecency(results, now, half)

	if len(results) != 3 {
		t.Fatalf("recency dropped results: %d of 3 left", len(results))
	}
	if dated != 2 {
		t.Fatalf("dated = %d, want 2 (the undated record must not be counted)", dated)
	}
	if results[0].ID != "fact:fresh" {
		t.Fatalf("a day-old record scoring 0.049 should outrank a three-year-old scoring 0.050; got %q first", results[0].ID)
	}
	for _, r := range results {
		if r.Score <= 0 {
			t.Fatalf("%s: score %v — weighting must not zero a result out", r.ID, r.Score)
		}
	}
}

func TestRecencyCannotOverwhelmRelevance(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	half := 90 * 24 * time.Hour

	// The spread is ceiling/floor = 5x. A match five times stronger must still
	// win however old it is, or "bias" has quietly become "filter".
	results := []unifiedResult{
		{ID: "fact:ancient-but-exact", Score: 1.0, CreatedAt: now.Add(-50 * 365 * 24 * time.Hour).Format(time.RFC3339)},
		{ID: "fact:fresh-but-weak", Score: 1.0 / (recencyCeiling / recencyFloor) * 0.99, CreatedAt: now.Format(time.RFC3339)},
	}
	applyRecency(results, now, half)
	if results[0].ID != "fact:ancient-but-exact" {
		t.Fatalf("recency overwhelmed relevance: %q ranked first, scores %v vs %v",
			results[0].ID, results[0].Score, results[1].Score)
	}
}

func TestRecencyIsDeterministicAndFree(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	half := 90 * 24 * time.Hour
	at := now.Add(-45 * 24 * time.Hour).Format(time.RFC3339)

	first, _ := recencyMultiplierFor(at, now, half)
	for i := 0; i < 100; i++ {
		again, _ := recencyMultiplierFor(at, now, half)
		if again != first {
			t.Fatalf("multiplier drifted between calls: %v then %v", first, again)
		}
	}
	// A half-life of zero or less disables weighting rather than dividing by it.
	if got := recencyMultiplier(time.Hour, 0); got != recencyNeutralMultiplier {
		t.Fatalf("zero half-life should be neutral, got %v", got)
	}
}

// The unit tests above exercise the arithmetic. This one exercises the wiring:
// that retrievalOptions.Recency actually reaches the fusion point, and that the
// default really is off — because a ranking change nobody opted into is a
// regression, however good the ranking is.
func TestRetrievalRecencyIsOptInAndReachesRanking(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }

	fused := func() []unifiedResult {
		return []unifiedResult{
			{ID: "fact:old", Score: 0.050, CreatedAt: stamp(4 * 365 * 24 * time.Hour)},
			{ID: "fact:fresh", Score: 0.049, CreatedAt: stamp(12 * time.Hour)},
		}
	}

	// Off: pure relevance order, untouched.
	off := fused()
	sortUnifiedByScore(off)
	if off[0].ID != "fact:old" {
		t.Fatalf("without recency the higher raw score must lead; got %q", off[0].ID)
	}

	// On: the fresher record takes the lead on a near-tie.
	on := fused()
	applyRecency(on, now, defaultRecencyHalfLife)
	if on[0].ID != "fact:fresh" {
		t.Fatalf("with recency the day-old record should lead; got %q", on[0].ID)
	}

	// And the default in retrievalOptions is the zero value, so every existing
	// caller keeps the ordering it has today.
	var defaults retrievalOptions
	if defaults.Recency {
		t.Fatal("recency must default to off")
	}
	if defaults.RecencyHalfLife != 0 {
		t.Fatalf("half-life must default to zero so the package default applies, got %v", defaults.RecencyHalfLife)
	}
}
