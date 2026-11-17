package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func checkedProjectionFixture(t *testing.T, state projectionState) (string, *historySourceManifest) {
	t.Helper()
	brainDir := t.TempDir()
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
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
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, projectionStateRel, data, 0o600); err != nil {
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
		if state != projectionStateUnsupported || err == nil || !strings.Contains(err.Error(), brainstore.UnsupportedVersionCode) || string(before) != string(after) {
			t.Fatalf("state=%s unchanged=%v err=%v", state, string(before) == string(after), err)
		}
		_, health := projectionReceiptHealth(brainDir, source)
		if health["state"] != projectionStateUnsupported || health["error_code"] != brainstore.UnsupportedVersionCode || health["action"] == "" {
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
		manifest, err := bmanifest.Load(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{SessionID: "session-b", Branch: "main", TranscriptPath: "sessions/main/session-b.jsonl"})
		if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
			t.Fatal(err)
		}
		if _, state, err := loadProjectionStateChecked(brainDir, source); state != projectionStateStale || err == nil || !strings.Contains(err.Error(), brainstore.SourceStaleCode) {
			t.Fatalf("state=%s err=%v", state, err)
		}
		_, health := projectionReceiptHealth(brainDir, source)
		if health["state"] != projectionStateStale || health["current"] != false || health["error_code"] != brainstore.SourceStaleCode || health["action"] == "" {
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
	if err := writeBrainManifestAndReadme(storage.BrainDir, exportManifest{SchemaVersion: bmanifest.SchemaVersion, RepoKey: storage.Key}); err != nil {
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
	if err := brainstore.WriteBrainRelativeFileAtomic(storage.BrainDir, memoryMigrationProgressRel, corruptProgress, 0o600); err != nil {
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
	want := map[string]string{"corrupt.json": brainstore.StateCorruptCode, "future.json": brainstore.UnsupportedVersionCode}
	for _, issue := range payload.Issues {
		base := filepath.Base(issue.Path)
		if expected, ok := want[base]; ok && issue.Code == expected && issue.Action != "" {
			delete(want, base)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing typed issues %v in %+v\n%s", want, payload.Issues, out)
	}
	if payload.MigrationProgress.State != "unavailable" || payload.MigrationProgress.ErrorCode != brainstore.StateCorruptCode {
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
		{name: "malformed", data: "{broken", code: brainstore.StateCorruptCode},
		{name: "trailing", data: `{"schema_version":1,"operation_id":"op:0123456789abcdef","started_at":"2026-08-09T00:00:00Z","completed":[]} {}`, code: brainstore.StateCorruptCode},
		{name: "newer", data: `{"schema_version":999,"operation_id":"op:0123456789abcdef","started_at":"2026-08-09T00:00:00Z","completed":[],"future_field":true}`, code: brainstore.UnsupportedVersionCode},
		{name: "duplicate completed", data: `{"schema_version":1,"operation_id":"op:0123456789abcdef","started_at":"2026-08-09T00:00:00Z","completed":["history/short-term-v1.json","history/short-term-v1.json"]}`, code: brainstore.StateCorruptCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, memoryMigrationProgressRel, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, present, err := loadMemoryMigrationProgress(brainDir); !present || err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("present=%v err=%v, want %s", present, err, tc.code)
			}
		})
	}
}

func TestGenerationPruningPreservesUnknownNewerReceipts(t *testing.T) {
	brainDir, _ := writeSessionNavigationFixture(t)
	futureGeneration := strings.Repeat("f", 40)
	futureDir := filepath.Join(brainDir, filepath.FromSlash(historystore.GenerationsDir), futureGeneration)
	if err := os.MkdirAll(futureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	futureReceipt := []byte(`{"schema_version":999}`)
	for path, data := range map[string][]byte{
		filepath.Join(futureDir, historystore.IndexFileName):            []byte(`{"generated_at":"2026-08-09T00:00:00Z","records":[]}`),
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
