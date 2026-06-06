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
