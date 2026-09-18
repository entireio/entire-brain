package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestVizFactGraphDeduplicatesAndTruncatesWithoutDanglingEdges(t *testing.T) {
	dir := t.TempDir()
	a := factRecord{ID: "fact:a", Text: "Keep token validation independent of transports", Kind: "convention", Status: "active", Confidence: "high", Locus: []string{" TOKEN.go "}, RelatedIDs: []string{"fact:b", "fact:missing"}, Provenance: []factAnchor{{SessionID: "session-public"}}}
	b := factRecord{ID: "fact:b", Text: "Verify token expiry before access", Kind: "learning", Locus: []string{"token.go"}, RelatedIDs: []string{"fact:a"}}
	if err := writeFacts(dir, "main", []factRecord{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(dir, "feature", []factRecord{a, {ID: "", Text: "invalid record"}}); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, filepath.FromSlash(factsFileRelPath("broken")))
	if err := os.MkdirAll(filepath.Dir(corrupt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &vizServer{brainDir: dir, branch: "main", provider: "gh", owner: "example", repo: "repo", manifest: &exportManifest{Sources: &brainSources{Facts: &factSourceManifest{Branches: []string{"main", "feature", "broken"}}}}}
	request := func(query string) vizFeatureGraph {
		t.Helper()
		r := httptest.NewRecorder()
		s.handleFacts(r, httptest.NewRequest(http.MethodGet, "/api/facts"+query, nil))
		if r.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
		var graph vizFeatureGraph
		if err := json.Unmarshal(r.Body.Bytes(), &graph); err != nil {
			t.Fatal(err)
		}
		return graph
	}
	full := request("")
	if full.Total != 2 || full.Truncated || len(full.Nodes) != 2 || full.Nodes[0].ID != a.ID || full.Nodes[1].ID != b.ID {
		t.Fatalf("full graph=%+v", full)
	}
	if full.Nodes[0].Text != a.Text || !strings.Contains(full.Nodes[0].Meta, "conf high") || !strings.Contains(full.Nodes[0].Link, "session-public") {
		t.Fatalf("fact provenance=%+v", full.Nodes[0])
	}
	if len(full.Warnings) != 1 || !strings.Contains(full.Warnings[0], "facts[broken]") {
		t.Fatalf("warnings=%v", full.Warnings)
	}
	if !reflect.DeepEqual(full.Edges, []vizGEdge{{From: a.ID, To: b.ID, Type: "related"}, {From: a.ID, To: b.ID, Type: "shared locus"}}) {
		t.Fatalf("edges=%+v", full.Edges)
	}
	limited := request("?limit=1")
	if limited.Total != 2 || !limited.Truncated || !reflect.DeepEqual(limited.Nodes, full.Nodes[:1]) || len(limited.Edges) != 0 {
		t.Fatalf("limited graph=%+v", limited)
	}
}

func TestVizNodePublicResponseAgreesAcrossSQLiteAndSnapshot(t *testing.T) {
	repo := t.TempDir()
	env := semanticTestEnv(t, repo)
	runner := semanticFixtureRunner(repo, semanticFixtureSnapshotWithCallerSymbol())
	if err := runSemanticIndex(context.Background(), &cobra.Command{}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repo); err != nil {
		t.Fatal(err)
	}
	dir, err := brainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &vizServer{brainDir: dir, repoDir: repo, branch: "main", manifest: manifest, provider: "gh", owner: "example", repo: "repo"}
	id := "target"
	request := func(id string, wantStatus int) vizNodeResp {
		t.Helper()
		r := httptest.NewRecorder()
		s.handleNode(r, httptest.NewRequest(http.MethodGet, "/api/node?id="+url.QueryEscape(id), nil))
		if r.Code != wantStatus {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
		var result vizNodeResp
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
	if err := writeBrainManifestAndReadme(dir, *manifest); err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(dir, "main", []factRecord{{ID: "fact:token", Branch: "main", Text: "ValidateToken checks token expiry", Status: factStatusActive}}); err != nil {
		t.Fatal(err)
	}
	s.branch = "main"
	search := httptest.NewRecorder()
	s.handleSearch(search, httptest.NewRequest(http.MethodGet, "/api/search?q=ValidateToken&limit=999999", nil))
	var results vizSearchResp
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
	manifest.Sources.Semantic.StorePath = ""
	manifest.Sources.Semantic.GenerationPath = ""
	snapshot := request(id, http.StatusOK)
	if !reflect.DeepEqual(store, snapshot) {
		t.Fatalf("store=%+v snapshot=%+v", store, snapshot)
	}
	request("nonexistent", http.StatusNotFound)
	manifest.Sources.Semantic.SnapshotPath = "../outside.ndjson"
	invalid := request(id, http.StatusOK)
	if invalid.Symbol.ID != "" || len(invalid.Warnings) == 0 {
		t.Fatalf("unsafe path produced evidence: %+v", invalid)
	}
}
