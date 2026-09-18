//go:build brain_cgo

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func memoryVectorRegressionFixture(t *testing.T) (string, time.Time, historyIndex, *memoryContextTestEmbedder) {
	t.Helper()
	brainDir, _, _ := shortTermFixture(t)
	now := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	_ = runMemoryProjectionLaneTest(t, brainDir, now)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	return brainDir, now, index, &memoryContextTestEmbedder{}
}

func TestMemoryVectorSyncSharedBudgetResumeAndIdempotence(t *testing.T) {
	brainDir, now, index, embedder := memoryVectorRegressionFixture(t)
	wantHistory, wantConversation := map[string]struct{}{}, map[string]struct{}{}
	for _, record := range index.Records {
		if record.Kind != "request" && record.Kind != conversationKind {
			wantHistory[record.ID] = struct{}{}
		}
		if record.Kind == conversationKind {
			wantConversation[record.ID] = struct{}{}
		}
	}
	historyTotal, conversationTotal := len(wantHistory), len(wantConversation)
	if historyTotal == 0 || conversationTotal == 0 {
		t.Fatalf("fixture lacks both vector populations: history=%d conversation=%d", historyTotal, conversationTotal)
	}

	total := memoryVectorSyncPass{}
	passes := 0
	for {
		pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now.Add(time.Duration(passes)*time.Second), embedder, 1)
		if err != nil {
			t.Fatalf("pass %d: %v", passes, err)
		}
		if pass.HistoryAdded+pass.ConversationAdded > 1 {
			t.Fatalf("shared batch budget exceeded: %+v", pass)
		}
		total.HistoryAdded += pass.HistoryAdded
		total.ConversationAdded += pass.ConversationAdded
		passes++
		if !pending {
			break
		}
		if passes > historyTotal+conversationTotal+2 {
			t.Fatalf("sync failed to converge: total=%+v", total)
		}
	}
	if total.HistoryAdded != historyTotal || total.ConversationAdded != conversationTotal || embedder.calls != historyTotal+conversationTotal {
		t.Fatalf("totals=%+v calls=%d want history=%d conversation=%d", total, embedder.calls, historyTotal, conversationTotal)
	}
	progress, present, err := loadMemoryVectorProgress(brainDir)
	if err != nil || !present || progress.Pending || progress.CompleteSourceDigest != progress.SourceDigest || !progress.ResetComplete {
		t.Fatalf("completed progress=%+v present=%t err=%v", progress, present, err)
	}

	historyStore, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("history vector store unavailable in brain_cgo build")
	}
	conversationStore, ok := newConversationVectorStore(brainDir, conversationVectorModelID(embedder.ID()), embedder.Dim())
	if !ok {
		t.Fatal("conversation vector store unavailable in brain_cgo build")
	}
	historyBefore, ok := historyStore.ids()
	if !ok {
		t.Fatal("completed history vector store unavailable")
	}
	conversationBefore, ok := conversationStore.ids()
	if !ok {
		t.Fatal("completed conversation vector store unavailable")
	}
	if !reflect.DeepEqual(historyBefore, wantHistory) || !reflect.DeepEqual(conversationBefore, wantConversation) {
		t.Fatalf("batched IDs history=%v want=%v conversation=%v want=%v", historyBefore, wantHistory, conversationBefore, wantConversation)
	}
	callsBefore := embedder.calls
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now.Add(time.Hour), embedder, 1)
	if err != nil || pending || pass.HistoryAdded != 0 || pass.ConversationAdded != 0 || embedder.calls != callsBefore {
		t.Fatalf("idempotent pass=%+v pending=%t calls=%d->%d err=%v", pass, pending, callsBefore, embedder.calls, err)
	}
	historyAfter, ok := historyStore.ids()
	if !ok {
		t.Fatal("history vector store unavailable after idempotent pass")
	}
	conversationAfter, ok := conversationStore.ids()
	if !ok {
		t.Fatal("conversation vector store unavailable after idempotent pass")
	}
	if !reflect.DeepEqual(historyAfter, historyBefore) || !reflect.DeepEqual(conversationAfter, conversationBefore) {
		t.Fatal("idempotent pass changed persisted vector IDs")
	}
}

func TestMemoryVectorSyncCancellationPersistsRetryableProgress(t *testing.T) {
	brainDir, now, _, embedder := memoryVectorRegressionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(ctx, brainDir, now, embedder, 8)
	if err != nil || !pending || pass.HistoryAdded != 0 || pass.ConversationAdded != 0 {
		t.Fatalf("cancelled pass=%+v pending=%t err=%v", pass, pending, err)
	}
	progress, present, loadErr := loadMemoryVectorProgress(brainDir)
	if loadErr != nil || !present || !progress.Pending || progress.ErrorCode != "" || !progress.ResetComplete {
		t.Fatalf("cancelled progress=%+v present=%t err=%v", progress, present, loadErr)
	}
	if _, fullErr := syncMemoryProjectionVectorsFully(ctx, brainDir, now, embedder, nil); fullErr != context.Canceled {
		t.Fatalf("full sync cancellation error=%v", fullErr)
	}

	pass, pending, err = syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now.Add(time.Minute), embedder, memoryVectorBatchRecords)
	if err != nil || pending || pass.HistoryAdded+pass.ConversationAdded == 0 {
		t.Fatalf("retry pass=%+v pending=%t err=%v", pass, pending, err)
	}
	progress, _, err = loadMemoryVectorProgress(brainDir)
	if err != nil || progress.Pending || progress.ErrorCode != "" || progress.CompleteSourceDigest != progress.SourceDigest {
		t.Fatalf("retried progress=%+v err=%v", progress, err)
	}
}

func TestMemoryVectorSyncEmbeddingFailurePersistsHealthForRetry(t *testing.T) {
	brainDir, now, _, _ := memoryVectorRegressionFixture(t)
	failing := &fixedIDMemoryEmbedder{id: "test-failing", fail: true}
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, failing, 2)
	if err == nil || !pending || pass.HistoryAdded != 0 || pass.ConversationAdded != 0 {
		t.Fatalf("failed pass=%+v pending=%t err=%v", pass, pending, err)
	}
	progress, present, loadErr := loadMemoryVectorProgress(brainDir)
	if loadErr != nil || !present || !progress.Pending || progress.ErrorCode != "memory_vector_sync_failed" || !progress.ResetComplete {
		t.Fatalf("failed progress=%+v present=%t err=%v", progress, present, loadErr)
	}

	failing.fail = false
	if _, retryErr := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now.Add(time.Minute), failing, nil); retryErr != nil {
		t.Fatal(retryErr)
	}
	progress, _, loadErr = loadMemoryVectorProgress(brainDir)
	if loadErr != nil || progress.Pending || progress.ErrorCode != "" || progress.CompleteSourceDigest != progress.SourceDigest {
		t.Fatalf("retried progress=%+v err=%v", progress, loadErr)
	}
}

func TestMemoryVectorSyncConversationFailurePreservesCompletedHistoryForRetry(t *testing.T) {
	brainDir, now, index, _ := memoryVectorRegressionFixture(t)
	historyTotal := 0
	for _, record := range index.Records {
		if record.Kind != "request" && record.Kind != conversationKind {
			historyTotal++
		}
	}
	embedder := &stagedFailureMemoryEmbedder{failAfter: historyTotal}
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, embedder, memoryVectorBatchRecords)
	if err == nil || !pending || pass.HistoryAdded != historyTotal || pass.ConversationAdded != 0 {
		t.Fatalf("conversation failure pass=%+v pending=%t err=%v", pass, pending, err)
	}
	progress, present, loadErr := loadMemoryVectorProgress(brainDir)
	if loadErr != nil || !present || !progress.Pending || progress.ErrorCode != "memory_vector_sync_failed" || progress.HistoryAdded != historyTotal {
		t.Fatalf("conversation failure progress=%+v present=%t err=%v", progress, present, loadErr)
	}
	embedder.failAfter = 1 << 30
	if _, retryErr := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now.Add(time.Minute), embedder, nil); retryErr != nil {
		t.Fatal(retryErr)
	}
	progress, _, loadErr = loadMemoryVectorProgress(brainDir)
	if loadErr != nil || progress.Pending || progress.ErrorCode != "" || progress.CompleteSourceDigest != progress.SourceDigest {
		t.Fatalf("conversation retry progress=%+v err=%v", progress, loadErr)
	}
}

type stagedFailureMemoryEmbedder struct {
	calls     int
	failAfter int
}

func (*stagedFailureMemoryEmbedder) ID() string               { return "test-staged-failure" }
func (*stagedFailureMemoryEmbedder) Dim() int                 { return 2 }
func (e *stagedFailureMemoryEmbedder) Embed(string) []float32 { return e.embed() }
func (e *stagedFailureMemoryEmbedder) EmbedContext(context.Context, string) []float32 {
	return e.embed()
}
func (e *stagedFailureMemoryEmbedder) embed() []float32 {
	e.calls++
	if e.calls > e.failAfter {
		return nil
	}
	return []float32{1, 0}
}

func TestMemoryVectorSyncRejectsUnreadableCurrentIndex(t *testing.T) {
	brainDir, now, _, embedder := memoryVectorRegressionFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath))
	if err := os.WriteFile(indexPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, embedder, 1)
	if err == nil || pending || pass != (memoryVectorSyncPass{}) {
		t.Fatalf("unreadable-index pass=%+v pending=%t err=%v", pass, pending, err)
	}
}

func TestMemoryVectorSyncModelChangeRebuildsBothStoresWithoutStaleIDs(t *testing.T) {
	brainDir, now, index, first := memoryVectorRegressionFixture(t)
	if _, err := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now, first, nil); err != nil {
		t.Fatal(err)
	}
	firstHistory, ok := newHistoryVectorStore(brainDir, first.ID(), first.Dim())
	if !ok {
		t.Fatal("history store unavailable")
	}
	firstConversation, ok := newConversationVectorStore(brainDir, conversationVectorModelID(first.ID()), first.Dim())
	if !ok {
		t.Fatal("conversation store unavailable")
	}
	if err := firstHistory.upsert(map[string][]float32{"stale-history": {1, 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := firstConversation.upsert(map[string][]float32{"stale-conversation": {1, 1}}, nil); err != nil {
		t.Fatal(err)
	}
	second := &fixedIDMemoryEmbedder{id: "test-context-v2"}
	pass, err := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now.Add(time.Hour), second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pass.HistoryAdded == 0 || pass.ConversationAdded == 0 {
		t.Fatalf("model reset did not rebuild both stores: %+v", pass)
	}
	secondHistory, ok := newHistoryVectorStore(brainDir, second.ID(), second.Dim())
	if !ok {
		t.Fatal("replacement history store unavailable")
	}
	secondConversation, ok := newConversationVectorStore(brainDir, conversationVectorModelID(second.ID()), second.Dim())
	if !ok {
		t.Fatal("replacement conversation store unavailable")
	}
	historyIDs, ok := secondHistory.ids()
	if !ok {
		t.Fatal("replacement history store unreadable")
	}
	conversationIDs, ok := secondConversation.ids()
	if !ok {
		t.Fatal("replacement conversation store unreadable")
	}
	wantHistory, wantConversation := map[string]struct{}{}, map[string]struct{}{}
	for _, record := range index.Records {
		if record.Kind != "request" && record.Kind != conversationKind {
			wantHistory[record.ID] = struct{}{}
		}
		if record.Kind == conversationKind {
			wantConversation[record.ID] = struct{}{}
		}
	}
	if !reflect.DeepEqual(historyIDs, wantHistory) || !reflect.DeepEqual(conversationIDs, wantConversation) {
		t.Fatalf("replacement IDs history=%v want=%v conversation=%v want=%v", historyIDs, wantHistory, conversationIDs, wantConversation)
	}
	if _, exists := historyIDs["stale-history"]; exists {
		t.Fatal("model reset retained stale history ID")
	}
	if _, exists := conversationIDs["stale-conversation"]; exists {
		t.Fatal("model reset retained stale conversation ID")
	}
	progress, _, err := loadMemoryVectorProgress(brainDir)
	if err != nil || progress.ModelID != second.ID() || progress.Pending || progress.CompleteSourceDigest == "" {
		t.Fatalf("model reset progress=%+v err=%v", progress, err)
	}
}

func TestMemoryVectorSyncIncrementalPassPrunesDepartedIDs(t *testing.T) {
	brainDir, now, index, embedder := memoryVectorRegressionFixture(t)
	if _, err := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now, embedder, nil); err != nil {
		t.Fatal(err)
	}
	historyStore, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("history store unavailable")
	}
	conversationStore, ok := newConversationVectorStore(brainDir, conversationVectorModelID(embedder.ID()), embedder.Dim())
	if !ok {
		t.Fatal("conversation store unavailable")
	}
	if err := historyStore.upsert(map[string][]float32{"departed-history": {1, 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := conversationStore.upsert(map[string][]float32{"departed-conversation": {1, 1}}, nil); err != nil {
		t.Fatal(err)
	}
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now.Add(time.Minute), embedder, memoryVectorBatchRecords)
	if err != nil || pending || pass.HistoryDropped != 1 || pass.ConversationDrop != 1 || pass.HistoryAdded != 0 || pass.ConversationAdded != 0 {
		t.Fatalf("incremental prune pass=%+v pending=%t err=%v", pass, pending, err)
	}
	historyIDs, ok := historyStore.ids()
	if !ok {
		t.Fatal("history store unreadable after prune")
	}
	conversationIDs, ok := conversationStore.ids()
	if !ok {
		t.Fatal("conversation store unreadable after prune")
	}
	wantHistory, wantConversation := map[string]struct{}{}, map[string]struct{}{}
	for _, record := range index.Records {
		if record.Kind != "request" && record.Kind != conversationKind {
			wantHistory[record.ID] = struct{}{}
		}
		if record.Kind == conversationKind {
			wantConversation[record.ID] = struct{}{}
		}
	}
	if !reflect.DeepEqual(historyIDs, wantHistory) || !reflect.DeepEqual(conversationIDs, wantConversation) {
		t.Fatalf("pruned IDs history=%v want=%v conversation=%v want=%v", historyIDs, wantHistory, conversationIDs, wantConversation)
	}
}

func TestMemoryVectorSyncDigestEditResetsStaleVectorAndReopensExactIDs(t *testing.T) {
	brainDir, now, index, _ := memoryVectorRegressionFixture(t)
	embedder := &digestAwareMemoryEmbedder{}
	if _, err := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now, embedder, nil); err != nil {
		t.Fatal(err)
	}
	var targetID string
	for i := range index.Records {
		if index.Records[i].Kind != "request" && index.Records[i].Kind != conversationKind {
			targetID = index.Records[i].ID
			index.Records[i].Summary += " digest-edited"
			break
		}
	}
	if targetID == "" {
		t.Fatal("fixture has no history-vector record")
	}
	oldStore, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("history store unavailable")
	}
	before, ok := oldStore.knnCos([]float32{1, 0}, len(index.Records)+1)
	if !ok || before[targetID] < 0.99 {
		t.Fatalf("pre-edit target score=%v ok=%t", before[targetID], ok)
	}

	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(brainDir, manifest.Sources.History.IndexPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	newDigest := "sha256:" + hex.EncodeToString(sum[:])
	manifest.Sources.History.IndexDigest = newDigest
	manifest.Sources.History.IndexBytes = int64(len(data))
	manifest.Sources.History.IndexSHA256 = historyIndexBytesFingerprint(data)
	manifest.Sources.History.RecordsFingerprint = historyRecordsFingerprint(index.Records)
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	pass, err := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now.Add(time.Hour), embedder, nil)
	if err != nil || pass.HistoryDropped == 0 || pass.ConversationDrop == 0 {
		t.Fatalf("digest reset pass=%+v err=%v", pass, err)
	}
	progress, present, err := loadMemoryVectorProgress(brainDir)
	if err != nil || !present || progress.SourceDigest != newDigest || progress.CompleteSourceDigest != newDigest || progress.Pending {
		t.Fatalf("digest progress=%+v present=%t err=%v", progress, present, err)
	}
	reopened, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("reopened history store unavailable")
	}
	after, ok := reopened.knnCos([]float32{1, 0}, len(index.Records)+1)
	if !ok || after[targetID] > 0.01 {
		t.Fatalf("stale vector survived digest reset: score=%v ok=%t", after[targetID], ok)
	}
	ids, ok := reopened.ids()
	if !ok {
		t.Fatal("reopened history IDs unavailable")
	}
	want := map[string]struct{}{}
	for _, record := range index.Records {
		if record.Kind != "request" && record.Kind != conversationKind {
			want[record.ID] = struct{}{}
		}
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("reopened IDs=%v want=%v", ids, want)
	}
}

type digestAwareMemoryEmbedder struct{}

func (*digestAwareMemoryEmbedder) ID() string { return "test-digest-aware" }
func (*digestAwareMemoryEmbedder) Dim() int   { return 2 }
func (*digestAwareMemoryEmbedder) Embed(text string) []float32 {
	if strings.Contains(text, "digest-edited") {
		return []float32{0, 1}
	}
	return []float32{1, 0}
}
func (e *digestAwareMemoryEmbedder) EmbedContext(ctx context.Context, text string) []float32 {
	if ctx.Err() != nil {
		return nil
	}
	return e.Embed(text)
}

func TestMemoryVectorSyncInitialProgressWriteFailurePreventsResetMutation(t *testing.T) {
	brainDir, now, _, embedder := memoryVectorRegressionFixture(t)
	raw, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("history store unavailable")
	}
	if err := raw.upsert(map[string][]float32{"preserve-on-write-failure": {1, 0}}, nil); err != nil {
		t.Fatal(err)
	}
	progressPath := filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel))
	originalHook := memoryVectorProgressBeforeCommitCheck
	var hookErr error
	memoryVectorProgressBeforeCommitCheck = func(string) {
		hookErr = os.MkdirAll(progressPath, 0o700)
	}
	t.Cleanup(func() { memoryVectorProgressBeforeCommitCheck = originalHook })
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, embedder, 4)
	if hookErr != nil || err == nil || !pending || pass != (memoryVectorSyncPass{}) {
		t.Fatalf("initial write failure pass=%+v pending=%t hook_err=%v err=%v", pass, pending, hookErr, err)
	}
	ids, ok := raw.ids()
	if !ok {
		t.Fatal("history store unavailable after failed reset intent")
	}
	if _, preserved := ids["preserve-on-write-failure"]; !preserved {
		t.Fatalf("reset mutated store before durable intent: %v", ids)
	}
}

func TestMemoryVectorSyncProgressTakeoverRejectsSQLiteReset(t *testing.T) {
	brainDir, now, _, embedder := memoryVectorRegressionFixture(t)
	raw, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("history store unavailable")
	}
	if err := raw.upsert(map[string][]float32{"preserve-on-takeover": {1, 0}}, nil); err != nil {
		t.Fatal(err)
	}
	originalHook := memoryGuardedVectorStoreBeforeMutation
	var hookErr error
	var once sync.Once
	memoryGuardedVectorStoreBeforeMutation = func(dir string, _ map[string][]float32, _ []string) {
		once.Do(func() {
			hookErr = writeBrainRelativeFileAtomic(dir, memoryVectorProgressRel, []byte(`{"schema_version":3,"future":true}`+"\n"), 0o600)
		})
	}
	t.Cleanup(func() { memoryGuardedVectorStoreBeforeMutation = originalHook })
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, embedder, 4)
	var typed *memoryVectorProgressLoadError
	if hookErr != nil || !errors.As(err, &typed) || typed.Code != memoryErrUnsupportedVersion || !pending || pass.HistoryDropped != 0 {
		t.Fatalf("takeover pass=%+v pending=%t typed=%+v hook_err=%v err=%v", pass, pending, typed, hookErr, err)
	}
	ids, ok := raw.ids()
	if !ok {
		t.Fatal("history store unavailable after takeover")
	}
	if _, preserved := ids["preserve-on-takeover"]; !preserved {
		t.Fatalf("takeover reset mutated SQLite store: %v", ids)
	}
}

func TestMemoryVectorSyncSQLiteMutationFailurePersistsPendingHealth(t *testing.T) {
	brainDir, now, _, embedder := memoryVectorRegressionFixture(t)
	store, ok := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	if !ok {
		t.Fatal("history store unavailable")
	}
	raw, ok := store.(*historyVecStore)
	if !ok {
		t.Fatalf("history store type=%T", store)
	}
	if err := os.MkdirAll(filepath.Dir(raw.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(raw.path, 0o700); err != nil {
		t.Fatal(err)
	}
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, embedder, 4)
	if err == nil || !pending || pass.HistoryAdded != 0 {
		t.Fatalf("SQLite mutation failure pass=%+v pending=%t err=%v", pass, pending, err)
	}
	progress, present, loadErr := loadMemoryVectorProgress(brainDir)
	if loadErr != nil || !present || !progress.Pending || progress.ErrorCode != "memory_vector_sync_failed" || !progress.ResetComplete {
		t.Fatalf("SQLite failure progress=%+v present=%t err=%v", progress, present, loadErr)
	}
}

func TestMemoryVectorSyncFinalProgressWriteFailureKeepsCommittedVectorsRepairable(t *testing.T) {
	brainDir, now, index, embedder := memoryVectorRegressionFixture(t)
	progressPath := filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel))
	originalHook := memoryVectorProgressBeforeCommitCheck
	calls := 0
	var hookErr error
	memoryVectorProgressBeforeCommitCheck = func(string) {
		calls++
		if calls == 3 {
			if err := os.Remove(progressPath); err != nil {
				hookErr = err
				return
			}
			hookErr = os.Mkdir(progressPath, 0o700)
		}
	}
	t.Cleanup(func() { memoryVectorProgressBeforeCommitCheck = originalHook })
	pass, pending, err := syncMemoryProjectionVectorsWithEmbedder(context.Background(), brainDir, now, embedder, memoryVectorBatchRecords)
	if hookErr != nil || err == nil || !strings.Contains(err.Error(), "memory_vector_progress_write_failed") || pending || pass.HistoryAdded == 0 || pass.ConversationAdded == 0 {
		t.Fatalf("final write failure pass=%+v pending=%t calls=%d hook_err=%v err=%v", pass, pending, calls, hookErr, err)
	}
	historyStore, _ := newHistoryVectorStore(brainDir, embedder.ID(), embedder.Dim())
	conversationStore, _ := newConversationVectorStore(brainDir, conversationVectorModelID(embedder.ID()), embedder.Dim())
	historyIDs, historyOK := historyStore.ids()
	conversationIDs, conversationOK := conversationStore.ids()
	if !historyOK || !conversationOK || len(historyIDs) == 0 || len(conversationIDs) == 0 {
		t.Fatalf("committed vectors unavailable after progress failure: history=%v/%t conversation=%v/%t", historyIDs, historyOK, conversationIDs, conversationOK)
	}
	wantHistory, wantConversation := map[string]struct{}{}, map[string]struct{}{}
	for _, record := range index.Records {
		if record.Kind != "request" && record.Kind != conversationKind {
			wantHistory[record.ID] = struct{}{}
		}
		if record.Kind == conversationKind {
			wantConversation[record.ID] = struct{}{}
		}
	}
	if !reflect.DeepEqual(historyIDs, wantHistory) || !reflect.DeepEqual(conversationIDs, wantConversation) {
		t.Fatalf("committed IDs differ after progress failure: history=%v want=%v conversation=%v want=%v", historyIDs, wantHistory, conversationIDs, wantConversation)
	}
	memoryVectorProgressBeforeCommitCheck = originalHook
	if err := os.Remove(progressPath); err != nil {
		t.Fatal(err)
	}
	if _, retryErr := syncMemoryProjectionVectorsFully(context.Background(), brainDir, now.Add(time.Minute), embedder, nil); retryErr != nil {
		t.Fatalf("repair retry failed: %v", retryErr)
	}
	progress, present, loadErr := loadMemoryVectorProgress(brainDir)
	if loadErr != nil || !present || progress.Pending || progress.CompleteSourceDigest != progress.SourceDigest {
		t.Fatalf("repair progress=%+v present=%t err=%v", progress, present, loadErr)
	}
}

type deadlineMemoryEmbedder struct{}

func (*deadlineMemoryEmbedder) ID() string                  { return "test-deadline" }
func (*deadlineMemoryEmbedder) Dim() int                    { return 2 }
func (*deadlineMemoryEmbedder) Embed(string) []float32      { return nil }
func (*deadlineMemoryEmbedder) historyFusionEligible() bool { return true }
func (*deadlineMemoryEmbedder) EmbedContext(ctx context.Context, _ string) []float32 {
	<-ctx.Done()
	return nil
}

func TestRunMemoryVectorLaneParentDeadlineYieldsForContinuation(t *testing.T) {
	brainDir, now, _, _ := memoryVectorRegressionFixture(t)
	originalEmbedder := defaultEmbedder()
	defaultEmbedderInst = &deadlineMemoryEmbedder{}
	t.Cleanup(func() { defaultEmbedderInst = originalEmbedder })
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "deadline-test")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stats := runMemoryVectorLane(ctx, brainDir, now, "deadline-owner")
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) || !stats.VectorPending || !stats.VectorContinue || !slices.Contains(stats.HealthIssues, "memory_vector_sync_failed") {
		t.Fatalf("deadline stats=%+v ctx_err=%v", stats, ctx.Err())
	}
}

type fixedIDMemoryEmbedder struct {
	id   string
	fail bool
}

func (e *fixedIDMemoryEmbedder) ID() string             { return e.id }
func (e *fixedIDMemoryEmbedder) Dim() int               { return 2 }
func (e *fixedIDMemoryEmbedder) Embed(string) []float32 { return []float32{0, 1} }
func (e *fixedIDMemoryEmbedder) EmbedContext(ctx context.Context, _ string) []float32 {
	if ctx.Err() != nil || e.fail {
		return nil
	}
	return []float32{0, 1}
}
