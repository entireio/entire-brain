package cli

import (
	"strings"
	"testing"
	"time"
)

func TestUnsyncedHostedFactsBlindSpot(t *testing.T) {
	brainDir := t.TempDir()
	if note := unsyncedHostedFactsBlindSpot(brainDir, "main"); note != "" {
		t.Fatalf("no binding must mean no note, got %q", note)
	}
	if err := writeHostedRepoBinding(brainDir, hostedRepoBinding{RepoID: "repo1", BaseURL: "https://api.example.com/api/v1"}); err != nil {
		t.Fatal(err)
	}
	note := unsyncedHostedFactsBlindSpot(brainDir, "feature/x")
	if !strings.Contains(note, "has never synced") || !strings.Contains(note, "feature/x") {
		t.Fatalf("connected-but-unsynced branch must get the note, got %q", note)
	}
	if err := writeHostedFactsBinding(brainDir, "feature/x", "repo1", "https://api.example.com/api/v1"); err != nil {
		t.Fatal(err)
	}
	if note := unsyncedHostedFactsBlindSpot(brainDir, "feature/x"); note != "" {
		t.Fatalf("a synced branch must get no note, got %q", note)
	}
}

func TestHostedNoteWinsOverTheOtherBranchNote(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Now().UTC()
	if err := writeFacts(brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", now),
	}); err != nil {
		t.Fatal(err)
	}
	writeDistillFixtureAt(t, brainDir, now)
	declareFactsAt(t, brainDir, now, now, 1, "main")
	if err := writeHostedRepoBinding(brainDir, hostedRepoBinding{RepoID: "repo1", BaseURL: "https://api.example.com/api/v1"}); err != nil {
		t.Fatal(err)
	}
	note := emptyResultBlindSpotOnBranch(brainDir, "feature/x")
	if !strings.Contains(note, "has never synced") {
		t.Fatalf("the hosted note is actionable and must win over the other-branch note, got %q", note)
	}
}
