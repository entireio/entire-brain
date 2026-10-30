package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func filelessCheckpointFixture(t *testing.T) (Options, string, string) {
	return filelessCheckpointFixtureWithTranscript(t, conversationClaudeFixture)
}

func filelessCheckpointFixtureWithTranscript(t *testing.T, transcript string) (Options, string, string) {
	t.Helper()
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
	root := checkpointPath("aaa111aaa111")
	files := map[string]string{
		".entire/settings.json":   `{"enabled":true}`,
		root + "/metadata.json":   `{"branch":"main","sessions":[{"metadata":"/` + root + `/0/metadata.json","transcript":"/` + root + `/0/full.jsonl"}]}`,
		root + "/0/metadata.json": `{"checkpoint_id":"aaa111aaa111","session_id":"session-one","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
		root + "/0/full.jsonl":    transcript,
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
	opts := Options{Runner: ExecRunner{}, Env: EntireEnv{RepoRoot: repo, PluginDataDir: t.TempDir(), PluginCacheDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginConfigDir: t.TempDir()}, Now: func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }}
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, repo)
	if err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir, repo
}

func runFilelessExport(t *testing.T, opts Options, output string, history bool) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	err := runExport(context.Background(), cmd, opts, exportCommandOptions{outputDir: output, outputExplicit: output != "", scope: exportScopeAll, entireBinary: "entire", rawTranscript: true, historyIndex: history})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFilelessExportIndexesAndReadsWithoutSessionsTree(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", true)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpointBackedSessions(manifest) || len(manifest.Sources.Sessions.Sessions) != 1 {
		t.Fatalf("sessions = %+v", manifest.Sources.Sessions)
	}
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	if _, err := os.Stat(filepath.Join(brain, rel)); !os.IsNotExist(err) {
		t.Fatalf("persistent file exists: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(brain, "sessions")); err != nil {
		t.Fatal(err)
	}
	data, err := readCanonicalHistoryTranscript(context.Background(), brain, rel)
	if err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("git read: %v %q", err, data)
	}
	index, _, _, err := buildBrainHistoryIndexSnapshotContext(context.Background(), brain, opts.Now(), nil, manifest, brainstore.EmptySessionTombstones())
	if err != nil {
		t.Fatal(err)
	}
	expanded := 0
	for _, record := range index.Records {
		if record.Kind != conversationKind {
			continue
		}
		if _, err := expandConversationExchange(brain, record); err != nil {
			t.Fatal(err)
		}
		expanded++
	}
	if expanded == 0 {
		t.Fatal("no conversations indexed")
	}
	data, size, _, sha, err := readPatternCorpusTranscript(context.Background(), brain, rel)
	if err != nil || size != int64(len(conversationClaudeFixture)) || sha == "" || len(data) == 0 {
		t.Fatalf("pattern transcript: %d %s %v", size, sha, err)
	}
	// Stray files cannot override checkpoint content, but privacy verification
	// must report them even when there are no tombstones.
	if err := writeTranscriptFile(brain, rel, []byte("stray")); err != nil {
		t.Fatal(err)
	}
	data, err = readCanonicalHistoryTranscript(context.Background(), brain, rel)
	if err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("stray file overrode checkpoint: %q %v", data, err)
	}
	report, err := verifySessionPrivacy(brain)
	if err != nil || report.Clean {
		t.Fatalf("stray verify: %+v %v", report, err)
	}
}

func TestFilelessMigrationAndStandaloneBundle(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	runFilelessExport(t, opts, bundle, false)
	manifest, err := bmanifest.Load(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointBackedSessions(manifest) {
		t.Fatal("bundle must carry files")
	}
	// A persistent brain can outlive the worktree that originally exported it.
	manifest.RepoRoot = filepath.Join(t.TempDir(), "removed-worktree")
	if err := writeBrainManifestAndReadme(bundle, *manifest); err != nil {
		t.Fatal(err)
	}
	session := manifest.Sources.Sessions.Sessions[0]
	if err := os.MkdirAll(filepath.Dir(brain), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bundle, brain); err != nil {
		t.Fatal(err)
	}
	runFilelessExport(t, opts, "", false)
	if _, err := os.Stat(filepath.Join(brain, session.TranscriptPath)); !os.IsNotExist(err) {
		t.Fatalf("cache not swept: %v", err)
	}
	data, err := readCanonicalHistoryTranscript(context.Background(), brain, session.TranscriptPath)
	if err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("migration read: %q %v", data, err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := addTranscriptBundlePath(context.Background(), tw, newTranscriptStore(opts.Runner, brain), session.TranscriptPath, opts.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	header, err := tr.Next()
	if err != nil || header.Name != session.TranscriptPath {
		t.Fatalf("tar header: %+v %v", header, err)
	}
	data, err = io.ReadAll(tr)
	if err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("portable transcript: %q %v", data, err)
	}
}

func TestFilelessMigrationRetainsDivergentCache(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	runFilelessExport(t, opts, bundle, false)
	manifest, _ := bmanifest.Load(bundle)
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	if err := os.MkdirAll(filepath.Dir(brain), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bundle, brain); err != nil {
		t.Fatal(err)
	}
	// A live session's cached export can legitimately run ahead of its last
	// checkpoint; divergence must retain the file, not block every export.
	cached := filepath.Join(brain, filepath.FromSlash(rel))
	if err := os.WriteFile(cached, []byte(conversationClaudeFixture+`{"newer":true}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runFilelessExport(t, opts, "", false)
	if _, err := os.Stat(cached); err != nil {
		t.Fatal("lost retained divergent copy:", err)
	}
	data, err := readCanonicalHistoryTranscript(context.Background(), brain, rel)
	if err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("migration read: %q %v", data, err)
	}
	// The checkpoint-backed sweep on the next export must keep re-proving the
	// retained copy, not delete it as a stray.
	runFilelessExport(t, opts, "", false)
	if _, err := os.Stat(cached); err != nil {
		t.Fatal("retained copy swept by checkpoint-backed export:", err)
	}
	// Once the checkpoint holds every cached byte again, the file sweeps.
	if err := os.WriteFile(cached, []byte(conversationClaudeFixture), 0600); err != nil {
		t.Fatal(err)
	}
	runFilelessExport(t, opts, "", false)
	if _, err := os.Stat(cached); !os.IsNotExist(err) {
		t.Fatalf("reconciled cache not swept: %v", err)
	}
}

func TestFilelessMigrationSweepsCacheBehindCheckpoint(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	runFilelessExport(t, opts, bundle, false)
	manifest, _ := bmanifest.Load(bundle)
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	if err := os.MkdirAll(filepath.Dir(brain), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bundle, brain); err != nil {
		t.Fatal(err)
	}
	// The checkpoint can run ahead of the last disk export; a cached prefix
	// is fully recoverable from the checkpoint and must still be swept.
	cached := filepath.Join(brain, filepath.FromSlash(rel))
	if err := os.WriteFile(cached, []byte(conversationClaudeFixture[:len(conversationClaudeFixture)/2]), 0600); err != nil {
		t.Fatal(err)
	}
	runFilelessExport(t, opts, "", false)
	if _, err := os.Stat(cached); !os.IsNotExist(err) {
		t.Fatalf("stale cache prefix not swept: %v", err)
	}
	data, err := readCanonicalHistoryTranscript(context.Background(), brain, rel)
	if err != nil || string(data) != conversationClaudeFixture {
		t.Fatalf("migration read: %q %v", data, err)
	}
}

func TestFilelessMigrationRetainsUnrecoverableCache(t *testing.T) {
	opts, brain, repo := filelessCheckpointFixture(t)
	bundle := filepath.Join(t.TempDir(), "bundle")
	runFilelessExport(t, opts, bundle, false)
	manifest, _ := bmanifest.Load(bundle)
	rel := manifest.Sources.Sessions.Sessions[0].TranscriptPath
	if err := os.MkdirAll(filepath.Dir(brain), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bundle, brain); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "update-ref", "-d", v1MainRef)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("delete fixture ref: %v %s", err, out)
	}
	if inventory, err := planTranscriptCacheSweep(context.Background(), opts.Runner, brain, opts.Env.RepoRoot, nil); err == nil {
		if inventory != nil {
			inventory.Close()
		}
		t.Fatal("missing checkpoint must refuse migration")
	}
	if _, err := os.Stat(filepath.Join(brain, rel)); err != nil {
		t.Fatal("lost retained copy:", err)
	}
}

func TestFilelessDistillCacheSkipsReadsAndInvalidatesIdentity(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", false)
	manifest, _ := bmanifest.Load(brain)
	session := manifest.Sources.Sessions.Sessions[0]
	distillOpts := distillCommandOptions{checkpointBacked: true, transcriptMode: "raw", cacheSalt: "salt"}
	fingerprint := distillFingerprintForSession(session, "main", "", distillOpts)
	cache := distillCache{Version: distillCacheVersion, Sessions: map[string]string{distillSessionCacheKey("main", session.SessionID): fingerprint}}
	if err := saveDistillCache(brain, cache); err != nil {
		t.Fatal(err)
	}
	// Make any attempted read fail, while the manifest identity remains valid.
	manifest.RepoRoot = filepath.Join(t.TempDir(), "missing-repo")
	manifest.CheckpointGitDir = filepath.Join(manifest.RepoRoot, ".git")
	if err := writeBrainManifestAndReadme(brain, *manifest); err != nil {
		t.Fatal(err)
	}
	resolve := func(exportSession) string { return "main" }
	if err := validateDistillCanonicalInputs(context.Background(), brain, []exportSession{session}, distillOpts, resolve); err != nil {
		t.Fatal("cache hit read transcript:", err)
	}
	prepared := <-startSessionPrefetch(context.Background(), brain, "", nil, []exportSession{session}, cache, distillOpts, resolve)
	if !prepared.cached || prepared.readErr != nil {
		t.Fatalf("cached preparation: %+v", prepared)
	}
	plan, err := buildDistillPlanContext(context.Background(), brain, manifest, distillOpts, "salt")
	if err != nil || plan.CachedSessions != 1 {
		t.Fatalf("cached plan: %+v %v", plan, err)
	}
	for _, change := range []string{"checkpoint", "mode", "salt", "index"} {
		changed, options := session, distillOpts
		switch change {
		case "checkpoint":
			changed.LatestCheckpoint = "bbb222bbb222"
		case "mode":
			options.transcriptMode = "compact"
		case "salt":
			options.cacheSalt = "new"
		case "index":
			changed.SessionIndex++
		}
		if distillFingerprintForSession(changed, "main", "", options) == fingerprint {
			t.Fatalf("identity ignored %s", change)
		}
	}
	stones := brainstore.EmptySessionTombstones()
	stones.Excluded[session.SessionID] = sessionTombstone{At: opts.Now(), Reason: "purged"}
	if err := brainstore.SaveSessionTombstones(brain, stones); err != nil {
		t.Fatal(err)
	}
	if _, err := readCanonicalHistoryTranscript(context.Background(), brain, session.TranscriptPath); err == nil || !strings.Contains(err.Error(), "tombstoned") {
		t.Fatalf("gate: %v", err)
	}
}

func TestFilelessLegacyDistillCacheGrandfathersOnce(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", false)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	session := manifest.Sources.Sessions.Sessions[0]
	legacyOptions := distillCommandOptions{}
	legacySalt := distillCacheSalt("prompt", "reconcile", 0.8, legacyOptions)
	options := distillCommandOptions{checkpointBacked: true, transcriptMode: "raw", legacyCacheSalt: legacySalt}
	options.cacheSalt = distillCacheSalt("prompt", "reconcile", 0.8, options)
	if legacySalt == options.cacheSalt {
		t.Fatal("preprocessing version not included in checkpoint salt")
	}
	old := distillSessionFingerprint(session, "main", preprocessTranscriptForDistill(conversationClaudeFixture), legacySalt)
	key := distillSessionCacheKey("main", session.SessionID)
	cache := distillCache{Version: distillCacheVersion, Sessions: map[string]string{key: old}}
	prepared := <-startSessionPrefetch(context.Background(), brain, "", nil, []exportSession{session}, cache, options, func(exportSession) string { return "main" })
	if !prepared.cached || prepared.readErr != nil || prepared.fingerprint == old || !strings.HasPrefix(prepared.fingerprint, "checkpoint-sha256:") {
		t.Fatalf("legacy migration must produce identity cache hit: %+v", prepared)
	}
	options.legacyCacheSalt = "changed-configuration"
	if grandfatheredContentFingerprint(old, session, "main", preprocessTranscriptForDistill(conversationClaudeFixture), options) {
		t.Fatal("changed configuration grandfathered")
	}
}

func TestFilelessSourceIdentitySurvivesBundleMaterialization(t *testing.T) {
	source := &sessionSourceManifest{TranscriptStorage: transcriptStorageCheckpoint, TranscriptMode: "raw", Sessions: []exportSession{{SessionID: "s1", LatestCheckpoint: "cp1", TranscriptPath: "sessions/main/s1.jsonl"}}}
	before := sessionSourceFingerprint(source)
	source.TranscriptStorage = ""
	if sessionSourceFingerprint(source) != before {
		t.Fatal("materialization invalidated portable history receipts")
	}
	source.Sessions[0].SessionIndex++
	if sessionSourceFingerprint(source) == before {
		t.Fatal("source identity ignored checkpoint session index")
	}
}

func TestFilelessReadFinishRechecksTombstone(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", false)
	manifest, _ := bmanifest.Load(brain)
	session := manifest.Sources.Sessions.Sessions[0]
	_, finish, err := newTranscriptStore(opts.Runner, brain).Open(context.Background(), session.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	stones := brainstore.EmptySessionTombstones()
	stones.Excluded[session.SessionID] = sessionTombstone{At: opts.Now(), Reason: "purged"}
	if err := brainstore.SaveSessionTombstones(brain, stones); err != nil {
		t.Fatal(err)
	}
	if err := finish(); err == nil || !strings.Contains(err.Error(), "tombstoned") {
		t.Fatalf("finish published after purge: %v", err)
	}
	report, err := verifySessionPrivacy(brain)
	if err != nil || !report.Clean {
		t.Fatalf("file-less privacy verify: %+v %v", report, err)
	}
}
