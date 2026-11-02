package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// TestRefreshMirrorsCheckpointDeletion is the strict-mirror canary: a secret
// planted in one session of the checkpoint branch, whose checkpoint is then
// deleted from the v1 tip, must be gone from every brain artifact after the
// next refresh — sessions tree, history index, FTS, conversation retrieval,
// and facts — while the other session and its facts survive. Authored facts
// and pulled copies (no transcript anchor) are never touched.
func TestRefreshMirrorsCheckpointDeletion(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	cleanRoot := checkpointPath("aaa111aaa111")
	secretRoot := checkpointPath("bbb222bbb222")
	cleanTranscript := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Fix the export cursor"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: cursor reuses unchanged transcripts."}]}}
`
	secretTranscript := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"rotate the leaked key ` + privacyCanary + ` now"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: rotated ` + privacyCanary + ` and revoked the old credential."}]}}
`
	files := map[string]string{
		".entire/settings.json":         `{"enabled":true}`,
		cleanRoot + "/metadata.json":    `{"branch":"main","sessions":[{"metadata":"/` + cleanRoot + `/0/metadata.json","transcript":"/` + cleanRoot + `/0/full.jsonl"}]}`,
		cleanRoot + "/0/metadata.json":  `{"checkpoint_id":"aaa111aaa111","session_id":"session-clean","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
		cleanRoot + "/0/full.jsonl":     cleanTranscript,
		secretRoot + "/metadata.json":   `{"branch":"main","sessions":[{"metadata":"/` + secretRoot + `/0/metadata.json","transcript":"/` + secretRoot + `/0/full.jsonl"}]}`,
		secretRoot + "/0/metadata.json": `{"checkpoint_id":"bbb222bbb222","session_id":"session-secret","branch":"main","created_at":"2026-01-02T00:00:00Z"}`,
		secretRoot + "/0/full.jsonl":    secretTranscript,
	}
	for rel, data := range files {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "checkpoint fixture")
	git("update-ref", v1MainRef, "HEAD")
	opts := Options{Runner: ExecRunner{}, Env: EntireEnv{RepoRoot: repo, PluginDataDir: t.TempDir(), PluginCacheDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginConfigDir: t.TempDir()}, Now: func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }}
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, repo)
	if err != nil {
		t.Fatal(err)
	}
	brain := storage.BrainDir
	runFilelessExport(t, opts, "", true)

	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Sources.Sessions.Sessions) != 2 {
		t.Fatalf("fixture exported %d sessions, want 2", len(manifest.Sources.Sessions.Sessions))
	}
	rels := map[string]string{}
	for _, session := range manifest.Sources.Sessions.Sessions {
		rels[session.SessionID] = filepath.ToSlash(session.TranscriptPath)
	}
	now := opts.Now()
	facts := []factRecord{
		{
			ID: "fact:secret-only", Kind: factKindDecision, Text: "rotated the leaked key " + privacyCanary,
			Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
			Provenance: []factAnchor{{SessionID: "session-secret", Transcript: rels["session-secret"]}},
		},
		{
			ID: "fact:corroborated", Kind: factKindConvention, Text: "the cursor reuses unchanged transcripts",
			Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
			Provenance: []factAnchor{
				{SessionID: "session-secret", Transcript: rels["session-secret"]},
				{SessionID: "session-clean", Transcript: rels["session-clean"]},
			},
		},
		{
			ID: "fact:authored", Kind: factKindConvention, Text: "authored memories are never dropped by the mirror",
			Branch: "main", Origin: "authored", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: "fact:pulled", Kind: factKindConvention, Text: "a pulled teammate fact has no transcript anchor",
			Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
			Provenance: []factAnchor{{SessionID: "teammate-session-not-in-catalog"}},
		},
	}
	if err := factstore.Write(brain, "main", facts); err != nil {
		t.Fatal(err)
	}
	if err := updateFactSourceManifest(brain, now); err != nil {
		t.Fatal(err)
	}

	// Precondition: the canary is indexed and the secret fact is served.
	index, err := historystore.LoadIndex(brain, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range index.Records {
		if strings.Contains(record.Summary, privacyCanary) {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture invalid: the canary never reached the history index")
	}

	// Delete the secret checkpoint from the v1 tip, as `entire checkpoint
	// delete` does, and refresh the brain's mirror.
	git("rm", "-r", "-q", secretRoot)
	git("-c", "commit.gpgsign=false", "commit", "-qm", "delete secret checkpoint")
	git("update-ref", v1MainRef, "HEAD")
	runFilelessExport(t, opts, "", true)
	if _, _, err := dropFactsForMissingSessions(brain, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// The secret is gone from every artifact...
	assertCanaryAbsent(t, brain)
	after, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Sources.Sessions.Sessions) != 1 || after.Sources.Sessions.Sessions[0].SessionID != "session-clean" {
		t.Fatalf("manifest sessions after deletion = %+v", after.Sources.Sessions.Sessions)
	}
	remaining, err := factstore.Load(brain, "main")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]factRecord{}
	for _, fact := range remaining {
		byID[fact.ID] = fact
	}
	if _, ok := byID["fact:secret-only"]; ok {
		t.Fatal("single-source fact anchored to the deleted session survived")
	}
	corroborated, ok := byID["fact:corroborated"]
	if !ok {
		t.Fatal("corroborated fact was deleted with the session")
	}
	if len(corroborated.Provenance) != 1 || corroborated.Provenance[0].SessionID != "session-clean" {
		t.Fatalf("corroborated fact anchors = %+v, want only the surviving session", corroborated.Provenance)
	}
	if _, ok := byID["fact:authored"]; !ok {
		t.Fatal("authored fact was dropped by the mirror")
	}
	pulled, ok := byID["fact:pulled"]
	if !ok {
		t.Fatal("pulled fact (no transcript anchor) was dropped by the mirror")
	}
	if len(pulled.Provenance) != 1 {
		t.Fatalf("pulled fact anchors = %+v, want untouched", pulled.Provenance)
	}

	// ...while the surviving session still answers.
	cleanIndex, err := historystore.LoadIndex(brain, after.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	surviving := false
	for _, record := range cleanIndex.Records {
		if strings.Contains(record.Summary, "cursor reuses unchanged transcripts") {
			surviving = true
		}
	}
	if !surviving {
		t.Fatal("the surviving session's records were dropped with the deleted one")
	}
}
