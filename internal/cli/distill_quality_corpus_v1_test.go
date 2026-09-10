package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildDistillQualityCorpusV1CompleteUnsetAndConfirmationExclusion(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"}, fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "git@github.com:example/repo.git\n"}}}
	opts := Options{Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()}, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []exportSession{{SessionID: "private-s1", Branch: "main", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-time.Hour)}, {SessionID: "private-s2", Branch: "main", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now}}
	contents := []string{`{"type":"event_msg","payload":{"type":"user_message","message":"Always preserve rollback tokens ghp_12345678901234567890123456789012 from /Users/private-name/work/repo."}}`, `{"type":"event_msg","payload":{"type":"user_message","message":"Thanks."}}`}
	for i, s := range sessions {
		path := filepath.Join(storage.BrainDir, filepath.FromSlash(s.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents[i]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, DefaultBranch: "main", Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}}}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "development")
	result, err := buildDistillQualityCorpusV1(context.Background(), opts, repoDir, out, "development", "seed-v1", "", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sessions != 2 || result.Turns != 2 || result.ZeroCandidateSessions != 1 {
		t.Fatalf("manifest = %+v", result)
	}
	data, err := os.ReadFile(result.CorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-s1") || strings.Contains(string(data), "sessions/main") || strings.Contains(string(data), "ghp_123") || strings.Contains(string(data), "private-name") {
		t.Fatalf("public corpus leaked private source or secret: %s", data)
	}
	if strings.Count(string(data), `"should_emit":null`) != 2 || strings.Count(string(data), `"reference_facts":null`) != 2 {
		t.Fatalf("labels were not unset: %s", data)
	}
	var rows []distillQualityCorpusTurnV1
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		var row distillQualityCorpusTurnV1
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if len(rows[0].Candidates) == 0 || len(rows[1].Candidates) != 0 {
		t.Fatalf("candidate membership/zero candidate rows = %+v", rows)
	}
	for _, path := range []string{result.CorpusPath, result.PrivateSourceMapPath, result.FamilyInventoryPath, filepath.Join(out, "manifest.json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	privateData, _ := os.ReadFile(result.PrivateSourceMapPath)
	if !strings.Contains(string(privateData), "private-s1") || !strings.Contains(string(privateData), "sessions/main/s1.jsonl") {
		t.Fatal("private source map lost canonical provenance")
	}

	// Add a new session with the exact same source bytes under a different ID.
	// Confirmation must exclude both the known family and the duplicate source.
	duplicate := exportSession{SessionID: "private-s3", Branch: "main", TranscriptPath: "sessions/main/s3.jsonl", CreatedAt: now.Add(time.Hour)}
	duplicatePath := filepath.Join(storage.BrainDir, filepath.FromSlash(duplicate.TranscriptPath))
	if err := os.WriteFile(duplicatePath, []byte(contents[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, duplicate)
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	inv := distillQualityCorpusInventoryV1{Contract: "distill_development_family_inventory_v1", SchemaVersion: 1, FamilyDigests: []string{distillCandidateCacheDigestV2("family-v1\x00private-s1")}, SourceDigests: []string{distillCandidateCacheDigestV2(contents[0])}}
	invData, _ := json.Marshal(inv)
	invPath := filepath.Join(t.TempDir(), "prior.json")
	if err := os.WriteFile(invPath, invData, 0o600); err != nil {
		t.Fatal(err)
	}
	confirmation, err := buildDistillQualityCorpusV1(context.Background(), opts, repoDir, filepath.Join(t.TempDir(), "confirmation"), "confirmation", "seed-v1", invPath, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if confirmation.Sessions != 1 || confirmation.ExcludedContaminatedFamilies != 2 || confirmation.ZeroCandidateSessions != 1 {
		t.Fatalf("confirmation exclusion = %+v", confirmation)
	}
}

func TestBuildDistillQualityCorpusV1ConfirmationRequiresInventory(t *testing.T) {
	if _, err := buildDistillQualityCorpusV1(context.Background(), Options{}, ".", filepath.Join(t.TempDir(), "out"), "confirmation", "seed", "", "", "", time.Now()); err == nil {
		t.Fatal("confirmation accepted without prior development inventory")
	}
}
