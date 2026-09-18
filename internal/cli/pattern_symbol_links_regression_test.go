package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	dir, err := brainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	source := mustSemanticSource(t, env)
	db, err := sql.Open(sqliteDriverName, filepath.Join(t.TempDir(), "corpus.sqlite"))
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
	// The snapshot path must provide the same usable symbol identities as SQLite.
	withoutStore := *source
	withoutStore.StorePath = ""
	withoutStore.GenerationPath = ""
	if err := linkEpisodeSymbols(db, dir, &withoutStore); err != nil {
		t.Fatal(err)
	}
	if got := read(); !reflect.DeepEqual(first, got) {
		t.Fatalf("snapshot/store differ: %v -> %v", first, got)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(source.StorePath)), []byte("corrupt store"), 0o600); err != nil {
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
