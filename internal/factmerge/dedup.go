package factmerge

import (
	"sort"
	"strings"
)

// Sort orders records by path, then text, then id so the on-disk ndjson is
// deterministic regardless of insertion order.
func Sort(records []Record) {
	sort.Slice(records, func(i, j int) bool {
		li, lj := strings.Join(records[i].Paths, ","), strings.Join(records[j].Paths, ",")
		if li != lj {
			return li < lj
		}
		if records[i].Text != records[j].Text {
			return records[i].Text < records[j].Text
		}
		return records[i].ID < records[j].ID
	})
}

// Upsert inserts incoming into the set keyed by id, or — when a fact with
// the same id already exists — unions the new provenance anchors into the
// existing fact and advances its UpdatedAt. Exact duplicates (same normalized
// text and paths) therefore collapse for free, which is what makes re-distilling
// a turn idempotent. It returns the updated set; the input slice may be reused.
//
// Upsert deliberately does not implement merge/supersede across *different*
// ids: that is the agent's judgment during distillation, handled separately.
func Upsert(records []Record, incoming Record) []Record {
	for i := range records {
		if records[i].ID != incoming.ID {
			continue
		}
		records[i].Provenance = UnionAnchors(records[i].Provenance, incoming.Provenance)
		if incoming.UpdatedAt.After(records[i].UpdatedAt) {
			records[i].UpdatedAt = incoming.UpdatedAt
		}
		// Fill Kind from a re-distill when the stored fact lacks a valid one, but
		// don't overwrite an existing valid kind (avoids thrash between an
		// agent-labeled and an inferred value across runs).
		if !ValidFactKind(records[i].Kind) && ValidFactKind(incoming.Kind) {
			records[i].Kind = incoming.Kind
		}
		return records
	}
	return append(records, incoming)
}

// UnionAnchors appends anchors from b that are not already present in a,
// preserving order. Anchors are compared on their identifying fields so the
// same source turn is never recorded twice.
func UnionAnchors(a, b []Anchor) []Anchor {
	out := make([]Anchor, 0, len(a)+len(b))
	appendAnchor := func(anchor Anchor) {
		for index := range out {
			existing := &out[index]
			existingEnd, anchorEnd := existing.EndLine, anchor.EndLine
			if existingEnd == 0 {
				existingEnd = existing.Line
			}
			if anchorEnd == 0 {
				anchorEnd = anchor.Line
			}
			samePhysical := existing.SessionID == anchor.SessionID && existing.Commit == anchor.Commit &&
				existing.CheckpointID == anchor.CheckpointID && existing.TurnID == anchor.TurnID &&
				existing.Transcript == anchor.Transcript && existing.Line == anchor.Line && existingEnd == anchorEnd &&
				existing.DistillTurnID == anchor.DistillTurnID
			if !samePhysical {
				continue
			}
			if existing.EndLine == 0 && anchor.EndLine != 0 {
				existing.EndLine = anchorEnd
			}
			existing.Verified = existing.Verified || anchor.Verified
			return
		}
		out = append(out, anchor)
	}
	for _, anchor := range a {
		appendAnchor(anchor)
	}
	for _, anchor := range b {
		appendAnchor(anchor)
	}
	return out
}
