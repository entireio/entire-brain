package cli

import (
	"reflect"
	"testing"
	"time"
)

func TestSelectRetrievalArm(t *testing.T) {
	for name, want := range map[string]retrievalArm{
		"": flatArm, "flat": flatArm, "scoped": scopedArm,
		"scoped-floor": scopedFloorArm, "outline": outlineArm,
	} {
		got, err := selectRetrievalArm(name)
		if err != nil {
			t.Errorf("selectRetrievalArm(%q) errored: %v", name, err)
		} else if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("selectRetrievalArm(%q) selected the wrong implementation", name)
		}
	}
	if _, err := selectRetrievalArm("bogus"); err == nil {
		t.Error("expected an error for an unknown arm")
	}
}

func TestScopedFloorRecoversToK(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		// One locus match for "ValidateToken"; the rest are relevant prose with no locus.
		{ID: "loc", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"validatetoken"}, Text: "`ValidateToken` checks expiry.", UpdatedAt: now},
		{ID: "p1", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Text: "token expiry is handled centrally.", UpdatedAt: now},
		{ID: "p2", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Text: "token expiry uses a grace window.", UpdatedAt: now},
	}
	// Hard scoped narrows to the single locus match.
	hard := scopedArm(facts, "ValidateToken expiry token", 5, nil)
	if len(hard) != 1 || hard[0].ID != "loc" {
		t.Fatalf("hard scoped should return only the locus match, got %+v", hard)
	}
	// scoped-floor keeps the locus match first, then backfills flat-ranked facts.
	floor := scopedFloorArm(facts, "ValidateToken expiry token", 5, nil)
	if len(floor) <= 1 {
		t.Fatalf("scoped-floor should backfill beyond the single locus match, got %+v", floor)
	}
	if floor[0].ID != "loc" {
		t.Fatalf("scoped-floor should keep the locus match on top, got %+v", floor)
	}
	// No duplicates.
	seen := map[string]bool{}
	for _, f := range floor {
		if seen[f.ID] {
			t.Fatalf("scoped-floor returned a duplicate: %s", f.ID)
		}
		seen[f.ID] = true
	}
}

func TestScopedArmNarrowsThenFallsBack(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "a", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"validatetoken"}, Text: "`ValidateToken` returns an error on expiry.", UpdatedAt: now},
		{ID: "b", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Text: "Caching is best-effort.", UpdatedAt: now},
		{ID: "c", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Text: "The store is SQLite.", UpdatedAt: now},
	}
	// A code-locus query narrows to the locus-matching fact only.
	got := scopedArm(facts, "ValidateToken expiry", 10, nil)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("scoped arm should narrow to the locus match, got %+v", got)
	}
	// A conceptual query (no code locus) falls back to flat — does not starve to 0.
	got = scopedArm(facts, "how does caching work", 10, nil)
	if len(got) == 0 {
		t.Fatalf("conceptual query should fall back to flat, got nothing")
	}
}

func TestOutlineArmRanksWithinSubtree(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "cli1", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/facts.go"}, Text: "factRecord stores a durable fact in internal/cli/facts.go.", UpdatedAt: now},
		{ID: "cli2", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/distill.go"}, Text: "Distill parses agent output in internal/cli/distill.go.", UpdatedAt: now},
		{ID: "db1", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/db/store.go"}, Text: "The store is SQLite in internal/db/store.go.", UpdatedAt: now},
	}
	// A query about internal/cli content should keep the result within that
	// subtree (db1 lives under internal/db and is excluded).
	got := outlineArm(facts, "internal/cli/facts.go factRecord", 10, nil)
	if len(got) == 0 {
		t.Fatalf("outline arm returned nothing")
	}
	for _, f := range got {
		if f.ID == "db1" {
			t.Fatalf("outline arm should not surface a fact from a different subtree: %+v", got)
		}
	}
}
