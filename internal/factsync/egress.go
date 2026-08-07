package factsync

import "github.com/ashtom/entire-brain/internal/factmerge"

// SanitizeForEgress redacts a fact's LOCAL-ONLY provenance coordinates before its
// facts leave this member for the shared, cross-member head. A provenance Anchor mixes
// two kinds of data:
//
//   - cross-member-meaningful, opaque ids another member can carry but never resolve
//     against its own machine: SessionID, Commit, CheckpointID, TurnID, Verified;
//   - LOCAL filesystem coordinates that are meaningless to — and would leak this
//     member's directory layout at — any other member: Transcript (a brain-relative
//     path on THIS machine) and Line (an offset into that local file).
//
// The reasoning layer treats the local coordinates as OPAQUE and never resolves a peer's
// anchor (ADR-P1: local-only anchors stay opaque cross-member). SanitizeForEgress makes
// that guarantee structural: it clears Transcript+Line on every anchor so the synced
// blob carries no member's local path, while keeping the opaque ids so provenance still
// unions correctly (UnionAnchors dedups by value; a shared fact from two members keeps
// both members' SessionIDs). Fact identity is unaffected — RecordID derives from text +
// paths only, never provenance.
//
// It returns a deep copy; the caller's in-memory local facts (which the member still
// wants full local provenance for) are never mutated.
func SanitizeForEgress(records []factmerge.Record) []factmerge.Record {
	if records == nil {
		return nil
	}
	out := make([]factmerge.Record, len(records))
	for i, r := range records {
		r.Provenance = sanitizeAnchors(r.Provenance)
		out[i] = r
	}
	return out
}

func sanitizeAnchors(anchors []factmerge.Anchor) []factmerge.Anchor {
	if anchors == nil {
		return nil
	}
	out := make([]factmerge.Anchor, len(anchors))
	for i, a := range anchors {
		// Drop the local filesystem coordinates; keep the opaque cross-member ids.
		a.Transcript = ""
		a.Line = 0
		out[i] = a
	}
	return out
}
