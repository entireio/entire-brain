package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkedProjectionFixture(t *testing.T, state projectionState) (string, *historySourceManifest) {
	t.Helper()
	brainDir := t.TempDir()
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		RepoKey:       "test/receipt-health",
		Sources: &brainSources{Sessions: &sessionSourceManifest{
			DefaultBranch: "main",
			Sessions: []exportSession{{
				SessionID: "session-a", Branch: "main", TranscriptPath: "sessions/main/session-a.jsonl",
			}},
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	data, digest, err := encodeProjectionState(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(brainDir, projectionStateRel, data, 0o600); err != nil {
		t.Fatal(err)
	}
	source := &historySourceManifest{
		ProjectionStatePath:   projectionStateRel,
		ProjectionStateDigest: digest,
		SessionsFingerprint:   brainSessionsFingerprint(brainDir),
	}
	return brainDir, source
}

func validProjectionStateForTest(now time.Time) projectionState {
	return projectionState{
		SchemaVersion: projectionSchemaVersion, ReconcilerVersion: projectionReconcilerVersion, GeneratedAt: now,
		Sessions: []projectionReceipt{{
			SessionRef: "conversation-session:session-a", SessionID: "session-a", Branch: "main",
			InputDigest: "sha256:" + strings.Repeat("a", 64), ExchangeCount: 1, CompletedAt: now,
		}},
	}
}

func TestProjectionStateCheckedClassifiesFailures(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	t.Run("absent", func(t *testing.T) {
		if _, state, err := loadProjectionStateChecked(t.TempDir(), nil); state != projectionStateAbsent || err != nil {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("current", func(t *testing.T) {
		brainDir, source := checkedProjectionFixture(t, validProjectionStateForTest(now))
		if _, state, err := loadProjectionStateChecked(brainDir, source); state != projectionStateCurrent || err != nil {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		future := validProjectionStateForTest(now)
		future.SchemaVersion = projectionSchemaVersion + 1
		brainDir, source := checkedProjectionFixture(t, future)
		before, _ := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(projectionStateRel)))
		_, state, err := loadProjectionStateChecked(brainDir, source)
		after, _ := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(projectionStateRel)))
		if state != projectionStateUnsupported || err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) || string(before) != string(after) {
			t.Fatalf("state=%s unchanged=%v err=%v", state, string(before) == string(after), err)
		}
		_, health := projectionReceiptHealth(brainDir, source)
		if health["state"] != projectionStateUnsupported || health["error_code"] != memoryErrUnsupportedVersion || health["action"] == "" {
			t.Fatalf("unsupported health=%+v", health)
		}
	})
	t.Run("corrupt unsorted", func(t *testing.T) {
		bad := validProjectionStateForTest(now)
		bad.Sessions = append([]projectionReceipt{{
			SessionRef: "conversation-session:z", SessionID: "z", InputDigest: "sha256:" + strings.Repeat("b", 64), CompletedAt: now,
		}}, bad.Sessions...)
		brainDir, source := checkedProjectionFixture(t, bad)
		if _, state, err := loadProjectionStateChecked(brainDir, source); state != projectionStateCorrupt || err == nil {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("unsafe", func(t *testing.T) {
		_, source := checkedProjectionFixture(t, validProjectionStateForTest(now))
		source.ProjectionStatePath = "../outside.json"
		if _, state, err := loadProjectionStateChecked(t.TempDir(), source); state != projectionStateUnsafe || err == nil {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("manifest digest stale", func(t *testing.T) {
		brainDir, source := checkedProjectionFixture(t, validProjectionStateForTest(now))
		source.ProjectionStateDigest = "sha256:" + strings.Repeat("0", 64)
		if _, state, err := loadProjectionStateChecked(brainDir, source); state != projectionStateStale || err == nil {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("canonical sessions stale", func(t *testing.T) {
		brainDir, source := checkedProjectionFixture(t, validProjectionStateForTest(now))
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{SessionID: "session-b", Branch: "main", TranscriptPath: "sessions/main/session-b.jsonl"})
		if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
			t.Fatal(err)
		}
		if _, state, err := loadProjectionStateChecked(brainDir, source); state != projectionStateStale || err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) {
			t.Fatalf("state=%s err=%v", state, err)
		}
		_, health := projectionReceiptHealth(brainDir, source)
		if health["state"] != projectionStateStale || health["current"] != false || health["error_code"] != memoryErrSourceStale || health["action"] == "" {
			t.Fatalf("stale health=%+v", health)
		}
	})
}

func TestMemoryStatusReportsJobIssuesWithoutMutation(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, RepoKey: storage.Key}); err != nil {
		t.Fatal(err)
	}
	jobsDir := filepath.Join(storage.BrainDir, filepath.FromSlash(memoryJobsDirRel))
	if err := os.MkdirAll(jobsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"corrupt.json": []byte("{broken"),
		"future.json":  []byte(`{"schema_version":999,"job_id":"future","session_ref":"conversation-session:future"}`),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(jobsDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	corruptProgress := []byte("{broken")
	if err := writeBrainRelativeFileAtomic(storage.BrainDir, memoryMigrationProgressRel, corruptProgress, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, newMemoryStatusCommand(opts), "--json")
	if err != nil {
		t.Fatalf("status must remain available: %v\n%s", err, out)
	}
	var payload struct {
		Issues            []memoryHealthIssue `json:"state_issues"`
		MigrationProgress struct {
			State     string `json:"state"`
			ErrorCode string `json:"error_code"`
		} `json:"migration_progress"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"corrupt.json": memoryErrStateCorrupt, "future.json": memoryErrUnsupportedVersion}
	for _, issue := range payload.Issues {
		base := filepath.Base(issue.Path)
		if expected, ok := want[base]; ok && issue.Code == expected && issue.Action != "" {
			delete(want, base)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing typed issues %v in %+v\n%s", want, payload.Issues, out)
	}
	if payload.MigrationProgress.State != "unavailable" || payload.MigrationProgress.ErrorCode != memoryErrStateCorrupt {
		t.Fatalf("migration progress health=%+v\n%s", payload.MigrationProgress, out)
	}
	for name, before := range files {
		after, readErr := os.ReadFile(filepath.Join(jobsDir, name))
		if readErr != nil || string(after) != string(before) {
			t.Fatalf("status mutated %s: err=%v before=%q after=%q", name, readErr, before, after)
		}
	}
	if after, err := os.ReadFile(filepath.Join(storage.BrainDir, filepath.FromSlash(memoryMigrationProgressRel))); err != nil || string(after) != string(corruptProgress) {
		t.Fatalf("status mutated corrupt migration progress: err=%v data=%q", err, after)
	}
}

func TestMemoryMigrationProgressFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		code string
	}{
		{name: "malformed", data: "{broken", code: memoryErrStateCorrupt},
		{name: "trailing", data: `{"schema_version":1,"operation_id":"op:0123456789abcdef","started_at":"2026-08-09T00:00:00Z","completed":[]} {}`, code: memoryErrStateCorrupt},
		{name: "newer", data: `{"schema_version":999,"operation_id":"op:0123456789abcdef","started_at":"2026-08-09T00:00:00Z","completed":[],"future_field":true}`, code: memoryErrUnsupportedVersion},
		{name: "duplicate completed", data: `{"schema_version":1,"operation_id":"op:0123456789abcdef","started_at":"2026-08-09T00:00:00Z","completed":["history/short-term-v1.json","history/short-term-v1.json"]}`, code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			if err := writeBrainRelativeFileAtomic(brainDir, memoryMigrationProgressRel, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, present, err := loadMemoryMigrationProgress(brainDir); !present || err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("present=%v err=%v, want %s", present, err, tc.code)
			}
		})
	}
}

func TestMemoryMigrationInventoryRejectsAbstractDirectoryAlias(t *testing.T) {
	brainDir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel))); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	findings := detectMemoryMigrations(brainDir, nil)
	found := false
	for _, finding := range findings {
		found = found || finding.Path == abstractsDirRel && finding.State == memoryErrStateUnsafe
	}
	if !found {
		t.Fatalf("unsafe abstract directory missing from migration findings: %+v", findings)
	}
}

func validAbstractForView(view conversationSessionView, digest string) sessionAbstract {
	now := time.Date(2026, 8, 9, 14, 0, 0, 0, time.UTC)
	artifact := sessionAbstract{
		SchemaVersion: abstractSchemaVersion, SessionRef: view.Ref, SessionDigest: digest, GeneratedAt: now,
		Overview: abstractStatement{Text: "validated navigation", EvidenceIDs: []string{view.Records[0].ID}},
		Coverage: struct {
			TotalTurns     int                     `json:"total_turns"`
			IncludedTurns  int                     `json:"included_turns"`
			IncludedRanges []abstractCoverageRange `json:"included_ranges,omitempty"`
		}{TotalTurns: len(view.Records), IncludedTurns: len(view.Records), IncludedRanges: []abstractCoverageRange{{StartTurn: view.Records[0].TurnOrdinal, EndTurn: view.Records[len(view.Records)-1].TurnOrdinal}}},
	}
	artifact.Generator.Kind = "local"
	artifact.Generator.Provider = "ollama"
	artifact.Generator.Model = "test-1"
	artifact.Generator.ContractVersion = 1
	return artifact
}

func writeAbstractBytesForTest(t *testing.T, brainDir, digest string, data []byte) {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(abstractRel(digest)))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAbstractReadsFailClosedOnAdversarialArtifacts(t *testing.T) {
	newFixture := func(t *testing.T) (string, conversationSessionView, string) {
		brainDir, _ := writeSessionNavigationFixture(t)
		view := sessionViewForTest(t, brainDir)
		if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "fake"}}); err != nil {
			t.Fatal(err)
		}
		return brainDir, view, sessionViewDigest(view)
	}
	t.Run("fabricated citation", func(t *testing.T) {
		brainDir, view, digest := newFixture(t)
		artifact := validAbstractForView(view, digest)
		artifact.Overview.EvidenceIDs = []string{"conversation:fabricated"}
		data, _ := json.Marshal(artifact)
		writeAbstractBytesForTest(t, brainDir, digest, append(data, '\n'))
		if status, exposed := sessionAbstractStatus(brainDir, view); status != abstractStatusCorrupt || exposed != nil {
			t.Fatalf("status=%s exposed=%+v", status, exposed)
		}
	})
	t.Run("session ref mismatch", func(t *testing.T) {
		brainDir, view, digest := newFixture(t)
		artifact := validAbstractForView(view, digest)
		artifact.SessionRef = "conversation-session:other"
		data, _ := json.Marshal(artifact)
		writeAbstractBytesForTest(t, brainDir, digest, append(data, '\n'))
		if status, exposed := sessionAbstractStatus(brainDir, view); status != abstractStatusCorrupt || exposed != nil {
			t.Fatalf("status=%s exposed=%+v", status, exposed)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		brainDir, view, digest := newFixture(t)
		artifact := validAbstractForView(view, digest)
		data, _ := json.Marshal(artifact)
		data = append(data, []byte(strings.Repeat(" ", abstractArtifactMax))...)
		writeAbstractBytesForTest(t, brainDir, digest, data)
		if status, exposed := sessionAbstractStatus(brainDir, view); status != abstractStatusCorrupt || exposed != nil {
			t.Fatalf("status=%s exposed=%+v", status, exposed)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		brainDir, view, digest := newFixture(t)
		artifact := validAbstractForView(view, digest)
		artifact.SchemaVersion++
		data, _ := json.Marshal(artifact)
		writeAbstractBytesForTest(t, brainDir, digest, append(data, '\n'))
		if status, exposed := sessionAbstractStatus(brainDir, view); status != abstractStatusUnsupported || exposed != nil {
			t.Fatalf("status=%s exposed=%+v", status, exposed)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		brainDir, view, digest := newFixture(t)
		artifact := validAbstractForView(view, digest)
		data, _ := json.Marshal(artifact)
		target := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(target, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(brainDir, filepath.FromSlash(abstractRel(digest)))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if status, exposed := sessionAbstractStatus(brainDir, view); status != memoryErrStateUnsafe || exposed != nil {
			t.Fatalf("status=%s exposed=%+v", status, exposed)
		}
		inventory := loadSessionAbstractInventory(brainDir)
		if len(inventory.Issues) != 1 || inventory.Issues[0].Code != memoryErrStateUnsafe {
			t.Fatalf("inventory issues=%+v", inventory.Issues)
		}
	})
	t.Run("stale digest", func(t *testing.T) {
		brainDir, view, _ := newFixture(t)
		staleDigest := "sha256:" + strings.Repeat("b", 64)
		artifact := validAbstractForView(view, staleDigest)
		data, _ := json.Marshal(artifact)
		writeAbstractBytesForTest(t, brainDir, staleDigest, append(data, '\n'))
		if status, exposed := sessionAbstractStatus(brainDir, view); status != abstractStatusStale || exposed != nil {
			t.Fatalf("status=%s exposed=%+v", status, exposed)
		}
	})
}

func TestAbstractResolverScansInventoryOncePerGet(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	original := sessionAbstractReadDirectory
	defer func() { sessionAbstractReadDirectory = original }()
	scans := 0
	sessionAbstractReadDirectory = func(brainDir string) (memoryStateDirectory, error) {
		scans++
		return original(brainDir)
	}
	found, missing, err := getUnifiedBatchOptions(t.TempDir(), brainDir, "main", []string{view.Ref}, getOptions{})
	if err != nil || len(found) != 1 || len(missing) != 0 {
		t.Fatalf("get found=%d missing=%v err=%v", len(found), missing, err)
	}
	if scans != 1 {
		t.Fatalf("abstract inventory scans=%d, want exactly one per get operation", scans)
	}
}

func TestPrivacyVerifyFailsClosedOnUnsafeAbstractInventory(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	stones := emptySessionTombstones()
	stones.Excluded["nav-sess"] = sessionTombstone{At: time.Now().UTC()}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	nameHash := sha256.Sum256([]byte("unsafe"))
	name := hex.EncodeToString(nameHash[:]) + ".json"
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	if _, err := verifySessionPrivacy(brainDir); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("privacy verification must fail closed on unsafe abstract entry: %v", err)
	}
}

func TestAbstractInventoryCeilingReportsDegradedBeforeProvider(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	view := sessionViewForTest(t, brainDir)
	if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{Enabled: true, Provider: "unavailable"}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(brainDir, filepath.FromSlash(abstractsDirRel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= abstractInventoryMaxFiles; i++ {
		if err := os.WriteFile(filepath.Join(dir, "padding-"+strings.Repeat("0", 8)+time.Unix(int64(i), 0).UTC().Format("150405.000000000")+".txt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolver := newSessionAbstractResolver(brainDir)
	if !resolver.inventory.Truncated || resolver.inventory.Scanned > abstractInventoryMaxFiles {
		t.Fatalf("inventory truncated=%v scanned=%d", resolver.inventory.Truncated, resolver.inventory.Scanned)
	}
	if status, exposed := resolver.status(view); status != abstractStatusDegraded || exposed != nil {
		t.Fatalf("status=%s exposed=%+v", status, exposed)
	}
}

func TestMemoryRepairRefusesFalseSuccessWhenPrivacyRemainsDirty(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 16, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	rel := "sessions/main/private.jsonl"
	full := filepath.Join(storage.BrainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"private repair canary\"}]}}\n")
	if err := os.WriteFile(full, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now.Add(-time.Hour), RepoKey: storage.Key, DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now.Add(-time.Hour), DefaultBranch: "main", Sessions: []exportSession{{
			SessionID: "private-session", Branch: "main", TranscriptPath: rel, CreatedAt: now.Add(-2 * time.Hour),
		}}}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now.Add(-time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	stones := emptySessionTombstones()
	stones.Excluded["private-session"] = sessionTombstone{At: now, Reason: "excluded"}
	if err := saveSessionTombstones(storage.BrainDir, stones); err != nil {
		t.Fatal(err)
	}
	pre, err := verifySessionPrivacy(storage.BrainDir)
	if err != nil || pre.Clean {
		t.Fatalf("dirty precondition clean=%v err=%v findings=%+v", pre.Clean, err, pre.Findings)
	}
	oldRebuild := rebuildMemoryProjection
	defer func() { rebuildMemoryProjection = oldRebuild }()
	rebuildMemoryProjection = func(string, time.Time) error { return nil }
	out, err := execute(t, newMemoryRepairCommand(opts))
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) || !strings.Contains(err.Error(), "entire brain privacy purge <session-id>") {
		t.Fatalf("repair must return actionable dirty-privacy failure: err=%v out=%s", err, out)
	}
	if strings.Contains(out, "repair: rebuilt") {
		t.Fatalf("repair falsely reported success: %s", out)
	}
}

func TestGenerationPruningPreservesUnknownNewerReceipts(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	futureGeneration := strings.Repeat("f", 40)
	futureDir := filepath.Join(brainDir, filepath.FromSlash(historyGenerationsDir), futureGeneration)
	if err := os.MkdirAll(futureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	futureReceipt := []byte(`{"schema_version":999}`)
	for path, data := range map[string][]byte{
		filepath.Join(futureDir, historyIndexFileName):                  []byte(`{"generated_at":"2026-08-09T00:00:00Z","records":[]}`),
		filepath.Join(futureDir, projectionStateFileName):               futureReceipt,
		filepath.Join(brainDir, filepath.FromSlash(projectionStateRel)): futureReceipt,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneInactiveHistoryGenerations(brainDir)
	for _, path := range []string{filepath.Join(futureDir, projectionStateFileName), filepath.Join(brainDir, filepath.FromSlash(projectionStateRel))} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != string(futureReceipt) {
			t.Fatalf("unknown-newer receipt was mutated at %s: err=%v data=%q", path, err, data)
		}
		if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pruneInactiveHistoryGenerations(brainDir)
	for _, path := range []string{filepath.Join(futureDir, projectionStateFileName), filepath.Join(brainDir, filepath.FromSlash(projectionStateRel))} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "{corrupt" {
			t.Fatalf("unreadable receipt was mutated at %s: err=%v data=%q", path, err, data)
		}
	}
}
