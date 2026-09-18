package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryVectorSyncClosedInputsLeaveNoProgress(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
		embed Embedder
	}{
		{name: "unset-embedder", embed: nil},
		{name: "missing-manifest", embed: &memoryContextTestEmbedder{}},
		{name: "missing-history-source", embed: &memoryContextTestEmbedder{}, setup: func(t *testing.T, dir string) {
			t.Helper()
			if err := writeBrainManifestAndReadme(dir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/no-history"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "legacy-digestless-history", embed: &memoryContextTestEmbedder{}, setup: func(t *testing.T, dir string) {
			t.Helper()
			if err := writeBrainManifestAndReadme(dir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/legacy", Sources: &brainSources{History: &historySourceManifest{PrivacyIdentity: "absent"}}}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), dir, now, tc.embed, 0)
			if err != nil || pending || pass != (memoryVectorSyncPass{}) {
				t.Fatalf("pass=%+v pending=%t err=%v", pass, pending, err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, filepath.FromSlash(memoryVectorProgressRel))); !os.IsNotExist(statErr) {
				t.Fatalf("unexpected progress state: %v", statErr)
			}
		})
	}
}

func TestMemoryVectorSyncRejectsPrivacyEpochMismatch(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 18, 8, 15, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/privacy-mismatch", Sources: &brainSources{History: &historySourceManifest{IndexDigest: digest, PrivacyIdentity: "sha256:" + strings.Repeat("b", 64)}}}
	if err := writeBrainManifestAndReadme(dir, manifest); err != nil {
		t.Fatal(err)
	}
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), dir, now, &memoryContextTestEmbedder{}, 1)
	if err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) || !pending || pass != (memoryVectorSyncPass{}) {
		t.Fatalf("pass=%+v pending=%t err=%v", pass, pending, err)
	}
}

func TestMemoryVectorSyncPreservesCorruptProgress(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 18, 8, 20, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("c", 64)
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/corrupt-progress", Sources: &brainSources{History: &historySourceManifest{IndexDigest: digest, PrivacyIdentity: "absent"}}}
	if err := writeBrainManifestAndReadme(dir, manifest); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"schema_version":2,"model_id":`)
	if err := writeBrainRelativeFileAtomic(dir, memoryVectorProgressRel, before, 0o600); err != nil {
		t.Fatal(err)
	}
	_, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), dir, now, &memoryContextTestEmbedder{}, 1)
	if err == nil || !pending || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("pending=%t err=%v", pending, err)
	}
	after, readErr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(memoryVectorProgressRel)))
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("corrupt progress changed: after=%q err=%v", after, readErr)
	}
}

func TestRunMemoryVectorLaneSuccessAndFailureHealth(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 30, 0, 0, time.UTC)
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "enabled-for-test")
	originalEmbedder := defaultEmbedder()
	defaultEmbedderInst = memoryVectorLaneEmbedder{}
	t.Cleanup(func() { defaultEmbedderInst = originalEmbedder })

	successDir := t.TempDir()
	if err := writeBrainManifestAndReadme(successDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/lane-success", Sources: &brainSources{History: &historySourceManifest{PrivacyIdentity: "absent"}}}); err != nil {
		t.Fatal(err)
	}
	success := runMemoryVectorLane(context.Background(), successDir, now, "success-owner")
	if success.VectorPending || success.VectorContinue || len(success.HealthIssues) != 0 {
		t.Fatalf("success stats=%+v", success)
	}
	successLog, err := os.ReadFile(filepath.Join(successDir, filepath.FromSlash(memoryWorkerLogRel)))
	if err != nil || !strings.Contains(string(successLog), "vector_sync_complete") {
		t.Fatalf("success log=%q err=%v", successLog, err)
	}

	failureDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(failureDir, exportManifestFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	failure := runMemoryVectorLane(context.Background(), failureDir, now, "failure-owner")
	if failure.VectorContinue || !memoryVectorContainsString(failure.HealthIssues, "memory_vector_sync_failed") {
		t.Fatalf("failure stats=%+v", failure)
	}
	failureLog, err := os.ReadFile(filepath.Join(failureDir, filepath.FromSlash(memoryWorkerLogRel)))
	if err != nil || !strings.Contains(string(failureLog), "vector_sync_failed") || !strings.Contains(string(failureLog), "owner=failure-") {
		t.Fatalf("failure log=%q err=%v", failureLog, err)
	}

	logFailureDir := t.TempDir()
	if err := writeBrainManifestAndReadme(logFailureDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/lane-log-failure", Sources: &brainSources{History: &historySourceManifest{PrivacyIdentity: "absent"}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(logFailureDir, filepath.FromSlash(memoryWorkerLogRel)), 0o700); err != nil {
		t.Fatal(err)
	}
	logFailure := runMemoryVectorLane(context.Background(), logFailureDir, now, "log-owner")
	if !memoryVectorContainsString(logFailure.HealthIssues, "memory_worker_log_write_failed") {
		t.Fatalf("log failure stats=%+v", logFailure)
	}
}

type memoryVectorLaneEmbedder struct{}

func (memoryVectorLaneEmbedder) ID() string                  { return "test-lane" }
func (memoryVectorLaneEmbedder) Dim() int                    { return 2 }
func (memoryVectorLaneEmbedder) Embed(string) []float32      { return []float32{1, 0} }
func (memoryVectorLaneEmbedder) historyFusionEligible() bool { return true }
func (memoryVectorLaneEmbedder) EmbedContext(context.Context, string) []float32 {
	return []float32{1, 0}
}

func memoryVectorContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
