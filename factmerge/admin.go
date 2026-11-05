package factmerge

import (
	"time"
)

// Retract marks a fact retracted (no longer true) without deleting it, so
// the change stays auditable; GC prunes retracted facts later. Returns
// whether the fact was found, and whether its status actually changed.
func Retract(facts []Record, id string, now time.Time) (found, changed bool) {
	i := IndexOf(facts, id)
	if i < 0 {
		return false, false
	}
	if facts[i].Status == StatusRetracted {
		return true, false
	}
	facts[i].Status = StatusRetracted
	facts[i].UpdatedAt = now
	return true, true
}

// GCResult reports the outcome of a garbage-collection pass.
type GCResult struct {
	Kept    []Record
	Pruned  []Record
	Orphans []Record // top-level category no longer in the taxonomy; never pruned
}

// GC prunes retracted facts and superseded facts older than the retention
// window, and reports taxonomy-orphaned facts (whose top-level category no
// longer exists) without ever deleting them. Orphans that are also prunable
// (retracted/old-superseded) are pruned; active orphans are kept and reported.
//
// Whether a fact's paths are still known is injected via knownTopLevel so the
// core stays free of the taxonomy loader: knownTopLevel(path) reports whether
// path's top-level category still exists.
func GC(facts []Record, knownTopLevel func(path string) bool, now time.Time, retention time.Duration) GCResult {
	cutoff := now.Add(-retention)
	var result GCResult
	for _, f := range facts {
		if !allPathsKnown(f, knownTopLevel) {
			result.Orphans = append(result.Orphans, f)
		}
		switch f.Status {
		case StatusRetracted:
			result.Pruned = append(result.Pruned, f)
			continue
		case StatusSuperseded:
			if f.UpdatedAt.Before(cutoff) {
				result.Pruned = append(result.Pruned, f)
				continue
			}
		}
		result.Kept = append(result.Kept, f)
	}
	return result
}

// allPathsKnown reports whether every path's top-level category is known
// according to the injected predicate.
func allPathsKnown(f Record, knownTopLevel func(path string) bool) bool {
	for _, p := range f.Paths {
		if !knownTopLevel(p) {
			return false
		}
	}
	return true
}
