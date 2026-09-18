package cli

import "testing"

func TestFactsBM25Toggle(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
	if factsBM25Enabled() {
		t.Fatal("facts BM25 must be off by default")
	}
	for _, on := range []string{"1", "true", "yes", "ON"} {
		t.Setenv("ENTIRE_BRAIN_FACTS_BM25", on)
		if !factsBM25Enabled() {
			t.Fatalf("facts BM25 should be enabled for %q", on)
		}
	}
}

func TestFactsFTSScores(t *testing.T) {
	facts := []factRecord{
		{ID: "f1", Text: "Use tabs not spaces in Go source", Paths: []string{"preferences.coding.style"}},
		{ID: "f2", Text: "Reconcile supersedes a fact when a new claim contradicts an active one", Paths: []string{"architecture.facts"}},
	}

	scores, ok := factsFTSScores(facts, "how does reconcile supersede a fact")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if _, hit := scores["f2"]; !hit {
		t.Fatalf("expected f2 to match a reconcile query, got %v", scores)
	}
	if _, hit := scores["f1"]; hit {
		t.Fatalf("f1 (coding style) should not match a reconcile query, got %v", scores)
	}

	// Absent terms: searchable query, no matches.
	if scores, ok := factsFTSScores(facts, "xylophone zebra quokka"); !ok || len(scores) != 0 {
		t.Fatalf("absent-term query = scores:%v ok:%t, want searchable empty result", scores, ok)
	}
}
