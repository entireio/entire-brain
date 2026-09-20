package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

func writePublishPrivacyFacts(t *testing.T, brainDir string) {
	t.Helper()
	records := []factRecord{
		{ID: "secret", Branch: "main", Text: "excluded-canary", Provenance: []factAnchor{{SessionID: "secret-sess"}}},
		{ID: "path-only", Branch: "main", Text: "path-only-canary", Provenance: []factAnchor{{Transcript: "sessions/main/private.jsonl"}}},
		{ID: "mixed", Branch: "main", Text: "corroborated", Provenance: []factAnchor{{SessionID: "secret-sess"}, {SessionID: "public-sess"}}},
		{ID: "seed", Branch: "main", Text: "seed-derived"},
	}
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, records); err != nil {
		t.Fatal(err)
	}
	writePublishFile(t, filepath.Join(brainDir, factsFileRelPath("main")), buf.Bytes())
}

func TestPublishPrivacyFiltersBeforeRedaction(t *testing.T) {
	brainDir := t.TempDir()
	writePublishBrainFixture(t, brainDir)
	writePublishPrivacyFacts(t, brainDir)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions = &sessionSourceManifest{Sessions: []exportSession{{SessionID: "secret-sess", TranscriptPath: "sessions/main/private.jsonl"}}}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writePublishFile(t, filepath.Join(brainDir, exportManifestFileName), manifestData)
	tombstonePrivacyFixture(t, brainDir) // Failed cleanup leaves the facts on disk.
	art := newTestBrainArtifact()
	artifacts, err := collectFactArtifacts(art, brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %d", len(artifacts))
	}
	data := artifacts[0].Data
	if strings.Contains(string(data), "excluded-canary") || strings.Contains(string(data), "path-only-canary") {
		t.Fatal("excluded fact entered the publish payload")
	}
	for _, keep := range []string{"corroborated", "seed-derived"} {
		if !strings.Contains(string(data), keep) {
			t.Fatalf("lost permitted fact %s", keep)
		}
	}
	if art.Facts[0].Content.Digest != contentDigest(data) || art.Facts[0].Content.Size != int64(len(data)) {
		t.Fatal("manifest does not describe filtered bytes")
	}
}

func TestPublishPrivacySerializesExclusionAndUpload(t *testing.T) {
	for _, failUpload := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failUpload], func(t *testing.T) {
			t.Setenv(envBrainAllowHosted, "1")
			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
			t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
			dataDir := t.TempDir()
			brainDir := publishBrainDir(dataDir)
			writePublishBrainFixture(t, brainDir)
			writePublishPrivacyFacts(t, brainDir)
			received := make(chan wirePublishBody, 1)
			lockErrors := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body wirePublishBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				received <- body
				unlock, err := acquireBrainPrivacySideEffectLockTimeout(brainDir, 20*time.Millisecond)
				if unlock != nil {
					unlock()
				}
				lockErrors <- err
				if failUpload {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, `{"status":"ok","stored":[{"kind":"manifest","ref":"head"}]}`)
			}))
			defer srv.Close()
			// Exclusion owns the serialization boundary before publishing starts.
			unlock, err := acquireBrainPrivacySideEffectLock(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			cmd := newPublishCmd(t, t.TempDir(), dataDir, newPublishFixtureRunner())
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"publish", "--repo-id", testPublishRepoID, "--api-url", srv.URL, "--token", testPublishToken})
			done := make(chan error, 1)
			go func() { done <- cmd.Execute() }()
			premature := false
			select {
			case <-received:
				premature = true
			case <-time.After(100 * time.Millisecond):
			}
			tombstonePrivacyFixture(t, brainDir)
			unlock()
			unlock = nil
			select {
			case err := <-done:
				if (err != nil) != failUpload {
					t.Errorf("publish error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("publish did not finish")
			}
			if premature {
				t.Fatal("upload crossed the concurrent exclusion lock")
			}
			select {
			case body := <-received:
				for _, artifact := range body.Artifacts {
					if bytes.Contains(artifact.Data, []byte("excluded-canary")) {
						t.Error("uploaded excluded content")
					}
				}
			default:
				t.Fatal("no upload received")
			}
			if err := <-lockErrors; err == nil || !strings.Contains(err.Error(), memoryErrPrivacyBusy) {
				t.Errorf("privacy lock was not held throughout upload: %v", err)
			}
			released, err := acquireBrainPrivacySideEffectLockTimeout(brainDir, 20*time.Millisecond)
			if err != nil {
				t.Fatalf("privacy lock leaked after upload: %v", err)
			}
			released()
		})
	}
}

func TestPublishPrivacyCorruptPolicyRefusesUpload(t *testing.T) {
	t.Setenv(envBrainAllowHosted, "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	dataDir := t.TempDir()
	brainDir := publishBrainDir(dataDir)
	writePublishBrainFixture(t, brainDir)
	writePublishFile(t, filepath.Join(brainDir, sessionTombstonesPath), []byte("{broken"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("upload made with unreadable privacy policy") }))
	defer srv.Close()
	cmd := newPublishCmd(t, t.TempDir(), dataDir, newPublishFixtureRunner())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"publish", "--repo-id", testPublishRepoID, "--api-url", srv.URL, "--token", testPublishToken})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("publish error = %v", err)
	}
	unlock, err := acquireBrainPrivacySideEffectLockTimeout(brainDir, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("lock leaked after collection failure: %v", err)
	}
	unlock()
}
