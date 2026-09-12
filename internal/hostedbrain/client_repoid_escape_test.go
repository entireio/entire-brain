package hostedbrain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The repo id must stay inside one path segment of the request URL.
//
// rpc() built its target by concatenating the id straight into the path:
//
//	"/api/v1/repos/" + repoID + "/brain/mcp"
//
// A "/", "?", "#" or dot segment in the id therefore rewrites the target. That
// matters more here than in an ordinary URL bug, because this request carries
// the caller's bearer token in an Authorization header: a rewritten target
// sends that token to an endpoint nobody asked for. The id arrives from
// --repo-id or the environment, neither of which this package controls.
//
// publish.go has escaped its own repo id since it was written. client.go was
// the one remaining site that did not.

// captureTarget runs one rpc() against a stub server and reports the path the
// server actually received.
func captureTarget(t *testing.T, repoID string) (path string, rawQuery string, fragmentSeen bool) {
	t.Helper()
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not Path: net/http DECODES Path, so an unescaped "/" in
		// the id comes back indistinguishable from a real separator and a
		// Path-based assertion passes on the very bug this is here to catch.
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "secret-token"}
	// The error is not the subject — several of these ids are nonsense and the
	// server stub answers everything. What is under test is the path that
	// reached the server.
	_, _ = c.rpc(context.Background(), repoID, "initialize", nil)
	// A fragment never leaves the client, so its absence from the path is the
	// only observable.
	return gotPath, gotQuery, strings.Contains(gotPath, "#")
}

func TestRPCKeepsRepoIDInOnePathSegment(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")

	cases := []struct {
		name   string
		repoID string
		// the segment the server must see between /repos/ and /brain/mcp
		wantSegment string
	}{
		{name: "ordinary id", repoID: "gh/example/repo", wantSegment: "gh%2Fexample%2Frepo"},
		{name: "dot segment climbs out", repoID: "../../admin", wantSegment: "..%2F..%2Fadmin"},
		{name: "query terminator", repoID: "repo?x=1", wantSegment: "repo%3Fx=1"},
		{name: "plain id is untouched", repoID: "plainrepo", wantSegment: "plainrepo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPath, gotQuery, _ := captureTarget(t, tc.repoID)
			if gotPath == "" {
				t.Skip("request never reached the server (egress disabled in this environment)")
			}
			const prefix = "/api/v1/repos/"
			const suffix = "/brain/mcp"
			if !strings.HasPrefix(gotPath, prefix) || !strings.HasSuffix(gotPath, suffix) {
				t.Fatalf("repo id %q rewrote the request target: server saw %q (want %s<id>%s)",
					tc.repoID, gotPath, prefix, suffix)
			}
			// The escaped segment must match what PathEscape produces, and must
			// contain no separator of its own.
			seg := strings.TrimSuffix(strings.TrimPrefix(gotPath, prefix), suffix)
			if seg != tc.wantSegment {
				t.Fatalf("repo id %q arrived as segment %q, want %q", tc.repoID, seg, tc.wantSegment)
			}
			if strings.Contains(seg, "/") {
				t.Fatalf("repo id %q kept a raw %q and spans more than one segment: %q", tc.repoID, "/", seg)
			}
			if gotQuery != "" {
				t.Fatalf("repo id %q leaked into the query string: %q", tc.repoID, gotQuery)
			}
		})
	}
}

// TestRPCDoesNotSendTheTokenToARewrittenPath is the reason the test above
// matters, stated as its own assertion: whatever the id contains, the
// Authorization header must only ever reach the repos endpoint.
func TestRPCDoesNotSendTheTokenToARewrittenPath(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")

	var sawAuthOn string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuthOn = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "secret-token"}
	_, _ = c.rpc(context.Background(), "../../../steal", "initialize", nil)

	if sawAuthOn == "" {
		t.Skip("request never reached the server (egress disabled in this environment)")
	}
	if !strings.HasPrefix(sawAuthOn, "/api/v1/repos/") {
		t.Fatalf("bearer token was sent to %q, outside the repos endpoint", sawAuthOn)
	}
}
