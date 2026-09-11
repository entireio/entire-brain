package cli

import (
	"context"
	"errors"
	"github.com/spf13/cobra"
	"path/filepath"
	"testing"
	"time"
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
	brain := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brain)
	if err != nil {
		t.Fatal(err)
	}
	if needed, _, err := semanticRefreshNeeded(ctx, opts, brain, repo, manifest, false, "entire"); err != nil || needed {
		t.Fatalf("legacy unchanged tree: %v %v", needed, err)
	}
	key := fakeCommandKey("entire", "graph", "version", "--json")
	runner.responses[key] = fakeCommandResponse{err: errors.New("unknown graph version command")}
	needed, warning, err := semanticRefreshNeeded(ctx, opts, brain, repo, manifest, false, "entire")
	if err != nil || needed || warning == "" {
		t.Fatalf("probe failure aborted warm reuse: %v %q %v", needed, warning, err)
	}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "changed-tree\n"}
	needed, warning, err = semanticRefreshNeeded(ctx, opts, brain, repo, manifest, false, "entire")
	if err != nil || !needed || warning == "" {
		t.Fatalf("probe failure masked changed tree: %v %q %v", needed, warning, err)
	}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree111\n"}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := semanticRefreshNeeded(cancelled, opts, brain, repo, manifest, false, "entire"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
	runner.responses[fakeCommandKey("entire", "graph", "version", "--json")] = fakeCommandResponse{stdout: `{"identity_revision":"scope-1"}`}
	if needed, _, err := semanticRefreshNeeded(ctx, opts, brain, repo, manifest, false, "entire"); err != nil || !needed {
		t.Fatalf("provider upgrade ignored: %v %v", needed, err)
	}
	report, err := semanticStaleReport(ctx, opts, repo)
	if err != nil {
		t.Fatal(err)
	}
	if report.Axes["provider"].State == "ok" {
		t.Fatal("old identity reported current")
	}
	// Snapshot and manifest must attest the same identity even on import/readback.
	if err := validateSemanticSourceMatchesSnapshot(&semanticSourceManifest{IdentityRevision: "scope-1"}, semanticHeader{}, semanticCounts{}); err == nil {
		t.Fatal("mixed snapshot/manifest identity accepted")
	}
}
