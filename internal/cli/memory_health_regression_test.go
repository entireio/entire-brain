package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryVectorHealthReportsPersistedStatesWithoutMutation(t *testing.T) {
	currentDigest := "sha256:" + strings.Repeat("a", 64)
	staleDigest := "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name                         string
		pending, reset               bool
		digest, complete, code, want string
	}{
		{name: "pending", pending: true, reset: true, digest: currentDigest, want: "pending"},
		{name: "reset incomplete", pending: true, digest: currentDigest, complete: currentDigest, want: "pending"},
		{name: "current", reset: true, digest: currentDigest, complete: currentDigest, want: "current"},
		{name: "stale", reset: true, digest: staleDigest, complete: staleDigest, want: "stale"},
		{name: "degraded overrides current", reset: true, digest: currentDigest, complete: currentDigest, code: "memory_vector_sync_failed", want: "degraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			progress := memoryVectorProgress{SchemaVersion: memoryVectorSchema, ModelID: "test-model", SourceDigest: tc.digest, CompleteSourceDigest: tc.complete, ResetComplete: tc.reset, Pending: tc.pending, ErrorCode: tc.code, UpdatedAt: time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)}
			if err := saveMemoryVectorProgress(dir, progress); err != nil {
				t.Fatal(err)
			}
			before := privacyTreeDigest(t, dir)
			got := memoryVectorProgressHealth(dir, &historySourceManifest{IndexDigest: currentDigest})
			if got["state"] != tc.want || got["schema_state"] != "current" || got["model_id"] != "test-model" || got["pending"] != tc.pending {
				t.Fatalf("health=%+v want state=%s", got, tc.want)
			}
			if tc.want == "stale" && got["error_code"] != memoryErrSourceStale || tc.want == "degraded" && got["error_code"] != tc.code {
				t.Fatalf("missing failure reason: %+v", got)
			}
			if privacyTreeDigest(t, dir) != before {
				t.Fatal("health check mutated vector progress")
			}
		})
	}
}

func TestMemoryVectorHealthRejectsInvalidProgressWithoutMutation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"corrupt", "{broken", "corrupt"},
		{"newer", `{"schema_version":999,"private":"PRIVATE_VECTOR_PROGRESS_CANARY"}`, "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, filepath.FromSlash(memoryVectorProgressRel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			before := privacyTreeDigest(t, dir)
			got := memoryVectorProgressHealth(dir, nil)
			if got["state"] != tc.want || got["schema_state"] != tc.want || got["present"] != true {
				t.Fatalf("health=%+v", got)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "PRIVATE_VECTOR_PROGRESS_CANARY") {
				t.Fatal("health leaked unknown progress contents")
			}
			if privacyTreeDigest(t, dir) != before {
				t.Fatal("invalid progress was rewritten")
			}
		})
	}
}
