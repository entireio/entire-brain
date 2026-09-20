package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivacyRetryPreservesTranscriptScope(t *testing.T) {
	for _, keep := range []bool{false, true} {
		name := "purge"
		if keep {
			name = "exclude"
		}
		t.Run(name, func(t *testing.T) {
			b := writePrivacyFixture(t)
			now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
			first, err := buildSessionPurgePlan(b, "secret-sess")
			if err != nil {
				t.Fatal(err)
			}
			old := first.Transcripts[0].Path
			tx := privacyTransaction{SessionID: "secret-sess", Operation: name, State: privacyStateGuarded, StartedAt: now, Artifacts: append(first.Transcripts, first.Transcripts...), SessionRefs: first.SessionRefs}
			if err := writePrivacyTransaction(b, tx, now); err != nil {
				t.Fatal(err)
			}
			stones := loadSessionTombstones(b)
			stones.Excluded["secret-sess"] = sessionTombstone{At: now, Reason: "purged"}
			if err := saveSessionTombstones(b, stones); err != nil {
				t.Fatal(err)
			}
			m, err := loadBrainManifest(b)
			if err != nil {
				t.Fatal(err)
			}
			// Refresh now knows a different export for this session. The durable old
			// path must remain in scope alongside the current manifest path.
			current := "sessions/main/reexported.jsonl"
			m.Sources.Sessions.Sessions[1].TranscriptPath = current
			if err := os.WriteFile(filepath.Join(b, current), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := writeBrainManifestAndReadme(b, *m); err != nil {
				t.Fatal(err)
			}
			fact := factRecord{ID: "path-only", Text: "secret", Branch: "main", Status: factStatusActive, CreatedAt: now, UpdatedAt: now, Provenance: []factAnchor{{Transcript: old}}}
			if err := writeFacts(b, "main", []factRecord{fact}); err != nil {
				t.Fatal(err)
			}
			guard, err := loadSessionReadGuard(b, m)
			if err != nil {
				t.Fatal(err)
			}
			if !guard.blocksRecord(historyRecord{Path: old}) || !guard.blocksFactAnchor(fact.Provenance[0]) {
				t.Error("durable path lost from serving guard")
			}
			retry, err := buildSessionPurgePlan(b, "secret-sess")
			if err != nil {
				t.Fatal(err)
			}
			if len(retry.Transcripts) != 2 || retry.FactsDeleted != 1 {
				t.Errorf("retry lost union or path-only fact: %+v", retry)
			}
			if err := executeSessionCleanup(b, "secret-sess", retry, now.Add(time.Minute), name, keep); err != nil {
				t.Fatal(err)
			}
			m, err = loadBrainManifest(b)
			if err != nil {
				t.Fatal(err)
			}
			if !historyIndexCurrent(b, m) {
				t.Error("durable exclusions caused a spurious history rebuild")
			}
			if _, err := buildHistoryShortTermLocked(b, now.Add(90*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(b, old)); keep && err != nil || !keep && !os.IsNotExist(err) {
				t.Fatalf("old transcript after %s: %v", name, err)
			}
			// A later refresh drops every manifest entry for the excluded session.
			m, err = loadBrainManifest(b)
			if err != nil {
				t.Fatal(err)
			}
			m.Sources.Sessions.Sessions = m.Sources.Sessions.Sessions[:1]
			if err := writeBrainManifestAndReadme(b, *m); err != nil {
				t.Fatal(err)
			}
			again, err := buildSessionPurgePlan(b, "secret-sess")
			if err != nil {
				t.Fatal(err)
			}
			if len(again.Transcripts) != 2 {
				t.Fatalf("completed retry lost durable inventory: %+v", again.Transcripts)
			}
			if err := executeSessionPurge(b, "secret-sess", again, now.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			report, err := verifySessionPrivacy(b)
			if err != nil || !report.Clean {
				t.Fatalf("verify: %+v %v", report, err)
			}
			if err := os.WriteFile(filepath.Join(b, old), []byte("survivor"), 0600); err != nil {
				t.Fatal(err)
			}
			report, err = verifySessionPrivacy(b)
			if err != nil || report.Clean {
				t.Fatalf("verify lost durable path: %+v %v", report, err)
			}
		})
	}
}

func TestPrivacyRetryRejectsUnsafeDurableScope(t *testing.T) {
	for _, path := range []string{"../outside", "/tmp/outside", "sessions/../../outside", "sessions/../facts/store", "sessions//odd.jsonl"} {
		t.Run(path, func(t *testing.T) {
			b := writePrivacyFixture(t)
			now := time.Now().UTC()
			tx := privacyTransaction{SchemaVersion: privacyTransactionVersion, SessionID: "secret-sess", Operation: "purge", State: privacyStateGuarded, StartedAt: now, UpdatedAt: now, Artifacts: []purgeArtifact{{Path: path}}}
			data, err := json.Marshal(tx)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeBrainRelativeFileAtomic(b, privacyTransactionRel("secret-sess"), data, 0600); err != nil {
				t.Fatal(err)
			}
			stones := loadSessionTombstones(b)
			stones.Excluded["secret-sess"] = sessionTombstone{At: now, Reason: "purged"}
			if err := saveSessionTombstones(b, stones); err != nil {
				t.Fatal(err)
			}
			m, err := loadBrainManifest(b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := buildSessionPurgePlan(b, "secret-sess"); err == nil {
				t.Error("plan accepted unsafe durable path")
			}
			if _, err := loadSessionReadGuard(b, m); err == nil {
				t.Error("guard accepted unsafe durable path")
			}
			if _, err := verifySessionPrivacy(b); err == nil {
				t.Error("verify accepted unsafe durable path")
			}
			after, err := os.ReadFile(filepath.Join(b, privacyTransactionRel("secret-sess")))
			if err != nil || string(after) != string(data) {
				t.Fatal("invalid durable transaction was modified")
			}
		})
	}
}
