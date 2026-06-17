package cli

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// seedMemberCorpus builds a repo corpus at brainDir with `count` episodes of one
// intent + command shape (enough to promote a repo-scope task/procedure), tagged
// with repoKey. Used to exercise workspace aggregation.
func seedMemberCorpus(t *testing.T, brainDir, repoKey, intentSig string, count int, heads ...string) {
	t.Helper()
	path, err := prepareBrainRelativeSQLiteFile(brainDir, patternCorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range patternCorpusSchema {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	now := time.Now()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s:ep%d", intentSig, i)
		insertCorpusEpisode(t, db, id, intentSig, "success", len(heads), now)
		insertCorpusShape(t, db, id, heads...)
	}
	if err := buildPatternCandidates(db, repoKey, now); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceCorpusAggregatesSharedTask(t *testing.T) {
	env := EntireEnv{PluginDataDir: t.TempDir()}
	aDir, err := brainDirForKey(env, "gh/acme/a")
	if err != nil {
		t.Fatal(err)
	}
	bDir, err := brainDirForKey(env, "gh/acme/b")
	if err != nil {
		t.Fatal(err)
	}
	// Both repos share deploy:release via the same shape; repo A also has a
	// repo-only migrate task that must NOT reach the workspace level.
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	seedMemberCorpus(t, aDir, "gh/acme/a", "migrate:schema", 4, "goose up", "goose status")
	seedMemberCorpus(t, bDir, "gh/acme/b", "deploy:release", 4, "mise build", "mise deploy")

	manifest := workspaceManifest{
		Name:  "plat",
		Repos: []workspaceRepo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/b"}},
	}
	counts, err := buildWorkspacePatternCorpus(env, manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if counts.WithCorpus != 2 {
		t.Fatalf("expected 2 member corpora, got %d (warnings: %v)", counts.WithCorpus, counts.Warnings)
	}

	wsDir, err := workspaceDir(env, "plat")
	if err != nil {
		t.Fatal(err)
	}
	views, _, ok := loadCorpusPatternViews(wsDir)
	if !ok {
		t.Fatal("expected a workspace corpus")
	}
	var deploy *patternView
	for i := range views {
		if views[i].Scope != "workspace" {
			t.Errorf("workspace corpus should only hold workspace-scope rows, got %q", views[i].Scope)
		}
		if views[i].Type == "task" && views[i].IntentSig == "deploy:release" {
			deploy = &views[i]
		}
		if views[i].IntentSig == "migrate:schema" {
			t.Errorf("repo-only task must not be promoted to the workspace: %+v", views[i])
		}
	}
	if deploy == nil {
		t.Fatal("expected the shared deploy:release task at workspace scope")
	}
	if deploy.Repos != 2 {
		t.Errorf("shared task n_repos = %d, want 2", deploy.Repos)
	}
	if len(deploy.RepoBreakdown) != 2 {
		t.Errorf("expected a 2-repo breakdown, got %+v", deploy.RepoBreakdown)
	}
	if deploy.Workspace != "plat" {
		t.Errorf("workspace = %q, want plat", deploy.Workspace)
	}
	if deploy.Example == nil {
		t.Error("workspace pattern should carry a member anchor")
	}
}

func TestSplitWorkspaceIDPattern(t *testing.T) {
	repoKey, id, err := splitWorkspaceID("gh/acme/a/pattern:abc123")
	if err != nil {
		t.Fatal(err)
	}
	if repoKey != "gh/acme/a" || id != "pattern:abc123" {
		t.Errorf("split = (%q,%q), want (gh/acme/a, pattern:abc123)", repoKey, id)
	}
	if _, _, err := splitWorkspaceID("pattern:abc"); err == nil {
		t.Error("a repo-unqualified pattern id must error")
	}
}

func TestWorkspaceCorpusHandlesMissingMember(t *testing.T) {
	env := EntireEnv{PluginDataDir: t.TempDir()}
	aDir, _ := brainDirForKey(env, "gh/acme/a")
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	// gh/acme/missing has no corpus on disk.
	manifest := workspaceManifest{
		Name:  "plat",
		Repos: []workspaceRepo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/missing"}},
	}
	counts, err := buildWorkspacePatternCorpus(env, manifest, time.Now())
	if err != nil {
		t.Fatalf("missing member must not fail the build: %v", err)
	}
	if counts.WithCorpus != 1 {
		t.Errorf("expected 1 member with corpus, got %d", counts.WithCorpus)
	}
	if len(counts.Warnings) == 0 {
		t.Error("expected a warning for the missing member corpus")
	}
	// Only one repo has the task -> nothing reaches the workspace level.
	if counts.Patterns != 0 {
		t.Errorf("a single-repo pattern must not promote to workspace, got %d", counts.Patterns)
	}
}
