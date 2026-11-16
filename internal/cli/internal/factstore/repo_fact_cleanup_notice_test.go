package factstore

import (
	"os"
	"path/filepath"
	"testing"
)

// A retained set that does not change must not re-announce itself: the warning
// fires when the set first appears (or changes), and stays silent on every
// later pass — a never-shrinking set otherwise reprints forever.
func TestCleanupRetainedIsNewlyRetainedOnlyWhenTheSetChanges(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	// A parked legacy dir with a record, and NO flat store: the cleanup must
	// retain (nothing proves the legacy content is represented).
	legacy := filepath.Join(brainDir, DirName, legacyParkPrefix+"old", "branch")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, FileName), []byte(`{"id":"fact:aaaaaaaaaaaaaaaaaaaaaaaa","paths":["p.q.r"],"text":"x","branch":"b","origin":"distilled","status":"active","provenance":[{"session_id":"s"}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := cleanupLegacyLocked(brainDir)
	if first.State != "retained" || len(first.Retained) == 0 {
		t.Fatalf("fixture must retain, got %+v", first)
	}
	if !CleanupNoticeDue(brainDir, first) {
		t.Fatal("the first retained set must be due a notice")
	}
	if CleanupNoticeDue(brainDir, first) {
		t.Fatal("the same retained set must not be due a second notice")
	}

	// A differently-SHAPED set for the same problem (the out-of-lock verifier
	// aggregates into one string) is a change and is due exactly once.
	aggregate := CleanupStatus{State: "retained", Retained: []string{"aggregate: " + first.Retained[0]}}
	if !CleanupNoticeDue(brainDir, aggregate) {
		t.Fatal("a changed retained set must be due a notice")
	}
	if CleanupNoticeDue(brainDir, aggregate) {
		t.Fatal("the changed set must also report only once")
	}
}
