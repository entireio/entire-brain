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

// TestRankFactsKindDoesNotOutrankRelevance locks in the COR-2311 rule: fact
// kind contributes nothing to ranking. The former flat closed-negative bonus
// applied to ~17% of the corpus on any match and let topically-similar
// closed-negatives displace the decision fact, so text relevance and recency
// decide alone, and kind never creates a match on its own.
func TestRankFactsKindDoesNotOutrankRelevance(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		// Same text-match strength against the query, different kinds. The
		// convention is more recent, so it wins the tie on recency, not kind.
		{ID: "fact:conv", Kind: factKindConvention, Paths: []string{"workflow.testing.rules"}, Text: "Expansion terms are reviewed in the weekly retrieval sync.", Status: factStatusActive, UpdatedAt: now.Add(time.Hour)},
		{ID: "fact:neg", Kind: factKindClosedNegative, Paths: []string{"workflow.testing.rules"}, Text: "Expansion terms were tried for recall and rejected; revisit only with a new model.", Status: factStatusActive, UpdatedAt: now},
	}
	got := rankFacts(facts, "expansion terms", 10, false)
	if len(got) != 2 || got[0].ID != "fact:conv" {
		t.Fatalf("kind must not outrank the more recent equally-matching convention, got %+v", got)
	}

	// Kind alone must not create a match: a closed-negative sharing no term
	// with the query stays unsurfaced.
	got = rankFacts(facts, "database migrations", 10, false)
	if len(got) != 0 {
		t.Fatalf("fact kind must not create matches, got %+v", got)
	}
}

// TestRankFactsRecencyPrior locks in the COR-2311 recency prior: a year-old
// fact with a marginally stronger text match loses to a current one, and a
// dateless fact is neutral (never demoted for carrying no timestamp).
func TestRankFactsRecencyPrior(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	stale := factRecord{ID: "fact:stale", Paths: []string{"architecture.cache.rules"},
		Text:   "Retrieval cache keys use the repo root; retrieval cache entries expire daily.",
		Status: factStatusActive, UpdatedAt: now.Add(-400 * 24 * time.Hour)}
	fresh := factRecord{ID: "fact:fresh", Paths: []string{"architecture.cache.rules"},
		Text:   "Retrieval cache keys use the commit hash now.",
		Status: factStatusActive, UpdatedAt: now.Add(-24 * time.Hour)}
	got := rankFactsAt([]factRecord{stale, fresh}, "retrieval cache", 10, false, now)
	if len(got) != 2 || got[0].ID != "fact:fresh" {
		t.Fatalf("recency prior should lift the current fact over the stale near-duplicate, got %+v", got)
	}

	// A dateless fact keeps the neutral multiplier: with the stronger match it
	// still outranks a fresh weak match.
	dateless := factRecord{ID: "fact:dateless", Paths: []string{"architecture.cache.rules"},
		Text:   "Retrieval cache keys use the repo root; retrieval cache entries expire daily.",
		Status: factStatusActive}
	weakFresh := factRecord{ID: "fact:weak", Paths: []string{"architecture.cache.rules"},
		Text:   "Cache sizing is tuned per deployment profile and audited quarterly by the retrieval team.",
		Status: factStatusActive, UpdatedAt: now.Add(-time.Hour)}
	got = rankFactsAt([]factRecord{weakFresh, dateless}, "retrieval cache keys", 10, false, now)
	if len(got) == 0 || got[0].ID != "fact:dateless" {
		t.Fatalf("a dateless strong match must not be demoted below a fresh weak match, got %+v", got)
	}
}

// TestRankFactsCollapsesRelatedPairs locks in the COR-2311 collapse rule:
// facts cross-linked via related_ids (distill judged them related below the
// merge threshold) surface as one result — the highest-ranked wins, the freed
// slot backfills — and --all skips the collapse for store introspection.
func TestRankFactsCollapsesRelatedPairs(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	a := factRecord{ID: "fact:a", RelatedIDs: []string{"fact:b"},
		Text:   "Sync retries use exponential backoff with a five minute cap.",
		Status: factStatusActive, UpdatedAt: now}
	// Identical text: the pair ties on BM25 and the newer member wins the family.
	b := factRecord{ID: "fact:b", RelatedIDs: []string{"fact:a"},
		Text:   "Sync retries use exponential backoff with a five minute cap.",
		Status: factStatusActive, UpdatedAt: now.Add(-time.Hour)}
	other := factRecord{ID: "fact:other",
		Text:   "Sync receipts are written before unlink.",
		Status: factStatusActive, UpdatedAt: now}

	got := rankFactsAt([]factRecord{a, b, other}, "sync retries backoff", 2, false, now)
	ids := make([]string, len(got))
	for i, f := range got {
		ids[i] = f.ID
	}
	if len(got) != 2 || got[0].ID != "fact:a" || got[1].ID != "fact:other" {
		t.Fatalf("linked pair should collapse to the top-ranked member and backfill, got %v", ids)
	}

	// A one-sided link still collapses: only the lower-ranked fact names the pair.
	a2 := a
	a2.RelatedIDs = nil
	got = rankFactsAt([]factRecord{a2, b, other}, "sync retries backoff", 3, false, now)
	for _, f := range got {
		if f.ID == "fact:b" {
			t.Fatalf("one-sided related_ids should still collapse the pair, got %+v", got)
		}
	}

	// --all is store introspection: nothing collapses.
	got = rankFactsAt([]factRecord{a, b, other}, "sync retries backoff", 10, true, now)
	if len(got) != 3 {
		t.Fatalf("includeAll must skip the collapse, got %+v", got)
	}
}
