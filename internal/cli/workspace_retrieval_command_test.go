package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/spf13/cobra"
)

func TestWorkspaceRetrievalCommandSerialEquivalent(t *testing.T) {
	t.Parallel()
	opts, manifest := workspaceRetrievalFixture(t, 3, 20)
	for i, repo := range manifest.Repos[:2] {
		writeWorkspaceDocIndex(t, opts.Env, repo.RepoKey, docRecord{ID: fmt.Sprint(i), Text: "checkpoint retry policy", Path: "docs/retry.md"})
	}
	broken, err := base.BrainDirForKey(opts.Env, manifest.Repos[2].RepoKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, factstore.RepositoryPath), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	// A runner without the concurrency capability exercises exactly the serial
	// command path, including qualification, grouping, source selection and errors.
	serial := opts
	serial.Runner = runOnlyCommandRunner{inner: &fakeCommandRunner{}}
	for _, source := range []string{"fact", "doc", "all"} {
		for _, verb := range []string{"search", "query"} {
			for _, jsonOut := range []bool{false, true} {
				args := []string{"workspace", verb, manifest.Name, "checkpoint retry", "--limit", "3", "--source", source}
				if jsonOut {
					args = append(args, "--json")
				}
				want, err := execute(t, NewRootCommand(serial), args...)
				if err != nil {
					t.Fatal(err)
				}
				got, err := execute(t, NewRootCommand(opts), args...)
				if err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("%v: concurrent output differs\ngot %s\nwant %s", args, got, want)
				}
				if !jsonOut {
					continue
				}
				var payload struct {
					Results []workspaceRetrieveResult `json:"results"`
				}
				if err := json.Unmarshal([]byte(got), &payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Results) != len(manifest.Repos) {
					t.Fatalf("member disappeared: %s", got)
				}
				for i, group := range payload.Results {
					if group.RepoKey != manifest.Repos[i].RepoKey || len(group.Results) > 3 {
						t.Fatalf("group changed: %+v", group)
					}
					if i == 2 && source != "doc" && group.Error == "" {
						t.Fatalf("broken member lost its error: %+v", group)
					}
					for _, result := range group.Results {
						if source != "all" && result.Source != source {
							t.Fatalf("source leaked: %+v", result)
						}
						if !strings.HasPrefix(result.ID, group.RepoKey+"/") {
							t.Fatalf("unqualified result: %+v", result)
						}
					}
				}
			}
		}
	}
}

func TestWorkspaceRetrievalCommandCancelledBeforeAdmission(t *testing.T) {
	t.Parallel()
	opts, manifest := workspaceRetrievalFixture(t, 2, 10)
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			t.Parallel()
			var ctx context.Context
			var cancel context.CancelFunc
			expected := context.Canceled
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
				expected = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			cancel()
			cmd := &cobra.Command{}
			out := &bytes.Buffer{}
			cmd.SetOut(out)
			cmd.SetContext(ctx)
			err := runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 10, json: true}, retrieval.ModeLexical, manifest.Name, "checkpoint retry")
			if !errors.Is(err, expected) || out.Len() != 0 {
				t.Fatalf("err=%v bytes=%d", err, out.Len())
			}
		})
	}
}

func TestWorkspaceRetrievalCommandCancelsActiveMembers(t *testing.T) {
	t.Parallel()
	opts, manifest := workspaceRetrievalFixture(t, 2, 10)
	var dirs []string
	for _, repo := range manifest.Repos {
		writeWorkspaceDocIndex(t, opts.Env, repo.RepoKey, docRecord{ID: "retry", Text: "checkpoint retry"})
		dir, err := base.BrainDirForKey(opts.Env, repo.RepoKey)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
		unlock, err := brainstore.AcquireBrainWriteLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(sync.OnceFunc(unlock))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() {
		done <- runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 10, source: "doc", json: true}, retrieval.ModeLexical, manifest.Name, "checkpoint retry")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		waiting := 0
		for _, dir := range dirs {
			files, err := filepath.Glob(filepath.Join(dir, brainstore.BrainLockDirName, "write-foreground-*.lock"))
			if err != nil {
				t.Fatal(err)
			}
			waiting += len(files)
		}
		if waiting == len(dirs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("members did not reach cache-lock waits concurrently")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := receiveRetrievalTest(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("cancelled command wrote %d bytes", out.Len())
	}
}

func TestWorkspaceRetrievalGraphLookupHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := retrieveWorkspaceGraphCrossEdgesContext(ctx, EntireEnv{}, "unused", "retry", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("graph lookup ignored cancellation: %v", err)
	}
}

func TestWorkspaceRetrievalCommandWithoutContext(t *testing.T) {
	t.Parallel()
	opts, manifest := workspaceRetrievalFixture(t, 2, 10)
	cmd := &cobra.Command{}
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	if cmd.Context() != nil {
		t.Fatal("fixture unexpectedly has a context")
	}
	if err := runWorkspaceRetrieve(cmd, opts, workspaceRetrieveOptions{limit: 3, source: "fact", json: true}, retrieval.ModeLexical, manifest.Name, "checkpoint retry"); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Results []workspaceRetrieveResult `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("got %d member groups", len(payload.Results))
	}
}
