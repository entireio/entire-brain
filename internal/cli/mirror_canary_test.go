package cli

import (
	"context"
	"encoding/json"
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
		{
			ID: "fact:secret-compact", Kind: factKindDecision, Text: "compact anchors of the deleted session also drop " + privacyCanary,
			Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
			Provenance: []factAnchor{
				{SessionID: "session-secret", Transcript: rels["session-secret"]},
				{SessionID: "session-secret"},
			},
		},
		{
			ID: "fact:mixed-foreign", Kind: factKindConvention, Text: "a foreign anchor keeps this fact alive",
			Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
			Provenance: []factAnchor{
				{SessionID: "session-secret", Transcript: rels["session-secret"]},
				{SessionID: "teammate-session-not-in-catalog"},
			},
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
	if _, err := mirrorBrainToCheckpointCatalog(brain, now.Add(time.Minute)); err != nil {
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
	if _, ok := byID["fact:secret-compact"]; ok {
		t.Fatal("a compact (no-transcript) anchor of the deleted session kept its fact alive")
	}
	mixed, ok := byID["fact:mixed-foreign"]
	if !ok {
		t.Fatal("a fact corroborated by a foreign anchor was deleted with the session")
	}
	if len(mixed.Provenance) != 1 || mixed.Provenance[0].SessionID != "teammate-session-not-in-catalog" {
		t.Fatalf("mixed-provenance fact anchors = %+v, want only the foreign anchor", mixed.Provenance)
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

// TestMirrorKeepsFactsForUnavailableCatalogSessions pins the membership rule:
// a session whose transcript bytes vanished locally is dropped from the
// manifest's session list, but it is still in the checkpoint catalog, so the
// mirror must not delete the facts distilled from it.
func TestMirrorKeepsFactsForUnavailableCatalogSessions(t *testing.T) {
	opts, repo, git := vanishedCacheFixture(t)
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, repo)
	if err != nil {
		t.Fatal(err)
	}
	brain := storage.BrainDir
	runFilelessExport(t, opts, brain, false)
	runFilelessExport(t, opts, "", false)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	rels := map[string]string{}
	for _, session := range manifest.Sources.Sessions.Sessions {
		rels[session.SessionID] = filepath.ToSlash(session.TranscriptPath)
	}
	now := opts.Now()
	facts := []factRecord{{
		ID: "fact:from-vanishing", Kind: factKindDecision, Text: "distilled from the session whose bytes vanish",
		Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
		Provenance: []factAnchor{{SessionID: "session-gone", Transcript: rels["session-gone"]}},
	}}
	if err := factstore.Write(brain, "main", facts); err != nil {
		t.Fatal(err)
	}

	blob := git("rev-parse", v1MainRef+":"+checkpointPath("bbb222bbb222")+"/0/full.jsonl")
	if err := os.Remove(filepath.Join(repo, ".git", "objects", blob[:2], blob[2:])); err != nil {
		t.Fatal("destroy transcript object:", err)
	}
	runFilelessExport(t, opts, "", false)
	after, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Sources.Sessions.Sessions) != 1 {
		t.Fatalf("manifest sessions = %+v, want only the servable one", after.Sources.Sessions.Sessions)
	}
	foundUnavailable := false
	for _, entry := range after.Sources.Sessions.UnavailableSessions {
		if entry.SessionID == "session-gone" {
			foundUnavailable = true
		}
	}
	if !foundUnavailable {
		t.Fatalf("manifest did not record the unavailable catalog member: %+v", after.Sources.Sessions.UnavailableSessions)
	}
	if _, err := mirrorBrainToCheckpointCatalog(brain, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	remaining, err := factstore.Load(brain, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != "fact:from-vanishing" || len(remaining[0].Provenance) != 1 {
		t.Fatalf("mirror deleted facts of an unavailable but cataloged session: %+v", remaining)
	}
}

// TestMirrorSkipsPartialCatalogs pins the gate: a branch-scoped or
// checkpoint-limited export writes a partial catalog, and the mirror must not
// judge membership against it.
func TestMirrorSkipsPartialCatalogs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(source *sessionSourceManifest)
	}{
		{name: "branch scope", mutate: func(source *sessionSourceManifest) { source.Scope = exportScopeBranch }},
		{name: "checkpoint limit", mutate: func(source *sessionSourceManifest) { source.CheckpointLimit = 5 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := writePrivacyFixture(t)
			manifest, err := bmanifest.Load(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(manifest.Sources.Sessions)
			if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			facts := []factRecord{{
				ID: "fact:outside-window", Kind: factKindDecision, Text: "distilled from a session outside the partial catalog",
				Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
				Provenance: []factAnchor{{SessionID: "session-not-in-this-window", Transcript: "sessions/other/older.jsonl"}},
			}}
			if err := factstore.Write(brainDir, "main", facts); err != nil {
				t.Fatal(err)
			}
			counts, err := mirrorBrainToCheckpointCatalog(brainDir, now)
			if err != nil {
				t.Fatal(err)
			}
			if !counts.empty() {
				t.Fatalf("mirror acted on a partial catalog: %+v", counts)
			}
			remaining, err := factstore.Load(brainDir, "main")
			if err != nil || len(remaining) != 1 {
				t.Fatalf("partial catalog changed the fact store: %+v %v", remaining, err)
			}
		})
	}
}

// TestMirrorSweepsAbstractsOfMissingSessions pins the abstract half of the
// mirror: a session abstract is an LLM summary of transcript content, so when
// the session leaves the checkpoint catalog, the abstract and its egress
// receipt leave the brain at the next refresh.
func TestMirrorSweepsAbstractsOfMissingSessions(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	digest := sessionViewDigest(view)
	data, err := json.Marshal(validAbstractForView(view, digest))
	if err != nil {
		t.Fatal(err)
	}
	writeAbstractBytesForTest(t, brainDir, digest, data)
	receipt := abstractEgressReceipt{
		OperationID: "aabbccddeeff00112233", SessionRef: view.Ref, SessionDigest: digest,
		Provider: "codex", Model: "test-1", StartedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), Status: "started",
	}
	if err := writeAbstractEgressReceipt(brainDir, receipt); err != nil {
		t.Fatal(err)
	}

	// While the session is in the catalog, the mirror removes nothing.
	counts, err := mirrorBrainToCheckpointCatalog(brainDir, time.Date(2026, 11, 1, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Abstracts != 0 || counts.EgressReceipts != 0 {
		t.Fatalf("mirror removed artifacts of a live session: %+v", counts)
	}
	if _, ok := loadSessionAbstract(brainDir, digest); !ok {
		t.Fatal("abstract of a live session was removed")
	}

	// Remove the session from the catalog (its checkpoint is gone) and mirror.
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions = nil
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	counts, err = mirrorBrainToCheckpointCatalog(brainDir, time.Date(2026, 11, 1, 12, 2, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Abstracts != 1 || counts.EgressReceipts != 1 {
		t.Fatalf("mirror counts = %+v, want one abstract and one receipt removed", counts)
	}
	if _, ok := loadSessionAbstract(brainDir, digest); ok {
		t.Fatal("abstract of the deleted session survived the mirror")
	}
	receipts, err := loadAbstractEgressReceiptsChecked(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("egress receipts of the deleted session survived: %+v", receipts)
	}
}

// TestDistillRebaseDoesNotResurrectMirrorStrippedAnchors pins the provenance
// three-way merge: an anchor the mirror removed from disk after distill's
// baseline read was removed deliberately, and the flush must not union it
// back from distill's in-memory copy. Anchors added by either side survive.
func TestDistillRebaseDoesNotResurrectMirrorStrippedAnchors(t *testing.T) {
	gone := factAnchor{SessionID: "session-secret", Transcript: "sessions/main/secret.jsonl"}
	kept := factAnchor{SessionID: "session-clean", Transcript: "sessions/main/clean.jsonl"}
	added := factAnchor{SessionID: "session-new", Transcript: "sessions/main/new.jsonl"}

	baseline := []factAnchor{gone, kept}
	disk := []factAnchor{kept}               // the mirror stripped `gone`
	local := []factAnchor{gone, kept, added} // distill's in-memory copy plus a new anchor

	merged := mergeDistillAnchors(baseline, disk, local)
	ids := make([]string, 0, len(merged))
	for _, anchor := range merged {
		ids = append(ids, anchor.SessionID)
	}
	if len(merged) != 2 || merged[0].SessionID != "session-clean" || merged[1].SessionID != "session-new" {
		t.Fatalf("merged anchors = %v, want [session-clean session-new]", ids)
	}

	// Symmetric: an anchor the local side dropped is not resurrected from disk.
	merged = mergeDistillAnchors(baseline, []factAnchor{gone, kept}, []factAnchor{kept})
	if len(merged) != 1 || merged[0].SessionID != "session-clean" {
		t.Fatalf("local removal resurrected: %+v", merged)
	}

	// No baseline (concurrent insertion): pure union, nothing dropped.
	merged = mergeDistillAnchors(nil, []factAnchor{kept}, []factAnchor{added})
	if len(merged) != 2 {
		t.Fatalf("baseline-free union = %+v", merged)
	}
}
