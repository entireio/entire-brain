package hostedbrain

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/repoid"
)

// TestRPCRejectsRepoIDThatIsNotOneSafeSegment is the assertion url.PathEscape alone
// cannot make.
//
// Escaping is necessary but NOT sufficient: url.PathEscape leaves "." and ".."
// completely untouched, because a dot is an unreserved character. So a repo id of
// ".." escapes to ".." and the assembled target is
//
//	/api/v1/repos/../brain/mcp
//
// which every dot-segment-normalizing hop between here and the handler — net/http's
// own ServeMux, nginx, a CDN, RFC 3986 §5.2.4 resolution — collapses to
//
//	/api/v1/brain/mcp
//
// i.e. a route the caller never asked for, reached with the caller's bearer token
// attached. A test that asserts the escaped id against url.PathEscape is a tautology
// and cannot see this. This test asserts the RESULTING PATH instead.
//
// The fix is a rule, not an escape: a repo id must be exactly one safe path segment.
func TestRPCRejectsRepoIDThatIsNotOneSafeSegment(t *testing.T) {
	hostile := map[string]string{
		"empty":              "",
		"blank":              "   ",
		"dot":                ".",
		"dotdot":             "..",
		"dotdot padded":      " .. ",
		"triple dot segment": "...",
		"slash":              "a/b",
		"leading slash":      "/admin",
		"trailing slash":     "repo/",
		"backslash":          `a\b`,
		"encoded slash":      "..%2f..",
		"query":              "x?admin=1",
		"fragment":           "x#frag",
		"space":              "repo 1",
		"percent":            "repo%41",
		"nul":                "repo\x00",
		"newline":            "repo\n",
		"traversal chain":    "../../admin",
	}
	for name, repoID := range hostile {
		t.Run(name, func(t *testing.T) {
			var sent int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sent++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL, Token: "secret"}
			_, _, err := c.Initialize(context.Background(), repoID)
			if err == nil {
				t.Fatalf("repo id %q was accepted; want a refusal before any request", repoID)
			}
			if !errors.Is(err, repoid.ErrInvalid) {
				t.Errorf("repo id %q: error = %v; want it to wrap repoid.ErrInvalid", repoID, err)
			}
			if sent != 0 {
				t.Errorf("repo id %q: %d request(s) reached the server; want the bearer token to never leave the process", repoID, sent)
			}
		})
	}
}

// TestRPCAcceptedRepoIDLandsOnTheIntendedPath pins the whole target for ids that are
// allowed through: the api prefix and the /brain/mcp suffix survive, the id occupies
// exactly one segment, nothing leaks into the query, and — the part that matters —
// the path is already in normal form, so no downstream normalizer can move it.
func TestRPCAcceptedRepoIDLandsOnTheIntendedPath(t *testing.T) {
	for _, repoID := range []string{
		"01JABCDEFGHJKMNPQRSTVWXYZ0",
		"repo-123",
		"a.b.c",
		"repo_1~x",
	} {
		t.Run(repoID, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"test"}}}`)
			}))
			defer srv.Close()

			c := &Client{BaseURL: srv.URL}
			if _, _, err := c.Initialize(context.Background(), repoID); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			want := "/api/v1/repos/" + repoID + "/brain/mcp"
			if gotPath != want {
				t.Fatalf("request path = %q, want %q", gotPath, want)
			}
			if gotQuery != "" {
				t.Errorf("query string = %q, want empty", gotQuery)
			}
			// Normal form: dot-segment removal is a no-op, so no hop can rewrite it.
			if cleaned := path.Clean(gotPath); cleaned != gotPath {
				t.Errorf("path %q is not in normal form: a normalizing hop rewrites it to %q", gotPath, cleaned)
			}
		})
	}
}

// TestRPCCannotReachAnotherRoute is the end-to-end consequence, served by a real
// http.ServeMux — which performs the RFC 3986 dot-segment cleanup that the raw
// handler above does not. Without the segment rule, repo id ".." lands on the
// /api/v1/brain/ route; with it, nothing is sent at all.
func TestRPCCannotReachAnotherRoute(t *testing.T) {
	reached := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "repos:" + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"test"}}}`)
	})
	// The sibling route a traversal lands on once "/repos/../" is normalized away.
	mux.HandleFunc("/api/v1/brain/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "elsewhere:" + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "secret"}
	if _, _, err := c.Initialize(context.Background(), ".."); err == nil {
		t.Fatal("repo id \"..\" was accepted")
	}
	close(reached)
	for hit := range reached {
		if strings.HasPrefix(hit, "elsewhere:") {
			t.Fatalf("repo id \"..\" traversed out of the repos prefix and reached %s", hit)
		}
		t.Fatalf("repo id \"..\" produced a request at all: %s", hit)
	}
}
