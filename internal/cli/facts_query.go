package cli

import (
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

// factLocusBoost is added per shared code identifier between the query and a
// fact (see factstore.LocusOverlap) — a strong, high-precision signal.
const factLocusBoost = 60

// factClosedNegativeBoost lifts a closed-negative fact above similarly-matching
// peers: when the query touches a settled-dead-end's territory, the "this was
// tried and failed" entry is the highest-value thing the brain can surface (it
// prevents re-exploration, not just re-derivation). Applied only when the fact
// already matched (score > 0), so kind alone never creates a match — and no
// pre-existing fact carries the kind, so measured baselines are unchanged
// until distill/reclassify produce closed-negatives.
const factClosedNegativeBoost = 30

// factLexicalScore is the legacy integer scorer: term/substring score, locus
// boost, and the closed-negative kind boost. rankFacts moved to BM25
// (facts_bm25.go); the post-failure hook still uses this scorer for its
// threshold semantics (hookPostFailureMinScore is calibrated to this scale).
func factLexicalScore(f factRecord, query string, queryLocus []string) int {
	score := factQueryScore(f, query)
	if overlap := factstore.LocusOverlapTokens(queryLocus, base.FactLocusOf(f)); overlap > 0 {
		score += overlap * factLocusBoost
	}
	if score > 0 && factstore.KindOrInferred(f) == factKindClosedNegative {
		score += factClosedNegativeBoost
	}
	return score
}

// scoredFact pairs a fact with its query score for ranking.
type scoredFact struct {
	Record factRecord
	Score  float64
}

// rankFacts scores facts against a query and returns the top matches, highest
// score first with most-recently-updated as the tiebreak. Superseded and
// retracted facts are excluded unless includeAll is set. An empty query returns
// the most recent facts (no scoring), which makes `recall ""` a useful "what do
// you know about this branch" listing.
func rankFacts(facts []factRecord, query string, limit int, includeAll bool) []factRecord {
	return rankFactsAt(facts, query, limit, includeAll, time.Now())
}

// rankFactsAt is rankFacts with an explicit clock for the recency prior, so
// ranking tests are not a race with the wall clock.
func rankFactsAt(facts []factRecord, query string, limit int, includeAll bool, now time.Time) []factRecord {
	if limit <= 0 {
		limit = 10
	}
	candidates := make([]factRecord, 0, len(facts))
	for _, f := range facts {
		if !includeAll && f.Status != factStatusActive {
			continue
		}
		candidates = append(candidates, f)
	}

	if strings.TrimSpace(query) == "" {
		sort.SliceStable(candidates, func(i, j int) bool {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		})
		if len(candidates) > limit {
			candidates = candidates[:limit]
		}
		return candidates
	}

	queryLocus := base.FactLocus(query)
	terms := factQueryTerms(query)
	corpus := newBM25Corpus(candidates)
	scored := make([]scoredFact, 0, len(candidates))
	for i, f := range candidates {
		if score := factBM25Score(f, corpus, i, terms, queryLocus); score > 0 {
			// Recency prior (COR-2311): a soft multiplier, never a filter — it
			// reorders neighbours and breaks stale-vs-current ties; a dateless
			// fact stays neutral. Applied after the >0 match check so time
			// never creates a match.
			score *= retrieval.RecencyPrior(f.UpdatedAt, now)
			scored = append(scored, scoredFact{Record: f, Score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Record.UpdatedAt.After(scored[j].Record.UpdatedAt)
	})
	if !includeAll {
		scored = collapseRelatedFacts(scored)
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]factRecord, len(scored))
	for i, s := range scored {
		out[i] = s.Record
	}
	return out
}

// collapseRelatedFacts gives one top result per related_ids family
// (COR-2311): when distill judges two facts related below the merge threshold,
// both stay active and cross-linked (factmerge.ApplyActions), and until the
// compaction loop (COR-2273) settles them they would surface as near-duplicate
// neighbours. The highest-ranked member represents the family in the primary
// ranks; linked lower-ranked members are demoted behind every unlinked result
// rather than dropped, because a below-threshold relative can be a
// complementary fact, not a duplicate (trail 2821 finding 01M71XZN03ZT) — so
// an alternate still surfaces whenever the limit leaves room, and disappears
// only by ordinary truncation. Greedy in rank order: a chain A-B, B-C with A
// ranked first keeps A and C in the primary ranks (C is not linked to A
// directly). Links are checked in both directions, so a one-sided related_ids
// entry still demotes the pair's lower member.
// ponytail: O(kept * related_ids) per candidate; fine at recall's k<=128.
func collapseRelatedFacts(scored []scoredFact) []scoredFact {
	if len(scored) < 2 {
		return scored
	}
	kept := make([]scoredFact, 0, len(scored))
	demoted := make([]scoredFact, 0, 2)
	keptIDs := make(map[string]bool, len(scored))
	claimed := make(map[string]bool)
	for _, s := range scored {
		linked := claimed[s.Record.ID]
		if !linked {
			for _, rid := range s.Record.RelatedIDs {
				if keptIDs[rid] {
					linked = true
					break
				}
			}
		}
		if linked {
			demoted = append(demoted, s)
			continue
		}
		kept = append(kept, s)
		keptIDs[s.Record.ID] = true
		for _, rid := range s.Record.RelatedIDs {
			claimed[rid] = true
		}
	}
	return append(kept, demoted...)
}

// factQueryScore scores a fact against a query, reusing the history index's
// term-matching machinery so facts and history rank consistently. The score
// combines a whole-query substring match, per-term overlap, and a bonus when a
// term hits a taxonomy path (a path match is a strong topical signal).
func factQueryScore(record factRecord, query string) int {
	haystack := record.Text + " " + strings.Join(record.Paths, " ")
	score := 0
	if historyTextMatchesQuery(haystack, query) {
		score += 200
	}
	terms := historyQueryTerms(query)
	if len(terms) == 0 {
		return score
	}
	normalizedText := normalizeHistorySearchText(haystack)
	normalizedPaths := normalizeHistorySearchText(strings.Join(record.Paths, " "))
	matches := 0
	for _, term := range terms {
		if strings.Contains(normalizedText, term) {
			matches++
			score += 10
			if strings.Contains(normalizedPaths, term) {
				score += 8
			}
		}
	}
	if matches == 0 && score < 200 {
		return 0
	}
	if matches == len(terms) {
		score += 20 // all query terms present
	}
	return score
}
