package hostedbrain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ashtom/entire-brain/internal/brainwire"
)

// hostedMCPServer stands up an httptest server that speaks entire-api's brain MCP
// contract (brain_mcp.go): JSON-RPC initialize / tools/list / tools/call over a single
// POST. schemaVersion controls what initialize advertises (for negotiation tests).
func hostedMCPServer(t *testing.T, schemaVersion string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		reply := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		}
		switch req.Method {
		case "initialize":
			si := map[string]any{"name": "entire-brain-hosted", "version": "0.1.0"}
			if schemaVersion != "" {
				si["brainSchemaVersion"] = schemaVersion
			}
			reply(map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": si})
		case "tools/list":
			reply(map[string]any{"tools": []map[string]any{{"name": "brain_search", "description": "search"}}})
		case "tools/call":
			var p struct {
				Name      string `json:"name"`
				Arguments struct {
					Query string `json:"query"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text := func(v any) {
				b, _ := json.Marshal(v)
				reply(map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}})
			}
			switch p.Name {
			case "brain_search":
				if p.Arguments.Query == "" {
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32000, "message": "query is required"}})
					return
				}
				text([]SearchResult{{Fact: Fact{ID: "fact:a", Text: "the build uses bazel", Status: "active"}, Score: 2}})
			case "brain_get":
				text(GetResult{Found: true, Fact: Fact{ID: "fact:a", Text: "the build uses bazel", Status: "active"}})
			case "brain_multi_get":
				text(MultiGetResult{Facts: []Fact{{ID: "fact:a", Status: "active"}}, Missing: []string{"fact:x"}})
			case "brain_status":
				text(StatusResult{Branch: "main", Active: 2, Superseded: 1, Retracted: 0, Total: 3})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32000, "message": "unknown tool"}})
			}
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}))
}

func TestClientInitializeNegotiatesSchema(t *testing.T) {
	ctx := context.Background()

	// Compatible schema (matches local brainwire.BrainSchemaVersion major) → ok.
	ts := hostedMCPServer(t, brainwire.BrainSchemaVersion)
	defer ts.Close()
	c := &Client{BaseURL: ts.URL, Token: "tok"}
	info, _, err := c.Initialize(ctx, "repo1")
	if err != nil {
		t.Fatalf("initialize (compatible): %v", err)
	}
	if info.Name != "entire-brain-hosted" || info.BrainSchemaVersion != brainwire.BrainSchemaVersion {
		t.Fatalf("serverInfo = %+v", info)
	}

	// Incompatible MAJOR → hard error before any facts are read.
	ts2 := hostedMCPServer(t, "2.0")
	defer ts2.Close()
	c2 := &Client{BaseURL: ts2.URL, Token: "tok"}
	if _, _, err := c2.Initialize(ctx, "repo1"); err == nil {
		t.Fatal("initialize with incompatible major should error")
	}

	// A server that advertises no schema version → accepted (older server), no negotiation.
	ts3 := hostedMCPServer(t, "")
	defer ts3.Close()
	c3 := &Client{BaseURL: ts3.URL, Token: "tok"}
	if _, _, err := c3.Initialize(ctx, "repo1"); err != nil {
		t.Fatalf("initialize (no schema advertised) should be accepted: %v", err)
	}
}

func TestClientListToolsAndSearch(t *testing.T) {
	ctx := context.Background()
	ts := hostedMCPServer(t, brainwire.BrainSchemaVersion)
	defer ts.Close()
	c := &Client{BaseURL: ts.URL, Token: "tok"}

	tools, err := c.ListTools(ctx, "repo1")
	if err != nil || len(tools) != 1 || tools[0].Name != "brain_search" {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}

	results, err := c.Search(ctx, "repo1", "main", "build bazel", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Fact.ID != "fact:a" || results[0].Score != 2 {
		t.Fatalf("Search results = %+v; want fact:a score 2", results)
	}

	// A tool error (empty query reaches the server) propagates as a *jsonrpcError.
	if _, err := c.CallTool(ctx, "repo1", "brain_search", map[string]any{"query": ""}); err == nil {
		t.Fatal("empty-query CallTool should return the server's JSON-RPC error")
	} else {
		var je *jsonrpcError
		if !errors.As(err, &je) || je.Code != -32000 {
			t.Fatalf("error = %v; want *jsonrpcError code -32000", err)
		}
	}
}

func TestClientTypedWrappers(t *testing.T) {
	ctx := context.Background()
	ts := hostedMCPServer(t, brainwire.BrainSchemaVersion)
	defer ts.Close()
	c := &Client{BaseURL: ts.URL, Token: "tok"}

	if g, err := c.Get(ctx, "repo1", "main", "fact:a"); err != nil || !g.Found || g.Fact.ID != "fact:a" {
		t.Fatalf("Get = %+v, %v", g, err)
	}
	if mg, err := c.MultiGet(ctx, "repo1", "main", []string{"fact:a", "fact:x"}); err != nil || len(mg.Facts) != 1 || len(mg.Missing) != 1 {
		t.Fatalf("MultiGet = %+v, %v", mg, err)
	}
	if s, err := c.Status(ctx, "repo1", "main"); err != nil || s.Active != 2 || s.Total != 3 {
		t.Fatalf("Status = %+v, %v", s, err)
	}
}

func TestClientTypedTransportErrors(t *testing.T) {
	ctx := context.Background()
	statusServer := func(code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"title":"denied"}`))
		}))
	}
	for _, tc := range []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusServiceUnavailable, ErrNotConfigured},
	} {
		ts := statusServer(tc.code)
		c := &Client{BaseURL: ts.URL, Token: "tok"}
		_, _, err := c.Initialize(ctx, "repo1")
		if !errors.Is(err, tc.want) {
			t.Fatalf("status %d → %v; want %v", tc.code, err, tc.want)
		}
		ts.Close()
	}
}

func TestClientRespectsEgressGate(t *testing.T) {
	ctx := context.Background()
	ts := hostedMCPServer(t, brainwire.BrainSchemaVersion)
	defer ts.Close()
	c := &Client{BaseURL: ts.URL, Token: "tok"}

	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	if _, _, err := c.Initialize(ctx, "repo1"); !errors.Is(err, ErrNoEgress) {
		t.Fatalf("Initialize under no-egress = %v; want ErrNoEgress", err)
	}
	if _, err := c.Search(ctx, "repo1", "main", "q", 1); !errors.Is(err, ErrNoEgress) {
		t.Fatalf("Search under no-egress = %v; want ErrNoEgress", err)
	}

	// LOCAL_ONLY is the other gate; an unrecognized value fails closed (still on).
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "maybe")
	if _, err := c.ListTools(ctx, "repo1"); !errors.Is(err, ErrNoEgress) {
		t.Fatalf("ListTools under LOCAL_ONLY=maybe = %v; want ErrNoEgress (fail-closed)", err)
	}
}
