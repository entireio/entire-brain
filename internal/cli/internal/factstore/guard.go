package factstore

import (
	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func GuardBlocksFactAnchor(g brainstore.SessionReadGuard, anchor factmerge.Anchor) bool {
	return g.BlocksSession(anchor.SessionID) || g.BlocksPath(anchor.Transcript)
}

// GuardBlocksFact mirrors the purge rule at read time: a fact whose every
// provenance anchor points at excluded sessions is unreadable; a fact still
// corroborated by a non-excluded session (or with no session provenance at
// all, e.g. seed-derived) stays visible.
func GuardBlocksFact(g brainstore.SessionReadGuard, f factmerge.Record) bool {
	if g.Empty() || len(f.Provenance) == 0 {
		return false
	}
	for _, anchor := range f.Provenance {
		if !GuardBlocksFactAnchor(g, anchor) {
			return false
		}
	}
	return true
}

// GuardRecords filters a fact slice through the exclusion guard,
// returning the input unchanged when nothing is excluded.
func GuardRecords(guard brainstore.SessionReadGuard, facts []factmerge.Record) []factmerge.Record {
	if guard.Empty() || len(facts) == 0 {
		return facts
	}
	kept := make([]factmerge.Record, 0, len(facts))
	for _, fact := range facts {
		if !GuardBlocksFact(guard, fact) {
			kept = append(kept, fact)
		}
	}
	return kept
}
