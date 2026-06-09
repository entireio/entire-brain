package cli

import (
	"testing"
	"time"
)

// TestRankFactsFusedReachesTermDisjoint is the core Phase D claim in miniature:
// a relevant fact that shares NO term with the query is unreachable by the
// lexical ranker (the 0.667-ceiling failure) but surfaces once semantic fusion
// is on.
func TestRankFactsFusedReachesTermDisjoint(t *testing.T) {
	now := time.Now()
	facts := []factRecord{
		{ID: "a", Text: "indentation uses space characters rather than tab stops", Status: factStatusActive, UpdatedAt: now},
		{ID: "b", Text: "the nightly deployment pipeline publishes release artifacts", Status: factStatusActive, UpdatedAt: now},
		{ID: "c", Text: "code review requires two approvals before merge", Status: factStatusActive, UpdatedAt: now},
	}
	query := "what whitespace convention do we follow" // shares no token with fact "a"

	lexical := rankFacts(facts, query, 1, false)
	if len(lexical) != 0 {
		t.Fatalf("expected lexical to find nothing term-disjoint, got %d (%v)", len(lexical), lexical[0].ID)
	}

	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("embedding backend unavailable")
	}
	fused := rankFactsFused(facts, query, 1, false, rr)
	if len(fused) == 0 || fused[0].ID != "a" {
		t.Fatalf("expected semantic fusion to surface fact a, got %v", fused)
	}
}

// TestRankFactsFusedWhitespaceQueryIsLexical guards that a whitespace-only query
// takes the recency-listing path (matching rankFacts) rather than the semantic
// fusion path, even with a live reranker.
func TestRankFactsFusedWhitespaceQueryIsLexical(t *testing.T) {
	now := time.Now()
	facts := []factRecord{
		{ID: "a", Text: "retry backoff doubles each attempt", Status: factStatusActive, UpdatedAt: now},
		{ID: "b", Text: "unrelated fact about colors", Status: factStatusActive, UpdatedAt: now.Add(time.Hour)},
	}
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("embedding backend unavailable")
	}
	got := rankFactsFused(facts, "   ", 10, false, rr)
	want := rankFacts(facts, "   ", 10, false)
	if len(got) != len(want) {
		t.Fatalf("whitespace query diverged: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Fatalf("whitespace query order diverged at %d: %s vs %s", i, got[i].ID, want[i].ID)
		}
	}
}

// TestRankFactsFusedNilIsLexical guarantees the nil-reranker path is exactly the
// existing lexical ranking, so callers without an embedder are unaffected.
func TestRankFactsFusedNilIsLexical(t *testing.T) {
	now := time.Now()
	facts := []factRecord{
		{ID: "a", Text: "retry backoff doubles each attempt", Status: factStatusActive, UpdatedAt: now},
		{ID: "b", Text: "unrelated fact about colors", Status: factStatusActive, UpdatedAt: now},
	}
	q := "retry backoff"
	got := rankFactsFused(facts, q, 10, false, nil)
	want := rankFacts(facts, q, 10, false)
	if len(got) != len(want) {
		t.Fatalf("nil reranker diverged: got %d want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Fatalf("nil reranker order diverged at %d: %s vs %s", i, got[i].ID, want[i].ID)
		}
	}
}

// With no query embedding (embedder unavailable), the semantic RRF arm must be
// skipped entirely — otherwise every cosine is 0 and the arm reorders by the
// UpdatedAt tiebreaker. Here the weaker lexical match is newer, so the buggy
// path would tie the two and surface it first; the fix keeps the stronger
// lexical match on top.
func TestRankFactsFusedSkipsSemanticArmWhenEmbedderEmpty(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "fact:strong", Text: "checkpoint advance committed ref", Status: factStatusActive, UpdatedAt: older},
		{ID: "fact:weak", Text: "checkpoint notes", Status: factStatusActive, UpdatedAt: newer},
	}
	rr := newSemanticReranker(emptyEmbedder{}) // EmbedQuery → nil → no semantic arm
	got := rankFactsFused(facts, "checkpoint advance", 10, false, rr)
	if len(got) == 0 || got[0].ID != "fact:strong" {
		t.Fatalf("empty embedder must fall back to lexical-only (want fact:strong first), got %v", got)
	}
}

// Embedder down AND no lexical match → no results, not an arbitrary recency top-N.
func TestRankFactsFusedNoResultsWhenLexicalMissAndEmbedderEmpty(t *testing.T) {
	facts := []factRecord{
		{ID: "fact:a", Text: "alpha content", Status: factStatusActive},
		{ID: "fact:b", Text: "beta content", Status: factStatusActive},
	}
	rr := newSemanticReranker(emptyEmbedder{}) // no query vector → lexical-only
	if got := rankFactsFused(facts, "zzqqxxnomatch", 10, false, rr); len(got) != 0 {
		t.Fatalf("expected no results for a lexical miss with no embedder, got %v", got)
	}
}
