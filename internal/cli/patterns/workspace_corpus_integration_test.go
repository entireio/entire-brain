package patterns_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"github.com/entireio/entire-brain/internal/cli/workspace"
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

// DV5: the workspace build must materialize workspace_repo synapses and record a
// run, mirroring the repo corpus build.
func TestWorkspaceCorpusSynapsesAndRun(t *testing.T) {
	env := base.EntireEnv{PluginDataDir: t.TempDir()}
	aDir, _ := base.BrainDirForKey(env, "gh/acme/a")
	bDir, _ := base.BrainDirForKey(env, "gh/acme/b")
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	seedMemberCorpus(t, bDir, "gh/acme/b", "deploy:release", 4, "mise build", "mise deploy")
	manifest := workspace.Manifest{Name: "plat", Repos: []workspace.Repo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/b"}}}
	if _, err := patterncore.BuildWorkspaceCorpus(env, manifest, time.Now()); err != nil {
		t.Fatal(err)
	}
	wsDir, _ := workspace.DefaultStore(env).Dir("plat")
	db, err := patterntest.OpenCorpusMutableDB(wsDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var wsRepoEdges int
	db.QueryRow(`SELECT COUNT(*) FROM synapses WHERE kind='workspace_repo'`).Scan(&wsRepoEdges)
	if wsRepoEdges == 0 {
		t.Error("expected workspace_repo synapses materialized from the per-repo breakdown")
	}
	if run, ok := patterncore.LastRun(wsDir); !ok || run.Patterns == 0 {
		t.Errorf("expected a workspace run record with patterns, got ok=%v run=%+v", ok, run)
	}
}

func TestWorkspaceCorpusHandlesMissingMember(t *testing.T) {
	env := base.EntireEnv{PluginDataDir: t.TempDir()}
	aDir, _ := base.BrainDirForKey(env, "gh/acme/a")
	seedMemberCorpus(t, aDir, "gh/acme/a", "deploy:release", 4, "mise build", "mise deploy")
	// gh/acme/missing has no corpus on disk.
	manifest := workspace.Manifest{
		Name:  "plat",
		Repos: []workspace.Repo{{RepoKey: "gh/acme/a"}, {RepoKey: "gh/acme/missing"}},
	}
	counts, err := patterncore.BuildWorkspaceCorpus(env, manifest, time.Now())
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
