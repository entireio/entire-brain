package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/entireio/entire-brain/internal/cli/workspace"
)

// Fixed synthetic corpora make the worker/memory tradeoff reproducible without
// any user store, remote service, model, or checkout-dependent ranking input.
func workspaceRetrievalFixture(t testing.TB, members, factsPerMember int) (Options, workspace.Manifest) {
	t.Helper()
	root := t.TempDir()
	env := EntireEnv{PluginDataDir: filepath.Join(root, "data"), PluginConfigDir: filepath.Join(root, "config"), PluginCacheDir: filepath.Join(root, "cache"), PluginStateDir: filepath.Join(root, "state")}
	manifest := workspace.Manifest{SchemaVersion: workspace.SchemaVersion, Name: "retrieval"}
	for r := range members {
		key := fmt.Sprintf("local/member-%02d", r)
		dir, err := base.BrainDirForKey(env, key)
		if err != nil {
			t.Fatal(err)
		}
		facts := make([]factRecord, factsPerMember)
		for i := range facts {
			facts[i] = factRecord{ID: fmt.Sprintf("fact:%d-%d", r, i), Text: fmt.Sprintf("Checkpoint retry policy for component %d uses bounded backoff and preserves session provenance across recovery. The deployment region is local and callers must preserve the durable cursor after each successful write.", i), Paths: []string{"architecture.checkpoint.retry"}, Status: factStatusActive, Branch: "main", Origin: "authored", CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()}
		}
		if err := factstore.Write(dir, facts); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(bmanifest.Manifest{SchemaVersion: bmanifest.SchemaVersion, RepoKey: key})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, bmanifest.FileName), data, 0600); err != nil {
			t.Fatal(err)
		}
		manifest.Repos = append(manifest.Repos, workspace.Repo{RepoKey: key})
	}
	if err := workspace.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}
	return Options{Version: "test", Env: env, Runner: ExecRunner{}, Now: time.Now}, manifest
}

// Measures member store loading and ranking. End-to-end CLI latency and peak
// process RSS are separate measurements; -benchmem reports cumulative allocation.
func BenchmarkWorkspaceRetrievalWorkers(b *testing.B) {
	for _, members := range []int{1, 4, 8} {
		opts, manifest := workspaceRetrievalFixture(b, members, 10000)
		for _, workers := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("repos=%d/workers=%d", members, workers), func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					results, err := retrieveWorkspaceMembers(context.Background(), manifest.Repos, workers, func(ctx context.Context, repo workspace.Repo) workspaceRetrieveResult {
						dir, err := base.BrainDirForKey(opts.Env, repo.RepoKey)
						if err != nil {
							return workspaceRetrieveResult{Error: err.Error()}
						}
						found, err := retrieveUnifiedWithOptionsContext(ctx, "", dir, "main", "checkpoint retry", 10, retrieval.ModeLexical, retrievalOptions{Source: "fact"})
						result := workspaceRetrieveResult{RepoKey: repo.RepoKey, Results: found}
						if err != nil {
							result.Error = err.Error()
						}
						return result
					})
					if err != nil {
						b.Fatal(err)
					}
					for _, result := range results {
						if result.Error != "" || len(result.Results) != 10 {
							b.Fatalf("unexpected member: %+v", result)
						}
					}
				}
			})
		}
	}
}
