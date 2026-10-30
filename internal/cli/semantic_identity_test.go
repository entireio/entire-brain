package cli

import (
	"context"
	"errors"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSemanticRefreshIdentityWithUnchangedTree(t *testing.T) {
	repo := t.TempDir()
	env := semanticTestEnv(t, repo)
	runner := semanticFixtureRunner(repo, semanticFixtureSnapshot("1.0"))
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	ctx := context.Background()
	if err := runSemanticIndex(ctx, &cobra.Command{}, opts, semanticIndexOptions{graphBinary: "entire"}, repo); err != nil {
		t.Fatal(err)
	}
	brain := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	if needed, _, err := semanticRefreshNeeded(ctx, opts, brain, repo, manifest, false, "entire"); err != nil || needed {
		t.Fatalf("legacy unchanged tree: %v %v", needed, err)
	}
	delete(runner.graphStoreFixtures, repo+"\x00full")
	if needed, _, err := semanticRefreshNeeded(ctx, opts, brain, repo, manifest, false, "entire"); err != nil || !needed {
		t.Fatalf("missing provider entry did not trigger refresh: %v %v", needed, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := semanticRefreshNeeded(cancelled, opts, brain, repo, manifest, false, "entire"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
}
