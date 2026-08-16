package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A server with no manifest (unbuilt brain) must degrade to an empty graph with
// warnings, never a 500 — the UI relies on this to render its empty state.
func TestVizHandleGraph_UnbuiltBrain(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
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
	for _, want := range []string{"default-src 'self'", "connect-src 'self'", "script-src 'self'", "form-action 'none'", "object-src 'none'"} {
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
	n := nodeJSON(semanticRecord{ID: "id1", Name: "Foo", QualifiedName: "pkg.Foo", Kind: "function", FilePath: "a/b.go", StartLine: 12, Language: "Go", Signature: "func Foo()", ContainerID: "cont1"})
	if n.ID != "id1" || n.Name != "Foo" || n.Kind != "function" || n.File != "a/b.go" || n.Line != 12 {
		t.Fatalf("nodeJSON mapping wrong: %+v", n)
	}
	if n.QualifiedName != "pkg.Foo" || n.Language != "Go" || n.Signature != "func Foo()" || n.ContainerID != "cont1" {
		t.Fatalf("nodeJSON dropped a field: %+v", n)
	}
	e := edgeJSON(semanticRecord{FromID: "a", ToID: "b", Type: "CALLS", Confidence: 0.5, RelationScope: "file", Resolution: "exact"})
	if e.From != "a" || e.To != "b" || e.Type != "CALLS" || e.Scope != "file" || e.Resolution != "exact" {
		t.Fatalf("edgeJSON mapping wrong: %+v", e)
	}
	if e.Confidence != 0.5 {
		t.Fatalf("edgeJSON dropped Confidence: %+v", e)
	}
}

// The graph's relations query binds every symbol id TWICE (from_id + to_id). A
// large view must NOT exceed SQLITE_MAX_VARIABLE_NUMBER (~32766) — the query is
// chunked. With 20k symbols the old single query bound 40001 params and errored,
// which silently produced a zero-edge graph that collapses to a dot.
func TestFindSemanticRelationsForSymbols_ChunksLargeInClause(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "semantic.sqlite")
	db, err := sql.Open(sqliteDriverName, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE relations (id INTEGER PRIMARY KEY, from_id TEXT, to_id TEXT, type TEXT, confidence REAL, reason TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// rowid 1: sym-1 -> sym-2 ; rowid 2: sym-3 -> sym-1 (both incident to the set)
	if _, err := db.Exec(`INSERT INTO relations (from_id,to_id,type,confidence,reason) VALUES ('sym-1','sym-2','CALLS',1,''),('sym-3','sym-1','IMPORTS',1,'')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	syms := make([]semanticRecord, 20000) // 40001 bound params unchunked -> would error
	for i := range syms {
		syms[i] = semanticRecord{ID: fmt.Sprintf("sym-%d", i+1)}
	}
	rels, err := findSemanticRelationsForSymbolsInSQLite(dbPath, syms, 1000)
	if err != nil {
		t.Fatalf("large symbol set errored (chunking regression): %v", err)
	}
	if len(rels) != 2 {
		t.Fatalf("got %d relations, want 2", len(rels))
	}
	if rels[0].FromID != "sym-1" || rels[1].FromID != "sym-3" {
		t.Fatalf("relations not merged in rowid order across chunks: %+v", rels)
	}
}

// Every endpoint's ?limit goes through one policy: bad input keeps the
// default, and both the 0 sentinel and huge values clamp to the safety
// ceiling — no query parameter can request an unbounded graph.
func TestVizQueryLimit_ClampsToSafetyCeiling(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		def   int
		want  int
	}{
		{"", 3000, 3000},
		{"limit=25", 3000, 25},
		{"limit=0", 3000, vizGraphMaxView},
		{"limit=99999999", 3000, vizGraphMaxView},
		{"limit=-5", 3000, 3000},
		{"limit=abc", 3000, 3000},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/facts?"+tc.query, nil)
		if got := vizQueryLimit(r, tc.def); got != tc.want {
			t.Errorf("vizQueryLimit(%q, %d) = %d, want %d", tc.query, tc.def, got, tc.want)
		}
	}
}

// Repo links must work for every forge slug the store layer knows (et/tg/cs,
// not just gh/gl/bb) and for nested owner groups via the manifest RepoKey.
func TestParseRepoFromKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		key                   string
		provider, owner, repo string
	}{
		{"gh/acme/app", "gh", "acme", "app"},
		{"et/acme/app", "et", "acme", "app"},
		{"tg/acme/app", "tg", "acme", "app"},
		{"gl/group/sub/app", "gl", "group/sub", "app"}, // nested GitLab group
		{"local/hash123", "", "", ""},                  // no forge → no links
		{"unknown/acme/app", "", "", ""},
	}
	for _, tc := range cases {
		p, o, r := parseRepoFromKey(tc.key)
		if p != tc.provider || o != tc.owner || r != tc.repo {
			t.Errorf("parseRepoFromKey(%q) = %q/%q/%q, want %q/%q/%q", tc.key, p, o, r, tc.provider, tc.owner, tc.repo)
		}
	}
	if p, _, _ := parseRepoFromBrainDir("/data/repos/gh/acme/app"); p != "gh" {
		t.Errorf("parseRepoFromBrainDir fallback broken: provider = %q, want gh", p)
	}
}

// /api/docs node IDs must carry the "doc:" prefix that /api/search doc hits use
// (retrieve.go's unified convention) — otherwise clicking a doc search result
// can never focus its node.
func TestVizHandleDocs_PrefixedIDsMatchSearch(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	idx := docIndex{Records: []docRecord{
		{ID: "abc123", Path: "docs/a.md", Heading: "A", Line: 1, Text: "alpha"},
		{ID: "def456", Path: "docs/a.md", Heading: "B", Line: 9, Text: "beta"},
	}}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "docs", "index.json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	srv := &vizServer{brainDir: brainDir, branch: "main"}
	rec := httptest.NewRecorder()
	srv.handleDocs(rec, httptest.NewRequest(http.MethodGet, "/api/docs", nil))
	var resp vizFeatureGraph
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(resp.Nodes))
	}
	for _, n := range resp.Nodes {
		if !strings.HasPrefix(n.ID, "doc:") {
			t.Errorf("doc node ID %q lacks the doc: prefix search hits carry", n.ID)
		}
	}
	for _, e := range resp.Edges {
		if !strings.HasPrefix(e.From, "doc:") || !strings.HasPrefix(e.To, "doc:") {
			t.Errorf("doc edge %q -> %q references unprefixed IDs", e.From, e.To)
		}
	}
}

// The bind address is loopback ONLY — the no-egress invariant depends on it.
func TestVizListenAddrLoopback(t *testing.T) {
	t.Parallel()
	for _, port := range []int{0, 7788, 65535} {
		addr := vizListenAddr(port)
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Fatalf("vizListenAddr(%d) = %q; must bind 127.0.0.1 only", port, addr)
		}
		if strings.HasPrefix(addr, "0.0.0.0") || strings.HasPrefix(addr, "[::") {
			t.Fatalf("vizListenAddr(%d) = %q binds off-host", port, addr)
		}
	}
}

func TestVizSessionEndpointsOmitExcludedSessionCanary(t *testing.T) {
	const canary = "PRIVATE-VIZ-CANARY"
	manifest := &exportManifest{Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
		{SessionID: "private", Summary: &checkpointSummary{Intent: canary}, FilesTouched: []string{"private.go"}},
		{SessionID: "public", Summary: &checkpointSummary{Intent: "safe"}, FilesTouched: []string{"public.go"}},
	}}}}
	srv := &vizServer{
		brainDir: t.TempDir(), branch: "main", manifest: manifest,
		guard: sessionReadGuard{ids: map[string]sessionTombstone{"private": {At: time.Now().UTC()}}},
	}
	rec := httptest.NewRecorder()
	srv.handleSessions(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), canary) || strings.Contains(rec.Body.String(), "private.go") {
		t.Fatalf("sessions endpoint exposed excluded session: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var graph vizFeatureGraph
	if err := json.Unmarshal(rec.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 1 || graph.Nodes[0].ID != "public" {
		t.Fatalf("sessions endpoint nodes = %+v", graph.Nodes)
	}

	replay := httptest.NewRecorder()
	srv.handleSessionReplay(replay, httptest.NewRequest(http.MethodGet, "/api/session/replay?id=private", nil))
	if replay.Code != http.StatusNotFound || strings.Contains(replay.Body.String(), canary) {
		t.Fatalf("excluded replay status=%d body=%s", replay.Code, replay.Body.String())
	}
}

func TestVizReloadsPrivacyStateForEveryRequest(t *testing.T) {
	const canary = "PRIVATE-LIVE-VIZ-CANARY"
	brainDir := t.TempDir()
	manifest := &exportManifest{Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
		{SessionID: "private", TranscriptPath: "sessions/private.jsonl", Summary: &checkpointSummary{Intent: canary}},
		{SessionID: "public", TranscriptPath: "sessions/public.jsonl", Summary: &checkpointSummary{Intent: "safe"}},
	}}}}
	srv := &vizServer{brainDir: brainDir, branch: "main", manifest: manifest}
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.handleSessions(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
		return rec
	}

	before := request()
	if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), canary) {
		t.Fatalf("pre-exclusion response = %d %s", before.Code, before.Body.String())
	}
	stones := emptySessionTombstones()
	stones.Excluded["private"] = sessionTombstone{At: time.Now().UTC()}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	after := request()
	if after.Code != http.StatusOK || strings.Contains(after.Body.String(), canary) || strings.Contains(after.Body.String(), "private") {
		t.Fatalf("same server exposed a newly excluded session: %d %s", after.Code, after.Body.String())
	}
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath))); err != nil {
		t.Fatal(err)
	}
	disappeared := request()
	if disappeared.Code != http.StatusOK || strings.Contains(disappeared.Body.String(), canary) || strings.Contains(disappeared.Body.String(), "private") {
		t.Fatalf("disappeared policy resurrected an excluded session: %d %s", disappeared.Code, disappeared.Body.String())
	}

	writeSessionTombstoneBytes(t, brainDir, []byte("{broken"))
	corrupt := request()
	if corrupt.Code != http.StatusServiceUnavailable || strings.Contains(corrupt.Body.String(), canary) || !strings.Contains(corrupt.Body.String(), memoryErrStateCorrupt) {
		t.Fatalf("corrupt live policy did not fail closed: %d %s", corrupt.Code, corrupt.Body.String())
	}
}

// The CSP/no-egress headers must be present on REAL /api responses, not just when
// the middleware is exercised in isolation.
func TestVizSecurityHeaders_AppliedToMux(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
	m, err := srv.mux()
	if err != nil {
		t.Fatalf("mux: %v", err)
	}
	h := vizSecurityHeaders(m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("CSP header not applied to /api/summary through the mux")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff header not applied through the mux")
	}
}

// A gigantic ?limit must be clamped by the safety ceiling, not panic or overflow.
func TestVizHandleGraph_HugeLimitNoPanic(t *testing.T) {
	t.Parallel()
	srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, httptest.NewRequest(http.MethodGet, "/api/graph?limit=999999999", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp vizGraphResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// Every feature endpoint must degrade to a valid 200 on an unbuilt brain, never 500.
func TestVizFeatureHandlers_UnbuiltBrain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		handler func(*vizServer) http.HandlerFunc
	}{
		{"summary", "/api/summary", func(s *vizServer) http.HandlerFunc { return s.handleSummary }},
		{"search", "/api/search?q=foo", func(s *vizServer) http.HandlerFunc { return s.handleSearch }},
		{"facts", "/api/facts", func(s *vizServer) http.HandlerFunc { return s.handleFacts }},
		{"sessions", "/api/sessions", func(s *vizServer) http.HandlerFunc { return s.handleSessions }},
		{"history", "/api/history?limit=0", func(s *vizServer) http.HandlerFunc { return s.handleHistory }},
		{"docs", "/api/docs", func(s *vizServer) http.HandlerFunc { return s.handleDocs }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &vizServer{brainDir: t.TempDir(), branch: "main"}
			rec := httptest.NewRecorder()
			tc.handler(srv)(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", tc.name, rec.Code)
			}
			if !json.Valid(rec.Body.Bytes()) {
				t.Fatalf("%s: response is not valid JSON", tc.name)
			}
		})
	}
}

func TestVizSessionFallbackText(t *testing.T) {
	t.Parallel()
	if got := vizSessionFallbackText(exportSession{}); got != "No summary recorded." {
		t.Fatalf("no files: got %q, want the fallback note", got)
	}
	if got := vizSessionFallbackText(exportSession{FilesTouched: []string{"  ", ""}}); got != "No summary recorded." {
		t.Fatalf("blank files: got %q, want the fallback note", got)
	}
	got := vizSessionFallbackText(exportSession{FilesTouched: []string{"a/b/foo.go", "x/bar.go"}})
	for _, want := range []string{"**Files touched**", "- foo.go", "- bar.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("files: %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "more)") {
		t.Fatalf("files: unexpected overflow note in %q", got)
	}
	many := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		many = append(many, fmt.Sprintf("dir/f%d.go", i))
	}
	over := vizSessionFallbackText(exportSession{FilesTouched: many})
	if !strings.Contains(over, "_(+3 more)_") { // 11 touched, 8 shown -> 3 more
		t.Fatalf("overflow: %q missing the +3 more note", over)
	}
	if n := strings.Count(over, "- f"); n != 8 { // exactly 8 file bullets are shown
		t.Fatalf("overflow: got %d file bullets, want 8 in %q", n, over)
	}
}
