package factsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

// Two proposal endpoints address a proposal by id in the REQUEST TARGET:
//
//	GET  /api/v1/repos/{repo}/brain/facts/proposals/{id}
//	POST /api/v1/repos/{repo}/brain/facts/proposals/{id}/resolve
//
// newRequest then attaches the member's bearer token, and the resolve body carries
// the member's whole fact set. url.PathEscape was standing in for a check it cannot
// perform: "." and ".." are RFC 3986 unreserved, so PathEscape leaves them untouched
// and ".../proposals/.." reaches the wire, where a dot-segment-normalizing hop
// collapses it onto ".../facts" — the fact-set head route — and ".../proposals/../
// resolve" onto ".../facts/resolve".
//
// bindProposalID (the proposal-id binding this builds on) constrains the id a
// RESPONSE may carry. It cannot constrain the id a REQUEST is built from: that path
// is assembled and sent before any response exists, and ResolveProposal never reads a
// proposal back at all. So the id has to be checked before the target is built.
//
// Every test below asserts the RESULTING REQUEST — the path that reached the server,
// or that no request was made at all. None asserts the escaping function against
// itself.

// hostileProposalIDs: each one either adds a path segment, walks off the collection
// prefix, would be rewritten on its way out, or is simply not an id ProposalID could
// have derived.
var hostileProposalIDs = map[string]string{
	"empty":             "",
	"blank":             "   ",
	"dot":               ".",
	"dotdot":            "..",
	"dotdot padded":     " .. ",
	"triple dot":        "...",
	"traversal chain":   "../../admin",
	"bare prefix":       "prop-",
	"prefix plus dots":  "prop-..",
	"wrong prefix":      "facts",
	"no prefix":         "deadbeefdeadbeef",
	"too short":         "prop-deadbeef",
	"too long":          "prop-deadbeefdeadbeef0",
	"uppercase hex":     "prop-DEADBEEFDEADBEEF",
	"non hex":           "prop-deadbeefdeadbeeg",
	"slash":             "prop-deadbeefdeadbe/f",
	"leading slash":     "/admin",
	"backslash":         `prop-deadbeefdeadb\ef`,
	"encoded slash":     "..%2f..",
	"walk to sibling":   "prop-deadbeefdeadbeef/../../facts",
	"query":             "prop-deadbeefdeadbe?x",
	"fragment":          "prop-deadbeefdeadbe#f",
	"space":             "prop-deadbeef deadbe",
	"trailing newline":  "prop-deadbeefdeadbeef\n",
	"percent encoded":   "prop-%2e%2e%2e%2e%2e%2e",
	"unicode homoglyph": "prop-deadbeefdeadbeeｆ",
}

// validProposalID is well formed by construction: it is what ProposalID derives.
var validProposalID = ProposalID(factmerge.Proposal{
	Action:      "supersede",
	CandidateID: "fact:candidate",
	TargetID:    "fact:target",
	Branch:      "main",
	ProposedBy:  "member-a",
})

// proposalIDCountingServer answers every request with body and counts arrivals.
func proposalIDCountingServer(t *testing.T, body string) (*httptest.Server, *int) {
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

// TestProposalIDRuleIsTheDerivationRule ties the accepted set to the derived set. If
// ProposalID's shape ever changes, this fails rather than the rule silently rejecting
// every real id — the failure mode a hand-copied format check invites.
func TestProposalIDRuleIsTheDerivationRule(t *testing.T) {
	t.Parallel()
	proposals := []factmerge.Proposal{
		{},
		{Action: "merge", CandidateID: "fact:a", TargetID: "fact:b", Branch: "main", ProposedBy: "m1"},
		{Action: "supersede", CandidateID: "fact:../..", TargetID: "fact:b", Branch: "feature/x", ProposedBy: "m2"},
		{Action: "..", CandidateID: "..", TargetID: "..", Branch: "..", ProposedBy: ".."},
		{Action: "merge", CandidateID: "fact:ünïcøde", TargetID: "fact:%2f", Branch: "a b", ProposedBy: "m 3"},
	}
	for i, p := range proposals {
		id := ProposalID(p)
		if err := validateProposalID(id); err != nil {
			t.Errorf("proposal %d: validateProposalID(ProposalID(p)) = %v; the rule must accept exactly what the derivation produces", i, err)
		}
		// One segment, in normal form: what makes raw concatenation safe. Asserted by
		// joining, not by comparing the id to an escape of itself.
		if joined := path.Join("/collection", id); joined != "/collection/"+id {
			t.Errorf("proposal %d: derived id %q is not one normal-form segment (joins to %q)", i, id, joined)
		}
	}
}

// TestProposalIDMustBeDerivable covers BOTH id-bearing endpoints at once: a hostile id
// is refused before the request exists, so the bearer token — and, for resolve, the
// member's whole fact set — never leaves the process.
func TestProposalIDMustBeDerivable(t *testing.T) {
	calls := map[string]func(context.Context, *HTTPServer, string) error{
		"GetProposal": func(ctx context.Context, h *HTTPServer, id string) error {
			_, err := h.GetProposal(ctx, "repo1", "main", id)
			return err
		},
		"ResolveProposal": func(ctx context.Context, h *HTTPServer, id string) error {
			_, err := h.ResolveProposal(ctx, ResolveProposalRequest{
				RepoID: "repo1", Branch: "main", ProposalID: id,
				Decision: Reject, FactsUnchanged: true,
			})
			return err
		},
	}
	for method, call := range calls {
		for name, proposalID := range hostileProposalIDs {
			t.Run(method+"/"+name, func(t *testing.T) {
				srv, sent := proposalIDCountingServer(t, `{"found":true,"proposal":{"id":"","proposal":{}},"changed":true}`)
				h := &HTTPServer{BaseURL: srv.URL, Token: "secret"}
				err := call(context.Background(), h, proposalID)
				if err == nil {
					t.Fatalf("%s accepted proposal id %q; want a refusal before any request", method, proposalID)
				}
				if !errors.Is(err, ErrInvalidProposalID) {
					t.Errorf("%s proposal id %q: error = %v; want it to wrap ErrInvalidProposalID", method, proposalID, err)
				}
				if *sent != 0 {
					t.Errorf("%s proposal id %q: %d request(s) reached the server; want the token and fact set to never leave the process", method, proposalID, *sent)
				}
			})
		}
	}
}

// TestProposalIDLandsOnItsOwnSegment pins the two targets a valid id produces: the id
// occupies exactly one segment, the branch stays in the query, and the path is already
// in normal form — the property that makes it un-rewritable by any hop in between.
func TestProposalIDLandsOnItsOwnSegment(t *testing.T) {
	const repoID = "repo1"
	base := "/api/v1/repos/" + repoID + "/brain/facts/proposals"

	cases := map[string]struct {
		body      string
		call      func(context.Context, *HTTPServer) error
		wantPath  string
		wantQuery string
	}{
		"GetProposal": {
			// found=false is a normal outcome (already settled); the path is the assertion.
			body: `{"found":false}`,
			call: func(ctx context.Context, h *HTTPServer) error {
				_, err := h.GetProposal(ctx, repoID, "feature/x", validProposalID)
				if errors.Is(err, ErrProposalNotFound) {
					return nil
				}
				return err
			},
			wantPath:  base + "/" + validProposalID,
			wantQuery: "branch=feature%2Fx",
		},
		"ResolveProposal": {
			body: `{"factsRef":"f2","proposalsRef":"p2","changed":true}`,
			call: func(ctx context.Context, h *HTTPServer) error {
				_, err := h.ResolveProposal(ctx, ResolveProposalRequest{
					RepoID: repoID, Branch: "feature/x", ProposalID: validProposalID,
					Decision: Reject, FactsUnchanged: true,
				})
				return err
			},
			wantPath: base + "/" + validProposalID + "/resolve",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var gotPath, gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			h := &HTTPServer{BaseURL: srv.URL}
			if err := tc.call(context.Background(), h); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			assertProposalPath(t, gotPath, tc.wantPath)
			if gotQuery != tc.wantQuery {
				t.Errorf("query = %q, want %q", gotQuery, tc.wantQuery)
			}
		})
	}
}

// TestProposalIDCannotReachAnotherRoute is the end-to-end consequence, served by a
// real http.ServeMux — which performs the dot-segment cleanup a bare handler does not.
// Two handlers exist that no proposal endpoint addresses; reaching either one means
// the id moved the target.
func TestProposalIDCannotReachAnotherRoute(t *testing.T) {
	const repoID = "repo1"
	facts := "/api/v1/repos/" + repoID + "/brain/facts"

	var mu sync.Mutex
	var reached []string
	record := func(label string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			reached = append(reached, label+":"+r.URL.Path)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"found":false,"changed":true}`)
		}
	}
	drain := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := reached
		reached = nil
		return out
	}

	mux := http.NewServeMux()
	mux.HandleFunc(facts+"/proposals/", record("proposals"))
	// The two routes a dot segment in the id collapses onto. The first is the real
	// fact-set head — the target of Current, read with the same bearer token.
	mux.HandleFunc(facts, record("ELSEWHERE-head"))
	mux.HandleFunc(facts+"/resolve", record("ELSEWHERE-resolve"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The hazard is not theoretical: net/http's own ServeMux cleans the dot segment
	// and redirects, and any client following that redirect lands on the head route.
	// This subtest exercises the raw target the client would have built.
	t.Run("the collapse is real", func(t *testing.T) {
		for target, want := range map[string]string{
			facts + "/proposals/..":         "ELSEWHERE-head:" + facts,
			facts + "/proposals/../resolve": "ELSEWHERE-resolve:" + facts + "/resolve",
		} {
			resp, err := srv.Client().Get(srv.URL + target)
			if err != nil {
				t.Fatalf("GET %s: %v", target, err)
			}
			resp.Body.Close()
			got := drain()
			if len(got) != 1 || got[0] != want {
				t.Fatalf("GET %s reached %v, want [%s] — if this stops holding the demonstration below is vacuous", target, got, want)
			}
		}
	})

	// And the client refuses to build it.
	t.Run("the client refuses", func(t *testing.T) {
		h := &HTTPServer{BaseURL: srv.URL, Token: "secret"}
		if _, err := h.GetProposal(context.Background(), repoID, "main", ".."); err == nil {
			t.Error(`proposal id ".." was accepted by GetProposal`)
		}
		if _, err := h.ResolveProposal(context.Background(), ResolveProposalRequest{
			RepoID: repoID, Branch: "main", ProposalID: "..",
			Decision: Reject, FactsUnchanged: true,
		}); err == nil {
			t.Error(`proposal id ".." was accepted by ResolveProposal`)
		}
		for _, hit := range drain() {
			if strings.HasPrefix(hit, "ELSEWHERE-") {
				t.Fatalf(`proposal id ".." moved the target and reached %s with the member's bearer token`, hit)
			}
			t.Fatalf(`proposal id ".." produced a request at all: %s`, hit)
		}
	})
}

// assertProposalPath checks the exact target AND that it is in normal form.
func assertProposalPath(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("request path = %q, want %q", got, want)
	}
	if cleaned := path.Clean(got); cleaned != got {
		t.Errorf("path %q is not in normal form: a normalizing hop rewrites it to %q", got, cleaned)
	}
}
