package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const vizOptimizationSnapshotPath = "semantic/snapshots/viz-test/snapshot.ndjson"

func writeVizOptimizationSnapshot(t *testing.T, brainDir string, records []semanticRecord) *semanticSourceManifest {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(vizOptimizationSnapshotPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("{\"record_type\":\"header\"}\n")
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var symbols, relations int
	for _, record := range records {
		switch record.RecordType {
		case "symbol":
			symbols++
		case "relation":
			relations++
		}
	}
	return &semanticSourceManifest{SnapshotPath: vizOptimizationSnapshotPath, Symbols: symbols, Relations: relations}
}

func vizOptimizationSemanticRecords() []semanticRecord {
	return []semanticRecord{
		{RecordType: "symbol", ID: "leaf", Name: "Leaf", QualifiedName: "pkg.Leaf", Kind: "function", FilePath: "pkg/leaf.go", StartLine: 3, Language: "Go"},
		{RecordType: "symbol", ID: "hub", Name: "Hub", QualifiedName: "pkg.Hub", Kind: "function", FilePath: "pkg/a b.go", StartLine: 11, Language: "Go", Signature: "func Hub()", Blob: "func Hub() { Peer() }"},
		{RecordType: "symbol", ID: "peer", Name: "Peer", QualifiedName: "pkg.Peer", Kind: "function", FilePath: "pkg/peer.go", StartLine: 7, Language: "Go"},
		{RecordType: "symbol", ID: "tail", Name: "Tail", QualifiedName: "pkg.Tail", Kind: "function", FilePath: "pkg/tail.go", StartLine: 5, Language: "Go"},
		{RecordType: "relation", FromID: "hub", ToID: "peer", Type: "CALLS", Confidence: 0.9, RelationScope: "file", Resolution: "exact", Reason: "PRIVATE-RELATION-REASON"},
		// The graph is undirected for layout purposes, so the reverse relation must
		// not produce a second visual edge.
		{RecordType: "relation", FromID: "peer", ToID: "hub", Type: "CALLS", Confidence: 0.8},
		{RecordType: "relation", FromID: "hub", ToID: "tail", Type: "CALLS", Confidence: 0.7},
	}
}

func TestVizGraphSelectsConnectedSymbolsAndStableEdges(t *testing.T) {
	brainDir := t.TempDir()
	sem := writeVizOptimizationSnapshot(t, brainDir, vizOptimizationSemanticRecords())
	srv := &vizServer{brainDir: brainDir, branch: "main", manifest: &exportManifest{Sources: &brainSources{Semantic: sem}}}

	request := func(target string) (*httptest.ResponseRecorder, vizGraphResp) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.handleGraph(rec, httptest.NewRequest(http.MethodGet, target, nil))
		var response vizGraphResp
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode graph: %v", err)
		}
		return rec, response
	}

	firstRec, first := request("/api/graph?limit=2")
	secondRec, second := request("/api/graph?limit=2")
	if firstRec.Code != http.StatusOK || secondRec.Code != http.StatusOK {
		t.Fatalf("graph statuses = %d, %d", firstRec.Code, secondRec.Code)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("identical graph requests changed ordering:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if len(first.Nodes) != 2 {
		t.Fatalf("degree-ranked node count = %d, want 2", len(first.Nodes))
	}
	if got := []string{first.Nodes[0].ID, first.Nodes[1].ID}; !reflect.DeepEqual(got, []string{"hub", "peer"}) {
		t.Fatalf("degree-ranked nodes = %v, want [hub peer]", got)
	}
	if first.Total != 4 || !first.Truncated {
		t.Fatalf("graph metadata = total %d truncated %v, want 4/true", first.Total, first.Truncated)
	}
	if len(first.Edges) != 1 || first.Edges[0].From != "hub" || first.Edges[0].To != "peer" {
		t.Fatalf("visual edges = %+v, want one deduplicated hub-peer edge", first.Edges)
	}
	if strings.Contains(firstRec.Body.String(), "PRIVATE-RELATION-REASON") || strings.Contains(firstRec.Body.String(), "record_type") {
		t.Fatalf("graph exposed raw semantic fields: %s", firstRec.Body.String())
	}

	_, malformed := request("/api/graph?limit=not-a-number")
	if len(malformed.Nodes) != 4 || malformed.Truncated {
		t.Fatalf("malformed limit did not retain the safe default: %+v", malformed)
	}
}

func TestVizNodeAndReplayReturnContainedSemanticSubgraphs(t *testing.T) {
	brainDir := t.TempDir()
	sem := writeVizOptimizationSnapshot(t, brainDir, vizOptimizationSemanticRecords())
	manifest := &exportManifest{Sources: &brainSources{
		Semantic: sem,
		Sessions: &sessionSourceManifest{Sessions: []exportSession{{
			SessionID: "session-1", Agent: "Codex", Model: "test", FilesTouched: []string{"pkg/a b.go", "pkg/peer.go", "pkg/a b.go"},
			Summary: &checkpointSummary{Intent: "Connect Hub to Peer"}, CheckpointsCount: 2,
		}}},
	}}
	srv := &vizServer{brainDir: brainDir, branch: "feature/a b#c", provider: "gh", owner: "acme", repo: "widget", manifest: manifest}

	nodeRec := httptest.NewRecorder()
	srv.handleNode(nodeRec, httptest.NewRequest(http.MethodGet, "/api/node?id=hub", nil))
	var node vizNodeResp
	if err := json.Unmarshal(nodeRec.Body.Bytes(), &node); err != nil {
		t.Fatal(err)
	}
	if nodeRec.Code != http.StatusOK || node.Symbol.ID != "hub" || node.Snippet != "func Hub() { Peer() }" {
		t.Fatalf("node response = %d %+v", nodeRec.Code, node)
	}
	if len(node.Neighbors) != 2 {
		t.Fatalf("neighbor count = %d, want 2", len(node.Neighbors))
	}
	if got := []string{node.Neighbors[0].ID, node.Neighbors[1].ID}; !reflect.DeepEqual(got, []string{"peer", "tail"}) {
		t.Fatalf("neighbor order = %v, want [peer tail]", got)
	}
	if len(node.Relations) != 3 {
		t.Fatalf("incident relations = %d, want 3", len(node.Relations))
	}
	if !strings.Contains(node.Link, "/blob/feature/a%20b%23c/pkg/a%20b.go#L11") {
		t.Fatalf("source link did not escape branch and file segments: %q", node.Link)
	}
	if strings.Contains(nodeRec.Body.String(), "PRIVATE-RELATION-REASON") {
		t.Fatalf("node response exposed relation reason: %s", nodeRec.Body.String())
	}

	replayRec := httptest.NewRecorder()
	srv.handleSessionReplay(replayRec, httptest.NewRequest(http.MethodGet, "/api/session/replay?id=session-1", nil))
	var replay vizReplayResp
	if err := json.Unmarshal(replayRec.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replayRec.Code != http.StatusOK || replay.Session == nil || replay.Session.ID != "session-1" {
		t.Fatalf("replay session = %d %+v", replayRec.Code, replay.Session)
	}
	if !reflect.DeepEqual(replay.Files, []string{"pkg/a b.go", "pkg/peer.go"}) {
		t.Fatalf("replay files = %v", replay.Files)
	}
	if len(replay.Steps) != 2 || !reflect.DeepEqual(replay.Steps[0].IDs, []string{"hub"}) || !reflect.DeepEqual(replay.Steps[1].IDs, []string{"peer"}) {
		t.Fatalf("replay steps = %+v", replay.Steps)
	}
	if replay.Total != 2 || len(replay.Nodes) != 2 || len(replay.Edges) != 2 {
		t.Fatalf("replay subgraph = total %d nodes %d edges %d", replay.Total, len(replay.Nodes), len(replay.Edges))
	}
}

func TestVizSemanticPathEscapeFailsClosed(t *testing.T) {
	privateRoot := t.TempDir()
	brainDir := filepath.Join(privateRoot, "brain")
	if err := os.Mkdir(brainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(privateRoot, "outside-viz.ndjson")
	outsideData := "{\"record_type\":\"header\"}\n" +
		"{\"record_type\":\"symbol\",\"id\":\"private\",\"name\":\"PRIVATE-OUTSIDE-SNAPSHOT\",\"kind\":\"function\",\"blob\":\"PRIVATE-OUTSIDE-SNAPSHOT\"}\n"
	if err := os.WriteFile(outside, []byte(outsideData), 0o600); err != nil {
		t.Fatal(err)
	}
	sem := &semanticSourceManifest{SnapshotPath: "../outside-viz.ndjson", Symbols: 1}
	srv := &vizServer{brainDir: brainDir, manifest: &exportManifest{Sources: &brainSources{Semantic: sem}}}

	for _, target := range []string{"/api/graph", "/api/node?id=private"} {
		rec := httptest.NewRecorder()
		if strings.HasPrefix(target, "/api/node") {
			srv.handleNode(rec, httptest.NewRequest(http.MethodGet, target, nil))
		} else {
			srv.handleGraph(rec, httptest.NewRequest(http.MethodGet, target, nil))
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "warnings") {
			t.Fatalf("%s did not return a contained warning response: %d %s", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "PRIVATE-OUTSIDE-SNAPSHOT") {
			t.Fatalf("%s read data outside the brain: %s", target, rec.Body.String())
		}
	}
}

func TestVizHistoryFiltersPrivateAndMalformedRecordsDeterministically(t *testing.T) {
	brainDir := t.TempDir()
	index := historyIndex{Records: []historyRecord{
		{ID: "h1", Kind: "decision", Branch: "main", Path: "pkg/a.go", Summary: "Keep stable IDs", Terms: []string{"stable", "cache"}},
		{ID: "h2", Kind: "learning", Branch: "main", Path: "pkg/a.go", Summary: "Cache graph", Terms: []string{"cache"}},
		{ID: "h2", Kind: "learning", Summary: "duplicate must disappear"},
		{ID: "", Kind: "decision", Summary: "empty ID must disappear"},
		{ID: "private", Kind: "decision", SessionID: "secret-session", Summary: "PRIVATE-HISTORY-CANARY"},
		{ID: "h3", Kind: "validation", Branch: "main", Path: "pkg/b.go", Summary: "Ordering verified"},
	}}
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	source := &historySourceManifest{IndexPath: historyIndexPath, Records: len(index.Records)}
	srv := &vizServer{
		brainDir: brainDir,
		manifest: &exportManifest{Sources: &brainSources{History: source}},
		guard: sessionReadGuard{ids: map[string]sessionTombstone{"secret-session": {
			At: time.Date(2026, time.September, 17, 10, 0, 0, 0, time.UTC),
		}}},
	}

	rec := httptest.NewRecorder()
	srv.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history?limit=bad", nil))
	var graph vizFeatureGraph
	if err := json.Unmarshal(rec.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "PRIVATE-HISTORY-CANARY") {
		t.Fatalf("history privacy response = %d %s", rec.Code, rec.Body.String())
	}
	if len(graph.Nodes) != 3 {
		t.Fatalf("filtered history node count = %d, want 3", len(graph.Nodes))
	}
	if got := []string{graph.Nodes[0].ID, graph.Nodes[1].ID, graph.Nodes[2].ID}; !reflect.DeepEqual(got, []string{"h1", "h2", "h3"}) {
		t.Fatalf("filtered history order = %v", got)
	}
	if graph.Total != 3 || graph.Truncated {
		t.Fatalf("history metadata = total %d truncated %v", graph.Total, graph.Truncated)
	}
	if len(graph.Edges) != 2 || graph.Edges[0].From != "h1" || graph.Edges[0].To != "h2" || graph.Edges[1].From != "h2" || graph.Edges[1].To != "h3" {
		t.Fatalf("history timeline/dedup edges = %+v", graph.Edges)
	}
	if graph.Nodes[0].Color != colorSession || graph.Nodes[1].Color != "#22d3ee" || graph.Nodes[2].Color != colorHistory {
		t.Fatalf("history colors = %q %q %q", graph.Nodes[0].Color, graph.Nodes[1].Color, graph.Nodes[2].Color)
	}
}

func TestVizSummaryUsesManifestCountsWithoutLocalPathDisclosure(t *testing.T) {
	generated := time.Date(2026, time.September, 17, 10, 30, 0, 0, time.UTC)
	srv := &vizServer{
		repoDir:  "/Users/alice/private/customer-widget",
		brainDir: t.TempDir(), branch: "main", provider: "gh", owner: "acme", repo: "widget",
		manifest: &exportManifest{Sources: &brainSources{
			Semantic: &semanticSourceManifest{GeneratedAt: generated, Symbols: 4, Relations: 3, Files: 4},
			History:  &historySourceManifest{Records: 6},
			Facts:    &factSourceManifest{Facts: 5},
			Sessions: &sessionSourceManifest{Sessions: []exportSession{{SessionID: "one"}, {SessionID: "two"}}},
		}},
	}
	rec := httptest.NewRecorder()
	srv.handleSummary(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	var summary vizSummaryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Repo != "gh/acme/widget" || summary.GeneratedAt != generated.Format(time.RFC3339) {
		t.Fatalf("summary identity = %+v", summary)
	}
	if summary.Counts.Symbols != 4 || summary.Counts.Relations != 3 || summary.Counts.Files != 4 || summary.Counts.History != 6 || summary.Counts.Facts != 5 || summary.Counts.Sessions != 2 {
		t.Fatalf("summary counts = %+v", summary.Counts)
	}
	if strings.Contains(rec.Body.String(), "/Users/alice") {
		t.Fatalf("summary disclosed the local repository path: %s", rec.Body.String())
	}
}
