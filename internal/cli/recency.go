package cli

import (
	"math"
	"time"
)

// Recency weighting for retrieval.
//
// A brain accumulates. A decision recorded on the first day and one recorded
// last week rank identically on relevance alone, so the older answer keeps
// surfacing long after the newer one superseded it in practice. Ranking is the
// right place to express that, because it is the only one that does not throw
// anything away.
//
// Three properties, and they are the whole design:
//
//   - It is a bias, never a filter. Nothing is dropped, nothing is hidden. An
//     old record with a strong match still outranks a fresh record with a weak
//     one; the weight only decides ties and near-ties.
//   - It is deterministic. Pure arithmetic over a timestamp. No model, no
//     tokens, no network — the same constraint the rest of retrieval works
//     under.
//   - Absence of evidence is not evidence of age. A record with no usable
//     timestamp gets exactly 1.0, which is the same as saying "no opinion".
//     Classic history records carry no time, and they must not be quietly
//     demoted for it.
//
// It is opt-in. Turning it on silently would change every existing ranking
// under callers who never asked for it, and a retrieval order that shifts for
// unexplained reasons is worse than one that ignores time.
const (
	// The multiplier at age zero and at infinite age. A fresh record is worth
	// at most 1.5x a dateless one, an ancient record at least 0.3x — so a
	// five-fold spread end to end, which reorders neighbours without letting
	// recency overwhelm relevance.
	recencyCeiling = 1.5
	recencyFloor   = 0.3

	// The age at which the multiplier sits halfway between ceiling and floor.
	// Ninety days is a judgement, not a measurement: long enough that a
	// quarter-old decision is still treated as current, short enough that
	// last year's is not.
	defaultRecencyHalfLife = 90 * 24 * time.Hour

	// A record cannot be newer than now. Clock skew, a bad import or a
	// hand-edited timestamp should not mint a multiplier above the ceiling,
	// so future timestamps are clamped to age zero rather than trusted.
	recencyNeutralMultiplier = 1.0
)

// recencyMultiplier maps an age to a ranking multiplier on the closed interval
// [recencyFloor, recencyCeiling], halving the distance between them every
// halfLife.
//
//	age 0          -> 1.5   (ceiling)
//	age halfLife   -> 0.9   (floor + half the span)
//	age 3×halfLife -> 0.45
//	age ∞          -> 0.3   (floor)
func recencyMultiplier(age, halfLife time.Duration) float64 {
	if halfLife <= 0 {
		return recencyNeutralMultiplier
	}
	if age < 0 {
		// A future timestamp is a broken timestamp, not a very fresh record.
		age = 0
	}
	decayed := math.Pow(0.5, age.Seconds()/halfLife.Seconds())
	return recencyFloor + (recencyCeiling-recencyFloor)*decayed
}

// recencyMultiplierFor returns the multiplier for a record's timestamp, and
// whether a usable timestamp was found at all. An unparseable or absent
// timestamp yields the neutral 1.0 and false: the caller learns that the record
// was not weighted rather than being told it is old.
func recencyMultiplierFor(recordedAt string, now time.Time, halfLife time.Duration) (float64, bool) {
	if recordedAt == "" {
		return recencyNeutralMultiplier, false
	}
	parsed, err := time.Parse(time.RFC3339, recordedAt)
	if err != nil {
		// RFC3339 with sub-second precision is still RFC3339; anything else is
		// a record we decline to date rather than one we guess at.
		return recencyNeutralMultiplier, false
	}
	return recencyMultiplier(now.Sub(parsed), halfLife), true
}

// applyRecency reweights fused results in place and re-sorts them. It returns
// the number of results that carried a usable timestamp, which is what the
// caller reports so a user can tell "recency had no effect because everything
// is fresh" from "recency had no effect because nothing is dated".
func applyRecency(results []unifiedResult, now time.Time, halfLife time.Duration) int {
	dated := 0
	for i := range results {
		multiplier, ok := recencyMultiplierFor(results[i].CreatedAt, now, halfLife)
		if ok {
			dated++
		}
		results[i].Score *= multiplier
	}
	sortUnifiedByScore(results)
	return dated
}
