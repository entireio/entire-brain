package factmerge

import (
	"fmt"
	"time"
)

const (
	ActionNew       = "new"
	ActionMerge     = "merge"
	ActionSupersede = "supersede"

	// DefaultConfidenceThreshold gates automatic merge/supersede. At or
	// above it, the action applies; below it both facts stay active and the
	// action is queued as a proposal for `facts review`, so a low-confidence
	// machine judgment never silently rewrites memory.
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

// Proposal is a low-confidence merge/supersede the engine declined to apply
// automatically. It is queued for `facts review`; until resolved, both facts
// stay active and are cross-linked via RelatedIDs.
//
// ProposedBy carries cross-member ROUTING: the id of the member whose sync raised
// the proposal (the "candidate" side of the conflict). The merge core never sets it
// — it is stamped by the cross-member sync layer (internal/factsync), which knows
// who is syncing — so a proposal a team member opens is attributable and routable.
// It is omitempty and ignored by the single-user local review flow, keeping the
// on-disk format backward compatible.
type Proposal struct {
	Action      string  `json:"action"` // "merge" | "supersede"
	CandidateID string  `json:"candidate_id"`
	TargetID    string  `json:"target_id"`
	Confidence  float64 `json:"confidence"`
	Branch      string  `json:"branch,omitempty"`
	ProposedBy  string  `json:"proposed_by,omitempty"`
}

// ApplyActions folds a chronological sequence of agent actions into an
// active fact set, returning the updated set and any proposals that need human
// review. Actions are applied in order so each one sees the set as it stood at
// that point — a --force rebuild replays the same order and reconstructs the
// identical chain.
//
// High-confidence merge consolidates the candidate's provenance into the target
// and drops the candidate. High-confidence supersede marks the target
// superseded (never deleted) and adds the candidate as the active replacement.
// Below threshold, both facts stay active, are cross-linked, and a proposal is
// queued. An action whose target is unknown or is the candidate itself degrades
// to `new`.
func ApplyActions(active []Record, actions []Action, threshold float64, now time.Time) ([]Record, []Proposal) {
	var proposals []Proposal
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
				// cross-link them, and queue the decision for review.
				candidate.RelatedIDs = appendUniqueString(candidate.RelatedIDs, active[ti].ID)
				active[ti].RelatedIDs = appendUniqueString(active[ti].RelatedIDs, candidate.ID)
				active = Upsert(active, candidate)
				proposals = append(proposals, Proposal{
					Action:      action.Kind,
					CandidateID: candidate.ID,
					TargetID:    action.TargetID,
					Confidence:  action.Confidence,
					Branch:      candidate.Branch,
				})
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
			candidate.RelatedIDs = appendUniqueString(candidate.RelatedIDs, action.TargetID)
			active = Upsert(active, candidate)
		default:
			active = Upsert(active, candidate)
		}
	}
	return active, proposals
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
