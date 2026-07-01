package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A server with no manifest (unbuilt brain) must degrade to an empty graph with
// warnings, never a 500 — the UI relies on this to render its empty state.
func TestVizHandleGraph_UnbuiltBrain(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main", limit: 100}
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, httptest.NewRequest(http.MethodGet, "/api/graph", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp vizGraphResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 0 || len(resp.Edges) != 0 {
		t.Fatalf("nodes/edges = %d/%d, want empty", len(resp.Nodes), len(resp.Edges))
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("expected a warning for an unbuilt brain")
	}
}

func TestVizHandleNode_MissingID(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
	rec := httptest.NewRecorder()
	srv.handleNode(rec, httptest.NewRequest(http.MethodGet, "/api/node", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing id", rec.Code)
	}
}

func TestVizHandleNode_UnbuiltBrain(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
	rec := httptest.NewRecorder()
	srv.handleNode(rec, httptest.NewRequest(http.MethodGet, "/api/node?id=whatever", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp vizNodeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("expected a warning when there is no semantic graph")
	}
}

func TestVizHandleSessionReplay_MissingID(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
	rec := httptest.NewRecorder()
	srv.handleSessionReplay(rec, httptest.NewRequest(http.MethodGet, "/api/session/replay", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing id", rec.Code)
	}
}

func TestVizHandleSessionReplay_UnknownSession(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
	rec := httptest.NewRecorder()
	srv.handleSessionReplay(rec, httptest.NewRequest(http.MethodGet, "/api/session/replay?id=does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown session", rec.Code)
	}
	var resp vizReplayResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Warnings) == 0 {
		t.Fatal("expected a warning for an unknown session")
	}
}

// The CSP header is the machine-checkable no-egress proof; it must be present on
// every response.
func TestVizSecurityHeaders(t *testing.T) {
	t.Parallel()
	h := vizSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	for _, want := range []string{"default-src 'self'", "connect-src 'self'", "script-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q; got %q", want, csp)
		}
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}

func TestVizNodeEdgeJSON(t *testing.T) {
	t.Parallel()
	n := nodeJSON(semanticRecord{ID: "id1", Name: "Foo", QualifiedName: "pkg.Foo", Kind: "function", FilePath: "a/b.go", StartLine: 12, Language: "Go", Signature: "func Foo()"})
	if n.ID != "id1" || n.Name != "Foo" || n.Kind != "function" || n.File != "a/b.go" || n.Line != 12 {
		t.Fatalf("nodeJSON mapping wrong: %+v", n)
	}
	e := edgeJSON(semanticRecord{FromID: "a", ToID: "b", Type: "CALLS", Confidence: 0.5, RelationScope: "file", Resolution: "exact"})
	if e.From != "a" || e.To != "b" || e.Type != "CALLS" || e.Scope != "file" || e.Resolution != "exact" {
		t.Fatalf("edgeJSON mapping wrong: %+v", e)
	}
}
