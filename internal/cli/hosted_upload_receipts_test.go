package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/factsync"
)

func TestHostedUploadAcknowledgesVersionsAndTargets(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, _, url := hostedSyncHarness(t, fake, true)
	rec := hostedSyncTestFact("receipt test", opts.Now())
	write := func() {
		t.Helper()
		if err := factstore.Write(storage.BrainDir, "main", []factRecord{rec}); err != nil {
			t.Fatal(err)
		}
	}
	write()
	push := func(target string) {
		t.Helper()
		if _, _, err := pushHostedFacts(context.Background(), storage, &factsync.HTTPServer{BaseURL: url, Token: fakeJWT}, target, "main"); err != nil {
			t.Fatal(err)
		}
	}
	push("repo1")
	push("repo1")
	if err := factstore.Write(storage.BrainDir, "feature", []factRecord{rec}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pushHostedFacts(context.Background(), storage, &factsync.HTTPServer{BaseURL: url, Token: fakeJWT}, "repo1", "feature"); err != nil {
		t.Fatal(err)
	}
	if fake.upsertCount() != 1 {
		t.Fatalf("unchanged record resent: %d", fake.upsertCount())
	}
	// Identity stays fixed when an additional checkpoint is attached.
	rec.Provenance = append(rec.Provenance, factmerge.Anchor{SessionID: "s2", CheckpointID: "cp2", Commit: "commit2"})
	write()
	push("repo1")
	if fake.upsertCount() != 2 {
		t.Fatal("changed provenance was not uploaded")
	}
	// Another view can retain an older acknowledged version. Do not alternate
	// uploads of old and new versions each time those views are scanned.
	if _, _, err := pushHostedFacts(context.Background(), storage, &factsync.HTTPServer{BaseURL: url, Token: fakeJWT}, "repo1", "feature"); err != nil {
		t.Fatal(err)
	}
	push("repo1")
	if fake.upsertCount() != 2 {
		t.Fatal("acknowledged view versions were uploaded again")
	}
	push("repo2")
	if fake.upsertCount() != 3 {
		t.Fatal("receipt leaked across target repositories")
	}
	other := newHostedFakeServer(t, &hostedFake{})
	counts, _, err := pushHostedFacts(context.Background(), storage, &factsync.HTTPServer{BaseURL: other.URL, Token: fakeJWT}, "repo2", "main")
	if err != nil || counts.Added != 1 {
		t.Fatalf("receipt leaked across servers: %+v %v", counts, err)
	}
}

func TestHostedUploadConcurrentCompletionIncludesNewRecords(t *testing.T) {
	storage := repoStorage{BrainDir: t.TempDir()}
	first := hostedSyncTestFact("first", time.Now().UTC())
	second := hostedSyncTestFact("second", first.CreatedAt)
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{first}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var batches [][]factmerge.Record
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records, err := factmerge.ParseNDJSON(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		mu.Lock()
		batches = append(batches, records)
		n := len(batches)
		mu.Unlock()
		if n == 1 {
			close(entered)
			<-release
		}
		_ = json.NewEncoder(w).Encode(factsync.UpsertCounts{Added: len(records)})
	}))
	defer srv.Close()
	client := &factsync.HTTPServer{BaseURL: srv.URL, Token: fakeJWT}
	results := make(chan error, 2)
	push := func() {
		_, _, err := pushHostedFacts(context.Background(), storage, client, "repo1", "main")
		results <- err
	}
	go push()
	<-entered
	// A memory writer must not wait for the network upload lock.
	first.Provenance = append(first.Provenance, factmerge.Anchor{SessionID: "later", CheckpointID: "later-cp"})
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{first, second}); err != nil {
		close(release)
		t.Fatal(err)
	}
	go push()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(batches) != 2 || len(batches[0]) != 1 || len(batches[1]) != 2 {
		t.Fatalf("batches=%+v", batches)
	}
	foundUpdated, foundNew := false, false
	for _, rec := range batches[1] {
		if rec.ID == first.ID && len(rec.Provenance) == 2 {
			foundUpdated = true
		}
		if rec.ID == second.ID {
			foundNew = true
		}
	}
	if !foundUpdated || !foundNew {
		t.Fatal("concurrent record update was acknowledged without upload")
	}
	// A subsequent process-equivalent invocation uses persisted acknowledgements.
	push()
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 {
		t.Fatal("persisted receipts did not deduplicate")
	}
}

func TestHostedUploadPartialFailureRetriesOnlyUnacknowledgedChunk(t *testing.T) {
	storage := repoStorage{BrainDir: t.TempDir()}
	records := make([]factRecord, hostedUpsertChunk+1)
	now := time.Now().UTC()
	for i := range records {
		records[i] = hostedSyncTestFact(fmt.Sprintf("fact %d", i), now)
	}
	if err := factstore.Write(storage.BrainDir, "main", records); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batch, err := factmerge.ParseNDJSON(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		calls++
		sizes = append(sizes, len(batch))
		if calls == 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(factsync.UpsertCounts{Added: len(batch)})
	}))
	defer srv.Close()
	client := &factsync.HTTPServer{BaseURL: srv.URL, Token: fakeJWT}
	if _, _, err := pushHostedFacts(context.Background(), storage, client, "repo1", "main"); err == nil {
		t.Fatal("expected failed second chunk")
	}
	if _, _, err := pushHostedFacts(context.Background(), storage, client, "repo1", "main"); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 3 || sizes[0] != hostedUpsertChunk || sizes[1] != 1 || sizes[2] != 1 {
		t.Fatalf("batch sizes=%v", sizes)
	}
}

func TestDistillationCompletionSyncsAfterPersistence(t *testing.T) {
	f := newSetupTestFixture(t, "one", "two")
	var output bytes.Buffer
	cmd := setupTestCommand(t, &output, defaultSetupOptions())
	old := notifyHostedFactsSync
	t.Cleanup(func() { notifyHostedFactsSync = old })
	calls := 0
	notifyHostedFactsSync = func(_ context.Context, _ io.Writer, _ Options, _ string, storage repoStorage, branch string) (hostedSyncOutcome, error) {
		calls++
		facts, err := factstore.Load(storage.BrainDir, branch)
		if err != nil || len(facts) == 0 {
			t.Errorf("sync preceded persistence: %d %v", len(facts), err)
		}
		return hostedSyncOutcome{Attempted: true}, nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, maxSessions: 1, concurrency: 1, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute,
		run: func(context.Context, string, []string, []byte, time.Duration) (string, error) {
			return "project.tooling.stack\tUse Go.\n", nil
		}}
	if err := runDistill(context.Background(), cmd, f.opts, opts, f.repoDir); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("completion did not sync capped results")
	}
	calls = 0
	opts.dryRun = true
	if err := runDistill(context.Background(), cmd, f.opts, opts, f.repoDir); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("dry run synced")
	}
}

func TestHostedUploadWaitRespectsCancellation(t *testing.T) {
	storage := repoStorage{BrainDir: t.TempDir()}
	lock, err := acquireHostedUploadLock(context.Background(), storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := pushHostedFacts(ctx, storage, &factsync.HTTPServer{BaseURL: "https://unused.example"}, "repo1", "main"); err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
}

func TestHostedUploadAmbiguousAcknowledgementCanRetry(t *testing.T) {
	storage := repoStorage{BrainDir: t.TempDir()}
	rec := hostedSyncTestFact("ambiguous response", time.Now().UTC())
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{rec}); err != nil {
		t.Fatal(err)
	}
	stored := map[string]factmerge.Record{}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records, err := factmerge.ParseNDJSON(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		for _, rec := range records {
			stored[rec.ID] = rec
		}
		requests++
		// The server committed the batch, but the acknowledgement was interrupted.
		if requests == 1 {
			_, _ = w.Write([]byte("{"))
			return
		}
		_ = json.NewEncoder(w).Encode(factsync.UpsertCounts{Unchanged: len(records)})
	}))
	defer srv.Close()
	client := &factsync.HTTPServer{BaseURL: srv.URL, Token: fakeJWT}
	if _, _, err := pushHostedFacts(context.Background(), storage, client, "repo1", "main"); err == nil {
		t.Fatal("expected interrupted acknowledgement")
	}
	if _, _, err := pushHostedFacts(context.Background(), storage, client, "repo1", "main"); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || requests != 2 {
		t.Fatalf("stored=%d requests=%d", len(stored), requests)
	}
}
