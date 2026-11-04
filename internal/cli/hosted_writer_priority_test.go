package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/factsync"
)

// Block the server, not a timing assumption about the CPU. A foreground write
// must complete while the network is still blocked. A concurrent cache commit
// forces the prepared pull to retry instead of replacing newer feed state.
func TestHostedPullYieldsToForegroundAndRevalidatesCache(t *testing.T) {
	for _, mode := range []string{"local write", "concurrent sync", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			cacheChanged := mode == "concurrent sync"
			opts, storage, repoDir, _ := hostedSyncHarness(t, &hostedFake{}, false)
			remote := hostedSyncTestFact("remote memory", opts.Now())
			local := hostedSyncTestFact("foreground memory", opts.Now())
			retired := hostedSyncTestFact("retired while network waits", opts.Now())
			if err := factstore.Write(storage.BrainDir, []factRecord{retired}); err != nil {
				t.Fatal(err)
			}
			staleActive := retired
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				once.Do(func() {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				})
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []factsync.FeedItem{{Record: remote, TrailID: "merged", CreatedBy: "ulrich"}, {Record: staleActive, TrailID: "merged", CreatedBy: "ulrich"}}})
			}))
			defer srv.Close()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			ctx, cancel := context.WithTimeout(brainstore.BackgroundWrites(context.Background()), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, _, err := pullHostedFacts(ctx, opts, repoDir, storage, &factsync.HTTPServer{BaseURL: srv.URL}, "repo1", srv.URL, fakeJWT)
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("pull never reached network")
			}
			writeCtx, stopWrite := context.WithTimeout(context.Background(), time.Second)
			err := brainstore.WithBrainWriteLockContext(writeCtx, storage.BrainDir, func() error {
				retired.Status = factStatusRetracted
				if err := factstore.Write(storage.BrainDir, []factRecord{local, retired}); err != nil {
					return err
				}
				if mode == "disconnect" {
					data, err := json.Marshal(brainstore.HostedRepoBinding{OptedOut: true})
					if err != nil {
						return err
					}
					return brainstore.WriteBrainRelativeFileAtomic(storage.BrainDir, brainstore.HostedRepoBindingFile, data, 0600)
				}
				if cacheChanged {
					feedDir, err := hostedFeedDirForKey(opts.Env, storage.Key)
					if err != nil {
						return err
					}
					return writeHostedFeedCache(feedDir, hostedFeedState{RepoID: "repo1", BaseURL: srv.URL, Cursor: "newer"}, map[string]factsync.FeedItem{local.ID: {Record: local, CreatedBy: "ulrich"}})
				}
				return nil
			})
			stopWrite()
			if err != nil {
				t.Fatalf("foreground blocked by network: %v", err)
			}
			close(release)
			pullErr := <-done
			if mode == "disconnect" {
				if pullErr == nil {
					t.Fatal("pull ignored concurrent disconnect")
				}
			} else if pullErr != nil {
				t.Fatal(pullErr)
			}
			records, err := factstore.Load(storage.BrainDir)
			if err != nil {
				t.Fatal(err)
			}
			if indexOfFact(records, local.ID) < 0 || (mode != "disconnect" && indexOfFact(records, remote.ID) < 0) {
				t.Fatalf("write lost: %+v", records)
			}
			if records[indexOfFact(records, retired.ID)].Status != factStatusRetracted {
				t.Fatal("concurrent retirement revived")
			}
			if mode == "disconnect" && indexOfFact(records, remote.ID) >= 0 {
				t.Fatal("pull adopted records after disconnect")
			}
			if cacheChanged && calls.Load() < 2 {
				t.Fatal("stale cache was not retried")
			}
		})
	}
}
