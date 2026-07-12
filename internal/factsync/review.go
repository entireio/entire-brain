package factsync

import (
	"errors"
	"fmt"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// Decision is how a member resolves a cross-member review Proposal.
type Decision int

const (
	// Accept applies the proposed merge/supersede: the candidate consolidates into
	// (merge) or supersedes (supersede) the target. The losing fact is never deleted —
	// a superseded fact is retained with Status=superseded (auditable history).
	Accept Decision = iota
	// Reject keeps BOTH facts active and clears the cross-link — the two statements are
	// judged to coexist. Nothing is dropped.
	Reject
)

// ErrProposalNotApplicable is returned by Resolve when the proposal cannot be applied
// to the given fact set — e.g. its target or candidate is no longer present (a
// concurrent resolution already handled it). The caller should re-read the head and
// the open proposals and reconcile.
var ErrProposalNotApplicable = errors.New("factsync: proposal no longer applicable to the fact set")

// Resolve applies a member's Decision on a cross-member Proposal to the current fact
// set, returning the updated set. It is the second half of the cross-member conflict
// model: Sync RAISES a routed proposal (both facts kept, never overwritten); Resolve
// SETTLES it. The returned facts are then advanced through the normal Sync/CAS head, so
// every member converges on the resolution.
//
// Accept routes to factmerge.ApplyProposal (merge consolidates provenance and drops the
// candidate; supersede retires the target and keeps the candidate active). Reject routes
// to factmerge.RejectProposal (both stay active, cross-link cleared). A proposal whose
// target/candidate has vanished yields ErrProposalNotApplicable rather than a silent
// no-op, so a concurrent resolution is surfaced, not masked.
func Resolve(facts []factmerge.Record, p factmerge.Proposal, decision Decision, now time.Time) ([]factmerge.Record, error) {
	switch decision {
	case Accept:
		out, err := factmerge.ApplyProposal(facts, p, now)
		if err != nil {
			if errors.Is(err, factmerge.ErrStaleProposal) {
				return nil, ErrProposalNotApplicable
			}
			return nil, fmt.Errorf("factsync: apply proposal: %w", err)
		}
		return out, nil
	case Reject:
		// RejectProposal is a no-op when the facts are absent; guard so a stale reject is
		// surfaced too (both ids must still be present to have a link to clear).
		if factmerge.IndexOf(facts, p.CandidateID) < 0 || factmerge.IndexOf(facts, p.TargetID) < 0 {
			return nil, ErrProposalNotApplicable
		}
		return factmerge.RejectProposal(facts, p), nil
	default:
		return nil, errors.New("factsync: unknown review decision")
	}
}
