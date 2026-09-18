package cli

import (
	"reflect"
	"testing"
)

func TestThemeRankingDistinctRelevanceStrengthStableTiesAndCap(t *testing.T) {
	themes := []themeView{
		{ID: "repeat", Title: "Cache cache CACHE", Strength: 100},
		{ID: "tie-first", Title: "Cache", Description: "invalidation", Strength: 2},
		{ID: "weak", Title: "Cache invalidation", Strength: 1},
		{ID: "tie-second", Title: "Cache invalidation", Strength: 2},
		{ID: "noise", Title: "Database schema", Strength: 1000},
	}
	original := append([]themeView(nil), themes...)
	ids := func(views []themeView) []string {
		var result []string
		for _, v := range views {
			result = append(result, v.ID)
		}
		return result
	}
	terms := []string{"cache", "invalidation", "cache"}
	want := []string{"tie-first", "tie-second", "weak", "repeat"}
	if got := ids(rankTaskRelevantThemes(themes, terms, 10)); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranked IDs = %v, want %v", got, want)
	}
	if got := ids(rankTaskRelevantThemes(themes, terms, 2)); !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("limited IDs = %v, want %v", got, want[:2])
	}
	if !reflect.DeepEqual(themes, original) {
		t.Fatal("ranking mutated input themes")
	}
}
