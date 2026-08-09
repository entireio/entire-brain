package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeForwardStateFixture(t *testing.T, brainDir, rel string, data []byte) string {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireForwardStateBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("state changed\nwant: %q\n got: %q", want, got)
	}
}

func TestCoordinatorStatePreservesUnknownCorruptAndUnsafeLeaves(t *testing.T) {
	now := time.Date(2026, 8, 9, 21, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{name: "unknown-newer", data: []byte(`{"schema_version":2,"future":true}` + "\n"), code: memoryErrUnsupportedVersion},
		{name: "corrupt-current", data: []byte(`{"schema_version":1,"future":true}` + "\n"), code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			path := writeForwardStateFixture(t, brainDir, memoryCoordinatorStateRel, tc.data)
			coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
			if coordinator != nil {
				coordinator.close(now, "failed")
			}
			if err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("acquire error = %v, want %s", err, tc.code)
			}
			requireForwardStateBytes(t, path, tc.data)
		})
	}

	brainDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "coordinator-canary")
	canary := []byte("canary")
	if err := os.WriteFile(outside, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(memoryCoordinatorStateRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if coordinator, err := acquireMemoryCoordinator(brainDir, now, "test"); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		if coordinator != nil {
			coordinator.close(now, "failed")
		}
		t.Fatalf("unsafe acquire error = %v", err)
	}
	requireForwardStateBytes(t, outside, canary)
}

func TestCoordinatorStateCurrentVersionRecoversUnderCoordinatorLock(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 21, 10, 0, 0, time.UTC)
	old := memoryCoordinatorState{SchemaVersion: memoryCoordinatorSchema, OwnerToken: "old-owner", PID: 42, State: "running", StartedAt: now.Add(-time.Hour), HeartbeatAt: now.Add(-time.Hour)}
	data, _ := json.Marshal(old)
	writeForwardStateFixture(t, brainDir, memoryCoordinatorStateRel, data)
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.close(now.Add(time.Minute), "complete")
	state, err := loadMemoryCoordinatorState(brainDir)
	if err != nil || state.OwnerToken != coordinator.token || state.State != "running" {
		t.Fatalf("state = %+v err=%v", state, err)
	}
}

func TestCoordinatorHeartbeatPreservesReplacedUnknownNewerState(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 21, 15, 0, 0, time.UTC)
	coordinator, err := acquireMemoryCoordinator(brainDir, now, "test")
	if err != nil {
		t.Fatal(err)
	}
	future := []byte(`{"schema_version":2,"future":true}` + "\n")
	path := writeForwardStateFixture(t, brainDir, memoryCoordinatorStateRel, future)
	if err := coordinator.heartbeat(now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("heartbeat error = %v", err)
	}
	requireForwardStateBytes(t, path, future)
	coordinator.close(now.Add(2*time.Minute), "failed")
	requireForwardStateBytes(t, path, future)
}

func TestCancellationMarkerPreservesUnknownCorruptAndUnsafeLeaves(t *testing.T) {
	now := time.Date(2026, 8, 9, 21, 20, 0, 0, time.UTC)
	job := memoryJob{JobID: "job:forward-state", State: memoryJobStateRunning}
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{name: "unknown-newer", data: []byte(`{"schema_version":2,"job_id":"job:forward-state","future":true}` + "\n"), code: memoryErrUnsupportedVersion},
		{name: "corrupt-current", data: []byte(`{"schema_version":1,"job_id":"job:forward-state","future":true}` + "\n"), code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			path := writeForwardStateFixture(t, brainDir, memoryCancellationRel(job.JobID), tc.data)
			if err := writeMemoryCancellationRequest(brainDir, job, now); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("write error = %v, want %s", err, tc.code)
			}
			requireForwardStateBytes(t, path, tc.data)
			if err := removeMemoryCancellationRequest(brainDir, job.JobID); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("remove error = %v, want %s", err, tc.code)
			}
			requireForwardStateBytes(t, path, tc.data)
		})
	}

	brainDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "cancel-canary")
	canary := []byte("canary")
	if err := os.WriteFile(outside, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(memoryCancellationRel(job.JobID)))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := removeMemoryCancellationRequest(brainDir, job.JobID); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("unsafe remove error = %v", err)
	}
	requireForwardStateBytes(t, outside, canary)
}

func TestCancellationMarkerCurrentVersionIsIdempotentAndRemovable(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 21, 30, 0, 0, time.UTC)
	job := memoryJob{JobID: "job:current-cancel", State: memoryJobStateRunning}
	if err := writeMemoryCancellationRequest(brainDir, job, now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(memoryCancellationRel(job.JobID)))
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMemoryCancellationRequest(brainDir, job, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requireForwardStateBytes(t, path, first)
	if err := removeMemoryCancellationRequest(brainDir, job.JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("marker not removed: %v", err)
	}
}

func TestProjectionReceiptUnknownNewerIsClassifiedBeforeStrictDecodeAndPreserved(t *testing.T) {
	brainDir := t.TempDir()
	future := []byte(`{"schema_version":2,"future":true}` + "\n")
	path := writeForwardStateFixture(t, brainDir, projectionStateRel, future)
	sum := sha256.Sum256(future)
	source := &historySourceManifest{ProjectionStatePath: projectionStateRel, ProjectionStateDigest: "sha256:" + hex.EncodeToString(sum[:])}
	_, state, err := loadProjectionStateChecked(brainDir, source)
	if state != projectionStateUnsupported || err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("state=%s err=%v", state, err)
	}
	if _, err := writeProjectionState(brainDir, projectionState{SchemaVersion: projectionSchemaVersion, ReconcilerVersion: projectionReconcilerVersion, GeneratedAt: time.Now().UTC()}); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("write error = %v", err)
	}
	requireForwardStateBytes(t, path, future)
}

func TestProjectionReceiptCurrentVersionCanBeReplaced(t *testing.T) {
	brainDir := t.TempDir()
	first := projectionState{SchemaVersion: projectionSchemaVersion, ReconcilerVersion: projectionReconcilerVersion, GeneratedAt: time.Date(2026, 8, 9, 21, 40, 0, 0, time.UTC)}
	if _, err := writeProjectionState(brainDir, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.GeneratedAt = first.GeneratedAt.Add(time.Minute)
	if _, err := writeProjectionState(brainDir, second); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(brainDir, projectionStateRel))
	if err != nil || !strings.Contains(string(data), second.GeneratedAt.Format(time.RFC3339)) {
		t.Fatalf("receipt data=%s err=%v", data, err)
	}
}

func TestBrainManifestPreservesUnknownNewerCorruptAndUnsafeLeaves(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{name: "unknown-newer", data: []byte(`{"schema_version":4,"future":true}` + "\n"), code: memoryErrUnsupportedVersion},
		{name: "additive-current", data: []byte(`{"schema_version":3,"future":true}` + "\n"), code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			path := writeForwardStateFixture(t, brainDir, exportManifestFileName, tc.data)
			if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion}); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("write error = %v, want %s", err, tc.code)
			}
			requireForwardStateBytes(t, path, tc.data)
		})
	}

	brainDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "manifest-canary")
	canary := []byte("canary")
	if err := os.WriteFile(outside, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(brainDir, exportManifestFileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion}); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("unsafe write error = %v", err)
	}
	requireForwardStateBytes(t, outside, canary)
}

func TestBrainManifestSupportedVersionsRemainAdaptable(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			brainDir := t.TempDir()
			data := []byte(`{"schema_version":` + strconv.Itoa(version) + `}` + "\n")
			writeForwardStateFixture(t, brainDir, exportManifestFileName, data)
			manifest, err := loadBrainManifest(brainDir)
			if err != nil || manifest.SchemaVersion != version {
				t.Fatalf("manifest=%+v err=%v", manifest, err)
			}
			if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
				t.Fatal(err)
			}
			updated, err := loadBrainManifest(brainDir)
			if err != nil || updated.SchemaVersion != brainManifestSchemaVersion {
				t.Fatalf("updated=%+v err=%v", updated, err)
			}
		})
	}
}

func TestPrivacyTransactionPreservesUnknownCorruptAndUnsafeLeaves(t *testing.T) {
	now := time.Date(2026, 8, 9, 22, 0, 0, 0, time.UTC)
	sessionID := "forward-private"
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{name: "unknown-newer", data: []byte(`{"schema_version":3,"session_id":"forward-private","future":true}` + "\n"), code: memoryErrUnsupportedVersion},
		{name: "corrupt-current", data: []byte(`{"schema_version":2,"session_id":"forward-private","future":true}` + "\n"), code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			path := writeForwardStateFixture(t, brainDir, privacyTransactionRel(sessionID), tc.data)
			tx := privacyTransaction{SessionID: sessionID, Operation: "exclude", State: privacyStateRequested, StartedAt: now}
			if err := writePrivacyTransaction(brainDir, tx, now); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("write error = %v, want %s", err, tc.code)
			}
			requireForwardStateBytes(t, path, tc.data)
			if err := removePrivacyTransaction(brainDir, sessionID); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("remove error = %v, want %s", err, tc.code)
			}
			requireForwardStateBytes(t, path, tc.data)
		})
	}

	brainDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "privacy-canary")
	canary := []byte("canary")
	if err := os.WriteFile(outside, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(privacyTransactionRel(sessionID)))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := removePrivacyTransaction(brainDir, sessionID); err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) {
		t.Fatalf("unsafe remove error = %v", err)
	}
	requireForwardStateBytes(t, outside, canary)
}

func TestPrivacyTransactionCurrentVersionCanAdvanceAndBeRemoved(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 22, 10, 0, 0, time.UTC)
	tx := privacyTransaction{SessionID: "current-private", Operation: "purge", State: privacyStateRequested, StartedAt: now}
	if err := writePrivacyTransaction(brainDir, tx, now); err != nil {
		t.Fatal(err)
	}
	tx.State = privacyStateComplete
	if err := writePrivacyTransaction(brainDir, tx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	loaded, present, err := loadPrivacyTransactionChecked(brainDir, tx.SessionID)
	if err != nil || !present || loaded.State != privacyStateComplete {
		t.Fatalf("loaded=%+v present=%t err=%v", loaded, present, err)
	}
	if err := removePrivacyTransaction(brainDir, tx.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, present, err := loadPrivacyTransactionChecked(brainDir, tx.SessionID); err != nil || present {
		t.Fatalf("present=%t err=%v", present, err)
	}
}

func TestPrivacyTransactionLegacyVersionUpgradesWithoutLosingState(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 22, 15, 0, 0, time.UTC)
	sessionID := "legacy-private"
	legacy := []byte(fmt.Sprintf(`{"schema_version":%d,"session_id":%q,"operation":"exclude","state":"error","started_at":%q,"updated_at":%q,"artifacts":[{"path":"sessions/main/legacy.jsonl","bytes":12}],"error":"interrupted"}`+"\n",
		privacyTransactionLegacyVersion, sessionID, now.Format(time.RFC3339), now.Format(time.RFC3339)))
	writeForwardStateFixture(t, brainDir, privacyTransactionRel(sessionID), legacy)
	tx, present, err := loadPrivacyTransactionChecked(brainDir, sessionID)
	if err != nil || !present || tx.State != privacyStateError || len(tx.Artifacts) != 1 {
		t.Fatalf("legacy transaction=%+v present=%t err=%v", tx, present, err)
	}
	tx.State = privacyStateRequested
	tx.Error = ""
	if err := writePrivacyTransaction(brainDir, tx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	upgraded, present, err := loadPrivacyTransactionChecked(brainDir, sessionID)
	if err != nil || !present || upgraded.SchemaVersion != privacyTransactionVersion || len(upgraded.Artifacts) != 1 {
		t.Fatalf("upgraded transaction=%+v present=%t err=%v", upgraded, present, err)
	}
}

func TestPrivacyCleanupRefusesUnknownTransactionBeforeTombstoneMutation(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 22, 20, 0, 0, time.UTC)
	sessionID := "future-cleanup"
	future := []byte(`{"schema_version":3,"session_id":"future-cleanup","future":true}` + "\n")
	path := writeForwardStateFixture(t, brainDir, privacyTransactionRel(sessionID), future)
	if err := executeSessionCleanup(brainDir, sessionID, sessionPurgePlan{}, now, "excluded", true); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("cleanup error = %v", err)
	}
	requireForwardStateBytes(t, path, future)
	stones, state, err := loadSessionTombstonesChecked(brainDir)
	if err != nil || state.State != sessionTombstoneAbsent || len(stones.Excluded) != 0 {
		t.Fatalf("tombstones=%+v state=%+v err=%v", stones, state, err)
	}
}

func TestSessionTombstoneWriterPreservesAdditiveCurrentState(t *testing.T) {
	brainDir := t.TempDir()
	future := []byte(`{"version":1,"excluded":{},"future":true}` + "\n")
	path := writeForwardStateFixture(t, brainDir, sessionTombstonesPath, future)
	if err := saveSessionTombstones(brainDir, emptySessionTombstones()); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("write error = %v", err)
	}
	requireForwardStateBytes(t, path, future)
}
