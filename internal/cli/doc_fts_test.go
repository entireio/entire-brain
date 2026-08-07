package cli

import (
	"testing"
	"time"
)

func docTestIndex() docIndex {
	return docIndex{
		GeneratedAt: time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
		Records: []docRecord{
			{ID: "d1", Path: "seed/architecture.md", Heading: "Architecture", Line: 1,
				Text: "The brain has multiple sources: seed owns baseline knowledge, export owns session history."},
			{ID: "d2", Path: "seed/docs/feed.md", Heading: "Feed crawler", Line: 1,
				Text: "The feed crawler streams episodes with cursor-based backfill paging."},
		},
	}
}

func TestDocFTSRetrieves(t *testing.T) {
	brainDir := t.TempDir()
	index := docTestIndex()

	scored, ok := rankDocsViaFTS(brainDir, index, "how does the feed crawler backfill", 25)
	if !ok || len(scored) == 0 || scored[0].Record.ID != "d2" {
		t.Fatalf("expected d2 (feed crawler) first, got ok=%v %v", ok, scored)
	}

	// Absent terms: searchable query, no matches.
	if scored, ok := rankDocsViaFTS(brainDir, index, "xylophone zebra quokka", 25); ok && len(scored) != 0 {
		t.Fatalf("expected no matches for absent terms, got %v", scored)
	}

	// Rebuild determinism.
	a, _ := rankDocsViaFTS(brainDir, index, "baseline knowledge sources", 25)
	b, _ := rankDocsViaFTS(brainDir, index, "baseline knowledge sources", 25)
	if len(a) != len(b) {
		t.Fatalf("nondeterministic: %d vs %d", len(a), len(b))
	}
}

func TestDocFTSPrefersCurrentOverHistorical(t *testing.T) {
	brainDir := t.TempDir()
	index := docIndex{
		GeneratedAt: time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
		Records: []docRecord{
			{ID: "a-old", Path: "seed/docs/old.md", Text: "agent benchmark workflow", Historical: true},
			{ID: "z-current", Path: "seed/docs/current.md", Text: "agent benchmark workflow"},
		},
	}
	scored, ok := rankDocsViaFTS(brainDir, index, "agent benchmark workflow", 2)
	if !ok || len(scored) != 2 || scored[0].Record.ID != "z-current" {
		t.Fatalf("current FTS document should rank first, got ok=%v %+v", ok, scored)
	}
}
