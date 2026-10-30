package historystore

import "github.com/entireio/entire-brain/internal/cli/internal/brainstore"

func GuardBlocksRecord(g brainstore.SessionReadGuard, r Record) bool {
	return g.BlocksSession(r.SessionID) || g.BlocksPath(r.Path)
}

// FilterRecords returns the records passing pred, for the in-memory
// ranking arms. Never feed a filtered slice to the FTS ranker: the shared
// store's freshness identity must only ever see the on-disk index.
func FilterRecords(records []Record, pred func(Record) bool) []Record {
	kept := make([]Record, 0, len(records))
	for _, r := range records {
		if pred(r) {
			kept = append(kept, r)
		}
	}
	return kept
}
