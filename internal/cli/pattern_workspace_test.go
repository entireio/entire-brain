package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/entireio/entire-brain/internal/cli/workspace"
	"github.com/spf13/cobra"
)

// seedMemberCorpus builds a repo corpus at brainDir with `count` episodes of one
// intent + command shape (enough to promote a repo-scope task/procedure), tagged
// with repoKey. Used to exercise workspace aggregation.
func seedMemberCorpus(t *testing.T, brainDir, repoKey, intentSig string, count int, heads ...string) {
	t.Helper()
	path, err := base.PrepareBrainRelativeSQLiteFile(brainDir, patterncore.CorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open(sqlitedriver.Name, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range patterncore.CorpusSchema {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	now := time.Now()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s:ep%d", intentSig, i)
		patterntest.InsertCorpusEpisode(t, db, id, intentSig, "success", len(heads), now)
		patterntest.InsertCorpusShape(t, db, id, heads...)
	}
	if err := patterncore.BuildCandidates(db, repoKey, now); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceCorpusAggregatesSharedTask(t *testing.T) {
	env := EntireEnv{PluginDataDir: t.TempDir()}
	aDir, err := base.BrainDirForKey(env, "gh/acme/a")
	if err != nil {
		t.Fatal(err)
	}
	bDir, err := base.BrainDirForKey(env, "gh/acme/b")
	if err != nil {
		t.Fatal(err)
	}
	// Both repos share deploy:release via the same shape; repo A also has a
	// repo-only migrate task that must NOT reach the workspace level.
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	seedMemberCorpus(t, aDir, "gh/acme/a", "migrate:schema", 4, "goose up", "goose status")
	seedMemberCorpus(t, bDir, "gh/acme/b", "deploy:release", 4, "mise build", "mise deploy")

	manifest := workspace.Manifest{
		Name:  "plat",
		Repos: []workspace.Repo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/b"}},
	}
	counts, err := buildWorkspacePatternCorpus(env, manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if counts.WithCorpus != 2 {
		t.Fatalf("expected 2 member corpora, got %d (warnings: %v)", counts.WithCorpus, counts.Warnings)
	}

	wsDir, err := workspace.DefaultStore(env).Dir("plat")
	if err != nil {
		t.Fatal(err)
	}
	views, _, ok := patterncore.LoadCorpusPatternViews(wsDir)
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

func TestWorkspaceGetAggregatePatternID(t *testing.T) {
	env := EntireEnv{PluginDataDir: t.TempDir()}
	aDir, _ := base.BrainDirForKey(env, "gh/acme/a")
	bDir, _ := base.BrainDirForKey(env, "gh/acme/b")
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	seedMemberCorpus(t, bDir, "gh/acme/b", "deploy:release", 4, "mise build", "mise deploy")
	manifest := workspace.Manifest{Name: "plat", Repos: []workspace.Repo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/b"}}}
	if err := workspace.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := buildWorkspacePatternCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}

	// The id `workspace patterns` prints (scope=workspace).
	wsDir, _ := workspace.DefaultStore(env).Dir("plat")
	wdb, err := patterntest.OpenCorpusMutableDB(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	var aggID string
	if err := wdb.QueryRow(`SELECT id FROM patterns WHERE scope='workspace' LIMIT 1`).Scan(&aggID); err != nil {
		t.Fatal(err)
	}
	wdb.Close()

	// That printed id must be fetchable via `workspace get <ws> <id>`.
	opts := Options{Env: env, Now: time.Now}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := runWorkspaceGet(cmd, opts, "plat", []string{aggID}, "", false, getOptions{}); err != nil {
		t.Fatalf("workspace get aggregate id: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, aggID) || !strings.Contains(got, "workspace_pattern") {
		t.Errorf("aggregate workspace pattern not fetched:\n%s", got)
	}
	if !strings.Contains(got, "acme/a") || !strings.Contains(got, "acme/b") {
		t.Errorf("workspace pattern should show its repo breakdown:\n%s", got)
	}

	// Member-qualified ids still route to the member repo (existing behavior).
	out.Reset()
	if err := runWorkspaceGet(cmd, opts, "plat", []string{"gh/acme/a/pattern:does-not-exist"}, "", false, getOptions{}); err != nil {
		t.Fatalf("member-qualified get should not error: %v", err)
	}
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("member-qualified unknown id should report not found:\n%s", out.String())
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
	// Member-qualified theme: ids must also parse (drill into a member theme).
	rk, tid, err := splitWorkspaceID("gh/acme/a/theme:xyz")
	if err != nil || rk != "gh/acme/a" || tid != "theme:xyz" {
		t.Errorf("theme split = (%q,%q,%v), want (gh/acme/a, theme:xyz, nil)", rk, tid, err)
	}
}

// Copilot review: a member pattern a verifier rejected must not re-surface by
// contributing to a cross-repo aggregate.
func TestWorkspaceAggregationExcludesRejectedMemberPatterns(t *testing.T) {
	env := EntireEnv{PluginDataDir: t.TempDir()}
	aDir, _ := base.BrainDirForKey(env, "gh/acme/a")
	bDir, _ := base.BrainDirForKey(env, "gh/acme/b")
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	seedMemberCorpus(t, bDir, "gh/acme/b", "deploy:release", 4, "mise build", "mise deploy")
	manifest := workspace.Manifest{Name: "plat", Repos: []workspace.Repo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/b"}}}

	// Control: without rejection, the shared task aggregates to the workspace.
	if _, err := buildWorkspacePatternCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}
	wsDir, _ := workspace.DefaultStore(env).Dir("plat")
	if !workspaceHasDeploy(t, wsDir) {
		t.Fatal("control: shared deploy:release should aggregate to the workspace")
	}

	// Reject the dossier in member A → only B contributes → below the 2-repo floor.
	adb, err := patterntest.OpenCorpusMutableDB(aDir)
	if err != nil {
		t.Fatal(err)
	}
	var apid string
	if err := adb.QueryRow(`SELECT id FROM patterns WHERE type='task' AND intent_sig='deploy:release'`).Scan(&apid); err != nil {
		t.Fatal(err)
	}
	if _, err := adb.Exec(`INSERT INTO dossiers (pattern_id, cluster_key, fingerprint, json_redacted, verdict, status, created_at, updated_at) VALUES (?,?,?,?, 'rejected','current','t','t')`, apid, "k", "sha256:x", "{}"); err != nil {
		t.Fatal(err)
	}
	adb.Close()

	if _, err := buildWorkspacePatternCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}
	if workspaceHasDeploy(t, wsDir) {
		t.Error("a verifier-rejected member pattern must not contribute to the workspace aggregate")
	}
}

func workspaceHasDeploy(t *testing.T, wsDir string) bool {
	t.Helper()
	views, _, ok, err := patterncore.LoadCorpusPatternViewsChecked(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected readable workspace corpus")
	}
	for _, v := range views {
		if v.IntentSig == "deploy:release" {
			return true
		}
	}
	return false
}

// Copilot review: a workspace rebuild must clear the verifier-cache tables so a
// stale verdict cannot join onto a newly-rebuilt pattern with the same stable id.
func TestWorkspaceRebuildClearsStaleVerifierCache(t *testing.T) {
	env := EntireEnv{PluginDataDir: t.TempDir()}
	aDir, _ := base.BrainDirForKey(env, "gh/acme/a")
	bDir, _ := base.BrainDirForKey(env, "gh/acme/b")
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	seedMemberCorpus(t, bDir, "gh/acme/b", "deploy:release", 4, "mise build", "mise deploy")
	manifest := workspace.Manifest{Name: "plat", Repos: []workspace.Repo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/b"}}}
	if _, err := buildWorkspacePatternCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}
	wsDir, _ := workspace.DefaultStore(env).Dir("plat")
	db, err := patterntest.OpenCorpusMutableDB(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	// Inject a stale 'rejected' dossier keyed to the (stable) workspace pattern id.
	var pid string
	if err := db.QueryRow(`SELECT id FROM patterns WHERE scope='workspace' LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO dossiers (pattern_id, cluster_key, fingerprint, json_redacted, verdict, status, created_at, updated_at) VALUES (?,?,?,?, 'rejected','current','t','t')`, pid, "stale", "sha256:stale", "{}"); err != nil {
		t.Fatal(err)
	}
	// And a stray theme row (no FK to episodes, never populated at workspace scope).
	if _, err := db.Exec(`INSERT INTO themes (id, scope, title, fingerprint, status, created_at, updated_at) VALUES ('theme:stale','repo','stale','sha256:t','candidate','t','t')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Rebuild must drop the stale verdict (else patterncore.LoadCorpusPatternViews suppresses it).
	if _, err := buildWorkspacePatternCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}
	db2, err := patterntest.OpenCorpusMutableDB(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var dossiers, themes int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM dossiers`).Scan(&dossiers); err != nil {
		t.Fatal(err)
	}
	if dossiers != 0 {
		t.Errorf("workspace rebuild must clear stale dossiers, got %d", dossiers)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM themes`).Scan(&themes); err != nil {
		t.Fatal(err)
	}
	if themes != 0 {
		t.Errorf("workspace rebuild must clear stray themes, got %d", themes)
	}
	views, _, _ := patterncore.LoadCorpusPatternViews(wsDir)
	found := false
	for _, v := range views {
		if v.ID == pid {
			found = true
		}
	}
	if !found {
		t.Error("the rebuilt workspace pattern must not be suppressed by a stale verdict")
	}
}
