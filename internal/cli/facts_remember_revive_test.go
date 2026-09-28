package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// rememberReviveEnv wires an Options/brainDir pair the remember command can run
// against without touching a real repository or the user's config.
func rememberReviveEnv(t *testing.T, now time.Time) (Options, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	opts := Options{Version: "test", Env: env, Runner: semanticFixtureRunner(repoDir, ""), Now: func() time.Time { return now }}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	return opts, brainDir, "feature" // the fixture runner reports "feature" as the current branch
}

func runRememberForTest(t *testing.T, opts Options, text string) string {
	t.Helper()
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	rememberOpts := rememberCommandOptions{path: "preferences.coding.style", agent: "none"}
	if err := runRemember(context.Background(), cmd, opts, rememberOpts, text); err != nil {
		t.Fatalf("runRemember: %v", err)
	}
	return out.String()
}

// A fact id is content-derived, so re-authoring a statement that was retracted
// lands on the retired record. The author asserted it is true again, so the
// record must go back to active — otherwise `remember` reports success while the
// fact stays unrecallable and the write is silently lost.
func TestRememberRevivesRetractedFact(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	text := "The user prefers table-driven tests."
	paths := normalizeFactPaths([]string{"preferences.coding.style"})
	id := factRecordID(text, paths)
	seed := []factRecord{{
		ID: id, Paths: paths, Kind: factKindPreference, Text: text, Branch: branch,
		Origin: factOriginAuthored, Status: factStatusRetracted,
		Provenance: []factAnchor{{SessionID: "s1"}},
		CreatedAt:  now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}}
	if err := writeFacts(brainDir, branch, seed); err != nil {
		t.Fatal(err)
	}

	printed := runRememberForTest(t, opts, text)

	got, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 fact, got %d: %+v", len(got), got)
	}
	if got[0].Status != factStatusActive {
		t.Fatalf("re-authored fact is still %q after %q: the command reported success but the fact is not recallable", got[0].Status, printed)
	}
	if !got[0].UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %s, want the authoring time %s", got[0].UpdatedAt, now)
	}
	// Reviving must not discard the retired record's history.
	if got[0].CreatedAt.Equal(now) {
		t.Errorf("created_at was reset; the original record should be revived, not replaced")
	}
}

// The same holds for a fact retired by an applied supersede proposal: the author
// re-asserting it clears the tombstone pointer as well as the status.
func TestRememberRevivesSupersededFact(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	text := "The service listens on port 8080."
	paths := normalizeFactPaths([]string{"preferences.coding.style"})
	id := factRecordID(text, paths)
	seed := []factRecord{{
		ID: id, Paths: paths, Kind: factKindPreference, Text: text, Branch: branch,
		Origin: factOriginDistilled, Status: factStatusSuperseded, SupersededBy: "fact:newer",
		Provenance: []factAnchor{{SessionID: "s1"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}}
	if err := writeFacts(brainDir, branch, seed); err != nil {
		t.Fatal(err)
	}

	runRememberForTest(t, opts, text)

	got, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != factStatusActive || got[0].SupersededBy != "" {
		t.Fatalf("re-authored fact not revived: %+v", got)
	}
}

// An ordinary re-remember of an already-active fact stays a provenance union:
// reviving must not disturb the idempotent path.
func TestRememberActiveFactIsStillIdempotent(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	text := "The user prefers small pull requests."
	runRememberForTest(t, opts, text)
	runRememberForTest(t, opts, text)

	got, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != factStatusActive || got[0].Branch != branch {
		t.Fatalf("want one active fact, got %+v", got)
	}
}

// A repeated remember merges into the existing durable record. JSON must report
// that stored result, not the fresh incoming record that existed before upsert.
func TestRememberJSONReportsPersistedMergedFact(t *testing.T) {
	now := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	text := "The service keeps durable facts in branch-scoped stores."
	paths := normalizeFactPaths([]string{"preferences.coding.style"})
	createdAt := now.Add(-time.Hour)
	seed := factRecord{
		ID: factRecordID(text, paths), Paths: paths, Kind: factKindPreference,
		Text: text, Branch: branch, Origin: factOriginDistilled,
		Status: factStatusSuperseded, SupersededBy: "fact:newer",
		Confidence: "high", RelatedIDs: []string{"fact:related"},
		Provenance: []factAnchor{{SessionID: "prior-session", Commit: "prior111"}},
		CreatedAt:  createdAt, UpdatedAt: createdAt,
	}
	if err := writeFacts(brainDir, branch, []factRecord{seed}); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "remember", text,
		"--path", "preferences.coding.style", "--agent", "none",
		"--kind", factKindDecision, "--json")
	if err != nil {
		t.Fatalf("remember --json: %v\n%s", err, out)
	}
	var reported factRecord
	if err := json.Unmarshal([]byte(out), &reported); err != nil {
		t.Fatalf("decode remember JSON: %v\n%s", err, out)
	}
	stored, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored facts = %+v, want one merged fact", stored)
	}
	if !reflect.DeepEqual(reported, stored[0]) {
		t.Fatalf("remember JSON did not report the persisted fact:\nreported: %+v\nstored:   %+v", reported, stored[0])
	}
	if reported.Origin != factOriginDistilled || !reported.CreatedAt.Equal(createdAt) {
		t.Fatalf("original metadata was not retained: %+v", reported)
	}
	if reported.Status != factStatusActive || reported.SupersededBy != "" || reported.Kind != factKindDecision {
		t.Fatalf("reactivation or explicit kind missing from JSON: %+v", reported)
	}
	if len(reported.Provenance) != 2 {
		t.Fatalf("provenance = %+v, want retained and new anchors", reported.Provenance)
	}
}
