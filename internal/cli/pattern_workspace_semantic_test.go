package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// seedPromotableMember builds a member corpus whose (intentSig, heads) task is
// promotable (12 corroborated successes + noise), so it surfaces as a member
// task candidate for the workspace judgment layer.
func seedPromotableMember(t *testing.T, brainDir, repoKey, intentSig string, heads ...string) {
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
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("%s:ok%d", intentSig, i)
		insertCorpusEpisode(t, db, id, intentSig, "success", len(heads), now)
		insertCorpusShape(t, db, id, heads...)
		db.Exec(`INSERT INTO episode_facts (episode_id, fact_id, kind, paths, weight) VALUES (?,?,?,?,2)`,
			id, "fact:"+intentSig, "architecture", "ops/x.go")
	}
	for i := 0; i < 120; i++ {
		id := fmt.Sprintf("%s:noise%d", intentSig, i)
		insertCorpusEpisode(t, db, id, "other:thing", "neutral", 2, now)
		insertCorpusShape(t, db, id, "ls", "cat")
	}
	if err := buildPatternCandidates(db, repoKey, now); err != nil {
		t.Fatal(err)
	}
}

// releaseGroupingAgent groups repos whose task intent mentions "release" into one
// cross-repo family (with each repo's actual commands); it rejects generic git.
func releaseGroupingAgent(t *testing.T) distillAgentRunner {
	return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		var payload struct {
			Repos map[string][]map[string]any `json:"repos"`
		}
		if err := json.Unmarshal(input, &payload); err != nil {
			t.Fatalf("family proposal input not JSON: %v\n%s", err, input)
		}
		var perRepo []map[string]any
		generic := true
		for repo, tasks := range payload.Repos {
			for _, tk := range tasks {
				intent := fmt.Sprint(tk["intent"])
				if strings.Contains(intent, "release") {
					perRepo = append(perRepo, map[string]any{"repo_key": repo, "commands": tk["commands"]})
					generic = false
				}
			}
		}
		verdict := "accepted"
		if generic { // only generic git tasks present
			verdict = "rejected"
		}
		resp := map[string]any{"families": []map[string]any{{
			"title":           "release the build",
			"trigger":         "shipping a release",
			"purpose":         "build then publish a release",
			"common_workflow": []string{"build", "publish release"},
			"per_repo":        perRepo,
			"verdict":         verdict,
		}}}
		b, _ := json.Marshal(resp)
		return string(b), nil
	}
}

func twoRepoWorkspace(t *testing.T, a, aIntent string, aHeads []string, b, bIntent string, bHeads []string) (EntireEnv, workspaceManifest) {
	t.Helper()
	env := EntireEnv{PluginDataDir: t.TempDir()}
	aDir, _ := brainDirForKey(env, a)
	bDir, _ := brainDirForKey(env, b)
	seedPromotableMember(t, aDir, a, aIntent, aHeads...)
	seedPromotableMember(t, bDir, b, bIntent, bHeads...)
	manifest := workspaceManifest{Name: "plat", Repos: []workspaceRepo{{RepoKey: a}, {RepoKey: b}}}
	if _, err := buildWorkspacePatternCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}
	return env, manifest
}

// SR5(1): equivalent release workflows with DIFFERENT command names produce ONE
// cross-repo family after agent judgment (exact aggregation never would).
func TestWorkspaceFamilyMergesEquivalentWorkflows(t *testing.T) {
	env, manifest := twoRepoWorkspace(t,
		"gh/acme/a", "release:build", []string{"mise build", "mise deploy"},
		"gh/acme/b", "release:ship", []string{"make build", "make release"})

	stats, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", releaseGroupingAgent(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Verified != 1 {
		t.Fatalf("expected 1 accepted family, got %+v", stats)
	}
	wsDir, _ := workspaceDir(env, "plat")
	fams := loadAcceptedWorkspaceFamilies(wsDir)
	if len(fams) != 1 {
		t.Fatalf("expected 1 family, got %d", len(fams))
	}
	if fams[0].repoCount() != 2 {
		t.Errorf("family must span 2 repos, got %d: %+v", fams[0].repoCount(), fams[0].PerRepo)
	}
	// SR5(3): the family records the per-repo variation (different command names).
	cmds := map[string]string{}
	for _, v := range fams[0].PerRepo {
		cmds[v.RepoKey] = strings.Join(v.Commands, ",")
	}
	if !strings.Contains(cmds["gh/acme/a"], "mise deploy") || !strings.Contains(cmds["gh/acme/b"], "make release") {
		t.Errorf("family must keep each repo's actual commands, got %v", cmds)
	}
	// And the synthesis evidence surfaces the per-repo variations.
	ev := buildWorkspaceFamilyEvidence(fams[0])
	if !strings.Contains(ev, "Per-repo variations") || !strings.Contains(ev, "mise deploy") || !strings.Contains(ev, "make release") {
		t.Errorf("workspace family evidence missing per-repo variations:\n%s", ev)
	}
}

// SR5(2): two repos that only share generic git commit/push produce NO accepted
// family (so no workspace skill).
func TestWorkspaceFamilyRejectsGeneric(t *testing.T) {
	env, manifest := twoRepoWorkspace(t,
		"gh/acme/a", "commit:push", []string{"git add", "git push"},
		"gh/acme/b", "commit:push", []string{"git add", "git push"})

	if _, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", releaseGroupingAgent(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	wsDir, _ := workspaceDir(env, "plat")
	if fams := loadAcceptedWorkspaceFamilies(wsDir); len(fams) != 0 {
		t.Errorf("generic git workflows must not produce an accepted workspace family, got %d", len(fams))
	}
}

// verdictFamilyAgent groups release tasks into one family and returns the given verdict.
func verdictFamilyAgent(verdict string) distillAgentRunner {
	return func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		var payload struct {
			Repos map[string][]map[string]any `json:"repos"`
		}
		_ = json.Unmarshal(input, &payload)
		var perRepo []map[string]any
		for repo, tasks := range payload.Repos {
			for _, tk := range tasks {
				if strings.Contains(fmt.Sprint(tk["intent"]), "release") {
					perRepo = append(perRepo, map[string]any{"repo_key": repo, "commands": tk["commands"]})
				}
			}
		}
		resp := map[string]any{"families": []map[string]any{{
			"title": "release the build", "trigger": "shipping a release", "purpose": "build then publish",
			"common_workflow": []string{"build", "publish"}, "per_repo": perRepo, "verdict": verdict,
		}}}
		b, _ := json.Marshal(resp)
		return string(b), nil
	}
}

// Item #1: a verified workspace family survives a deterministic workspace refresh
// (unchanged member corpora) and is still listed without re-invoking the agent.
func TestWorkspaceFamilyPreservedAcrossRefresh(t *testing.T) {
	env, manifest := twoRepoWorkspace(t,
		"gh/acme/a", "release:build", []string{"mise build", "mise deploy"},
		"gh/acme/b", "release:ship", []string{"make build", "make release"})
	if _, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", releaseGroupingAgent(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	wsDir, _ := workspaceDir(env, "plat")
	if len(loadAcceptedWorkspaceFamilies(wsDir)) != 1 {
		t.Fatal("setup: expected 1 accepted family before refresh")
	}
	// Rebuild the workspace corpus with unchanged member corpora.
	if _, err := buildWorkspacePatternCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := len(loadAcceptedWorkspaceFamilies(wsDir)); got != 1 {
		t.Fatalf("workspace refresh must preserve accepted families, got %d", got)
	}
	// `workspace patterns skills` still lists it.
	fams := loadAcceptedWorkspaceFamilies(wsDir)
	if len(fams) != 1 || fams[0].repoCount() != 2 {
		t.Fatalf("family must still span 2 repos after refresh, got %+v", fams)
	}
	// And re-verifying with unchanged evidence reuses the cache (agent NOT re-run).
	failIfCalled := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		t.Fatal("agent must not re-run when member evidence is unchanged after refresh")
		return "", nil
	}
	stats, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", failIfCalled, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Cached != 1 {
		t.Errorf("expected the family served from cache after refresh, got %+v", stats)
	}
}

// Item #2: editing member task CONTENT (title) while keeping repos/intents stable
// invalidates the cached family proposal (the agent re-runs).
func TestWorkspaceFamilyCacheInvalidatesOnContentChange(t *testing.T) {
	env, manifest := twoRepoWorkspace(t,
		"gh/acme/a", "release:build", []string{"mise build", "mise deploy"},
		"gh/acme/b", "release:ship", []string{"make build", "make release"})
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return releaseGroupingAgent(t)(ctx, dir, args, input, timeout)
	}
	if _, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", run, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", run, time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unchanged member evidence must reuse cache, calls=%d", calls)
	}
	// Mutate member A's task title (content), same repo + intent.
	aDir, _ := brainDirForKey(env, "gh/acme/a")
	mdb, err := openPatternCorpusMutableDB(aDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mdb.Exec(`UPDATE patterns SET title='a brand new release title' WHERE intent_sig='release:build'`); err != nil {
		t.Fatal(err)
	}
	mdb.Close()
	if _, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", run, time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("changed member content must invalidate the cache, calls=%d", calls)
	}
}

// Item #3: a missing/unknown/non-accepting verdict must never surface a family or
// count as Verified.
func TestWorkspaceFamilyUnknownVerdictNotAccepted(t *testing.T) {
	for _, v := range []string{"", "bogus", "needs_split", "low_confidence"} {
		env, manifest := twoRepoWorkspace(t,
			"gh/acme/a", "release:build", []string{"mise build", "mise deploy"},
			"gh/acme/b", "release:ship", []string{"make build", "make release"})
		stats, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", verdictFamilyAgent(v), time.Now())
		if err != nil {
			t.Fatalf("verdict %q: %v", v, err)
		}
		if stats.Verified != 0 {
			t.Errorf("verdict %q must not count as Verified, got %+v", v, stats)
		}
		wsDir, _ := workspaceDir(env, "plat")
		if n := len(loadAcceptedWorkspaceFamilies(wsDir)); n != 0 {
			t.Errorf("verdict %q must not surface as an accepted family, got %d", v, n)
		}
	}
}

func TestWorkspaceFamilyNoEgressRejected(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	env, manifest := twoRepoWorkspace(t,
		"gh/acme/a", "release:build", []string{"mise build", "mise deploy"},
		"gh/acme/b", "release:ship", []string{"make build", "make release"})
	called := false
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		called = true
		return "", nil
	}
	_, err := proposeWorkspaceFamilies(context.Background(), env, manifest, t.TempDir(), "codex", "", "", run, time.Now())
	if err == nil || !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("expected no_egress rejection, got %v", err)
	}
	if called {
		t.Error("agent must not run under no-egress")
	}
}
