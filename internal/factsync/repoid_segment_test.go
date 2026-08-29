package factsync

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

// The fact-set and proposal transports interpolate the repo id into the request
// target by concatenation, then newRequest attaches the member's bearer token — and,
// on the write paths, the member's whole fact set. url.PathEscape was standing in for
// a check it cannot perform: "." and ".." are RFC 3986 unreserved, so PathEscape
// leaves them untouched and
//
//	/api/v1/repos/../brain/facts
//
// reaches the wire, where any dot-segment-normalizing hop collapses it to
// /api/v1/brain/facts — a route the caller never asked for.
//
// Every test below asserts the RESULTING REQUEST (its path, or that no request was
// made at all). None of them asserts the escaping function against itself, which is
// the tautology that let this survive the first fix.

// hostileRepoIDs is the shared set: each one either adds a path segment, walks off
// the /repos/ prefix, or would be silently rewritten on its way out.
var hostileRepoIDs = map[string]string{
	"empty":           "",
	"blank":           "   ",
	"dot":             ".",
	"dotdot":          "..",
	"dotdot padded":   " .. ",
	"triple dot":      "...",
	"slash":           "a/b",
	"leading slash":   "/admin",
	"backslash":       `a\b`,
	"encoded slash":   "..%2f..",
	"query":           "x?admin=1",
	"fragment":        "x#frag",
	"space":           "repo 1",
	"traversal chain": "../../admin",
}

// repoIDCountingServer answers every request with body and records how many arrived.
func repoIDCountingServer(t *testing.T, body string) (*httptest.Server, *int) {
	t.Helper()
	var sent int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &sent
}

// repoIDPathRecordingServer records the escaped path and raw query of the last request.
func repoIDPathRecordingServer(t *testing.T, body string, gotPath, gotQuery *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath, *gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFactsyncRepoIDMustBeOneSafeSegment covers EVERY repo-id-bearing endpoint on
// HTTPServer at once: a hostile id must be refused before the request exists, so the
// bearer token and (for the write paths) the fact set never leave the process.
func TestFactsyncRepoIDMustBeOneSafeSegment(t *testing.T) {
	calls := map[string]func(context.Context, *HTTPServer, string) error{
		"Current": func(ctx context.Context, h *HTTPServer, id string) error {
			_, _, _, err := h.Current(ctx, id, "main")
			return err
		},
		"Advance": func(ctx context.Context, h *HTTPServer, id string) error {
			_, err := h.Advance(ctx, id, "main", "old", []byte("{}\n"))
			return err
		},
		"ListProposals": func(ctx context.Context, h *HTTPServer, id string) error {
			_, err := h.ListProposals(ctx, id, "main")
			return err
		},
		"GetProposal": func(ctx context.Context, h *HTTPServer, id string) error {
			_, err := h.GetProposal(ctx, id, "main", "p1")
			return err
		},
		"PublishProposals": func(ctx context.Context, h *HTTPServer, id string) error {
			_, err := h.PublishProposals(ctx, id, "main", "old", nil)
			return err
		},
		"ResolveProposal": func(ctx context.Context, h *HTTPServer, id string) error {
			_, err := h.ResolveProposal(ctx, ResolveProposalRequest{
				RepoID: id, Branch: "main", ProposalID: "p1",
				Decision: Reject, FactsUnchanged: true,
			})
			return err
		},
	}
	for method, call := range calls {
		for name, repoID := range hostileRepoIDs {
			t.Run(method+"/"+name, func(t *testing.T) {
				srv, sent := repoIDCountingServer(t, `{"found":false,"proposals":[],"changed":true}`)
				h := &HTTPServer{BaseURL: srv.URL, Token: "secret"}
				err := call(context.Background(), h, repoID)
				if err == nil {
					t.Fatalf("%s accepted repo id %q; want a refusal before any request", method, repoID)
				}
				if !errors.Is(err, repoid.ErrInvalid) {
					t.Errorf("%s repo id %q: error = %v; want it to wrap repoid.ErrInvalid", method, repoID, err)
				}
				if *sent != 0 {
					t.Errorf("%s repo id %q: %d request(s) reached the server; want the bearer token to never leave the process", method, repoID, *sent)
				}
			})
		}
	}
}

// TestCurrentLandsOnTheIntendedPath pins the whole target of the fact-set head read:
// the id occupies exactly one segment, the branch stays in the query, and the path is
// already in normal form so no hop can move it.
func TestCurrentLandsOnTheIntendedPath(t *testing.T) {
	const repoID = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	var gotPath, gotQuery string
	srv := repoIDPathRecordingServer(t, `{"found":false}`, &gotPath, &gotQuery)

	h := &HTTPServer{BaseURL: srv.URL}
	if _, _, _, err := h.Current(context.Background(), repoID, "feature/x"); err != nil {
		t.Fatalf("Current: %v", err)
	}
	assertSegmentPath(t, gotPath, "/api/v1/repos/"+repoID+"/brain/facts")
	if gotQuery != "branch=feature%2Fx" {
		t.Errorf("query = %q, want the branch escaped into the query", gotQuery)
	}
}

// TestAdvanceLandsOnTheIntendedPath does the same for the compare-and-swap write —
// the request that carries the merged fact set.
func TestAdvanceLandsOnTheIntendedPath(t *testing.T) {
	const repoID = "repo_1~x"
	var gotPath, gotQuery string
	srv := repoIDPathRecordingServer(t, `{"newRef":"r2","changed":true}`, &gotPath, &gotQuery)

	h := &HTTPServer{BaseURL: srv.URL}
	if _, err := h.Advance(context.Background(), repoID, "main", "r1", []byte("{}\n")); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	assertSegmentPath(t, gotPath, "/api/v1/repos/"+repoID+"/brain/facts/advance")
	if gotQuery != "" {
		t.Errorf("query = %q, want empty", gotQuery)
	}
}

// TestProposalEndpointsLandOnTheIntendedPath pins the four proposal targets. The
// collection path is shared, so this is where a change to proposalsPath would show up
// as a moved route rather than as a refused id.
func TestProposalEndpointsLandOnTheIntendedPath(t *testing.T) {
	const repoID = "a.b.c"
	base := "/api/v1/repos/" + repoID + "/brain/facts/proposals"

	cases := map[string]struct {
		body string
		call func(context.Context, *HTTPServer) error
		want string
	}{
		"ListProposals": {
			body: `{"found":false,"proposals":[]}`,
			call: func(ctx context.Context, h *HTTPServer) error {
				_, err := h.ListProposals(ctx, repoID, "main")
				return err
			},
			want: base,
		},
		"GetProposal": {
			// 404 is a normal outcome (already settled); the path is the assertion.
			body: `{"found":false}`,
			call: func(ctx context.Context, h *HTTPServer) error {
				_, err := h.GetProposal(ctx, repoID, "main", "p1")
				if errors.Is(err, ErrProposalNotFound) {
					return nil
				}
				return err
			},
			want: base + "/p1",
		},
		"PublishProposals": {
			body: `{"ref":"r2","changed":true}`,
			call: func(ctx context.Context, h *HTTPServer) error {
				_, err := h.PublishProposals(ctx, repoID, "main", "r1", nil)
				return err
			},
			want: base,
		},
		"ResolveProposal": {
			body: `{"factsRef":"f2","proposalsRef":"p2","changed":true}`,
			call: func(ctx context.Context, h *HTTPServer) error {
				_, err := h.ResolveProposal(ctx, ResolveProposalRequest{
					RepoID: repoID, Branch: "main", ProposalID: "p1",
					Decision: Reject, FactsUnchanged: true,
				})
				return err
			},
			want: base + "/p1/resolve",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := repoIDPathRecordingServer(t, tc.body, &gotPath, &gotQuery)
			h := &HTTPServer{BaseURL: srv.URL}
			if err := tc.call(context.Background(), h); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			assertSegmentPath(t, gotPath, tc.want)
		})
	}
}

// TestFactsyncRepoIDCannotReachAnotherRoute is the end-to-end consequence, served by
// a real http.ServeMux — which performs the dot-segment cleanup a bare handler does
// not. Without the segment rule, repo id ".." lands on the sibling /api/v1/brain/
// route with the member's bearer token attached; with it, nothing is sent at all.
func TestFactsyncRepoIDCannotReachAnotherRoute(t *testing.T) {
	reached := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "repos:" + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"found":false}`)
	})
	mux.HandleFunc("/api/v1/brain/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "elsewhere:" + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"found":false}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "secret"}
	if _, _, _, err := h.Current(context.Background(), "..", "main"); err == nil {
		t.Fatal(`repo id ".." was accepted by Current`)
	}
	if _, err := h.Advance(context.Background(), "..", "main", "r1", []byte("{}\n")); err == nil {
		t.Fatal(`repo id ".." was accepted by Advance`)
	}
	if _, err := h.ListProposals(context.Background(), "..", "main"); err == nil {
		t.Fatal(`repo id ".." was accepted by ListProposals`)
	}
	close(reached)
	for hit := range reached {
		if strings.HasPrefix(hit, "elsewhere:") {
			t.Fatalf(`repo id ".." traversed out of the repos prefix and reached %s`, hit)
		}
		t.Fatalf(`repo id ".." produced a request at all: %s`, hit)
	}
}

// assertSegmentPath checks the exact target AND that it is in normal form, which is
// the property that makes it un-rewritable by any hop between here and the handler.
func assertSegmentPath(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("request path = %q, want %q", got, want)
	}
	if cleaned := path.Clean(got); cleaned != got {
		t.Errorf("path %q is not in normal form: a normalizing hop rewrites it to %q", got, cleaned)
	}
}
