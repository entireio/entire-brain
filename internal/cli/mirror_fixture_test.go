package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

const privacyCanary = "CANARY-XYZQ-SECRET-TOKEN"

// writePrivacyFixture builds a brain with two sessions: one clean, one whose
// request and response both carry the canary secret.
func writePrivacyFixture(t *testing.T) (brainDir string) {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	brainDir = t.TempDir()
	clean := "sessions/main/20260801T000000Z_clean.jsonl"
	secret := "sessions/main/20260802T000000Z_secret.jsonl"
	cleanBody := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Fix the export cursor"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: cursor reuses unchanged transcripts."}]}}
`
	secretBody := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"rotate the leaked key %s now"}]}}`+"\n"+
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: rotated %s and revoked the old credential."}]}}`+"\n", privacyCanary, privacyCanary)
	for rel, body := range map[string]string{clean: cleanBody, secret: secretBody} {
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now, RepoKey: "test/privacy", DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "clean-sess", Branch: "main", Agent: "claude", LatestCheckpoint: "cp1", TranscriptPath: clean, CreatedAt: now.Add(-2 * time.Hour)},
			{SessionID: "secret-sess", Branch: "main", Agent: "claude", LatestCheckpoint: "cp2", TranscriptPath: secret, CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

// assertCanaryAbsent proves the canary is gone from every retained local
// artifact: the history index JSON, the decompressed scan cache, FTS ranking,
// conversation retrieval, and the sessions tree.
func assertCanaryAbsent(t *testing.T, brainDir string) {
	t.Helper()
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	indexBytes, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(indexBytes), privacyCanary) {
		t.Fatal("canary survived in history/index.json")
	}
	cache := loadHistoryScanCache(brainDir)
	for rel, entry := range cache.Files {
		for _, record := range entry.Records {
			if strings.Contains(record.Summary, privacyCanary) {
				t.Fatalf("canary survived in scan cache entry %s", rel)
			}
		}
	}
	index, err := historystore.LoadIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if scored, ok := rankHistoryViaFTS(brainDir, index, "history", privacyCanary, 10); ok && len(scored) > 0 {
		t.Fatalf("canary reachable through FTS: %+v", scored)
	}
	if results, err := retrieveConversation(brainDir, privacyCanary, 10, retrieval.ModeLexical, retrievalOptions{}); err == nil {
		for _, result := range results {
			if strings.Contains(result.Text, privacyCanary) {
				t.Fatalf("canary reachable through conversation retrieval: %+v", result)
			}
		}
	}
	sessionsRoot := filepath.Join(brainDir, brainstore.SessionsDirName)
	_ = filepath.Walk(sessionsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), privacyCanary) {
			t.Fatalf("canary survived in exported transcript %s", path)
		}
		return nil
	})
}

func privacyRegressionCommandFixture(t *testing.T, now time.Time) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:acme/privacy-regression.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:       repoDir,
			PluginDataDir:  t.TempDir(),
			PluginCacheDir: t.TempDir(),
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	fixture := writePrivacyFixture(t)
	if err := os.MkdirAll(filepath.Dir(storage.BrainDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture, storage.BrainDir); err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

func privacyTreeDigest(t *testing.T, root string) [32]byte {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		_, _ = h.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func hasRetrievalCaveat(result unifiedResult, kind string) bool {
	for _, caveat := range result.Caveats {
		if caveat.Kind == kind {
			return true
		}
	}
	return false
}
