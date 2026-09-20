package cli

import (
	"fmt"
	"strings"
	"testing"
)

// Equal-score records must sort deterministically regardless of map iteration order.
var fuseTieDigests = []string{"d01", "d02", "d03", "d04", "d05", "d06", "d07", "d08"}

func fuseTieLists(records []historyRecord) [][]scoredHistoryRecord {
	lists := make([][]scoredHistoryRecord, 0, len(records))
	for _, record := range records {
		// One record per list, all at rank 0: every entry earns the identical
		// RRF contribution, so score can never break the tie.
		lists = append(lists, []scoredHistoryRecord{{Record: record}})
	}
	return lists
}

func fuseOrder(fused []scoredHistoryRecord, field func(historyRecord) string) string {
	parts := make([]string, len(fused))
	for i, s := range fused {
		parts[i] = field(s.Record)
	}
	return strings.Join(parts, " ")
}

// TestFuseScoredRankListsOrdersDegradedCopiesDeterministically pins the
// determinism contract of the shared history/conversation RRF merge.
//
// fuseScoredRankLists fuses by historyRecordReplacementKey. That key
// deliberately keeps degraded copies of one exchange distinct: when a record
// has no session id, recordReplacementKey falls back to the source digest
// ("It prevents two degraded sessions from replacing one another merely
// because an old producer emitted the same exchange ID"). Those copies reach
// the final sort as separate entries whose id, branch, session id, path and
// line are all equal, so the comparator must still impose a total order on
// them — otherwise the same query returns a different ranking from one call to
// the next.
func TestFuseScoredRankListsOrdersDegradedCopiesDeterministically(t *testing.T) {
	t.Parallel()
	records := make([]historyRecord, 0, len(fuseTieDigests))
	for _, digest := range fuseTieDigests {
		records = append(records, historyRecord{
			ID:           conversationIDPrefix + "abc123",
			Kind:         conversationKind,
			Branch:       "main",
			SessionID:    "", // degraded identity: key.Session falls back to SourceDigest
			Path:         "sessions/main/20260801T000000Z_session.jsonl",
			Line:         7,
			EndLine:      8,
			TurnOrdinal:  4,
			Summary:      "exchange copy " + digest,
			SourceDigest: digest,
			ContentRole:  conversationContentRole,
		})
	}
	keys := map[historyRecordReplacementKey]struct{}{}
	for _, record := range records {
		keys[recordReplacementKey(record)] = struct{}{}
	}
	if len(keys) != len(records) {
		t.Fatalf("fixture invalid: %d degraded copies collapsed into %d fusion identities", len(records), len(keys))
	}

	lists := fuseTieLists(records)
	want := strings.Join(fuseTieDigests, " ")
	for run := 0; run < 500; run++ {
		fused := fuseScoredRankLists(lists, len(records))
		if len(fused) != len(records) {
			t.Fatalf("run %d: fused %d records, want %d", run, len(fused), len(records))
		}
		got := fuseOrder(fused, func(r historyRecord) string { return r.SourceDigest })
		if got != want {
			t.Fatalf("run %d: fused order = %q, want %q; the same input produced a different ranking", run, got, want)
		}
	}
}

// TestFuseScoredRankListsTieBreakCoversTheFusionKey guards the invariant
// directly: every field that can make two fusion keys distinct must also be
// able to order the two records. Kind is part of the key for every record.
func TestFuseScoredRankListsTieBreakCoversTheFusionKey(t *testing.T) {
	t.Parallel()
	kinds := []string{"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8"}
	records := make([]historyRecord, 0, len(kinds))
	for i, kind := range kinds {
		records = append(records, historyRecord{
			ID:      "history:same",
			Kind:    kind, // distinct fusion key, identical in every compared field
			Path:    "sessions/main/20260801T000000Z_session.jsonl",
			Line:    3,
			Summary: fmt.Sprintf("record %d", i),
		})
	}
	keys := map[historyRecordReplacementKey]struct{}{}
	for _, record := range records {
		keys[recordReplacementKey(record)] = struct{}{}
	}
	if len(keys) != len(records) {
		t.Fatalf("fixture invalid: %d kinds collapsed into %d fusion identities", len(records), len(keys))
	}

	lists := fuseTieLists(records)
	want := strings.Join(kinds, " ")
	for run := 0; run < 500; run++ {
		fused := fuseScoredRankLists(lists, len(records))
		if len(fused) != len(records) {
			t.Fatalf("run %d: fused %d records, want %d", run, len(fused), len(records))
		}
		got := fuseOrder(fused, func(r historyRecord) string { return r.Kind })
		if got != want {
			t.Fatalf("run %d: fused order = %q, want %q; the same input produced a different ranking", run, got, want)
		}
	}
}
