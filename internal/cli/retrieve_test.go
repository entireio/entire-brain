package cli

import "testing"

func TestUnifiedIDsNotDoublePrefixed(t *testing.T) {
	// fact/history ids are already source-prefixed; docs are not.
	f := factsToUnified([]factRecord{{ID: "fact:abc", Text: "x"}})
	if f[0].ID != "fact:abc" {
		t.Fatalf("fact id double-prefixed: %s", f[0].ID)
	}
	h := historyToUnified([]scoredHistoryRecord{{Record: historyRecord{ID: "history:def", Summary: "y"}}})
	if h[0].ID != "history:def" {
		t.Fatalf("history id double-prefixed: %s", h[0].ID)
	}
	if d := docToUnified(docRecord{ID: "ghi", Text: "z"}); d.ID != "doc:ghi" {
		t.Fatalf("doc id: %s", d.ID)
	}
}

func TestRRFMergeUnifiedFavorsCrossListMatches(t *testing.T) {
	lists := [][]unifiedResult{
		{{ID: "a"}, {ID: "b"}},
		{{ID: "b"}, {ID: "c"}},
	}
	out := rrfMergeUnified(lists, 10)
	if len(out) != 3 || out[0].ID != "b" {
		t.Fatalf("expected b (in both lists) ranked first, got %v", out)
	}
}
