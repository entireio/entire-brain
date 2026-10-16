package cli

import (
	"testing"
	"time"
)

func bm25Fact(id, text string, paths ...string) factRecord {
	if len(paths) == 0 {
		paths = []string{"architecture.test"}
	}
	return factRecord{
		ID: id, Text: text, Paths: paths,
		Status: factStatusActive, UpdatedAt: time.Now(),
	}
}

// A document holding the query's rare term must outrank one matching only the
// common term, even when the common-term document repeats it. This is the
// measured failure of the flat scorer at corpus scale (term collisions), and
// the reason BM25 replaces it.
func TestBM25RareTermOutranksCommon(t *testing.T) {
	facts := []factRecord{
		bm25Fact("fact:common1", "the sync loop retries sync requests with sync batching"),
		bm25Fact("fact:common2", "sync clients refresh tokens before sync windows"),
		bm25Fact("fact:common3", "sync state is cached between sync runs"),
		bm25Fact("fact:rare", "sync retries use exponential backoff with jitter"),
	}
	got := rankFacts(facts, "sync backoff", 2, false)
	if len(got) == 0 || got[0].ID != "fact:rare" {
		t.Fatalf("expected fact:rare first, got %+v", factIDs(got))
	}
}

// Term repetition saturates: a fact matching two distinct query terms beats a
// fact stuffing one term many times.
func TestBM25SaturationPrefersDistinctTerms(t *testing.T) {
	facts := []factRecord{
		bm25Fact("fact:stuffed", "distill distill distill distill distill distill distill distill"),
		bm25Fact("fact:both", "the distill pipeline writes a manifest after each run"),
	}
	got := rankFacts(facts, "distill manifest", 2, false)
	if len(got) == 0 || got[0].ID != "fact:both" {
		t.Fatalf("expected fact:both first, got %+v", factIDs(got))
	}
}

// With equal term evidence, the shorter fact ranks first (length
// normalization), and recency still breaks exact ties.
func TestBM25LengthNormalization(t *testing.T) {
	long := "the embedder loads lazily on first query and caches vectors per branch " +
		"while unrelated sentences about many other subsystems pad this fact out " +
		"with words that do not touch the query at all in any way whatsoever"
	facts := []factRecord{
		bm25Fact("fact:long", long),
		bm25Fact("fact:short", "the embedder caches vectors per branch"),
	}
	got := rankFacts(facts, "embedder vectors", 2, false)
	if len(got) == 0 || got[0].ID != "fact:short" {
		t.Fatalf("expected fact:short first, got %+v", factIDs(got))
	}
}

// The locus boost survives the BM25 rewrite: a shared code identifier still
// lifts a fact above a plain-text match of similar strength.
func TestBM25KeepsLocusBoost(t *testing.T) {
	withLocus := bm25Fact("fact:locus", "retrieval reads the fact store at query time")
	withLocus.Locus = []string{"facts_query.go"}
	facts := []factRecord{
		bm25Fact("fact:plain", "retrieval reads the fact store during query handling"),
		withLocus,
	}
	got := rankFacts(facts, "retrieval in facts_query.go", 2, false)
	if len(got) == 0 || got[0].ID != "fact:locus" {
		t.Fatalf("expected fact:locus first, got %+v", factIDs(got))
	}
}

// Empty queries keep the recency-listing contract.
func TestBM25EmptyQueryListsByRecency(t *testing.T) {
	older := bm25Fact("fact:older", "anything")
	older.UpdatedAt = time.Now().Add(-time.Hour)
	newer := bm25Fact("fact:newer", "anything")
	got := rankFacts([]factRecord{older, newer}, "", 2, false)
	if len(got) != 2 || got[0].ID != "fact:newer" {
		t.Fatalf("expected recency listing, got %+v", factIDs(got))
	}
}

func TestFilterFacts(t *testing.T) {
	a := bm25Fact("fact:a", "x", "architecture.sync.auth")
	a.Kind = factKindDecision
	a.UpdatedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b := bm25Fact("fact:b", "x", "project.hosted")
	b.Kind = factKindGotcha
	b.UpdatedAt = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	facts := []factRecord{a, b}

	if got := filterFacts(facts, "architecture.sync", "", time.Time{}); len(got) != 1 || got[0].ID != "fact:a" {
		t.Fatalf("path filter: got %+v", factIDs(got))
	}
	if got := filterFacts(facts, "", factKindGotcha, time.Time{}); len(got) != 1 || got[0].ID != "fact:b" {
		t.Fatalf("kind filter: got %+v", factIDs(got))
	}
	since := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	if got := filterFacts(facts, "", "", since); len(got) != 1 || got[0].ID != "fact:b" {
		t.Fatalf("since filter: got %+v", factIDs(got))
	}
	if got := filterFacts(facts, "", "", time.Time{}); len(got) != 2 {
		t.Fatalf("no filter: got %+v", factIDs(got))
	}
}

func factIDs(facts []factRecord) []string {
	ids := make([]string, len(facts))
	for i, f := range facts {
		ids[i] = f.ID
	}
	return ids
}

// Non-ASCII queries must produce terms: the ASCII-only plain tokenizer alone
// yields nothing for CJK/Cyrillic and truncates accents (trail #278 review
// finding). The query side unions the Unicode-aware normalizer's tokens,
// mirroring what factSearchTokens does for documents.
func TestBM25UnicodeQueriesMatch(t *testing.T) {
	facts := []factRecord{
		bm25Fact("fact:cafe", "the café menu service caches prices"),
		bm25Fact("fact:cyrillic", "сервис кеширует цены меню"),
		bm25Fact("fact:cjk", "服务缓存菜单价格"),
		bm25Fact("fact:other", "unrelated retry backoff fact"),
	}
	for query, want := range map[string]string{
		"café menu":   "fact:cafe",
		"сервис цены": "fact:cyrillic",
		"菜单价格":        "fact:cjk",
	} {
		got := rankFacts(facts, query, 1, false)
		if len(got) == 0 || got[0].ID != want {
			t.Errorf("query %q: got %v, want %s", query, factIDs(got), want)
		}
	}
}
