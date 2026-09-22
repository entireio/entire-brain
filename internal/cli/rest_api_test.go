package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

// An unrecognised source turns every include flag off downstream, so the raw
// value produced 200 with zero results — an empty search presented as a
// complete answer, where the same typo on the CLI is an error. The REST surface
// has to give the same answer as the CLI and MCP for the same mistake.
func TestRESTRejectsAnUnknownSource(t *testing.T) {
	server, token := restServer(t)
	for _, bad := range []string{"facts", "histories", "nonsense", "fac"} {
		status, body := restGet(t, server, token, "/v1/search?q=x&source="+url.QueryEscape(bad))
		if status != http.StatusBadRequest {
			t.Fatalf("source=%q returned %d, want 400 — an empty result is not an answer", bad, status)
		}
		if !strings.Contains(string(body), "source") {
			t.Fatalf("source=%q: the error should name the parameter: %s", bad, body)
		}
	}
	// The valid ones, including the empty default, must still work.
	// Trimmed and case-folded, because the shared parser does that and the
	// point of routing through it is that REST answers exactly as the CLI does.
	for _, good := range []string{"", "all", "fact", "history", "doc", "FACT", " fact "} {
		if status, body := restGet(t, server, token, "/v1/search?q=x&source="+url.QueryEscape(good)); status != http.StatusOK {
			t.Fatalf("source=%q was refused with %d: %s", good, status, body)
		}
	}
}

// Every retrieval surface buffers its response and revalidates the
// session-exclusion guard immediately before emitting a byte, because ranking
// is slow enough for a concurrent tombstone to land after the first snapshot.
// This endpoint wrote straight to the socket and skipped it — over the network,
// and optionally beyond loopback.
//
// A source check, because the defect was an endpoint that looked fine in
// isolation and disagreed with every other surface; no response-level test of
// this handler alone would have surfaced it.
func TestRESTRetrievalGoesThroughThePrivacyWriteBoundary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".", "rest_api.go"))
	if err != nil {
		t.Fatalf("read rest_api.go: %v", err)
	}
	source := string(data)

	for _, required := range []string{
		"captureRetrievalPrivacyPolicy(",
		"revalidateRetrievalResponsePrivacy(",
		"writeRetrievalResponseBytes(",
	} {
		if !strings.Contains(source, required) {
			t.Fatalf("rest_api.go does not call %s; brain content can be served after an exclusion lands", required)
		}
	}

	// And no brain read may bypass it. Each of these handlers reads a brain,
	// so each must emit through the retrieval boundary rather than writeRESTJSON.
	for _, handler := range []string{"func restSearch(", "func restFacts(", "func restStatus("} {
		start := strings.Index(source, handler)
		if start < 0 {
			t.Fatalf("%s is gone; this test no longer covers what it claims", handler)
		}
		body := source[start:]
		if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
			body = body[:end]
		}
		if strings.Contains(body, "writeRESTJSON(") {
			t.Fatalf("%s emits brain content with writeRESTJSON, bypassing the privacy write boundary", handler)
		}
		if !strings.Contains(body, "writeRESTRetrieval(") {
			t.Fatalf("%s does not emit through writeRESTRetrieval", handler)
		}
	}
}

// The response has to be buffered before the guard runs: a policy change
// partway through a streamed encode would emit a half-written stale body, which
// is why the shared helper takes bytes rather than a writer.
func TestRESTRetrievalBuffersBeforeWriting(t *testing.T) {
	server, token := restServer(t)
	// A normal read must still work end to end through the new boundary.
	for _, path := range []string{"/v1/status", "/v1/facts", "/v1/search?q=retry"} {
		status, body := restGet(t, server, token, path)
		if status != http.StatusOK {
			t.Fatalf("%s returned %d: %s", path, status, body)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("%s returned a body that is not complete JSON (%v): %s", path, err, body)
		}
	}
}
