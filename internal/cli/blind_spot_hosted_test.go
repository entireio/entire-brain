package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestUnsyncedHostedFactsBlindSpot(t *testing.T) {
	brainDir := t.TempDir()
	if note := unsyncedHostedFactsBlindSpot(brainDir, "main"); note != "" {
		t.Fatalf("no binding must mean no note, got %q", note)
	}
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: "https://api.example.com/api/v1"}); err != nil {
		t.Fatal(err)
	}
	note := unsyncedHostedFactsBlindSpot(brainDir, "feature/x")
	if !strings.Contains(note, "has not synced") {
		t.Fatalf("connected-but-unsynced repo must get the note, got %q", note)
	}
	if err := writeHostedFactsBinding(brainDir, "repo1", "https://api.example.com/api/v1"); err != nil {
		t.Fatal(err)
	}
	if note := unsyncedHostedFactsBlindSpot(brainDir, "feature/x"); note != "" {
		t.Fatalf("a synced repo must get no note, got %q", note)
	}
}

func TestHostedNoteSurfacesOnAnEmptyResult(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Now().UTC()
	if err := factstore.Write(brainDir, []factRecord{
		vitalityTestFact("retries are capped at three", "main", now),
	}); err != nil {
		t.Fatal(err)
	}
	writeDistillFixtureAt(t, brainDir, now)
	declareFactsAt(t, brainDir, now, now, 1, "main")
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: "https://api.example.com/api/v1"}); err != nil {
		t.Fatal(err)
	}
	note := emptyResultBlindSpotOnBranch(brainDir, "feature/x")
	if !strings.Contains(note, "has not synced") {
		t.Fatalf("the hosted note is actionable and must surface, got %q", note)
	}
}

func TestUnsyncedNoteReturnsAfterReconnectingToADifferentTarget(t *testing.T) {
	brainDir := t.TempDir()
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo-old", BaseURL: "https://old.api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := writeHostedFactsBinding(brainDir, "repo-old", "https://old.api.example.com"); err != nil {
		t.Fatal(err)
	}
	if note := unsyncedHostedFactsBlindSpot(brainDir, "main"); note != "" {
		t.Fatalf("a repo synced against the connected target must get no note, got %q", note)
	}
	// Reconnect to a different hosted target: the stale marker must not
	// suppress the note — this repo has never synced against it.
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo-new", BaseURL: "https://new.api.example.com"}); err != nil {
		t.Fatal(err)
	}
	if note := unsyncedHostedFactsBlindSpot(brainDir, "main"); note == "" {
		t.Fatal("a stale marker from a previous target must not suppress the note")
	}
}

func TestUnsyncedNoteSilentAfterDisconnect(t *testing.T) {
	brainDir := t.TempDir()
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: "https://api.example.com", OptedOut: true}); err != nil {
		t.Fatal(err)
	}
	if note := unsyncedHostedFactsBlindSpot(brainDir, "main"); note != "" {
		t.Fatalf("an opted-out repo must not be nagged to sync, got %q", note)
	}
}

func TestUnsyncedNoteIgnoresAnEmptyBranch(t *testing.T) {
	brainDir := t.TempDir()
	if err := brainstore.WriteHostedRepoBinding(brainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}
	// emptyResultBlindSpot (the branchless surface) always passes branch "";
	// a nonsense `branch "" has not synced` note must not hide the coverage notes.
	if note := unsyncedHostedFactsBlindSpot(brainDir, ""); note != "" {
		t.Fatalf("an empty branch must get no hosted note, got %q", note)
	}
}
