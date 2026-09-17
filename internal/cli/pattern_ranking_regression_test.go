package cli

import (
	"slices"
	"testing"
)

func TestRankTaskRelevantPatternsRanksRelevanceBeforeStrength(t *testing.T) {
	views := []patternView{
		{ID: "same-overlap-weaker", Title: "Deploy service hotfix", Strength: 0.75},
		{ID: "one", Title: "Deploy service", Strength: 0.99},
		{ID: "two", Title: "Deploy service rollback", Strength: 0.10},
		{ID: "kind", Title: "Release notes", Kind: "deploy procedure", Strength: 0.20},
		{ID: "noise", Title: "Database migration", Strength: 1.00},
	}

	got := rankTaskRelevantPatterns(views, []string{"deploy", "service", "rollback"}, 10)
	if ids := patternViewIDs(got); !slices.Equal(ids, []string{"two", "one", "same-overlap-weaker", "kind"}) {
		t.Fatalf("rankTaskRelevantPatterns IDs = %v, want [two one same-overlap-weaker kind]", ids)
	}
}

func TestRankTaskRelevantPatternsCountsRepeatedWordsOnceAndKeepsStableTies(t *testing.T) {
	views := []patternView{
		{ID: "first", Title: "Deploy deploy", Strength: 0.50},
		{ID: "second", Title: "Deploy service", Strength: 0.50},
		{ID: "third", Title: "Deploy service", Strength: 0.50},
	}

	got := rankTaskRelevantPatterns(views, []string{"deploy", "service"}, 10)
	if ids := patternViewIDs(got); !slices.Equal(ids, []string{"second", "third", "first"}) {
		t.Fatalf("rankTaskRelevantPatterns IDs = %v, want [second third first]", ids)
	}
}

func TestRankTaskRelevantPatternsDropsNonOverlappingPatternsAndHonorsLimit(t *testing.T) {
	views := []patternView{
		{ID: "first", Title: "Deploy service rollback", Strength: 0.10},
		{ID: "second", Title: "Deploy service", Strength: 0.90},
		{ID: "third", Title: "Deploy", Strength: 0.80},
		{ID: "noise", Title: "Database migration", Strength: 1.00},
	}

	got := rankTaskRelevantPatterns(views, []string{"deploy", "service", "rollback"}, 2)
	if ids := patternViewIDs(got); !slices.Equal(ids, []string{"first", "second"}) {
		t.Fatalf("rankTaskRelevantPatterns IDs = %v, want [first second]", ids)
	}
	if got := rankTaskRelevantPatterns(views, []string{"unmatched"}, 2); len(got) != 0 {
		t.Fatalf("rankTaskRelevantPatterns with no matches = %#v, want empty", got)
	}
	if got := rankTaskRelevantPatterns(nil, []string{"deploy"}, 2); len(got) != 0 {
		t.Fatalf("rankTaskRelevantPatterns with nil views = %#v, want empty", got)
	}
}

func TestRankTaskRelevantPatternsReturnsNilForEmptyInputs(t *testing.T) {
	views := []patternView{{ID: "deploy", Title: "Deploy service", Strength: 0.50}}

	for name, tc := range map[string]struct {
		terms []string
		limit int
	}{
		"empty terms":    {terms: nil, limit: 5},
		"zero limit":     {terms: []string{"deploy"}, limit: 0},
		"negative limit": {terms: []string{"deploy"}, limit: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := rankTaskRelevantPatterns(views, tc.terms, tc.limit); got != nil {
				t.Fatalf("rankTaskRelevantPatterns = %#v, want nil", got)
			}
		})
	}
}

func patternViewIDs(views []patternView) []string {
	ids := make([]string, len(views))
	for i, view := range views {
		ids[i] = view.ID
	}
	return ids
}
