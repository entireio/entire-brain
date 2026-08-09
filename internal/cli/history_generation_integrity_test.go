package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func assertHistoryManifestUnchanged(t *testing.T, brainDir string, before *historySourceManifest) {
	t.Helper()
	after, _ := activeHistoryProjection(t, brainDir)
	if after.IndexPath != before.IndexPath || after.IndexDigest != before.IndexDigest || after.ProjectionStatePath != before.ProjectionStatePath {
		t.Fatalf("history manifest changed: before=%+v after=%+v", before, after)
	}
}

func writeExternalProjectionCanaries(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		"canary.txt":            []byte("external canary\n"),
		historyIndexFileName:    []byte("external index\n"),
		projectionStateFileName: []byte("external receipts\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, files
}

func assertExternalProjectionCanaries(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	for name, expected := range want {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != string(expected) {
			t.Fatalf("external canary %s changed: err=%v got=%q want=%q", name, err, got, expected)
		}
	}
}

func TestHistoryProjectionPromotionRejectsFinalGenerationSymlink(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	previous, _ := activeHistoryProjection(t, brainDir)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	external, canaries := writeExternalProjectionCanaries(t)
	final := filepath.Join(brainDir, filepath.FromSlash(historyGenerationsDir), prepared.generation)
	if err := os.Symlink(external, final); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err = withBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("publish error = %v, want symlink rejection", err)
	}
	assertHistoryManifestUnchanged(t, brainDir, previous)
	assertExternalProjectionCanaries(t, external, canaries)
}

func TestHistoryProjectionPromotionRejectsSwappedStagingSymlink(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	previous, _ := activeHistoryProjection(t, brainDir)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeHistoryProjectionDirectory(brainDir, prepared.stagingDir); err != nil {
		t.Fatal(err)
	}
	external, canaries := writeExternalProjectionCanaries(t)
	staging := filepath.Join(brainDir, filepath.FromSlash(prepared.stagingDir))
	if err := os.Symlink(external, staging); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err = withBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("publish error = %v, want symlink rejection", err)
	}
	if cleanupErr := discardPreparedHistoryProjection(brainDir, prepared); cleanupErr == nil || !strings.Contains(strings.ToLower(cleanupErr.Error()), "symlink") {
		t.Fatalf("cleanup error = %v, want fail-closed symlink rejection", cleanupErr)
	}
	assertHistoryManifestUnchanged(t, brainDir, previous)
	assertExternalProjectionCanaries(t, external, canaries)
}

func TestHistoryProjectionPromotionPreservesUnknownOrphanContents(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	previous, _ := activeHistoryProjection(t, brainDir)
	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(brainDir, filepath.FromSlash(historyGenerationsDir), prepared.generation)
	if err := os.MkdirAll(final, 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(final, "operator-note.txt")
	if err := os.WriteFile(unknown, []byte("preserve me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = withBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	if err == nil || !strings.Contains(err.Error(), "unknown entry") {
		t.Fatalf("publish error = %v, want unknown-entry rejection", err)
	}
	assertHistoryManifestUnchanged(t, brainDir, previous)
	if data, readErr := os.ReadFile(unknown); readErr != nil || string(data) != "preserve me\n" {
		t.Fatalf("unknown orphan content changed: err=%v data=%q", readErr, data)
	}
}

func TestHistoryProjectionCleanupQuarantineRecovery(t *testing.T) {
	t.Run("manifest-active generation is restored", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		activeRel := filepath.ToSlash(filepath.Dir(source.IndexPath))
		active := filepath.Join(brainDir, filepath.FromSlash(activeRel))
		quarantine := active + ".removing"
		if err := os.Rename(active, quarantine); err != nil {
			t.Fatal(err)
		}
		if err := withBrainWriteLock(brainDir, func() error { return recoverHistoryProjectionQuarantines(brainDir) }); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(quarantine); !os.IsNotExist(err) {
			t.Fatalf("active quarantine survived recovery: %v", err)
		}
		if _, err := loadBrainHistoryIndex(brainDir, source); err != nil {
			t.Fatalf("restored active generation is unreadable: %v", err)
		}
	})

	t.Run("inactive staging quarantine is finished", func(t *testing.T) {
		brainDir, _, now := historyProjectionFixture(t)
		prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
		if err != nil {
			t.Fatal(err)
		}
		staging := filepath.Join(brainDir, filepath.FromSlash(prepared.stagingDir))
		quarantine := staging + ".removing"
		if err := os.Rename(staging, quarantine); err != nil {
			t.Fatal(err)
		}
		if err := withBrainWriteLock(brainDir, func() error { return recoverHistoryProjectionQuarantines(brainDir) }); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{staging, quarantine} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("inactive cleanup debris survived at %s: %v", path, err)
			}
		}
	})

	t.Run("unknown quarantine contents fail closed", func(t *testing.T) {
		brainDir, _, now := historyProjectionFixture(t)
		previous, _ := activeHistoryProjection(t, brainDir)
		prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
		if err != nil {
			t.Fatal(err)
		}
		staging := filepath.Join(brainDir, filepath.FromSlash(prepared.stagingDir))
		quarantine := staging + ".removing"
		if err := os.Rename(staging, quarantine); err != nil {
			t.Fatal(err)
		}
		unknown := filepath.Join(quarantine, "unknown.txt")
		if err := os.WriteFile(unknown, []byte("keep\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		err = withBrainWriteLock(brainDir, func() error { return recoverHistoryProjectionQuarantines(brainDir) })
		if err == nil || !strings.Contains(err.Error(), "unknown entry") {
			t.Fatalf("recovery error = %v, want unknown-entry rejection", err)
		}
		if data, readErr := os.ReadFile(unknown); readErr != nil || string(data) != "keep\n" {
			t.Fatalf("unknown quarantine content changed: err=%v data=%q", readErr, data)
		}
		assertHistoryManifestUnchanged(t, brainDir, previous)
	})

	t.Run("unknown-newer quarantine is preserved", func(t *testing.T) {
		brainDir, _, now := historyProjectionFixture(t)
		prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
		if err != nil {
			t.Fatal(err)
		}
		staging := filepath.Join(brainDir, filepath.FromSlash(prepared.stagingDir))
		quarantine := staging + ".removing"
		if err := os.Rename(staging, quarantine); err != nil {
			t.Fatal(err)
		}
		receipt := filepath.Join(quarantine, projectionStateFileName)
		future := []byte(`{"schema_version":999}`)
		if err := os.WriteFile(receipt, future, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := withBrainWriteLock(brainDir, func() error { return recoverHistoryProjectionQuarantines(brainDir) }); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(receipt); err != nil || string(data) != string(future) {
			t.Fatalf("unknown-newer quarantine was mutated: err=%v data=%q", err, data)
		}
	})
}

func TestHistoryIndexReaderRetriesManifestAfterPrunedGeneration(t *testing.T) {
	brainDir, transcriptRel, now := historyProjectionFixture(t)
	previous, _ := activeHistoryProjection(t, brainDir)
	staleSource := *previous
	transcript := filepath.Join(brainDir, filepath.FromSlash(transcriptRel))
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Learning: retry the current generation after pruning."}]}}` + "\n")
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append=%v close=%v", writeErr, closeErr)
	}
	changed := now.Add(2 * time.Minute)
	if err := os.Chtimes(transcript, changed, changed); err != nil {
		t.Fatal(err)
	}
	current, err := writeBrainHistoryIndexAndSource(brainDir, changed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if current.IndexPath == staleSource.IndexPath {
		t.Fatal("fixture did not produce a new generation")
	}
	if _, err := os.Lstat(filepath.Join(brainDir, filepath.FromSlash(staleSource.IndexPath))); !os.IsNotExist(err) {
		t.Fatalf("old generation was not pruned: %v", err)
	}

	index, err := loadBrainHistoryIndex(brainDir, &staleSource)
	if err != nil {
		t.Fatalf("stale-manifest reader did not retry current generation: %v", err)
	}
	if index.storageIdentity != current.IndexPath {
		t.Fatalf("reader storage identity = %q, want current %q", index.storageIdentity, current.IndexPath)
	}
	receipts, receiptState, err := loadProjectionStateChecked(brainDir, &staleSource)
	if err != nil || receiptState != projectionStateCurrent {
		t.Fatalf("stale-manifest receipt reader did not retry current generation: state=%s err=%v", receiptState, err)
	}
	if !receipts.GeneratedAt.Equal(current.GeneratedAt) {
		t.Fatalf("receipt generation = %s, want current %s", receipts.GeneratedAt, current.GeneratedAt)
	}
}

func TestHistoryIndexDigestValidationAndLegacyClassification(t *testing.T) {
	t.Run("new manifest validates", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, index := activeHistoryProjection(t, brainDir)
		if !validSHA256Identity(source.IndexDigest) || source.IndexDigest != index.contentIdentity {
			t.Fatalf("digest=%q content identity=%q", source.IndexDigest, index.contentIdentity)
		}
	})

	t.Run("legacy manifest remains readable and requests migration", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source.IndexPath)))
		if err != nil {
			t.Fatal(err)
		}
		if err := writeBrainRelativeFileAtomic(brainDir, historyIndexPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		legacy := *source
		legacy.IndexPath = historyIndexPath
		legacy.IndexDigest = ""
		if _, err := loadBrainHistoryIndex(brainDir, &legacy); err != nil {
			t.Fatalf("legacy manifest must remain readable: %v", err)
		}
		found := false
		for _, finding := range detectMemoryMigrations(brainDir, &legacy) {
			found = found || finding.Path == legacy.IndexPath && finding.State == memoryErrMigrationRequired
		}
		if !found {
			t.Fatalf("legacy index digest migration not reported: %+v", detectMemoryMigrations(brainDir, &legacy))
		}
	})

	t.Run("generation without digest is migration-required and never served", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		legacyGeneration := *source
		legacyGeneration.IndexDigest = ""
		if _, err := loadBrainHistoryIndex(brainDir, &legacyGeneration); err == nil || !strings.Contains(err.Error(), memoryErrMigrationRequired) {
			t.Fatalf("digestless generation error = %v", err)
		}
		found := false
		for _, finding := range detectMemoryMigrations(brainDir, &legacyGeneration) {
			found = found || finding.Path == legacyGeneration.IndexPath && finding.State == memoryErrMigrationRequired
		}
		if !found {
			t.Fatalf("digestless generation migration not reported: %+v", detectMemoryMigrations(brainDir, &legacyGeneration))
		}
	})

	t.Run("tamper is corrupt", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		path := filepath.Join(brainDir, filepath.FromSlash(source.IndexPath))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadBrainHistoryIndex(brainDir, source); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("tampered index error = %v", err)
		}
		found := false
		for _, finding := range detectMemoryMigrations(brainDir, source) {
			found = found || finding.Path == source.IndexPath && finding.State == memoryErrStateCorrupt
		}
		if !found {
			t.Fatalf("corrupt index not exposed by status findings: %+v", detectMemoryMigrations(brainDir, source))
		}
	})

	t.Run("malformed manifest digest is corrupt", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		malformed := *source
		malformed.IndexDigest = "sha256:not-a-digest"
		if _, err := loadBrainHistoryIndex(brainDir, &malformed); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("malformed digest error = %v", err)
		}
	})
}

func TestHistoryIndexReadIsBoundedRegularAndNoFollow(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(source.IndexPath)))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES", strconv.Itoa(len(data)-1))
		if _, err := loadBrainHistoryIndex(brainDir, source); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
			t.Fatalf("oversized index error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		path := filepath.Join(brainDir, filepath.FromSlash(source.IndexPath))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), historyIndexFileName)
		if err := os.WriteFile(external, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := loadBrainHistoryIndex(brainDir, source); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
			t.Fatalf("symlinked index error = %v", err)
		}
		got, err := os.ReadFile(external)
		if err != nil || string(got) != string(data) {
			t.Fatalf("external index changed: err=%v", err)
		}
	})

	t.Run("receipt symlink", func(t *testing.T) {
		brainDir, _, _ := historyProjectionFixture(t)
		source, _ := activeHistoryProjection(t, brainDir)
		path := filepath.Join(brainDir, filepath.FromSlash(source.ProjectionStatePath))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), projectionStateFileName)
		if err := os.WriteFile(external, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, state, err := loadProjectionStateChecked(brainDir, source); err == nil || state != projectionStateUnsafe {
			t.Fatalf("symlinked receipt state=%s error=%v", state, err)
		}
		got, err := os.ReadFile(external)
		if err != nil || string(got) != string(data) {
			t.Fatalf("external receipt changed: err=%v", err)
		}
	})
}

func TestProjectionMutationsRefuseUnsupportedActiveReceipts(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	futureData, futureDigest, err := encodeProjectionState(projectionState{SchemaVersion: projectionSchemaVersion + 1, GeneratedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.History.ProjectionStatePath))
	if err := os.WriteFile(receiptPath, futureData, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Sources.History.ProjectionStateDigest = futureDigest
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}

	assertUnsupported := func(label string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
			t.Fatalf("%s error = %v, want %s", label, err, memoryErrUnsupportedVersion)
		}
	}
	err = withBrainWriteLock(brainDir, func() error {
		_, reconcileErr := reconcileMemoryJobsForTestLocked(brainDir, "test", now)
		return reconcileErr
	})
	assertUnsupported("reconcile", err)
	maintenanceSnapshot, snapshotErr := prepareMemoryReconcileSnapshot(context.Background(), brainDir)
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	err = withBrainWriteLock(brainDir, func() error {
		_, maintenanceErr := maintainMemoryWorkerStateLocked(brainDir, now, maintenanceSnapshot)
		return maintenanceErr
	})
	assertUnsupported("maintenance", err)

	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = withBrainWriteLock(brainDir, func() error {
		_, publishErr := publishBrainHistoryProjectionLocked(brainDir, prepared, nil)
		return publishErr
	})
	assertUnsupported("publish", err)
	after, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Sources.History.ProjectionStatePath != manifest.Sources.History.ProjectionStatePath ||
		after.Sources.History.ProjectionStateDigest != futureDigest {
		t.Fatalf("unsupported active receipt was switched: before=%+v after=%+v", manifest.Sources.History, after.Sources.History)
	}
}

func TestSessionTranscriptDigestIsContainedRegularAndNoFollow(t *testing.T) {
	brainDir, rel, _ := historyProjectionFixture(t)
	if digest, err := sessionTranscriptDigest(brainDir, rel); err != nil || !validSHA256Identity(digest) {
		t.Fatalf("valid transcript digest=%q err=%v", digest, err)
	}
	external := filepath.Join(t.TempDir(), "external.jsonl")
	canary := []byte("external transcript canary\n")
	if err := os.WriteFile(external, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	aliasRel := "sessions/main/alias.jsonl"
	alias := filepath.Join(brainDir, filepath.FromSlash(aliasRel))
	if err := os.Symlink(external, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for name, unsafeRel := range map[string]string{
		"escape":    "../external.jsonl",
		"symlink":   aliasRel,
		"directory": "sessions/main",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sessionTranscriptDigest(brainDir, unsafeRel); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
				t.Fatalf("digest error = %v, want %s", err, memoryErrStateUnsafe)
			}
		})
	}
	got, err := os.ReadFile(external)
	if err != nil || string(got) != string(canary) {
		t.Fatalf("external transcript changed: err=%v data=%q", err, got)
	}
}

func TestMemoryRepairRecoversCorruptHistoryIndexDigest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	rel := "sessions/main/repair.jsonl"
	full := filepath.Join(storage.BrainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"repair corrupt history"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: rebuild from canonical sessions."}]}}` + "\n"
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now.Add(-time.Hour), RepoKey: storage.Key, DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now.Add(-time.Hour), DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "repair-session", Branch: "main", LatestCheckpoint: "cp", TranscriptPath: rel, CreatedAt: now.Add(-2 * time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now.Add(-time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	before, _ := activeHistoryProjection(t, storage.BrainDir)
	path := filepath.Join(storage.BrainDir, filepath.FromSlash(before.IndexPath))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, newMemoryRepairCommand(opts), "--json")
	if err != nil {
		t.Fatalf("repair failed: %v\n%s", err, out)
	}
	var payload struct {
		Rebuilt      bool `json:"rebuilt"`
		IndexCurrent bool `json:"index_current"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Rebuilt || !payload.IndexCurrent {
		t.Fatalf("repair payload = %+v\n%s", payload, out)
	}
	after, _ := activeHistoryProjection(t, storage.BrainDir)
	if after.IndexPath == before.IndexPath || !validSHA256Identity(after.IndexDigest) {
		t.Fatalf("repair did not publish a new integrity-protected generation: before=%+v after=%+v", before, after)
	}
}
