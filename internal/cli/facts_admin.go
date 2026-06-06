package cli

import (
	"errors"
	"time"
)

// errStaleProposal is returned when a proposal references a fact that no longer
// exists (e.g. pruned or already resolved).
var errStaleProposal = errors.New("proposal references a fact that no longer exists")

// removeFactByID returns facts with the record of the given id removed.
func removeFactByID(facts []factRecord, id string) []factRecord {
	out := facts[:0:0]
	for _, f := range facts {
		if f.ID != id {
			out = append(out, f)
		}
	}
	return out
}

// applyProposal resolves a queued proposal by performing its merge/supersede.
// Merge consolidates the candidate's provenance into the target and removes the
// now-redundant candidate record; supersede marks the target superseded by the
// candidate, which stays active. Returns an error if either fact is gone (a
// stale proposal).
func applyProposal(facts []factRecord, p factProposal, now time.Time) ([]factRecord, error) {
	ci := indexOfFact(facts, p.CandidateID)
	ti := indexOfFact(facts, p.TargetID)
	if ci < 0 || ti < 0 {
		return facts, errStaleProposal
	}
	facts = clearConflictLink(facts, p.CandidateID, p.TargetID)
	ci = indexOfFact(facts, p.CandidateID)
	ti = indexOfFact(facts, p.TargetID)
	if p.Action == factActionMerge {
		facts[ti].Provenance = unionFactAnchors(facts[ti].Provenance, facts[ci].Provenance)
		facts[ti].UpdatedAt = now
		return removeFactByID(facts, p.CandidateID), nil
	}
	// supersede
	facts[ti].Status = factStatusSuperseded
	facts[ti].SupersededBy = facts[ci].ID
	facts[ti].UpdatedAt = now
	return facts, nil
}

// rejectProposal discards a proposal without changing fact status, removing the
// conflict cross-link so the pair is no longer flagged as conflicting.
func rejectProposal(facts []factRecord, p factProposal) []factRecord {
	return clearConflictLink(facts, p.CandidateID, p.TargetID)
}

// clearConflictLink removes the RelatedIDs cross-link between two facts.
func clearConflictLink(facts []factRecord, aID, bID string) []factRecord {
	for i := range facts {
		switch facts[i].ID {
		case aID:
			facts[i].RelatedIDs = removeString(facts[i].RelatedIDs, bID)
		case bID:
			facts[i].RelatedIDs = removeString(facts[i].RelatedIDs, aID)
		}
	}
	return facts
}

func removeString(values []string, target string) []string {
	out := values[:0:0]
	for _, v := range values {
		if v != target {
			out = append(out, v)
		}
	}
	return out
}

// promoteFacts carries a source branch's active facts into a target set under
// the given strategy, returning the merged set, any conflict proposals queued
// (keep-both), and the number of facts promoted.
//
//   - keep-both: copy source facts in; a same-path conflict with an existing
//     active target fact keeps both, cross-links them, and queues a proposal.
//   - prefer-source: source facts win — conflicting target facts are superseded.
//   - prefer-target: target facts win — conflicting source facts are skipped.
//
// Identical facts (same content-derived id) never conflict; their provenance is
// unioned.
func promoteFacts(source, target []factRecord, strategy, intoBranch string, now time.Time) ([]factRecord, []factProposal, int) {
	result := append([]factRecord(nil), target...)
	var proposals []factProposal
	promoted := 0
	for _, sf := range source {
		if sf.Status != factStatusActive {
			continue
		}
		sf.Branch = intoBranch
		if i := indexOfFact(result, sf.ID); i >= 0 {
			result[i].Provenance = unionFactAnchors(result[i].Provenance, sf.Provenance)
			continue
		}
		conflicts := activeConflictIndexes(result, sf)
		if len(conflicts) == 0 {
			result = append(result, sf)
			promoted++
			continue
		}
		switch strategy {
		case "prefer-target":
			continue // target wins; drop the source fact
		case "prefer-source":
			for _, ci := range conflicts {
				result[ci].Status = factStatusSuperseded
				result[ci].SupersededBy = sf.ID
				result[ci].UpdatedAt = now
			}
			result = append(result, sf)
			promoted++
		default: // keep-both
			for _, ci := range conflicts {
				result[ci].RelatedIDs = appendUniqueString(result[ci].RelatedIDs, sf.ID)
				sf.RelatedIDs = appendUniqueString(sf.RelatedIDs, result[ci].ID)
				proposals = append(proposals, factProposal{Action: factActionSupersede, CandidateID: sf.ID, TargetID: result[ci].ID, Confidence: 0, Branch: intoBranch})
			}
			result = append(result, sf)
			promoted++
		}
	}
	return result, proposals, promoted
}

// retractFact marks a fact retracted (no longer true) without deleting it, so
// the change stays auditable; `facts gc` prunes retracted facts later. Returns
// whether the fact was found, and whether its status actually changed.
func retractFact(facts []factRecord, id string, now time.Time) (found, changed bool) {
	i := indexOfFact(facts, id)
	if i < 0 {
		return false, false
	}
	if facts[i].Status == factStatusRetracted {
		return true, false
	}
	facts[i].Status = factStatusRetracted
	facts[i].UpdatedAt = now
	return true, true
}

// activeConflictIndexes returns indexes of active facts that share a taxonomy
// path with candidate but have a different id (a genuine same-path conflict).
func activeConflictIndexes(facts []factRecord, candidate factRecord) []int {
	paths := make(map[string]struct{}, len(candidate.Paths))
	for _, p := range candidate.Paths {
		paths[p] = struct{}{}
	}
	var idx []int
	for i := range facts {
		if facts[i].Status != factStatusActive || facts[i].ID == candidate.ID {
			continue
		}
		for _, p := range facts[i].Paths {
			if _, ok := paths[p]; ok {
				idx = append(idx, i)
				break
			}
		}
	}
	return idx
}

// gcResult reports the outcome of a garbage-collection pass.
type gcResult struct {
	Kept    []factRecord
	Pruned  []factRecord
	Orphans []factRecord // top-level category no longer in the taxonomy; never pruned
}

// gcFacts prunes retracted facts and superseded facts older than the retention
// window, and reports taxonomy-orphaned facts (whose top-level category no
// longer exists) without ever deleting them. Orphans that are also prunable
// (retracted/old-superseded) are pruned; active orphans are kept and reported.
func gcFacts(facts []factRecord, taxonomy factTaxonomy, now time.Time, retention time.Duration) gcResult {
	cutoff := now.Add(-retention)
	var result gcResult
	for _, f := range facts {
		if !factAllPathsKnown(taxonomy, f) {
			result.Orphans = append(result.Orphans, f)
		}
		switch f.Status {
		case factStatusRetracted:
			result.Pruned = append(result.Pruned, f)
			continue
		case factStatusSuperseded:
			if f.UpdatedAt.Before(cutoff) {
				result.Pruned = append(result.Pruned, f)
				continue
			}
		}
		result.Kept = append(result.Kept, f)
	}
	return result
}

// factAllPathsKnown reports whether every path's top-level category exists in
// the taxonomy.
func factAllPathsKnown(taxonomy factTaxonomy, f factRecord) bool {
	for _, p := range f.Paths {
		if !factPathKnownTopLevel(taxonomy, p) {
			return false
		}
	}
	return true
}
