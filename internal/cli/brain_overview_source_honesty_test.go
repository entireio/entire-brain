package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// overviewHistoryFixture builds a brain whose manifest declares a history index
// that really exists and really holds one decision record.
func overviewHistoryFixture(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	index := historyIndex{GeneratedAt: now, Records: []historyRecord{{
		ID:        "history-1",
		Kind:      "decision",
		Summary:   "chose postgres for checkpoint storage",
		SessionID: "session-1",
		CreatedAt: now.Format(time.RFC3339),
	}}}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(storage.BrainDir, historyIndexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		RepoKey:       storage.Key,
		GeneratedAt:   now,
		Sources: &brainSources{History: &historySourceManifest{
			GeneratedAt: now, IndexPath: historyIndexPath, Records: 1, Decisions: 1,
		}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

// TestOverviewReportsUnreadableHistoryIndex is the honesty contract for
// `overview`. recentDecisionMatches discarded loadBrainHistoryIndex's error and
// returned nil, so a corrupt history index removed the whole "recent decisions"
// section from a summary that still exited 0, still reported history as a live
// source, and said nothing about the failure.
func TestOverviewReportsUnreadableHistoryIndex(t *testing.T) {
	opts, brainDir := overviewHistoryFixture(t)

	healthy, err := execute(t, NewRootCommand(opts), "overview")
	if err != nil {
		t.Fatalf("healthy overview: %v\n%s", err, healthy)
	}
	if !strings.Contains(healthy, "chose postgres for checkpoint storage") {
		t.Fatalf("healthy overview lost the decision:\n%s", healthy)
	}

	indexPath := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	if err := os.WriteFile(indexPath, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	degraded, err := execute(t, NewRootCommand(opts), "overview")
	if err != nil {
		t.Fatalf("degraded overview: %v\n%s", err, degraded)
	}
	if strings.Contains(degraded, "chose postgres for checkpoint storage") {
		t.Fatalf("degraded overview still carried the decision:\n%s", degraded)
	}
	if !strings.Contains(degraded, "recent decisions unavailable") {
		t.Fatalf("overview dropped the recent-decisions section with no warning:\n%s", degraded)
	}

	degradedJSON, err := execute(t, NewRootCommand(opts), "overview", "--json")
	if err != nil {
		t.Fatalf("degraded overview --json: %v\n%s", err, degradedJSON)
	}
	var payload struct {
		RecentDecisions []map[string]any `json:"recent_decisions"`
		Warnings        []string         `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(degradedJSON), &payload); err != nil {
		t.Fatalf("decode overview JSON: %v\n%s", err, degradedJSON)
	}
	if len(payload.RecentDecisions) != 0 {
		t.Fatalf("degraded overview JSON still carried decisions: %v", payload.RecentDecisions)
	}
	found := false
	for _, warning := range payload.Warnings {
		if strings.Contains(warning, "recent decisions unavailable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("overview JSON has no warning for the unreadable history index: %v", payload.Warnings)
	}
}

// TestRecentDecisionMatchesReportsUnreadableIndex pins the seam itself: the
// helper must distinguish "no decisions" from "the index could not be read".
func TestRecentDecisionMatchesReportsUnreadableIndex(t *testing.T) {
	_, brainDir := overviewHistoryFixture(t)
	source := &historySourceManifest{IndexPath: historyIndexPath, Records: 1, Decisions: 1}

	matches, err := recentDecisionMatches(brainDir, source, 5, sessionReadGuard{})
	if err != nil {
		t.Fatalf("healthy recentDecisionMatches: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("healthy recentDecisionMatches = %d matches, want 1", len(matches))
	}

	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	matches, err = recentDecisionMatches(brainDir, source, 5, sessionReadGuard{})
	if err == nil {
		t.Fatalf("recentDecisionMatches returned %d matches and no error for an unreadable index", len(matches))
	}
	if len(matches) != 0 {
		t.Fatalf("recentDecisionMatches returned matches beside its error: %v", matches)
	}
}
