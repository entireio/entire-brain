//go:build brain_cgo && cgo

package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func prepareMemoryVectorResetFixture(t *testing.T) (string, time.Time, *memoryContextTestEmbedder, *historyVecStore, *historyVecStore, map[string]struct{}, map[string]struct{}) {
	t.Helper()
	brainDir, now, _, embedder := memoryVectorRegressionFixture(t)
	if _, err := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now, embedder, nil); err != nil {
		t.Fatal(err)
	}
	history, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("history store unavailable")
	}
	conversation, ok := newConversationVectorStore(brainDir, conversationVectorModelID(embedder.ID()), embedder.Dim())
	if !ok {
		t.Fatal("conversation store unavailable")
	}
	historyRaw := history.(*historyVecStore)
	conversationRaw := conversation.(*historyVecStore)
	historyIDs, historyOK := historyRaw.ids()
	conversationIDs, conversationOK := conversationRaw.ids()
	if !historyOK || !conversationOK || len(historyIDs) == 0 || len(conversationIDs) == 0 {
		t.Fatalf("fixture IDs history=%v/%t conversation=%v/%t", historyIDs, historyOK, conversationIDs, conversationOK)
	}
	progress, present, err := loadMemoryVectorProgress(brainDir)
	if err != nil || !present {
		t.Fatalf("progress=%+v present=%t err=%v", progress, present, err)
	}
	progress.ModelID = "force-reset"
	if err := saveMemoryVectorProgress(brainDir, progress); err != nil {
		t.Fatal(err)
	}
	return brainDir, now, embedder, historyRaw, conversationRaw, historyIDs, conversationIDs
}

func installVectorDeleteAbort(t *testing.T, path, trigger string) {
	t.Helper()
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE DELETE ON history_ids BEGIN SELECT RAISE(ABORT, 'reset delete blocked'); END`); err != nil {
		t.Fatal(err)
	}
}

func dropVectorDeleteAbort(t *testing.T, path, trigger string) {
	t.Helper()
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TRIGGER ` + trigger); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryVectorResetDeleteFailuresRemainRetryable(t *testing.T) {
	for _, target := range []string{"history", "conversation"} {
		t.Run(target, func(t *testing.T) {
			brainDir, now, embedder, history, conversation, historyBefore, conversationBefore := prepareMemoryVectorResetFixture(t)
			failedStore := history
			if target == "conversation" {
				failedStore = conversation
			}
			const trigger = "fail_reset_delete"
			installVectorDeleteAbort(t, failedStore.path, trigger)
			pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now.Add(time.Minute), embedder, memoryVectorBatchRecords)
			if err == nil || !strings.Contains(err.Error(), "reset delete blocked") || !pending {
				t.Fatalf("pass=%+v pending=%t err=%v", pass, pending, err)
			}
			progress, present, loadErr := loadMemoryVectorProgress(brainDir)
			if loadErr != nil || !present || progress.ResetComplete || !progress.Pending {
				t.Fatalf("progress=%+v present=%t err=%v", progress, present, loadErr)
			}
			historyAfter, historyOK := history.ids()
			conversationAfter, conversationOK := conversation.ids()
			if !historyOK || !conversationOK {
				t.Fatal("stores unreadable after reset failure")
			}
			if target == "history" {
				if !reflect.DeepEqual(historyAfter, historyBefore) || !reflect.DeepEqual(conversationAfter, conversationBefore) {
					t.Fatalf("history reset failure changed IDs: history=%v conversation=%v", historyAfter, conversationAfter)
				}
			} else if len(historyAfter) != 0 || !reflect.DeepEqual(conversationAfter, conversationBefore) {
				t.Fatalf("conversation failure state history=%v conversation=%v", historyAfter, conversationAfter)
			}
			dropVectorDeleteAbort(t, failedStore.path, trigger)
			if _, retryErr := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now.Add(2*time.Minute), embedder, nil); retryErr != nil {
				t.Fatalf("retry: %v", retryErr)
			}
			historyFinal, _ := history.ids()
			conversationFinal, _ := conversation.ids()
			if !reflect.DeepEqual(historyFinal, historyBefore) || !reflect.DeepEqual(conversationFinal, conversationBefore) {
				t.Fatalf("retry IDs history=%v want=%v conversation=%v want=%v", historyFinal, historyBefore, conversationFinal, conversationBefore)
			}
		})
	}
}

func TestMemoryVectorResetCompletionProgressFailureRetries(t *testing.T) {
	brainDir, now, embedder, history, conversation, historyBefore, conversationBefore := prepareMemoryVectorResetFixture(t)
	progressPath := filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel))
	original := memoryVectorProgressBeforeCommitCheck
	calls := 0
	var hookErr error
	memoryVectorProgressBeforeCommitCheck = func(string) {
		calls++
		if calls == 2 {
			if err := os.Remove(progressPath); err != nil {
				hookErr = err
				return
			}
			hookErr = os.Mkdir(progressPath, 0o700)
		}
	}
	t.Cleanup(func() { memoryVectorProgressBeforeCommitCheck = original })
	_, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now.Add(time.Minute), embedder, memoryVectorBatchRecords)
	if hookErr != nil || err == nil || !pending || calls != 2 {
		t.Fatalf("pending=%t calls=%d hook=%v err=%v", pending, calls, hookErr, err)
	}
	historyIDs, historyOK := history.ids()
	conversationIDs, conversationOK := conversation.ids()
	if !historyOK || !conversationOK || len(historyIDs) != 0 || len(conversationIDs) != 0 {
		t.Fatalf("reset stores not cleared: history=%v conversation=%v", historyIDs, conversationIDs)
	}
	memoryVectorProgressBeforeCommitCheck = original
	if err := os.Remove(progressPath); err != nil {
		t.Fatal(err)
	}
	if _, retryErr := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now.Add(2*time.Minute), embedder, nil); retryErr != nil {
		t.Fatalf("retry: %v", retryErr)
	}
	historyFinal, historyOK := history.ids()
	conversationFinal, conversationOK := conversation.ids()
	if !historyOK || !conversationOK || !reflect.DeepEqual(historyFinal, historyBefore) || !reflect.DeepEqual(conversationFinal, conversationBefore) {
		t.Fatalf("retry IDs history=%v conversation=%v", historyFinal, conversationFinal)
	}
	progress, present, loadErr := loadMemoryVectorProgress(brainDir)
	if loadErr != nil || !present || !progress.ResetComplete || progress.Pending {
		t.Fatalf("retry progress=%+v present=%v err=%v", progress, present, loadErr)
	}
}

func TestRunMemoryVectorLaneReportsSyncAndLogFailures(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, filepath.FromSlash(memoryWorkerLogRel)), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "enabled")
	original := defaultEmbedder()
	defaultEmbedderInst = memoryVectorLaneEmbedder{}
	t.Cleanup(func() { defaultEmbedderInst = original })
	stats := runMemoryVectorLane(context.Background(), brainDir, now, "dual-failure-owner")
	if stats.VectorContinue || !slices.Contains(stats.HealthIssues, "memory_vector_sync_failed") || !slices.Contains(stats.HealthIssues, "memory_worker_log_write_failed") {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestRunMemoryVectorLaneDisabledIsNoOp(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	if stats := runMemoryVectorLane(context.Background(), t.TempDir(), time.Now(), "disabled-owner"); !reflect.DeepEqual(stats, memoryWorkerStats{}) {
		t.Fatalf("disabled lane stats=%+v", stats)
	}
}
