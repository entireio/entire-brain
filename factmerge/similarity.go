package factmerge

import (
	"strings"
	"unicode"
)

// TextOverlap is the overlap coefficient |a ∩ b| / min(|a|, |b|) over the two
// texts' token sets, in [0, 1].
//
// Distill uses this lexical score to route candidates into batched comparisons.
// The smaller-set denominator keeps short statements close to longer statements
// that contain the same vocabulary. Even a score of 1 does not establish equal
// meaning or entailment: the longer statement can add distinct requirements.
// This is a cost heuristic, never authority to merge or supersede facts.
func TextOverlap(a, b string) float64 {
	ta, tb := textTokens(a), textTokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	intersection := 0
	for k := range ta {
		if _, ok := tb[k]; ok {
			intersection++
		}
	}
	m := len(ta)
	if len(tb) < m {
		m = len(tb)
	}
	return float64(intersection) / float64(m)
}

// jaccard is |a ∩ b| / |a ∪ b|, and 0 when both sets are empty (two facts that
// say nothing are not evidence of each other).
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		if v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

// textTokens lowercases and splits on every non-alphanumeric rune. No stopword
// list and no stemming: both would be an arbitrary, language-specific judgment
// baked into a core that is meant to be auditable and LLM-free.
func textTokens(text string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return stringSet(fields)
}
