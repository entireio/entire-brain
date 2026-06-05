package cli

import (
	"sort"
	"strings"
)

// scoredFact pairs a fact with its query score for ranking.
type scoredFact struct {
	Record factRecord
	Score  int
}

// rankFacts scores facts against a query and returns the top matches, highest
// score first with most-recently-updated as the tiebreak. Superseded and
// retracted facts are excluded unless includeAll is set. An empty query returns
// the most recent facts (no scoring), which makes `recall ""` a useful "what do
// you know about this branch" listing.
func rankFacts(facts []factRecord, query string, limit int, includeAll bool) []factRecord {
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

	scored := make([]scoredFact, 0, len(candidates))
	for _, f := range candidates {
		if score := factQueryScore(f, query); score > 0 {
			scored = append(scored, scoredFact{Record: f, Score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Record.UpdatedAt.After(scored[j].Record.UpdatedAt)
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]factRecord, len(scored))
	for i, s := range scored {
		out[i] = s.Record
	}
	return out
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
