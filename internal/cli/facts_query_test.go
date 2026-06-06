package cli

import (
	"testing"
	"time"
)

func TestRankFactsScoringAndFilter(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "fact:1", Paths: []string{"project.tooling.stack"}, Text: "The project uses MySQL for storage.", Status: factStatusActive, UpdatedAt: now},
		{ID: "fact:2", Paths: []string{"workflow.testing.rules"}, Text: "Run go test before pushing.", Status: factStatusActive, UpdatedAt: now.Add(time.Hour)},
		{ID: "fact:3", Paths: []string{"project.tooling.stack"}, Text: "MySQL replaced Supabase.", Status: factStatusSuperseded, UpdatedAt: now},
	}

	// Query matches the two MySQL facts; superseded excluded by default.
	got := rankFacts(facts, "mysql storage", 10, false)
	if len(got) != 1 || got[0].ID != "fact:1" {
		t.Fatalf("expected only the active MySQL fact, got %+v", got)
	}

	// includeAll surfaces the superseded one too.
	got = rankFacts(facts, "mysql", 10, true)
	if len(got) != 2 {
		t.Fatalf("includeAll should surface superseded match, got %d", len(got))
	}

	// A path term boosts score: "testing" should rank fact:2 first.
	got = rankFacts(facts, "testing", 10, false)
	if len(got) == 0 || got[0].ID != "fact:2" {
		t.Fatalf("path-term query should match fact:2, got %+v", got)
	}
}

func TestRankFactsEmptyQueryReturnsRecent(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "old", Status: factStatusActive, UpdatedAt: now},
		{ID: "new", Status: factStatusActive, UpdatedAt: now.Add(time.Hour)},
	}
	got := rankFacts(facts, "", 10, false)
	if len(got) != 2 || got[0].ID != "new" {
		t.Fatalf("empty query should return most-recent-first, got %+v", got)
	}
}

func TestRankFactsLimit(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	var facts []factRecord
	for i := 0; i < 20; i++ {
		facts = append(facts, factRecord{ID: string(rune('a' + i)), Text: "shared term here", Status: factStatusActive, UpdatedAt: now})
	}
	if got := rankFacts(facts, "shared", 5, false); len(got) != 5 {
		t.Fatalf("limit not applied: got %d", len(got))
	}
}
