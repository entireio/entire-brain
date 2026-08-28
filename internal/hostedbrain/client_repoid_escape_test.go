package hostedbrain

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRPCEscapesRepoID pins the repo id into exactly ONE path segment of the brain
// MCP endpoint.
//
// The url is assembled by concatenation, so an unescaped repo id is not merely
// cosmetic: it rewrites the request target. A "/" adds path segments (reaching a
// different route on the same origin), a "?" ends the path early and turns the rest
// of the template into a query string, a "#" truncates the path outright, and dot
// segments walk up the api prefix — each of which sends the caller's bearer token to
// an endpoint the caller never asked for. url.PathEscape is what every sibling call
// in this repo already does (factsync/httpserver.go, factsync/httpproposals.go,
// cli/publish.go); this test is the reason.
func TestRPCEscapesRepoID(t *testing.T) {
	const wantPrefix = "/api/v1/repos/"
	const wantSuffix = "/brain/mcp"

	cases := []struct {
		name   string
		repoID string
	}{
		{"extra path segments", "a/b"},
		{"dot segment traversal", "../../admin"},
		{"encoded traversal", "..%2f..%2fadmin"},
		{"query injection", "x?admin=1"},
		{"fragment truncation", "x#frag"},
		{"route hijack", "x/brain/facts/advance"},
		{"benign", "repo-123"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// EscapedPath is what an HTTP router matches on, so it is the
				// honest view of which route the request actually reached: a
				// percent-encoded separator stays inside one segment there,
				// while r.URL.Path has already decoded it away.
				gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"test"}}}`)
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL}
			if _, _, err := c.Initialize(context.Background(), tc.repoID); err != nil {
				t.Fatalf("Initialize: %v", err)
			}

			// The server must see the api prefix and the brain/mcp suffix intact,
			// with the repo id occupying exactly one segment between them and no
			// query string smuggled out of it.
			if gotQuery != "" {
				t.Errorf("repo id %q leaked into the query string: %q", tc.repoID, gotQuery)
			}
			middle, ok := trimPathTemplate(gotPath, wantPrefix, wantSuffix)
			if !ok {
				t.Fatalf("repo id %q rewrote the request target: got path %q, want %s<repo>%s", tc.repoID, gotPath, wantPrefix, wantSuffix)
			}
			if middle == "" {
				t.Errorf("repo id %q collapsed to an empty path segment", tc.repoID)
			}
			for i := 0; i < len(middle); i++ {
				if middle[i] == '/' {
					t.Errorf("repo id %q spans multiple path segments: %q", tc.repoID, middle)
					break
				}
			}
		})
	}
}

func trimPathTemplate(path, prefix, suffix string) (string, bool) {
	if len(path) < len(prefix)+len(suffix) {
		return "", false
	}
	if path[:len(prefix)] != prefix {
		return "", false
	}
	if path[len(path)-len(suffix):] != suffix {
		return "", false
	}
	return path[len(prefix) : len(path)-len(suffix)], true
}
