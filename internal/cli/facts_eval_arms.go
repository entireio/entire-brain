package cli

import (
	"fmt"
	"time"
)

// facts_eval_arms.go provides the three retrieval structures the Appendix-D A/B
// compares, behind one retrievalArm signature so the eval loop is arm-agnostic:
//
//	flat    — (a) the shipped Phase-A ranker over all branch facts.
//	scoped  — (b) locus-scoped: when the query names code, rank only the facts
//	          whose locus overlaps it (Appendix D's "the 8 facts about the package
//	          you're editing"); otherwise fall back to flat.
//	scoped-floor — (b') scoped with a recall floor: the locus-scoped facts rank
//	          first, then flat-ranked facts backfill up to k. Recovers the recall
//	          a hard locus filter drops while keeping the scoped facts on top.
//	outline — (c) outline-scoped: rank within the subtree of the best-matching
//	          outline node (progressive disclosure); on the root it equals flat.
//
// Each arm returns the surfaced facts; the eval scores them identically.
type retrievalArm func(facts []factRecord, query string, k int, rr *semanticReranker) []factRecord

func selectRetrievalArm(name string) (retrievalArm, error) {
	switch name {
	case "", "flat":
		return flatArm, nil
	case "scoped":
		return scopedArm, nil
	case "scoped-floor":
		return scopedFloorArm, nil
	case "outline":
		return outlineArm, nil
	default:
		return nil, fmt.Errorf("--arm must be flat, scoped, scoped-floor, or outline")
	}
}

func flatArm(facts []factRecord, query string, k int, rr *semanticReranker) []factRecord {
	return rankFactsFused(facts, query, k, false, rr)
}

// scopedArm narrows to the facts whose locus the query names before ranking.
// When the query has no code locus, or nothing matches, it falls back to flat so
// a conceptual query is never starved to zero.
func scopedArm(facts []factRecord, query string, k int, rr *semanticReranker) []factRecord {
	scoped := filterFactsByLocus(facts, query)
	if len(scoped) > 0 && len(scoped) < len(facts) {
		return rankFactsFused(scoped, query, k, false, rr)
	}
	return rankFactsFused(facts, query, k, false, rr)
}

// scopedFloorArm puts the locus-scoped facts first, then backfills with
// flat-ranked facts (not already surfaced) up to k. It targets the recall cost
// of the hard scoped filter — relevant facts that don't name the queried code —
// while keeping the high-precision locus matches at the top. When the query has
// no code locus (or every fact matches) it is exactly flat.
func scopedFloorArm(facts []factRecord, query string, k int, rr *semanticReranker) []factRecord {
	scoped := filterFactsByLocus(facts, query)
	if len(scoped) == 0 || len(scoped) == len(facts) {
		return rankFactsFused(facts, query, k, false, rr)
	}
	out := rankFactsFused(scoped, query, k, false, rr)
	if len(out) >= k {
		return out
	}
	seen := make(map[string]struct{}, len(out))
	for _, f := range out {
		seen[f.ID] = struct{}{}
	}
	for _, f := range rankFactsFused(facts, query, k, false, rr) {
		if len(out) >= k {
			break
		}
		if _, ok := seen[f.ID]; ok {
			continue
		}
		out = append(out, f)
		seen[f.ID] = struct{}{}
	}
	return out
}

// outlineArm ranks within the outline subtree that the top flat hit belongs to —
// the "load the relevant subtree, not the whole branch" hypothesis. On the
// locus-sparse root (where most facts home) the subtree is the whole branch, so
// it degenerates to flat; that degeneration is itself a measured result.
func outlineArm(facts []factRecord, query string, k int, rr *semanticReranker) []factRecord {
	ranked := rankFactsFused(facts, query, max(k*3, k), false, rr)
	if len(ranked) == 0 {
		return nil
	}
	outline := buildFactOutlineTree("", facts, time.Time{})
	home := factHomeKey(ranked[0])
	ids := subtreeFactIDs(outline, home)
	if len(ids) == 0 || len(ids) == len(facts) {
		return rankFactsFused(facts, query, k, false, rr)
	}
	subset := make([]factRecord, 0, len(ids))
	for _, f := range facts {
		if _, ok := ids[f.ID]; ok {
			subset = append(subset, f)
		}
	}
	return rankFactsFused(subset, query, k, false, rr)
}

// subtreeFactIDs collects the fact ids at and beneath an outline node.
func subtreeFactIDs(outline factOutline, key string) map[string]struct{} {
	ids := map[string]struct{}{}
	var walk func(k string)
	walk = func(k string) {
		n, ok := outline.Nodes[k]
		if !ok {
			return
		}
		for _, id := range n.LeafFactIDs {
			ids[id] = struct{}{}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(key)
	return ids
}
