package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/viz"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

func TestVizNodePublicResponseUsesGraphStore(t *testing.T) {
	repo := t.TempDir()
	env := semanticTestEnv(t, repo)
	runner := semanticFixtureRunner(repo, semanticFixtureSnapshotWithCallerSymbol())
	if err := runSemanticIndex(context.Background(), &cobra.Command{}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repo); err != nil {
		t.Fatal(err)
	}
	dir, err := base.BrainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadGraphManifest(context.Background(), Options{Env: env, Runner: runner}, repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &viz.Server{BrainDir: dir, RepoDir: repo, Branch: "main", Manifest: manifest, Provider: "gh", Owner: "example", Repo: "repo", Deps: vizDeps()}
	id := "target"
	request := func(id string, wantStatus int) viz.NodeResp {
		t.Helper()
		r := httptest.NewRecorder()
		s.HandleNode(r, httptest.NewRequest(http.MethodGet, "/api/node?id="+url.QueryEscape(id), nil))
		if r.Code != wantStatus {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
		var result viz.NodeResp
		if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	store := request(id, http.StatusOK)
	if store.Symbol.ID != id || len(store.Neighbors) != 1 || len(store.Relations) != 1 || len(store.Warnings) != 0 || store.Link == "" {
		t.Fatalf("node=%+v", store)
	}
	manifest.Sources.Facts = &factSourceManifest{Branches: []string{"main"}, Facts: 1}
	diskManifest, err := bmanifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	diskManifest.Sources.Facts = manifest.Sources.Facts
	if err := writeBrainManifestAndReadme(dir, *diskManifest); err != nil {
		t.Fatal(err)
	}
	if err := factstore.Write(dir, "main", []factRecord{{ID: "fact:token", Branch: "main", Text: "ValidateToken checks token expiry", Status: factStatusActive}}); err != nil {
		t.Fatal(err)
	}
	s.Branch = "main"
	search := httptest.NewRecorder()
	s.HandleSearch(search, httptest.NewRequest(http.MethodGet, "/api/search?q=ValidateToken&limit=999999", nil))
	var results viz.SearchResp
	if err := json.Unmarshal(search.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if search.Code != http.StatusOK || len(results.Hits) != 2 || results.Hits[0].ID != "target" || results.Hits[0].Source != "symbol" || results.Hits[0].Path != "internal/auth/token.go" || results.Hits[0].Line != 10 || results.Hits[1].ID != "fact:token" {
		t.Fatalf("search lost order/identity/provenance: status=%d %+v", search.Code, results)
	}
	missing := request("nonexistent", http.StatusNotFound)
	if missing.Symbol.ID != "" || len(missing.Neighbors) != 0 || len(missing.Relations) != 0 {
		t.Fatalf("missing node leaked other evidence: %+v", missing)
	}
	if err := os.WriteFile(runner.graphStoreFixtures[repo+"\x00full"].Path, []byte("corrupt provider store"), 0600); err != nil {
		t.Fatal(err)
	}
	invalid := request(id, http.StatusOK)
	if invalid.Symbol.ID != "" || len(invalid.Warnings) == 0 {
		t.Fatalf("unsafe path produced evidence: %+v", invalid)
	}
}
