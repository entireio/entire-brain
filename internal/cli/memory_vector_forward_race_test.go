package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMemoryVectorProgressCommitRechecksLateVNextLeaf(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 23, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("1", 64)
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		RepoKey:       "test/vector-late-vnext",
		Sources:       &brainSources{History: &historySourceManifest{IndexDigest: digest, PrivacyIdentity: "absent"}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	epoch, _, err := captureMemoryVectorEpoch(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// This is the worker's initial progress observation before embedding.
	if _, present, err := loadMemoryVectorProgress(brainDir); err != nil || present {
		t.Fatalf("initial progress present=%t err=%v", present, err)
	}

	future := []byte(`{"schema_version":3,"future":true}` + "\n")
	progressPath := filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel))
	originalHook := memoryVectorProgressBeforeCommitCheck
	var installErr error
	memoryVectorProgressBeforeCommitCheck = func(dir string) {
		installErr = writeBrainRelativeFileAtomic(dir, memoryVectorProgressRel, future, 0o600)
	}
	t.Cleanup(func() { memoryVectorProgressBeforeCommitCheck = originalHook })

	progress := memoryVectorProgress{ModelID: "test-model", SourceDigest: digest, Pending: true, UpdatedAt: now}
	err = saveMemoryVectorProgressAtEpoch(brainDir, epoch, progress)
	if installErr != nil {
		t.Fatalf("install late vNext progress: %v", installErr)
	}
	if err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("commit error = %v, want %s", err, memoryErrUnsupportedVersion)
	}
	after, readErr := os.ReadFile(progressPath)
	if readErr != nil || string(after) != string(future) {
		t.Fatalf("late vNext bytes changed: after=%q err=%v", after, readErr)
	}
}

func TestMemoryVectorProgressDirectWriterPreservesUnclassifiedLeaf(t *testing.T) {
	now := time.Date(2026, 8, 9, 23, 10, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("2", 64)
	progress := memoryVectorProgress{ModelID: "test-model", SourceDigest: digest, Pending: true, UpdatedAt: now}
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{name: "unknown-newer", data: []byte(`{"schema_version":3,"future":true}` + "\n"), code: memoryErrUnsupportedVersion},
		{name: "additive-current", data: []byte(`{"schema_version":2,"future":true}` + "\n"), code: memoryErrStateCorrupt},
		{name: "corrupt-current", data: []byte(`{"schema_version":2,"model_id":`), code: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			path := filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := saveMemoryVectorProgress(brainDir, progress); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("save error = %v, want %s", err, tc.code)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(tc.data) {
				t.Fatalf("progress bytes changed: after=%q err=%v", after, err)
			}
		})
	}
}

func TestMemoryVectorProgressDirectWriterReplacesCurrentLeaf(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 23, 20, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("3", 64)
	first := memoryVectorProgress{ModelID: "test-model", SourceDigest: digest, Pending: true, UpdatedAt: now}
	if err := saveMemoryVectorProgress(brainDir, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ResetComplete = true
	second.Pending = false
	second.CompleteSourceDigest = digest
	second.UpdatedAt = now.Add(time.Minute)
	if err := saveMemoryVectorProgress(brainDir, second); err != nil {
		t.Fatal(err)
	}
	loaded, present, err := loadMemoryVectorProgress(brainDir)
	if err != nil || !present || loaded.UpdatedAt != second.UpdatedAt || loaded.Pending {
		t.Fatalf("loaded=%+v present=%t err=%v", loaded, present, err)
	}
}

func memoryVectorOwnershipFixture(t *testing.T) (string, memoryVectorEpoch, *memoryVectorProgressOwnership, memoryVectorProgress) {
	t.Helper()
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 23, 30, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("4", 64)
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		RepoKey:       "test/vector-ownership",
		Sources:       &brainSources{History: &historySourceManifest{IndexDigest: digest, PrivacyIdentity: "absent"}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	progress := memoryVectorProgress{
		ModelID: "test-model", SourceDigest: digest, ResetComplete: true,
		Pending: true, UpdatedAt: now,
	}
	if err := saveMemoryVectorProgress(brainDir, progress); err != nil {
		t.Fatal(err)
	}
	epoch, _, err := captureMemoryVectorEpoch(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	_, present, ownership, err := loadMemoryVectorProgressWithOwnership(brainDir)
	if err != nil || !present {
		t.Fatalf("capture progress ownership present=%t err=%v", present, err)
	}
	return brainDir, epoch, ownership, progress
}

func TestMemoryGuardedVectorStoreRejectsLateVNextBeforeEveryMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
		add   map[string][]float32
	}{
		{name: "reset-drop", reset: true},
		{name: "add-upsert", add: map[string][]float32{"late-add": {0, 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, epoch, ownership, _ := memoryVectorOwnershipFixture(t)
			raw := &memoryVectorTestStore{vectors: map[string][]float32{
				"keep":    {1, 0},
				"drop-me": {0, 1},
			}}
			before := map[string][]float32{"keep": {1, 0}, "drop-me": {0, 1}}
			guarded := &memoryGuardedVectorStore{brainDir: brainDir, epoch: epoch, ownership: ownership, store: raw}

			future := []byte(`{"schema_version":3,"future":true}` + "\n")
			originalHook := memoryGuardedVectorStoreBeforeMutation
			var installErr error
			memoryGuardedVectorStoreBeforeMutation = func(dir string, _ map[string][]float32, _ []string) {
				installErr = writeBrainRelativeFileAtomic(dir, memoryVectorProgressRel, future, 0o600)
			}
			t.Cleanup(func() { memoryGuardedVectorStoreBeforeMutation = originalHook })

			var err error
			if tc.reset {
				_, err = clearMemoryVectorStore(context.Background(), guarded)
			} else {
				err = guarded.upsert(tc.add, nil)
			}
			if installErr != nil {
				t.Fatalf("install late vNext progress: %v", installErr)
			}
			var typed *memoryVectorProgressLoadError
			if !errors.As(err, &typed) || typed.Code != memoryErrUnsupportedVersion || typed.State != memoryVectorProgressUnsupported || typed.Version != memoryVectorSchema+1 {
				t.Fatalf("guarded mutation error = %#v, want typed unsupported vNext", err)
			}
			if !reflect.DeepEqual(raw.vectors, before) {
				t.Fatalf("vector IDs/rows changed before rejection: got=%v want=%v", raw.vectors, before)
			}
			after, readErr := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel)))
			if readErr != nil || string(after) != string(future) {
				t.Fatalf("late vNext bytes changed: after=%q err=%v", after, readErr)
			}
		})
	}
}

func TestMemoryGuardedVectorStoreClassifiesLateIncompatibleCurrentProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "additive-current", data: []byte(`{"schema_version":2,"future":true}` + "\n")},
		{name: "corrupt-current", data: []byte(`{"schema_version":2,"model_id":`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, epoch, ownership, _ := memoryVectorOwnershipFixture(t)
			raw := &memoryVectorTestStore{vectors: map[string][]float32{"keep": {1, 0}}}
			guarded := &memoryGuardedVectorStore{brainDir: brainDir, epoch: epoch, ownership: ownership, store: raw}
			originalHook := memoryGuardedVectorStoreBeforeMutation
			var installErr error
			memoryGuardedVectorStoreBeforeMutation = func(dir string, _ map[string][]float32, _ []string) {
				installErr = writeBrainRelativeFileAtomic(dir, memoryVectorProgressRel, tc.data, 0o600)
			}
			t.Cleanup(func() { memoryGuardedVectorStoreBeforeMutation = originalHook })

			err := guarded.upsert(map[string][]float32{"late-add": {0, 1}}, nil)
			if installErr != nil {
				t.Fatal(installErr)
			}
			var typed *memoryVectorProgressLoadError
			if !errors.As(err, &typed) || typed.Code != memoryErrStateCorrupt || typed.State != memoryVectorProgressCorrupt {
				t.Fatalf("guarded mutation error = %#v, want typed corrupt current leaf", err)
			}
			if _, added := raw.vectors["late-add"]; added || len(raw.vectors) != 1 {
				t.Fatalf("incompatible current takeover mutated vectors: %v", raw.vectors)
			}
			after, readErr := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel)))
			if readErr != nil || string(after) != string(tc.data) {
				t.Fatalf("incompatible current bytes changed: after=%q err=%v", after, readErr)
			}
		})
	}
}

func TestMemoryVectorOwnershipAdvancesOnlyForCurrentWorkerProgress(t *testing.T) {
	brainDir, epoch, ownership, progress := memoryVectorOwnershipFixture(t)
	raw := &memoryVectorTestStore{vectors: map[string][]float32{"keep": {1, 0}}}
	guarded := &memoryGuardedVectorStore{brainDir: brainDir, epoch: epoch, ownership: ownership, store: raw}
	before := append([]byte(nil), ownership.data...)

	progress.HistoryAdded = 1
	progress.UpdatedAt = progress.UpdatedAt.Add(time.Minute)
	if err := saveMemoryVectorProgressAtEpochOwned(brainDir, epoch, ownership, progress); err != nil {
		t.Fatalf("save owned incremental progress: %v", err)
	}
	if reflect.DeepEqual(ownership.data, before) {
		t.Fatal("successful owned progress save did not advance ownership")
	}
	if err := guarded.upsert(map[string][]float32{"owned-add": {0, 1}}, nil); err != nil {
		t.Fatalf("guard after owned progress update: %v", err)
	}

	takeover := progress
	takeover.HistoryAdded = 2
	takeover.UpdatedAt = takeover.UpdatedAt.Add(time.Minute)
	if err := saveMemoryVectorProgress(brainDir, takeover); err != nil {
		t.Fatalf("install valid current takeover: %v", err)
	}
	leafBefore, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel)))
	if err != nil {
		t.Fatal(err)
	}
	mutationErr := guarded.upsert(map[string][]float32{"stale-add": {1, 1}}, nil)
	if mutationErr == nil || !strings.Contains(mutationErr.Error(), memoryErrSourceStale) {
		t.Fatalf("valid current takeover mutation error = %v", mutationErr)
	}
	if _, added := raw.vectors["stale-add"]; added {
		t.Fatal("old ownership mutated vectors after a valid current takeover")
	}
	leafAfter, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel)))
	if err != nil || !reflect.DeepEqual(leafAfter, leafBefore) {
		t.Fatalf("valid current takeover bytes changed: err=%v", err)
	}
}
