package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
)

// fakeGraphStoreCommand models the provider-owned store for existing symbol
// fixtures. The native Graph package pins the projection itself. Here the
// SQLite file lives in the provider fixture directory, never in Brain storage.
func (r *fakeCommandRunner) fakeGraphStoreCommand(dir, name string, args []string) (fakeCommandResponse, bool) {
	if name != "entire" || len(args) < 2 || args[0] != "graph" || (args[1] != "index" && args[1] != "store") {
		return fakeCommandResponse{}, false
	}
	profile := "full"
	for i := 2; i+1 < len(args); i++ {
		if args[i] == "--profile" {
			profile = args[i+1]
		}
	}
	key := dir + "\x00" + profile
	if args[1] == "store" {
		if d, ok := r.graphStoreFixtures[key]; ok {
			raw, _ := json.Marshal(d)
			return fakeCommandResponse{stdout: string(raw)}, true
		}
		return fakeCommandResponse{stdout: `{"contract_version":1,"store_unavailable":true}`}, true
	}
	snapshot, ok := r.responses[fakeCommandKey("entire", "graph", "snapshot", "--repo", dir, "--format", "ndjson", "--no-network")]
	if !ok {
		return fakeCommandResponse{}, false
	}
	force := false
	for _, arg := range args {
		if arg == "--force" {
			force = true
		}
	}
	if d, ok := r.graphStoreFixtures[key]; ok && !force {
		if db, err := semstore.OpenReadDB(semstore.GraphStoreSource(d, "").StorePath); err == nil {
			db.Close()
			return fakeCommandResponse{stdout: `{"index_cache_hit":true}`}, true
		}
	}
	if snapshot.err != nil {
		return snapshot, true
	}
	ignore, err := loadBrainIgnore(dir)
	if err != nil {
		return fakeCommandResponse{err: err}, true
	}
	header, counts, filtered, err := filterSemanticSnapshot([]byte(snapshot.stdout), ignore, dir)
	if err != nil {
		return fakeCommandResponse{err: err}, true
	}
	providerDir, err := os.MkdirTemp(dir, ".graph-fixture-")
	if err != nil {
		return fakeCommandResponse{err: err}, true
	}
	generation, _, err := buildSemanticGeneration(context.Background(), providerDir, dir, "fixture", bytes.NewReader(filtered), "fixture", header, counts, time.Now())
	if err != nil {
		return fakeCommandResponse{err: err}, true
	}
	path := filepath.Join(providerDir, generation, semstore.SQLiteName)
	db, err := sql.Open(sqlitedriver.Name, path)
	if err != nil {
		return fakeCommandResponse{err: err}, true
	}
	for _, table := range []string{"symbols", "relations", "files", "reverse_relations", "symbol_fts"} {
		if _, err = db.Exec("CREATE VIEW brain_" + table + "_v1 AS SELECT * FROM " + table); err != nil {
			db.Close()
			return fakeCommandResponse{err: err}, true
		}
	}
	digest := fmt.Sprintf("fixture-%d", time.Now().UnixNano())
	raw, _ := json.Marshal(digest)
	_, err = db.Exec("INSERT INTO meta(key,value) VALUES('entry_digest',?)", string(raw))
	db.Close()
	if err != nil {
		return fakeCommandResponse{err: err}, true
	}
	if r.graphStoreFixtures == nil {
		r.graphStoreFixtures = map[string]graphStoreDescriptor{}
	}
	if header.Commit == "" {
		header.Commit = strings.TrimSpace(r.responses[fakeCommandKey("git", "rev-parse", "HEAD")].stdout)
	}
	if header.Tree == "" {
		header.Tree = strings.TrimSpace(r.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")].stdout)
	}
	header.Profile = profile
	r.graphStoreFixtures[key] = graphStoreDescriptor{ContractVersion: 1, Path: path, EntryDigest: digest, Commit: header.Commit, Tree: header.Tree, Header: header, Files: counts.Files, Symbols: counts.Symbols, Relations: counts.Relations}
	return fakeCommandResponse{stdout: `{}`}, true
}
