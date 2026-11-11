package factmerge

import (
	"fmt"
	"time"
)

const (
	ActionNew       = "new"
	ActionMerge     = "merge"
	ActionSupersede = "supersede"

	// DefaultConfidenceThreshold gates automatic merge/supersede in
	// ApplyActions. At or above it, the action applies; below it both facts
	// stay active and are cross-linked via RelatedIDs. Production reconcile
	// (internal/cli/reconcile_batch.go) never applies confidence — only tests
	// drive ApplyActions today; the nightly compaction loop (COR-2273) is the
	// prospective production caller.
	DefaultConfidenceThreshold = 0.75
)

// Action is the agent's decision for one distilled candidate, evaluated
// against the active facts at the candidate's path(s):
//
//   - new: a statement not already represented.
//   - merge <target>: the same meaning as an existing fact; consolidate.
//   - supersede <target>: contradicts/replaces an older fact.
//
// Confidence (0..1) gates whether merge/supersede apply automatically.
type Action struct {
	Kind       string
	TargetID   string
	Confidence float64
	Candidate  Record
}

// ApplyActions folds a chronological sequence of agent actions into an
// active fact set. Actions are applied in order so each one sees the set as it
// stood at that point — a --force rebuild replays the same order and
// reconstructs the identical chain.
//
// High-confidence merge consolidates the candidate's provenance into the target
// and drops the candidate. High-confidence supersede marks the target
// superseded (never deleted) and adds the candidate as the active replacement.
// Below threshold, both facts stay active and are cross-linked via RelatedIDs;
// the nightly compaction loop settles coexisting conflicts (COR-2273 — the
// review queue this used to feed was removed with the per-branch stores). An
// action whose target is unknown or is the candidate itself degrades to `new`.
func ApplyActions(active []Record, actions []Action, threshold float64, now time.Time) []Record {
	for _, action := range actions {
		candidate := action.Candidate
		if action.Confidence > 0 {
			candidate.Confidence = renderConfidence(action.Confidence)
		}
		switch action.Kind {
		case ActionMerge, ActionSupersede:
			ti := IndexOf(active, action.TargetID)
			if ti < 0 || action.TargetID == candidate.ID {
				active = Upsert(active, candidate) // unknown/self target: keep as new
				continue
			}
			if action.Confidence < threshold {
				// Low confidence: never silently rewrite. Keep both active,
				// cross-linked, for the compaction loop to settle.
				candidate = AddConflictLink(candidate, active[ti].ID)
				active[ti] = AddConflictLink(active[ti], candidate.ID)
				active = Upsert(active, candidate)
				continue
			}
			if action.Kind == ActionMerge {
				// Same meaning: consolidate provenance into the target and drop
				// the candidate. The target's text (the earlier phrasing) wins.
				active[ti].Provenance = UnionAnchors(active[ti].Provenance, candidate.Provenance)
				if now.After(active[ti].UpdatedAt) {
					active[ti].UpdatedAt = now
				}
				continue
			}
			// Supersede: retain the old fact, mark it superseded, and add the
			// candidate as the active replacement.
			active[ti].Status = StatusSuperseded
			active[ti].SupersededBy = candidate.ID
			active[ti].UpdatedAt = now
			candidate = AddConflictLink(candidate, action.TargetID)
			active = Upsert(active, candidate)
		default:
			active = Upsert(active, candidate)
		}
	}
	return active
}

// IndexOf returns the index of the fact with the given id, or -1.
func IndexOf(records []Record, id string) int {
	for i := range records {
		if records[i].ID == id {
			return i
		}
	}
	return -1
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// renderConfidence formats an action confidence for storage on a fact record's
// Confidence field (the plan keeps Confidence as a string).
func renderConfidence(confidence float64) string {
	return fmt.Sprintf("%.2f", confidence)
}
