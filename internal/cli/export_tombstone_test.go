package cli

import (
	"context"
	"testing"
	"time"
)

func tombstoneSessionList() []exportSession {
	return []exportSession{
		{SessionID: "keep-one", LatestCheckpoint: "aaa111aaa111", Branch: "main"},
		{SessionID: "test", LatestCheckpoint: "18b787bf552f", Branch: "main"},
		{SessionID: "keep-two", LatestCheckpoint: "ccc333ccc333", Branch: "dev"},
	}
}

func seedTombstone(t *testing.T, brainDir string, ids ...string) {
	t.Helper()
	stones := emptySessionTombstones()
	for _, id := range ids {
		stones.Excluded[id] = sessionTombstone{At: time.Unix(10, 0), Reason: "mock fixture"}
	}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
}

// The gap: `privacy exclude` says it excludes a session from all derived
// projections, but the export's local-transcript validation ran regardless —
// so a session whose bytes are not local blocked the whole export and could
// not be excluded past, because the check ran before the exclusion was ever
// consulted. Observed on a real brain: one mock-agent fixture blocked all
// 2,561 real sessions.
func TestExcludedSessionIsExemptFromLocalTranscriptValidation(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	seedTombstone(t, brainDir, "test")

	validated, exempt, err := partitionTombstonedSessionsAt(brainDir, tombstoneSessionList())
	if err != nil {
		t.Fatalf("partition failed: %v", err)
	}
	if exempt != 1 {
		t.Fatalf("exempted %d sessions, want 1", exempt)
	}
	for _, session := range validated {
		if session.SessionID == "test" {
			t.Fatal("an excluded session is still validated, so a session with no local bytes blocks the export")
		}
	}
	if len(validated) != 2 {
		t.Fatalf("validated %d sessions, want the 2 that are not excluded", len(validated))
	}
}

// The invariant that made the obvious fix wrong. Excluding must stay
// REVERSIBLE: the cache sweep learns what to retain from the selection, and
// retention is what lets `privacy include` recover the session. So the
// excluded session must remain in the exported session list — only validation
// skips it.
func TestExcludedSessionStaysInTheExportedSelection(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	seedTombstone(t, brainDir, "test")
	sessions := tombstoneSessionList()

	validated, _, err := partitionTombstonedSessionsAt(brainDir, sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("the caller's session list was mutated to %d entries; the excluded session must survive for cache retention", len(sessions))
	}
	var found bool
	for _, session := range sessions {
		if session.SessionID == "test" {
			found = true
		}
	}
	if !found {
		t.Fatal("the excluded session was removed from the list the caller exports, which would make privacy include unable to recover it")
	}
	if len(validated) >= len(sessions) {
		t.Fatal("nothing was exempted")
	}
}

// No tombstones must change nothing.
func TestValidationUnchangedWithoutTombstones(t *testing.T) {
	t.Parallel()
	validated, exempt, err := partitionTombstonedSessionsAt(t.TempDir(), tombstoneSessionList())
	if err != nil {
		t.Fatalf("no-tombstone path failed: %v", err)
	}
	if exempt != 0 || len(validated) != 3 {
		t.Fatalf("exempt=%d validated=%d with no tombstone store", exempt, len(validated))
	}
}

// An unreadable tombstone store must be reported, and the caller then
// validates everything rather than silently exempting sessions it cannot
// prove were excluded.
func TestUnreadableTombstonesAreReportedNotIgnored(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	if err := writeBrainRelativeFileAtomic(brainDir, sessionTombstonesPath, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	validated, exempt, err := partitionTombstonedSessionsAt(brainDir, tombstoneSessionList())
	if err == nil {
		t.Fatal("a corrupt tombstone store was silently treated as empty")
	}
	if exempt != 0 || len(validated) != 3 {
		t.Fatalf("a failed read must exempt nothing: exempt=%d validated=%d", exempt, len(validated))
	}
}

// An explicit --output bundle resolves no persistent brain and so has no
// tombstone store; it must be left alone, not treated as an error.
func TestExplicitOutputSkipsTombstoneExemption(t *testing.T) {
	t.Parallel()
	validated, exempt, err := partitionTombstonedSessions(context.Background(), Options{}, t.TempDir(), true, tombstoneSessionList())
	if err != nil {
		t.Fatalf("explicit output must not fail over tombstones: %v", err)
	}
	if exempt != 0 || len(validated) != 3 {
		t.Fatalf("explicit output exempted sessions: exempt=%d", exempt)
	}
}
