package cli

import "testing"

// TestCheckpointEntriesFromIDs_LimitKeepsTheNewest pins that --checkpoint-limit
// keeps the most recent checkpoints across BOTH ID formats.
//
// A ULID encodes a millisecond timestamp in its leading characters and is
// time-sortable; a legacy 12-hex ID is random throughout and carries no time.
// Sorting the mixed set as reversed strings ordered by byte value instead of
// recency — hex is lowercase, ULIDs are uppercase Crockford base32, so every
// hex ID beginning a-f sorted above every ULID. Under a limit that discarded
// the newest checkpoints (the ULIDs the git-refs backend mints) and kept the
// oldest ones from the pre-migration branch.
func TestCheckpointEntriesFromIDs_LimitKeepsTheNewest(t *testing.T) {
	t.Parallel()

	const legacyHex = "fedcba987654" // legacy 12-hex, no recoverable time
	const olderULID = "01AAAAAAAA4YW6J5M9GP655HZN"
	const newerULID = "01ZZZZZZZZ4YW6J5M9GP655HZN"

	for _, id := range []string{olderULID, newerULID} {
		if _, ok := checkpointULIDTime(id); !ok {
			t.Fatalf("fixture %q is not a recognized ULID checkpoint ID", id)
		}
	}

	ids := map[string]struct{}{
		legacyHex: {},
		olderULID: {},
		newerULID: {},
	}

	entries := checkpointEntriesFromIDs(ids, 2)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	got := []string{entries[0].CheckpointID, entries[1].CheckpointID}
	want := []string{newerULID, olderULID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("limit kept %v, want the two newest %v", got, want)
		}
	}
}

// TestCheckpointEntriesFromIDs_LegacyOnlyKeepsDescendingOrder keeps the previous
// behavior for a repo that has never minted a ULID: no time is recoverable from
// any of the IDs, so descending ID order stands.
func TestCheckpointEntriesFromIDs_LegacyOnlyKeepsDescendingOrder(t *testing.T) {
	t.Parallel()

	ids := map[string]struct{}{
		"aaaaaaaaaaaa": {},
		"ffffffffffff": {},
		"555555555555": {},
	}

	entries := checkpointEntriesFromIDs(ids, 0)
	want := []string{"ffffffffffff", "aaaaaaaaaaaa", "555555555555"}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(entries), len(want))
	}
	for i := range want {
		if entries[i].CheckpointID != want[i] {
			t.Fatalf("order %v, want %v", entries, want)
		}
	}
}
