package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRetrievalPrivacyRecheckDropsDirectAndNestedExcludedEvidence(t *testing.T) {
	opts, dir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
	if out, err := execute(t, NewRootCommand(opts), "privacy", "exclude", "secret-sess", "--json"); err != nil {
		t.Fatalf("exclude: %v %s", err, out)
	}
	rows := []unifiedResult{
		{ID: "keep-clean", SessionID: "clean-sess", Text: "public"},
		{ID: "drop-session", SessionID: "secret-sess", Text: "private"},
		{ID: "drop-path", Path: "sessions/main/20260802T000000Z_secret.jsonl", Text: "private"},
		{ID: "drop-session-ref", SessionRef: "conversation-session:missing", Text: "private"},
		{ID: "drop-evidence", EvidenceIDs: []string{conversationIDPrefix + "missing"}, Text: "private"},
		{ID: "drop-concept", ConceptMatches: []conceptMatch{{ConversationID: conversationIDPrefix + "missing"}}, Text: "private"},
		{ID: "keep-other-proof", EvidenceIDs: []string{"fact:retained"}, ConceptMatches: []conceptMatch{{ConversationID: "fact:retained"}}, Text: "public"},
	}
	before, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	kept, identity, err := revalidateRetrievalResponsePrivacy(dir, rows)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, row := range kept {
		ids = append(ids, row.ID)
	}
	if !reflect.DeepEqual(ids, []string{"keep-clean", "keep-other-proof"}) || identity == "" {
		t.Fatalf("revalidated ids=%v identity=%q", ids, identity)
	}
	after, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("revalidation mutated the ranked input")
	}
}

func TestRetrievalPrivacyRecheckDoesNotTreatCorruptionAsEmptyEvidence(t *testing.T) {
	for _, kind := range []string{"manifest", "tombstones", "history"} {
		t.Run(kind, func(t *testing.T) {
			opts, dir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
			if _, err := execute(t, NewRootCommand(opts), "privacy", "exclude", "secret-sess", "--json"); err != nil {
				t.Fatal(err)
			}
			rel := exportManifestFileName
			if kind == "tombstones" {
				rel = sessionTombstonesPath
			}
			if kind == "history" {
				manifest, err := loadBrainManifest(dir)
				if err != nil {
					t.Fatal(err)
				}
				rel = manifest.Sources.History.IndexPath
			}
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte("{corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
			got, identity, err := revalidateRetrievalResponsePrivacy(dir, []unifiedResult{{ID: "private-ranked-result", Text: "canary"}})
			if err == nil || got != nil || identity != "" {
				t.Fatalf("corrupt %s accepted: results=%+v identity=%q err=%v", kind, got, identity, err)
			}
		})
	}
}
