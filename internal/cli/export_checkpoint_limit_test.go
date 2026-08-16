package cli

import "testing"

// "0 means all" is the documented contract of --checkpoint-limit and is now the
// default. limitedCheckpointSet truncated unconditionally, so a zero limit
// selected nothing rather than everything: the export would have produced an
// empty corpus on the primary snapshot path.
func TestLimitedCheckpointSetTreatsZeroAsUnlimited(t *testing.T) {
	ids := map[string]struct{}{"a": {}, "b": {}, "c": {}}

	allowed := limitedCheckpointSet(ids, 0)

	if len(allowed) != len(ids) {
		t.Fatalf("zero limit must select every checkpoint, got %d of %d", len(allowed), len(ids))
	}
	for id := range ids {
		if _, ok := allowed[id]; !ok {
			t.Fatalf("checkpoint %q was dropped by an unlimited selection", id)
		}
	}
}

func TestLimitedCheckpointSetTreatsNegativeAsUnlimited(t *testing.T) {
	ids := map[string]struct{}{"a": {}, "b": {}}

	if allowed := limitedCheckpointSet(ids, -1); len(allowed) != len(ids) {
		t.Fatalf("negative limit must not truncate, got %d of %d", len(allowed), len(ids))
	}
}

func TestLimitedCheckpointSetStillHonoursAPositiveBound(t *testing.T) {
	ids := map[string]struct{}{"a": {}, "b": {}, "c": {}}

	if allowed := limitedCheckpointSet(ids, 2); len(allowed) != 2 {
		t.Fatalf("positive limit must still bound the set, got %d", len(allowed))
	}
}

func TestDefaultCheckpointLimitIsUnlimited(t *testing.T) {
	// The brain's value is recovering old decisions; a default cap silently
	// truncates exactly the history that makes it useful.
	if defaultCheckpointLimit != 0 {
		t.Fatalf("default checkpoint limit must be unlimited, got %d", defaultCheckpointLimit)
	}
}
