package cli

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// factsRelevantToChange ranks active facts whose locus names a changed file or a
// symbol defined in those files — the "changed files → relevant facts" tie-in
// between the durable-facts locus and the semantic graph. files are the changed
// paths; symbols are the semantic symbols in those files (already resolved by
// the caller). Ranked by overlap count, then recency, capped at limit.
func factsRelevantToChange(files []string, symbols []semanticRecord, facts []factRecord, limit int) []factRecord {
	if limit <= 0 {
		limit = 10
	}
	target := map[string]struct{}{}
	add := func(s string) {
		if s = strings.ToLower(strings.TrimSpace(s)); len(s) >= 3 {
			target[s] = struct{}{}
		}
	}
	for _, f := range files {
		add(f)
		add(filepath.Base(f))
	}
	for _, s := range symbols {
		add(s.Name)
		add(s.QualifiedName)
	}
	if len(target) == 0 {
		return nil
	}
	type scored struct {
		rec factRecord
		n   int
	}
	var hits []scored
	for _, f := range facts {
		if f.Status != factStatusActive {
			continue
		}
		n := 0
		for _, tok := range base.FactLocusOf(f) {
			if _, ok := target[tok]; ok {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{f, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].rec.UpdatedAt.After(hits[j].rec.UpdatedAt)
	})
	out := make([]factRecord, 0, min(limit, len(hits)))
	for _, h := range hits {
		out = append(out, h.rec)
		if len(out) >= limit {
			break
		}
	}
	return out
}
