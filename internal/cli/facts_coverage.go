package cli

import (
	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// coveredSessionIDs reports every session a COMPLETED distillation cited: the
// local store plus, when feedDir is non-empty, the hosted feed cache. The
// cache matters because the pull filter drops some teammate facts from the
// local store while their sessions were still distilled. Coverage is
// best-effort — a missing or unreadable store covers nothing rather than
// failing.
//
// Only distilled, non-candidate records count. A candidate is a mid-run flush
// of an UNFINISHED distill on this machine (the cache entry lands only when
// every chunk succeeds), and an authored fact is a human note, not a
// distillation — counting either would silently drop its session from every
// future backfill. A retracted distilled fact still counts: the distillation
// happened.
func coveredSessionIDs(brainDir string, feedDir string) map[string]bool {
	covered := make(map[string]bool)
	add := func(records []factmerge.Record) {
		for _, rec := range records {
			if rec.Origin != "distilled" || rec.Status == factStatusCandidate {
				continue
			}
			for _, anchor := range rec.Provenance {
				if anchor.SessionID != "" {
					covered[coveredSessionKey(anchor.SessionID, anchor.CheckpointID)] = true
				}
			}
		}
	}
	if records, err := factstore.Load(brainDir); err == nil {
		add(records)
	}
	if feedDir != "" {
		if _, items, err := loadHostedFeedCache(feedDir); err == nil {
			for _, item := range items {
				add([]factmerge.Record{item.Record})
			}
		}
	}
	return covered
}

// coveredSessionKey is the identity a covering fact must cite: the session AND
// its CURRENT latest checkpoint. Distillation stamps every fact with the
// session's latest checkpoint at distill time, so a transcript that grew since
// a teammate distilled carries a checkpoint no fact cites and is NOT covered —
// otherwise a clone with no cache entry to grow from skipped the new content
// forever (trail 2851 finding). Checkpoint-backed cache hits use the same
// "same latest checkpoint" definition of unchanged.
func coveredSessionKey(sessionID, latestCheckpoint string) string {
	return sessionID + "\x00" + latestCheckpoint
}
