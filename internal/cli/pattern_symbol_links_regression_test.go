package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/patterntest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	"github.com/spf13/cobra"
)

func TestPatternSymbolLinksRebuildCapsAndDropsStaleSemanticEvidence(t *testing.T) {
	repo := t.TempDir()
	env := semanticTestEnv(t, repo)
	var snapshot strings.Builder
	snapshot.WriteString(`{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111"}` + "\n")
	for i := 0; i < 30; i++ {
		record := semanticRecord{RecordType: "symbol", ID: fmt.Sprintf("symbol:%02d", i), Kind: "function", Name: fmt.Sprintf("Function%02d", i), FilePath: "src/linked.go", StartLine: i + 1, EndLine: i + 1, Language: "Go"}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Write(data)
		snapshot.WriteByte('\n')
	}
	snapshot.WriteString(`{"record_type":"symbol","id":"other-symbol","kind":"function","name":"Unrelated","file_path":"src/untouched.go","start_line":1,"end_line":1,"language":"Go"}` + "\n" + `{"record_type":"summary"}` + "\n")
	runner := semanticFixtureRunner(repo, snapshot.String())
	if err := runSemanticIndex(context.Background(), &cobra.Command{}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repo); err != nil {
		t.Fatal(err)
	}
	dir, err := base.BrainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := runner.graphStoreFixtures[repo+"\x00full"]
	source := semstore.GraphStoreSource(descriptor, dir)
	db, err := sql.Open(sqlitedriver.Name, filepath.Join(t.TempDir(), "corpus.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`CREATE TABLE episode_files(episode_id TEXT,path TEXT)`,
		`CREATE TABLE episode_symbols(episode_id TEXT,symbol_id TEXT,name TEXT,file_path TEXT,PRIMARY KEY(episode_id,symbol_id))`,
		`INSERT INTO episode_files VALUES('ep-a','src/linked.go'),('ep-b','src/linked.go'),('ep-unknown','src/missing.go'),('ep-empty','')`,
		`INSERT INTO episode_symbols VALUES('ep-stale','old-symbol','Old','src/deleted.go')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string][]string {
		t.Helper()
		rows, err := db.Query(`SELECT episode_id,symbol_id FROM episode_symbols ORDER BY episode_id,symbol_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string][]string{}
		for rows.Next() {
			var ep, id string
			if err := rows.Scan(&ep, &id); err != nil {
				t.Fatal(err)
			}
			out[ep] = append(out[ep], id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if err := linkEpisodeSymbols(db, dir, source); err != nil {
		t.Fatal(err)
	}
	first := read()
	if len(first) != 2 || len(first["ep-a"]) != 25 || len(first["ep-b"]) != 25 || !reflect.DeepEqual(first["ep-a"], first["ep-b"]) {
		t.Fatalf("capped membership=%v", first)
	}
	for _, id := range first["ep-a"] {
		if !strings.HasPrefix(id, "symbol:") {
			t.Fatalf("unrelated symbol linked: %s", id)
		}
	}
	if err := linkEpisodeSymbols(db, dir, source); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(first, got) {
		t.Fatalf("rebuild changed membership: %v -> %v", first, got)
	}
	// There is no retained snapshot fallback when Graph's store is unreadable.
	if err := os.WriteFile(descriptor.Path, []byte("corrupt store"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := linkEpisodeSymbols(db, dir, source); err != nil {
		t.Fatalf("unreadable semantic store must degrade: %v", err)
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("corrupt semantic source retained stale links: %v", got)
	}
	if _, err := db.Exec(`INSERT INTO episode_symbols VALUES('ep-stale','old-symbol','Old','src/deleted.go')`); err != nil {
		t.Fatal(err)
	}
	if err := linkEpisodeSymbols(db, dir, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("removed semantic source retained stale links: %v", got)
	}
}

func TestPatternCorpusLinksUsingLiveGraphContext(t *testing.T) {
	for _, state := range []string{"imported", "moved", "local", "foreign"} {
		t.Run(state, func(t *testing.T) {
			opts, repo, _, d := newGraphStoreFixture(t)
			runner := opts.Runner.(*fakeCommandRunner)
			d.Header.RepoKey = "gh/example/repo"
			if state == "local" {
				runner = semanticRepoKeyFixtureRunner(repo, "", providerLocalKeyFixtureSnapshot(repo))
				opts.Runner = runner
				d.Header.RepoKey = "local/" + filepath.Base(repo)
			}
			if state == "foreign" {
				d.Header.RepoKey = "gh/other/repo"
			}
			key := semanticStorageKeyForTest(t, runner, opts.Env, repo)
			brain := writeEpisodeFixture(t, opts.Now(), key, []sessionFixture{{id: "s1", agent: "Claude", branch: "main", checkpoint: "cp1", relPath: "sessions/main/s1.jsonl", transcript: `{"type":"user","message":{"content":"fix rendering in main.go"}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"edit1","name":"Edit","input":{"file_path":"main.go","old_string":"old","new_string":"new"}}]}}`}})
			if state == "moved" {
				if err := writeBrainSemanticSource(brain, key, &semanticSourceManifest{StoreOwner: "graph", GraphRepoRoot: filepath.Join(t.TempDir(), "old-checkout")}); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			runner.responses[fakeCommandKey("custom-entire", "graph", "store", "--repo", repo, "--profile", "full")] = fakeCommandResponse{stdout: string(data)}
			graph := patternCorpusGraphContext{Context: context.Background(), Options: opts, RepoDir: repo, Binary: "custom-entire"}
			if err := buildPatternCorpus(brain, opts.Now(), graph); err != nil {
				t.Fatal(err)
			}
			db := openCorpusReadSnapshot(t, brain)
			if n := patterntest.CorpusCount(t, db, "episode_files"); n == 0 {
				t.Fatal("fixture has no file evidence")
			}
			want := 2
			if state == "foreign" {
				want = 0
			}
			if n := patterntest.CorpusCount(t, db, "episode_symbols"); n != want {
				t.Fatalf("linked %d symbols, want %d", n, want)
			}
		})
	}
}
