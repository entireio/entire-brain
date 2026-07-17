package cli

import (
	"math"
	"testing"
	"time"
)

type controlledSemanticEmbedder struct {
	query []float32
	docs  map[string][]float32
	calls map[string]int
}

func (e *controlledSemanticEmbedder) Embed(text string) []float32 {
	if e.calls != nil {
		e.calls[text]++
	}
	return e.docs[text]
}
func (e *controlledSemanticEmbedder) EmbedQuery(string) []float32 { return e.query }
func (e *controlledSemanticEmbedder) Dim() int                    { return 2 }
func (e *controlledSemanticEmbedder) ID() string                  { return "controlled-semantic" }

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

func TestRankFactsFusedInvalidQueryVectorsFallBackExactlyToLexical(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "fact:strong", Text: "checkpoint advance committed ref", Status: factStatusActive, UpdatedAt: older},
		{ID: "fact:weak", Text: "checkpoint notes", Status: factStatusActive, UpdatedAt: newer},
		{ID: "fact:inactive", Text: "checkpoint advance", Status: factStatusSuperseded, UpdatedAt: newer.Add(time.Hour)},
	}
	want := rankFacts(facts, "checkpoint advance", 10, false)
	variants := map[string][]float32{
		"nil":       nil,
		"wrong-dim": {1},
		"zero":      {0, 0},
		"nan":       {float32(math.NaN()), 1},
		"inf":       {float32(math.Inf(1)), 1},
	}
	for name, query := range variants {
		t.Run(name, func(t *testing.T) {
			e := &controlledSemanticEmbedder{query: query, docs: map[string][]float32{
				facts[0].Text: {1, 0},
				facts[1].Text: {0, 1},
			}}
			rr := newSemanticReranker(e)
			got := rankFactsFused(facts, "checkpoint advance", 10, false, rr)
			assertFactIDsEqual(t, got, want)
			if rr.lastRun.Applied || rr.lastRun.QueryVectorValid {
				t.Fatalf("invalid query vector must not apply semantic ranks: %+v", rr.lastRun)
			}
		})
	}
}

func TestRankFactsFusedInvalidCandidateVectorsCannotEarnRecencyRanks(t *testing.T) {
	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	facts := []factRecord{
		{ID: "fact:strong", Text: "checkpoint advance committed ref", Status: factStatusActive, UpdatedAt: older},
		{ID: "fact:weak", Text: "checkpoint notes", Status: factStatusActive, UpdatedAt: newer},
		{ID: "fact:inactive", Text: "checkpoint advance exact inactive", Status: factStatusSuperseded, UpdatedAt: newer.Add(time.Hour)},
	}
	want := rankFacts(facts, "checkpoint advance", 10, false)
	variants := map[string][]float32{
		"nil":       nil,
		"wrong-dim": {1},
		"zero":      {0, 0},
		"nan":       {float32(math.NaN()), 1},
		"inf":       {float32(math.Inf(1)), 1},
	}
	for name, document := range variants {
		t.Run(name, func(t *testing.T) {
			e := &controlledSemanticEmbedder{
				query: []float32{1, 0},
				docs: map[string][]float32{
					facts[0].Text: document,
					facts[1].Text: document,
				},
			}
			rr := newSemanticReranker(e)
			got := rankFactsFused(facts, "checkpoint advance", 10, false, rr)
			assertFactIDsEqual(t, got, want)
			if !rr.lastRun.Applied || !rr.lastRun.QueryVectorValid || rr.lastRun.ValidCandidateVectors != 0 {
				t.Fatalf("candidate diagnostics = %+v", rr.lastRun)
			}
		})
	}
}

func TestSemanticRerankerRetriesFailureShapedCandidateVectors(t *testing.T) {
	text := "checkpoint advance committed ref"
	e := &controlledSemanticEmbedder{
		query: []float32{1, 0},
		docs:  map[string][]float32{text: {0, 0}},
		calls: map[string]int{},
	}
	rr := newSemanticReranker(e)
	got := rr.factVector(factRecord{ID: "fact:retry", Text: text})
	if validSemanticEmbedding(got, e.Dim()) || e.calls[text] != 1 || rr.cache["fact:retry"] != nil {
		t.Fatalf("failure-shaped result was cached: got=%v calls=%d cache=%v", got, e.calls[text], rr.cache)
	}
	e.docs[text] = []float32{1, 0}
	_ = rr.factVector(factRecord{ID: "fact:retry", Text: text})
	_ = rr.factVector(factRecord{ID: "fact:retry", Text: text})
	if e.calls[text] != 2 {
		t.Fatalf("failed vector was not retried or valid retry was not cached: calls=%d", e.calls[text])
	}

	e.docs[text] = []float32{0, 0}
	delete(rr.cache, "fact:retry")
	_ = rr.factVector(factRecord{ID: "fact:retry", Text: text})
	_ = rr.factVector(factRecord{ID: "fact:retry", Text: text})
	if e.calls[text] != 4 {
		t.Fatalf("failed vector must remain retryable, calls=%d want=4", e.calls[text])
	}
}

func TestValidSemanticEmbedding(t *testing.T) {
	for name, tc := range map[string]struct {
		vector []float32
		valid  bool
	}{
		"valid":     {[]float32{1, 0}, true},
		"nil":       {nil, false},
		"wrong-dim": {[]float32{1}, false},
		"zero":      {[]float32{0, 0}, false},
		"nan":       {[]float32{float32(math.NaN()), 0}, false},
		"inf":       {[]float32{float32(math.Inf(1)), 0}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := validSemanticEmbedding(tc.vector, 2); got != tc.valid {
				t.Fatalf("validSemanticEmbedding(%v) = %v, want %v", tc.vector, got, tc.valid)
			}
		})
	}
}

func TestSemanticFusionDepthCapsWithoutOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for name, tc := range map[string]struct {
		limit      int
		multiplier int
		candidates int
		want       int
	}{
		"two-times oversampling": {limit: 6, multiplier: 2, candidates: 44, want: 12},
		"exact candidate cap":    {limit: 22, multiplier: 2, candidates: 44, want: 44},
		"above candidate cap":    {limit: 23, multiplier: 2, candidates: 44, want: 44},
		"default limit":          {limit: 0, multiplier: 2, candidates: 44, want: 20},
		"full-depth comparison":  {limit: 6, multiplier: 0, candidates: 44, want: 44},
		"limit cannot overflow":  {limit: maxInt, multiplier: 2, candidates: 44, want: 44},
		"factor cannot overflow": {limit: 6, multiplier: maxInt, candidates: 44, want: 44},
		"empty candidates":       {limit: maxInt, multiplier: 2, candidates: 0, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := semanticFusionDepth(tc.limit, tc.multiplier, tc.candidates); got != tc.want {
				t.Fatalf("semanticFusionDepth(%d, %d, %d) = %d, want %d", tc.limit, tc.multiplier, tc.candidates, got, tc.want)
			}
		})
	}
}

func assertFactIDsEqual(t *testing.T, got, want []factRecord) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("fact count = %d, want %d: got=%v want=%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Fatalf("rank %d = %s, want %s", i+1, got[i].ID, want[i].ID)
		}
	}
}
