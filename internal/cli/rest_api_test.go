package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// MCP is the right protocol for an agent and the wrong one for everything else.
// A dashboard, a CI check or a notebook had to speak JSON-RPC or parse CLI
// output. These tests cover the resource endpoints and, more importantly, that
// adding them did not open a hole in the listener they share.

func restServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	opts := httpTestOptions(t)
	cfg := mcpHTTPConfig{Addr: "127.0.0.1:0", Token: "right-token", Loopback: true}
	server := httptest.NewServer(newMCPHTTPHandler(opts, cfg))
	t.Cleanup(server.Close)
	return server, "right-token"
}

func restGet(t *testing.T, server *httptest.Server, token, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, body
}

func TestRESTEndpointsAreBehindTheSameAuthAsMCP(t *testing.T) {
	server, token := restServer(t)
	// The whole reason REST shares the MCP listener is that one auth path
	// cannot drift from another. If a new endpoint could be reached without a
	// token, sharing the listener bought nothing.
	for _, path := range []string{"/v1/", "/v1/status", "/v1/facts", "/v1/search?q=x", "/v1/nonexistent"} {
		if status, _ := restGet(t, server, "", path); status != http.StatusUnauthorized {
			t.Fatalf("%s without a token returned %d, want 401", path, status)
		}
		if status, _ := restGet(t, server, "wrong", path); status != http.StatusUnauthorized {
			t.Fatalf("%s with a wrong token returned %d, want 401", path, status)
		}
	}
	if status, _ := restGet(t, server, token, "/v1/"); status != http.StatusOK {
		t.Fatalf("the correct token was refused: %d", status)
	}
}

func TestRESTRootListsWhatExists(t *testing.T) {
	server, token := restServer(t)
	status, body := restGet(t, server, token, "/v1/")
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	var payload struct {
		Endpoints []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// An API discoverable from its root is one fewer document to keep in sync.
	seen := map[string]bool{}
	for _, e := range payload.Endpoints {
		seen[e.Path] = true
	}
	for _, want := range []string{"/v1/status", "/v1/facts", "/v1/search"} {
		if !seen[want] {
			t.Fatalf("the root does not list %s: %s", want, body)
		}
	}
}

func TestRESTRefusesUnboundedAndNonsenseLimits(t *testing.T) {
	server, token := restServer(t)
	// An unbounded limit on an endpoint that serialises records turns one
	// request into all of memory.
	for _, bad := range []string{"0", "-1", "abc", "100000"} {
		status, body := restGet(t, server, token, "/v1/facts?limit="+bad)
		if status != http.StatusBadRequest {
			t.Fatalf("limit=%s returned %d, want 400", bad, status)
		}
		if !strings.Contains(string(body), "limit") {
			t.Fatalf("limit=%s: the error should name the parameter: %s", bad, body)
		}
	}
	if status, _ := restGet(t, server, token, "/v1/facts?limit=5"); status != http.StatusOK {
		t.Fatalf("a valid limit was refused: %d", status)
	}
}

func TestRESTSearchRequiresAQueryAndSaysSo(t *testing.T) {
	server, token := restServer(t)
	status, body := restGet(t, server, token, "/v1/search")
	if status != http.StatusBadRequest {
		t.Fatalf("a search with no q returned %d, want 400", status)
	}
	// An error that shows the shape of a working call is worth more than one
	// that states a rule.
	if !strings.Contains(string(body), "/v1/search?q=") {
		t.Fatalf("the error should show a working example: %s", body)
	}
}

func TestRESTUnknownPathIsA404ThatPointsSomewhere(t *testing.T) {
	server, token := restServer(t)
	status, body := restGet(t, server, token, "/v1/nope")
	if status != http.StatusNotFound {
		t.Fatalf("unknown path returned %d, want 404", status)
	}
	if !strings.Contains(string(body), "/v1/") {
		t.Fatalf("a 404 should point at the discovery endpoint: %s", body)
	}
}

func TestRESTDidNotDisturbTheMCPEndpoint(t *testing.T) {
	server, token := restServer(t)
	// Mounting REST on this listener must not have shadowed the JSON-RPC POST
	// that was already there.
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MCP POST returned %d after REST was added", resp.StatusCode)
	}
	var payload struct {
		Result struct {
			Tools []struct{ Name string } `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Result.Tools) == 0 {
		t.Fatal("MCP tools/list came back empty after REST was mounted")
	}
}
