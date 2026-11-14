package cli

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRankFactsPreservesDistinctRecordValues(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "first", Text: "checkpoint retry", Status: factStatusActive, Author: "alice", UpdatedAt: now, RelatedIDs: []string{"second"}},
		{ID: "second", Text: "checkpoint retry", Status: factStatusActive, Author: "bob", UpdatedAt: now},
		{ID: "third", Text: "checkpoint retry", Status: factStatusActive, Author: "carol", UpdatedAt: now},
	}
	before := slices.Clone(facts)
	got := rankFactsAt(facts, "checkpoint retry", 3, false, now)
	want := []factRecord{facts[0], facts[2], facts[1]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranking lost record values or family order: got %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(facts, before) {
		t.Fatal("ranking modified its input")
	}
	got[0].Author = "changed"
	if facts[0].Author != "alice" {
		t.Fatal("result record aliases the input record")
	}
}

func TestRankFactsRecentListingDoesNotReorderInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		includeAll bool
		want       []string
	}{
		{"active", false, []string{"recent", "old"}},
		{"all", true, []string{"inactive", "recent", "old"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
			facts := []factRecord{
				{ID: "old", Status: factStatusActive, UpdatedAt: now.Add(-time.Hour)},
				{ID: "inactive", Status: factStatusSuperseded, UpdatedAt: now.Add(time.Hour)},
				{ID: "recent", Status: factStatusActive, UpdatedAt: now},
			}
			before := slices.Clone(facts)
			got := rankFactsAt(facts, "", 10, tc.includeAll, now)
			if !reflect.DeepEqual(factIDs(got), tc.want) {
				t.Fatalf("recent listing: got %v, want %v", factIDs(got), tc.want)
			}
			if !reflect.DeepEqual(facts, before) {
				t.Fatal("recent listing reordered or modified its input")
			}
		})
	}
}
