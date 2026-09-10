package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildDistillQualitySnapshotV1PairedPrivateStores(t *testing.T) {
	now := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
	}}
	opts := Options{Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()}, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []exportSession{
		{SessionID: "keep", Branch: "main", TranscriptPath: "sessions/main/keep.jsonl", CreatedAt: now.Add(-time.Hour)},
		{SessionID: "zero", Branch: "main", TranscriptPath: "sessions/main/zero.jsonl", CreatedAt: now},
		{SessionID: "excluded", Branch: "main", TranscriptPath: "sessions/main/excluded.jsonl", CreatedAt: now.Add(time.Hour)},
	}
	contents := map[string][]byte{
		"keep":     []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"Always keep rollback verification."}}`),
		"zero":     []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"Thanks."}}`),
		"excluded": []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"Secret tombstoned instruction."}}`),
	}
	for _, s := range sessions {
		path := filepath.Join(storage.BrainDir, filepath.FromSlash(s.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents[s.SessionID], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: storage.Key, DefaultBranch: "main", Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}, Facts: &factSourceManifest{Facts: 99, Proposals: 3, TaxonomyPath: factsTaxonomyPath}}}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if err := writeFactTaxonomy(storage.BrainDir, defaultFactTaxonomy(now)); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(storage.BrainDir, factsDirName+"/"+factsFileName, []byte("private fact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(storage.BrainDir, "facts/proposals/main.ndjson", []byte("private proposal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tombstonePrivacyFixtureSession(t, storage.BrainDir, "excluded")

	out := filepath.Join(t.TempDir(), "snapshot")
	result, err := buildDistillQualitySnapshotV1(context.Background(), opts, repoDir, out, "main", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.SelectedSessions != 2 || result.ZeroCandidateSessions != 1 || !result.PrivateSourceSnapshot || result.ReferenceLabelsComplete || result.ReleaseGateEvidence {
		t.Fatalf("manifest = %+v", result)
	}
	if !validSHA256Identity(result.SourceScopeDigest) || !validSHA256Identity(result.TaxonomyDigest) || !validSHA256Identity(result.TaxonomySnapshotDigest) {
		t.Fatalf("unbound digests: %+v", result)
	}
	if len(result.Arms) != 2 {
		t.Fatalf("arms = %+v", result.Arms)
	}
	var firstKeep []byte
	var firstTaxonomy []byte
	for _, arm := range result.Arms {
		info, err := os.Stat(arm.BrainDir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("brain directory mode: %v %v", info, err)
		}
		armManifest, err := loadBrainManifest(arm.BrainDir)
		if err != nil {
			t.Fatal(err)
		}
		if armManifest.Sources == nil || armManifest.Sources.Sessions == nil || len(armManifest.Sources.Sessions.Sessions) != 2 || armManifest.Sources.Facts != nil {
			t.Fatalf("arm source manifest leaked state: %+v", armManifest.Sources)
		}
		for _, forbidden := range []string{factsDirName + "/" + factsFileName, "facts/proposals/main.ndjson", distillCachePath, distillCandidateResultCacheV2Path} {
			if _, err := os.Lstat(filepath.Join(arm.BrainDir, filepath.FromSlash(forbidden))); !os.IsNotExist(err) {
				t.Fatalf("arm contains forbidden state %s: %v", forbidden, err)
			}
		}
		got, err := os.ReadFile(filepath.Join(arm.BrainDir, "sessions/main/keep.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if firstKeep == nil {
			firstKeep = got
		} else if !bytes.Equal(firstKeep, got) {
			t.Fatal("paired arm source bytes differ")
		}
		if !bytes.Equal(got, contents["keep"]) {
			t.Fatal("arm source differs from canonical bytes")
		}
		taxonomyBytes, err := os.ReadFile(filepath.Join(arm.BrainDir, factsTaxonomyPath))
		if err != nil {
			t.Fatal(err)
		}
		if distillQualitySHA256V1(taxonomyBytes) != result.TaxonomySnapshotDigest {
			t.Fatal("arm taxonomy bytes do not match pinned snapshot digest")
		}
		if firstTaxonomy == nil {
			firstTaxonomy = taxonomyBytes
		} else if !bytes.Equal(firstTaxonomy, taxonomyBytes) {
			t.Fatal("paired arm taxonomy bytes differ")
		}
		if _, err := os.Stat(filepath.Join(arm.BrainDir, "sessions/main/excluded.jsonl")); !os.IsNotExist(err) {
			t.Fatalf("tombstoned source copied: %v", err)
		}
		for _, privateFile := range []string{filepath.Join(arm.BrainDir, "sessions/main/keep.jsonl"), filepath.Join(arm.BrainDir, factsTaxonomyPath), filepath.Join(arm.BrainDir, exportManifestFileName)} {
			info, err := os.Stat(privateFile)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("%s mode = %o", privateFile, info.Mode().Perm())
			}
		}

		// Exercise the real dry-run command against the arm's isolated plugin roots.
		armOpts := opts
		armOpts.Env.PluginConfigDir, armOpts.Env.PluginDataDir, armOpts.Env.PluginStateDir, armOpts.Env.PluginCacheDir = arm.ConfigDir, arm.DataDir, arm.StateDir, arm.CacheDir
		cmd := newDistillCommand(armOpts)
		var commandOut bytes.Buffer
		cmd.SetOut(&commandOut)
		cmd.SetErr(&commandOut)
		cmd.SetArgs([]string{"--dry-run", "--pipeline", "candidates", "--agent", "command", "--agent-command", "true", repoDir})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%s arm dry-run: %v\n%s", arm.Name, err, commandOut.String())
		}
		if !strings.Contains(commandOut.String(), "distill dry-run: 2 sessions") {
			t.Fatalf("%s arm selected wrong scope: %s", arm.Name, commandOut.String())
		}
	}
	if info, err := os.Stat(filepath.Join(out, "manifest.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("top manifest mode: %v %v", info, err)
	}
	manifest.Sources.Sessions.Sessions[0].Kind = "review"
	manifest.Sources.Sessions.Sessions[0].LatestCheckpoint = "authority-changed"
	manifest.Sources.Sessions.Sessions[0].FilesTouched = []string{"different.go"}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	changed, err := buildDistillQualitySnapshotV1(context.Background(), opts, repoDir, filepath.Join(t.TempDir(), "changed-authority"), "main", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SourceScopeDigest == result.SourceScopeDigest {
		t.Fatal("source scope digest ignored changed session authority metadata")
	}
}

func TestBuildDistillQualitySnapshotV1RejectsOutputInsideRepository(t *testing.T) {
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
	}}
	opts := Options{Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()}, Runner: runner, Now: time.Now}
	_, err := buildDistillQualitySnapshotV1(context.Background(), opts, repoDir, filepath.Join(repoDir, "snapshot"), "", "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Fatalf("inside-repository output error = %v", err)
	}
	storage, storageErr := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if storageErr != nil {
		t.Fatal(storageErr)
	}
	_, err = buildDistillQualitySnapshotV1(context.Background(), opts, repoDir, filepath.Join(storage.BrainDir, "snapshot"), "", "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "outside the Brain") {
		t.Fatalf("inside-Brain output error = %v", err)
	}
}

func TestBuildDistillQualitySnapshotV1RejectsAggregateSourceOverflowBeforeWrites(t *testing.T) {
	now := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
	}}
	opts := Options{Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()}, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"type":"event_msg","payload":{"type":"user_message","message":"Always verify rollback."}}`)
	sessions := []exportSession{
		{SessionID: "one", Branch: "main", TranscriptPath: "sessions/main/one.jsonl", CreatedAt: now.Add(-time.Minute)},
		{SessionID: "two", Branch: "main", TranscriptPath: "sessions/main/two.jsonl", CreatedAt: now},
	}
	for _, source := range sessions {
		path := filepath.Join(storage.BrainDir, filepath.FromSlash(source.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, DefaultBranch: "main", Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}}}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "snapshot")
	_, err = buildDistillQualitySnapshotLimitedV1(context.Background(), opts, repoDir, out, "", "", now, int64(len(data)+1))
	if err == nil || !strings.Contains(err.Error(), "narrow the scope with --branch or --session") {
		t.Fatalf("aggregate overflow error = %v", err)
	}
	if _, statErr := os.Lstat(out); !os.IsNotExist(statErr) {
		t.Fatalf("overflow created output directory before validation: %v", statErr)
	}
}

func TestDistillQualitySnapshotSourceIdentityBindsAuthorityMetadata(t *testing.T) {
	base := exportSession{SessionID: "same", Branch: "main", Kind: "coding", Agent: "codex", LatestCheckpoint: "cp-1", FilesTouched: []string{"a.go"}, TranscriptPath: "sessions/main/same.jsonl"}
	digest := distillQualitySHA256V1([]byte("identical transcript bytes"))
	want, err := distillQualitySnapshotSourceIdentityBytesV1(base, "main", digest)
	if err != nil {
		t.Fatal(err)
	}
	variants := []exportSession{
		func() exportSession { changed := base; changed.Kind = "review"; return changed }(),
		func() exportSession { changed := base; changed.LatestCheckpoint = "cp-2"; return changed }(),
		func() exportSession { changed := base; changed.FilesTouched = []string{"b.go"}; return changed }(),
	}
	for _, variant := range variants {
		got, err := distillQualitySnapshotSourceIdentityBytesV1(variant, "main", digest)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(got, want) {
			t.Fatalf("authority metadata change did not alter source identity: %+v", variant)
		}
	}
}
