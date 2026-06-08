package cli

import (
	"testing"
	"time"
)

func ftsTestIndex() historyIndex {
	return historyIndex{
		GeneratedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			{ID: "d1", Kind: "decision", Path: "sessions/main/20260601T000000Z_a.jsonl", Line: 1,
				Summary: "We chose the static embedding model over a transformer bi-encoder to keep the build pure Go."},
			{ID: "d2", Kind: "decision", Path: "sessions/main/20260601T000001Z_b.jsonl", Line: 2,
				Summary: "Reconcile decides to supersede a fact when a new claim contradicts an active one."},
			{ID: "t1", Kind: "tool_call", Path: "sessions/main/20260601T000002Z_c.jsonl", Line: 3,
				Summary: "apply_patch updated README.md with go test instructions."},
		},
	}
}

func ftsExcerpts(scored []scoredHistoryRecord) []string {
	out := make([]string, len(scored))
	for i, s := range scored {
		out[i] = s.Record.ID
	}
	return out
}

// TestHistoryFTSSurfacesLowCoverageParaphrase locks in the Stage 1a fix as a
// contrast: a multi-term natural-language query whose relevant record shares only
// a single distinctive term ("embedding") is dropped entirely by the substring
// scorer's 3-of-N coverage gate (the matches:null defect), but surfaced by BM25,
// which scores the rare term instead of gating on coverage. (Rank-order quality
// needs a realistic corpus and is validated by eval, not a 3-doc unit fixture.)
func TestHistoryFTSSurfacesLowCoverageParaphrase(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()
	query := "reasons we picked one embedding approach over another"

	// Precondition: the coverage gate returns nothing for this low-overlap query.
	if gated := rankHistoryRecordsScored(index, "decisions", query, 25, 0); len(gated) != 0 {
		t.Fatalf("precondition: expected coverage gate to drop the query, got %v", ftsExcerpts(gated))
	}

	scored, ok := rankHistoryViaFTS(brainDir, index, "decisions", query, 25)
	if !ok {
		t.Fatal("rankHistoryViaFTS returned ok=false; expected BM25 results")
	}
	found := false
	for _, s := range scored {
		if s.Record.ID == "d1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected BM25 to surface d1, got %v", ftsExcerpts(scored))
	}
}

// TestHistoryFTSHonestEmpty verifies a query whose terms appear in no record
// returns nothing — the strict-contract property the precision surface depends on.
func TestHistoryFTSHonestEmpty(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()

	scored, ok := rankHistoryViaFTS(brainDir, index, "decisions", "xylophone zebra quokka", 25)
	if !ok {
		t.Fatal("expected ok=true for a searchable (if unmatched) query")
	}
	if len(scored) != 0 {
		t.Fatalf("expected no matches for absent terms, got %v", ftsExcerpts(scored))
	}
}

// TestHistoryFTSKindFilter confirms the kind filter excludes other-kind records:
// a tool_call must not surface under kind=decisions even when it matches terms.
func TestHistoryFTSKindFilter(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()

	scored, ok := rankHistoryViaFTS(brainDir, index, "decisions", "go test README apply_patch", 25)
	if !ok {
		t.Fatal("expected ok=true")
	}
	for _, s := range scored {
		if s.Record.Kind != "decision" {
			t.Fatalf("kind=decisions returned a %s record: %v", s.Record.Kind, ftsExcerpts(scored))
		}
	}

	scored, ok = rankHistoryViaFTS(brainDir, index, "tool-paths", "go test README apply_patch", 25)
	if !ok || len(scored) == 0 || scored[0].Record.ID != "t1" {
		t.Fatalf("expected t1 under tool-paths, got ok=%v %v", ok, ftsExcerpts(scored))
	}
}

// TestHistoryFTSRequestGating verifies user-prompt ("request") records are kept
// out of general history ranking (they add noise) but surface via the explicit
// `requests` kind.
func TestHistoryFTSRequestGating(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{
		GeneratedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Records: []historyRecord{
			{ID: "d1", Kind: "decision", Path: "sessions/main/20260601T000000Z_a.jsonl", Line: 1,
				Summary: "We chose FTS5 BM25 for history ranking."},
			{ID: "r1", Kind: "request", Path: "sessions/main/20260601T000000Z_a.jsonl", Line: 2,
				Summary: "How does history ranking work, should we use BM25?"},
		},
	}
	general, ok := rankHistoryViaFTS(brainDir, index, "history", "history ranking bm25", 25)
	if !ok {
		t.Fatal("general ok=false")
	}
	for _, s := range general {
		if s.Record.Kind == "request" {
			t.Fatalf("request record leaked into general history ranking: %v", ftsExcerpts(general))
		}
	}
	reqs, ok := rankHistoryViaFTS(brainDir, index, "requests", "history ranking bm25", 25)
	if !ok || len(reqs) == 0 || reqs[0].Record.ID != "r1" {
		t.Fatalf("requests kind should surface r1, got ok=%v %v", ok, ftsExcerpts(reqs))
	}
}

// TestHistoryFTSRebuildDeterministic verifies the derived index is a rebuildable
// artifact: a second open over the same truth returns identical ranking.
func TestHistoryFTSRebuildDeterministic(t *testing.T) {
	brainDir := t.TempDir()
	index := ftsTestIndex()
	query := "supersede a fact during reconcile"

	first, ok := rankHistoryViaFTS(brainDir, index, "decisions", query, 25)
	if !ok {
		t.Fatal("first query ok=false")
	}
	second, ok := rankHistoryViaFTS(brainDir, index, "decisions", query, 25)
	if !ok {
		t.Fatal("second query ok=false")
	}
	if len(first) != len(second) {
		t.Fatalf("nondeterministic length: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Record.ID != second[i].Record.ID {
			t.Fatalf("nondeterministic order at %d: %v vs %v", i, ftsExcerpts(first), ftsExcerpts(second))
		}
	}
}
