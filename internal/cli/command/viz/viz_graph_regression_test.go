package viz

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestVizFactGraphDeduplicatesAndTruncatesWithoutDanglingEdges(t *testing.T) {
	dir := t.TempDir()
	a := factmerge.Record{ID: "fact:a", Text: "Keep token validation independent of transports", Kind: "convention", Status: "active", Confidence: "high", Locus: []string{" TOKEN.go "}, RelatedIDs: []string{"fact:b", "fact:missing"}, Provenance: []factmerge.Anchor{{SessionID: "session-public"}}}
	b := factmerge.Record{ID: "fact:b", Text: "Verify token expiry before access", Kind: "learning", Locus: []string{"token.go"}, RelatedIDs: []string{"fact:a"}}
	if err := factstore.Write(dir, "main", []factmerge.Record{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := factstore.Write(dir, "feature", []factmerge.Record{a, {ID: "", Text: "invalid record"}}); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, filepath.FromSlash(factstore.FileRelPath("broken")))
	if err := os.MkdirAll(filepath.Dir(corrupt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{BrainDir: dir, Branch: "main", Provider: "gh", Owner: "example", Repo: "repo", Manifest: &bmanifest.Manifest{Sources: &bmanifest.Sources{Facts: &bmanifest.FactSource{Branches: []string{"main", "feature", "broken"}}}}}
	request := func(query string) FeatureGraph {
		t.Helper()
		r := httptest.NewRecorder()
		s.HandleFacts(r, httptest.NewRequest(http.MethodGet, "/api/facts"+query, nil))
		if r.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
		var graph FeatureGraph
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
	if !reflect.DeepEqual(full.Edges, []GEdge{{From: a.ID, To: b.ID, Type: "related"}, {From: a.ID, To: b.ID, Type: "shared locus"}}) {
		t.Fatalf("edges=%+v", full.Edges)
	}
	limited := request("?limit=1")
	if limited.Total != 2 || !limited.Truncated || !reflect.DeepEqual(limited.Nodes, full.Nodes[:1]) || len(limited.Edges) != 0 {
		t.Fatalf("limited graph=%+v", limited)
	}
}
