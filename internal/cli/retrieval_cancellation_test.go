package cli

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

func TestRetrievalContextCancelledBeforeRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, source := range []string{"fact", "history", "doc", "conversation", "all"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			found, err := retrieveUnifiedWithOptionsContext(ctx, "", t.TempDir(), "main", "retry", 10, retrieval.ModeLexical, retrievalOptions{Source: source})
			if !errors.Is(err, context.Canceled) || len(found) != 0 {
				t.Fatalf("got %v, %v", found, err)
			}
		})
	}
}

func TestRetrievalContextStopsBetweenSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := factstore.Write(dir, []factRecord{{ID: "fact:a", Text: "retry policy", Status: factStatusActive}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := retrievalEngineDepsContext(ctx)
	deps.RankFacts = func(facts []factRecord, _ string, _ int, _ bool) []factRecord { cancel(); return facts }
	deps.RankHistoryLexical = func(string, *historySourceManifest, string, string, int) ([]scoredHistoryRecord, error) {
		t.Error("history started after cancellation")
		return nil, nil
	}
	found, err := retrieval.UnifiedWithOptionsContext(ctx, deps, "", dir, "main", "retry", 10, retrieval.ModeLexical, retrievalOptions{})
	if !errors.Is(err, context.Canceled) || len(found) != 0 {
		t.Fatalf("got %v, %v", found, err)
	}
}

func TestRetrievalContextJoinsCancelledDocCacheWait(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unlock, err := brainstore.AcquireBrainWriteLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(unlock)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		db, err := openDocFTSContext(ctx, dir, docTestIndex())
		if db != nil {
			_ = db.Close()
		}
		done <- err
	}()
	// A foreground intent file proves acquisition is already waiting on the
	// held lock, so cancelling before the goroutine starts cannot pass this test.
	deadline := time.Now().Add(2 * time.Second)
	for {
		intents, err := filepath.Glob(filepath.Join(dir, brainstore.BrainLockDirName, "write-foreground-*.lock"))
		if err != nil {
			t.Fatal(err)
		}
		if len(intents) > 0 {
			break
		}
		if time.Now().After(deadline) {
			release()
			<-done
			t.Fatal("cache waiter never reached lock acquisition")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		release()
		<-done
		t.Fatal("cache waiter did not stop while write lock stayed held")
	}
	// No handle or waiter may remain after the cancelled call has returned.
}

func TestRetrievalContextPreservesRanking(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	facts := []factRecord{{ID: "fact:a", Text: "retry bounded checkpoint", Status: factStatusActive}, {ID: "fact:b", Text: "retry policy", Status: factStatusActive}}
	if err := factstore.Write(dir, facts); err != nil {
		t.Fatal(err)
	}
	want, err := retrieveUnifiedWithOptions("", dir, "main", "retry checkpoint", 10, retrieval.ModeLexical, retrievalOptions{Source: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := retrieveUnifiedWithOptionsContext(context.Background(), "", dir, "main", "retry checkpoint", 10, retrieval.ModeLexical, retrievalOptions{Source: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranking changed:\ngot %+v\nwant %+v", got, want)
	}
}

// A context that cancels at a record boundary makes the mid-scan test
// deterministic, without timing a goroutine against corpus size.
type cancelOnCheckContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *cancelOnCheckContext) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRetrievalContextStopsFactScoring(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelOnCheckContext{Context: parent, cancel: cancel, remaining: 5}
	facts := make([]factRecord, 100)
	for i := range facts {
		facts[i] = factRecord{Text: "retry checkpoint", Status: factStatusActive}
	}
	if got := newBM25CorpusContext(ctx, facts); len(got.docTokens[len(facts)-1]) != 0 {
		t.Fatal("corpus scan continued after cancellation")
	}
	if !errors.Is(parent.Err(), context.Canceled) {
		t.Fatal("test did not reach the cancellation boundary")
	}
	if got := rankFactsAtContext(parent, facts, "retry", 10, false, time.Now()); len(got) != 0 {
		t.Fatalf("cancelled scorer returned %d records", len(got))
	}
}
