package cli

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/entityindex"
	"github.com/spf13/cobra"
)

type upgradeGraphRunner struct{ graph string }

func (r *upgradeGraphRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name == "entire" && len(args) > 0 && args[0] == "graph" {
		return (ExecRunner{}).Run(ctx, dir, r.graph, args[1:]...)
	}
	return (ExecRunner{}).Run(ctx, dir, name, args...)
}

func (r *upgradeGraphRunner) Stream(ctx context.Context, dir, name string, args ...string) (CommandStream, error) {
	if name == "entire" && len(args) > 0 && args[0] == "graph" {
		return (ExecRunner{}).Stream(ctx, dir, r.graph, args[1:]...)
	}
	return (ExecRunner{}).Stream(ctx, dir, name, args...)
}

// Run against actual pre/post-upgrade Graph builds. The source tree and source
// commit stay unchanged during the provider switch; mocks cannot establish this.
func TestGraphIdentityUpgradeLive(t *testing.T) {
	before, after := os.Getenv("BRAIN_TEST_GRAPH_BEFORE"), os.Getenv("BRAIN_TEST_GRAPH_AFTER")
	if before == "" || after == "" {
		t.Skip("set BRAIN_TEST_GRAPH_BEFORE and BRAIN_TEST_GRAPH_AFTER to Graph binaries")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, key := range []string{"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	repo := t.TempDir()
	ctx := context.Background()
	r := &upgradeGraphRunner{before}
	git := func(args ...string) string {
		t.Helper()
		out, stderr, err := r.Run(ctx, repo, "git", args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, stderr)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "Upgrade test")
	git("config", "user.email", "upgrade@example.invalid")
	git("remote", "add", "origin", "https://github.com/example/repo.git")
	write := func(value string) {
		t.Helper()
		for _, ext := range []string{"js", "ts"} {
			src := "class A {\n  m() {\n    function helper() { return " + value + "; }\n    return helper();\n  }\n}\n"
			if err := os.WriteFile(filepath.Join(repo, "a."+ext), []byte(src), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("1")
	git("add", ".")
	git("commit", "-m", "first\n\nEntire-Checkpoint: abc123abc123")
	first := git("rev-parse", "HEAD")
	write("2")
	git("commit", "-am", "second\n\nEntire-Checkpoint: def456def456")
	head := git("rev-parse", "HEAD")
	env := semanticTestEnv(t, repo)
	opts := Options{Env: env, Runner: r, Now: time.Now, Version: "test"}
	if err := runSemanticIndex(ctx, &cobra.Command{}, opts, semanticIndexOptions{graphBinary: "entire"}, repo); err != nil {
		t.Fatal(err)
	}
	_, storage, store, err := openEntityIndexStore(ctx, opts, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entityindex.Build(ctx, r, store, entityindex.BuildOptions{RepoDir: repo, Now: time.Now}); err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	oldID := "gh/example/repo:JavaScript:a.js:method:A.helper"
	symbols, err := loadSemanticSymbolsByIDSnapshot(filepath.Join(storage.BrainDir, manifest.Sources.Semantic.SnapshotPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := symbols[oldID]; !ok {
		t.Fatalf("old provider did not reproduce phantom method: %v", symbols)
	}
	r.graph = after
	revision, err := entityindex.ProviderIdentity(ctx, r, repo, "entire")
	if err != nil || revision == "" {
		t.Fatalf("new revision=%q, %v", revision, err)
	}
	needed, err := semanticRefreshNeeded(ctx, opts, storage.BrainDir, repo, manifest, false, "entire")
	if err != nil || !needed {
		t.Fatalf("unchanged tree upgrade not refreshed: %v %v", needed, err)
	}
	if err := runSemanticIndex(ctx, &cobra.Command{}, opts, semanticIndexOptions{graphBinary: "entire", force: true}, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := entityindex.Build(ctx, r, store, entityindex.BuildOptions{RepoDir: repo, IdentityRevision: revision}); err == nil {
		t.Fatal("mixed old and new history accepted")
	}
	cmd := newEntitiesMigrateCommand(opts)
	cmd.SetArgs([]string{repo})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := entityHistory(ctx, opts, repo, "A.m.helper", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Warnings) != 0 {
		t.Fatal(history.Warnings)
	}
	for _, ext := range []string{"js", "ts"} {
		found := false
		for _, m := range history.Matches {
			if m.EntityKey != "a."+ext+"#function#A.m.helper" {
				continue
			}
			found = true
			seen := map[string]bool{}
			checkpoints := map[string]bool{}
			for _, o := range m.Occurrences {
				seen[o.Commit] = true
				for _, c := range o.CheckpointIDs {
					checkpoints[c] = true
				}
			}
			if !seen[first] || !seen[head] || !checkpoints["abc123abc123"] || !checkpoints["def456def456"] {
				t.Fatalf("lost history/provenance: %+v", m)
			}
		}
		if !found {
			t.Fatalf("missing migrated %s history: %+v", ext, history)
		}
	}
	manifest, err = loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	symbols, err = loadSemanticSymbolsByIDSnapshot(filepath.Join(storage.BrainDir, manifest.Sources.Semantic.SnapshotPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := symbols[oldID]; ok {
		t.Fatal("old symbol survived refresh")
	}
	if _, ok := symbols["gh/example/repo:JavaScript:a.js:function:A.m.helper"]; !ok {
		t.Fatal("new symbol missing")
	}
	db, err := sql.Open(sqliteDriverName, filepath.Join(storage.BrainDir, manifest.Sources.Semantic.StorePath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var edges int
	if err := db.QueryRow(`SELECT count(*) FROM relations WHERE from_id = ? OR to_id = ?`, "gh/example/repo:JavaScript:a.js:function:A.m.helper", "gh/example/repo:JavaScript:a.js:function:A.m.helper").Scan(&edges); err != nil || edges == 0 {
		t.Fatalf("new helper relations missing: %d %v", edges, err)
	}
	if manifest.Sources.Semantic.IdentityRevision != revision {
		t.Fatal("manifest lost identity revision")
	}
	needed, err = semanticRefreshNeeded(ctx, opts, storage.BrainDir, repo, manifest, false, "entire")
	if err != nil || needed {
		t.Fatalf("fresh identity not reusable: %v %v", needed, err)
	}
	if git("rev-parse", "HEAD") != head {
		t.Fatal("migration modified source history")
	}
}
