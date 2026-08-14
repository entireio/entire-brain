package cli

import "reflect"

// brainBriefDeduplicateTestRoots removes test-query roots already carried as
// context symbols. It runs only at the packet boundary, after synthesis has
// consumed the complete report, so duplicate evidence still contributes to
// likely-file ranking and action guidance.
//
// DeepEqual is intentional: semanticRecord contains slices and evolves with
// the provider schema. A root is redundant only when the entire typed record
// matches, including slice nilness and every future field.
func brainBriefDeduplicateTestRoots(semantic *brainBriefSemantic) int {
	if semantic == nil || len(semantic.Context.Symbols) == 0 || len(semantic.Tests.Roots) == 0 {
		return 0
	}

	roots := semantic.Tests.Roots
	kept := roots
	removed := 0
	for i := range roots {
		duplicate := false
		for j := range semantic.Context.Symbols {
			// ID is only a rejection key: a match still requires whole-record
			// equality. This avoids reflective slice comparison for unrelated
			// records without making ID equality sufficient for dedupe.
			if roots[i].ID != semantic.Context.Symbols[j].ID {
				continue
			}
			if reflect.DeepEqual(roots[i], semantic.Context.Symbols[j]) {
				duplicate = true
				break
			}
		}
		if duplicate {
			if removed == 0 {
				kept = roots[:i]
			}
			removed++
			continue
		}
		if removed > 0 {
			kept = append(kept, roots[i])
		}
	}
	if removed > 0 {
		// roots[:0] is non-nil, preserving the JSON contract as [] when every
		// root is represented by context rather than regressing to null.
		semantic.Tests.Roots = kept
	}
	return removed
}
