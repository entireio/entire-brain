//go:build brain_cgo

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func memoryVectorSQLiteRowCounts(t *testing.T, store *historyVecStore) (int, int) {
	t.Helper()
	db, err := store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ids, vectors int
	if err := db.QueryRow(`SELECT count(*) FROM history_ids`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM vec_history`).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	return ids, vectors
}

func TestMemoryGuardedSQLiteStoreRejectsLateVNextBeforeDropAndAdd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset bool
		add   map[string][]float32
	}{
		{name: "reset-drop", reset: true},
		{name: "add-upsert", add: map[string][]float32{"late-add": {1, 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, epoch, ownership, _ := memoryVectorOwnershipFixture(t)
			rawStore, ok := newHistoryVectorStore(brainDir, "test-model", 2)
			if !ok {
				t.Fatal("brain_cgo build must provide history vector store")
			}
			raw, ok := rawStore.(*historyVecStore)
			if !ok {
				t.Fatalf("history store type = %T", rawStore)
			}
			if err := raw.upsert(map[string][]float32{
				"keep": {1, 0}, "drop-me": {0, 1},
			}, nil); err != nil {
				t.Fatal(err)
			}
			beforeIDs, ok := raw.ids()
			if !ok {
				t.Fatal("seeded vector store is unavailable")
			}
			beforeIDRows, beforeVectorRows := memoryVectorSQLiteRowCounts(t, raw)
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
			if !errors.As(err, &typed) || typed.Code != memoryErrUnsupportedVersion || typed.State != memoryVectorProgressUnsupported {
				t.Fatalf("guarded SQLite mutation error = %#v, want typed unsupported", err)
			}
			afterIDs, ok := raw.ids()
			if !ok || !reflect.DeepEqual(afterIDs, beforeIDs) {
				t.Fatalf("SQLite vector IDs changed: before=%v after=%v ok=%t", beforeIDs, afterIDs, ok)
			}
			afterIDRows, afterVectorRows := memoryVectorSQLiteRowCounts(t, raw)
			if afterIDRows != beforeIDRows || afterVectorRows != beforeVectorRows {
				t.Fatalf("SQLite rows changed: ids %d->%d vectors %d->%d", beforeIDRows, afterIDRows, beforeVectorRows, afterVectorRows)
			}
			leafAfter, readErr := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel)))
			if readErr != nil || string(leafAfter) != string(future) {
				t.Fatalf("late vNext bytes changed: after=%q err=%v", leafAfter, readErr)
			}
		})
	}
}
