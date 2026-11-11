package cli

import (
	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// coveredSessionIDs reports every session cited by fact provenance: the local
// store plus, when feedDir is non-empty, the hosted feed cache. The cache
// matters because the pull filter drops some teammate facts from the local
// store while their sessions were still distilled. Coverage is best-effort —
// a missing or unreadable store covers nothing rather than failing, and every
// status counts: a retracted fact still proves its session was distilled.
func coveredSessionIDs(brainDir string, feedDir string) map[string]bool {
	covered := make(map[string]bool)
	add := func(records []factmerge.Record) {
		for _, rec := range records {
			for _, anchor := range rec.Provenance {
				if anchor.SessionID != "" {
					covered[anchor.SessionID] = true
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
