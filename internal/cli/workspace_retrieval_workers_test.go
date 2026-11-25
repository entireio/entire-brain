package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/workspace"
)

func retrievalTestRepos(n int) []workspace.Repo {
	repos := make([]workspace.Repo, n)
	for i := range repos {
		repos[i] = workspace.Repo{RepoKey: fmt.Sprintf("local/%02d", i)}
	}
	return repos
}

func receiveRetrievalTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("workspace retrieval did not make progress")
		var zero T
		return zero
	}
}

func TestWorkspaceRetrievalWorkersBoundedAndOrdered(t *testing.T) {
	t.Parallel()
	repos := retrievalTestRepos(3)
	for _, workers := range []int{1, 2, 3, 20} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			t.Parallel()
			started := make(chan string, len(repos))
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var active, peak, calls atomic.Int32
			done := make(chan []workspaceRetrieveResult, 1)
			go func() {
				results, _ := retrieveWorkspaceMembers(context.Background(), repos, workers, func(ctx context.Context, repo workspace.Repo) workspaceRetrieveResult {
					n := active.Add(1)
					for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
					}
					calls.Add(1)
					started <- repo.RepoKey
					<-release
					active.Add(-1)
					return workspaceRetrieveResult{RepoKey: repo.RepoKey, Error: "member failure"}
				})
				done <- results
			}()
			want := min(workers, len(repos))
			for range want {
				receiveRetrievalTest(t, started)
			}
			select {
			case key := <-started:
				t.Fatalf("unbounded admission: %s", key)
			default:
			}
			close(release)
			results := receiveRetrievalTest(t, done)
			if int(peak.Load()) != want || active.Load() != 0 || int(calls.Load()) != len(repos) {
				t.Fatalf("peak=%d active=%d calls=%d", peak.Load(), active.Load(), calls.Load())
			}
			for i, result := range results {
				if result.RepoKey != repos[i].RepoKey || result.Error != "member failure" {
					t.Fatalf("position %d: %+v", i, result)
				}
			}
		})
	}
}

func TestWorkspaceRetrievalWorkersReverseCompletion(t *testing.T) {
	t.Parallel()
	repos := retrievalTestRepos(3)
	release := map[string]chan struct{}{}
	for _, repo := range repos {
		release[repo.RepoKey] = make(chan struct{})
	}
	defer func() {
		for _, ch := range release {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	started := make(chan string, 3)
	completed := make(chan string, 3)
	done := make(chan []workspaceRetrieveResult, 1)
	go func() {
		results, _ := retrieveWorkspaceMembers(context.Background(), repos, 3, func(ctx context.Context, repo workspace.Repo) workspaceRetrieveResult {
			started <- repo.RepoKey
			<-release[repo.RepoKey]
			completed <- repo.RepoKey
			return workspaceRetrieveResult{RepoKey: repo.RepoKey}
		})
		done <- results
	}()
	for range repos {
		receiveRetrievalTest(t, started)
	}
	for i := len(repos) - 1; i >= 0; i-- {
		close(release[repos[i].RepoKey])
		if got := receiveRetrievalTest(t, completed); got != repos[i].RepoKey {
			t.Fatalf("completion=%s", got)
		}
	}
	results := receiveRetrievalTest(t, done)
	for i, result := range results {
		if result.RepoKey != repos[i].RepoKey {
			t.Fatalf("position %d: %s", i, result.RepoKey)
		}
	}
}

func TestWorkspaceRetrievalWorkersCancelAndJoin(t *testing.T) {
	t.Parallel()
	for range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{}, 2)
		observed := make(chan struct{}, 2)
		cleanup := make(chan struct{})
		finish := sync.OnceFunc(func() { cancel(); close(cleanup) })
		t.Cleanup(finish)
		done := make(chan error, 1)
		var calls, exited atomic.Int32
		go func() {
			_, err := retrieveWorkspaceMembers(ctx, retrievalTestRepos(20), 2, func(ctx context.Context, repo workspace.Repo) workspaceRetrieveResult {
				calls.Add(1)
				started <- struct{}{}
				<-ctx.Done()
				observed <- struct{}{}
				<-cleanup
				exited.Add(1)
				return workspaceRetrieveResult{}
			})
			done <- err
		}()
		for range 2 {
			receiveRetrievalTest(t, started)
		}
		cancel()
		for range 2 {
			receiveRetrievalTest(t, observed)
		}
		select {
		case err := <-done:
			finish()
			t.Fatalf("returned before workers joined: %v", err)
		default:
		}
		finish()
		if err := receiveRetrievalTest(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		if calls.Load() != 2 || exited.Load() != 2 {
			t.Fatalf("calls=%d exited=%d", calls.Load(), exited.Load())
		}
	}
}

func TestWorkspaceRetrievalWorkersCancelledBeforeAdmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := retrieveWorkspaceMembers(ctx, retrievalTestRepos(5), 2, func(context.Context, workspace.Repo) workspaceRetrieveResult {
		t.Error("cancelled job started")
		return workspaceRetrieveResult{}
	})
	if !errors.Is(err, context.Canceled) || results != nil {
		t.Fatalf("got %v, %v", results, err)
	}
}
