package factsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A live round-trip against entire-api showed the drift these tests close: the server
// answers a violated cap with an actionable body —
//
//	400 {"title":"Bad Request","status":400,"detail":"branch is 600 bytes, limit is 512"}
//
// — and the client threw the body away, leaving the member with "unexpected status 400
// Bad Request": no statement of what was wrong and no statement of the limit.
//
// The other half of the fix is that improving the MESSAGE must not disturb the
// MEANING. Every status this transport reads is load-bearing control flow — 412/409
// is the CAS-loss signal the retry loops in sync.go and proposals.go key on, and
// 404/501/503 on the proposal endpoints is "this deployment has no queue", which
// degrades to the local queue instead of failing a sync whose facts already landed.
// So each surfacing test below has a sentinel test beside it, and the sentinel tests
// assert errors.Is, not the string.

// serverDetail is the sentence the hosted API returns for the cap that exposed this.
const serverDetail = "branch is 600 bytes, limit is 512"

const humaCapBody = `{"title":"Bad Request","status":400,"detail":"` + serverDetail + `"}`

// statusServer answers every request with one status and one body.
func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// factsyncCalls is every method on HTTPServer that reads a status, keyed by name.
// Each returns only the error, because that is the whole of what a member sees when
// the server refuses.
var factsyncCalls = map[string]func(context.Context, *HTTPServer) error{
	"Current": func(ctx context.Context, h *HTTPServer) error {
		_, _, _, err := h.Current(ctx, "repo", "main")
		return err
	},
	"Advance": func(ctx context.Context, h *HTTPServer) error {
		_, err := h.Advance(ctx, "repo", "main", "old", []byte("fact:a\n"))
		return err
	},
	"ListProposals": func(ctx context.Context, h *HTTPServer) error {
		_, err := h.ListProposals(ctx, "repo", "main")
		return err
	},
	"GetProposal": func(ctx context.Context, h *HTTPServer) error {
		_, err := h.GetProposal(ctx, "repo", "main", "p1")
		return err
	},
	"PublishProposals": func(ctx context.Context, h *HTTPServer) error {
		_, err := h.PublishProposals(ctx, "repo", "main", "old", nil)
		return err
	},
	"ResolveProposal": func(ctx context.Context, h *HTTPServer) error {
		_, err := h.ResolveProposal(ctx, ResolveProposalRequest{
			RepoID: "repo", Branch: "main", ProposalID: "p1",
			Decision: Reject, FactsUnchanged: true,
		})
		return err
	},
}

// TestEveryFactsyncCallSurfacesTheServersObjection is the regression for the drift:
// on a 400 the member must read the server's own sentence, not just the status line.
func TestEveryFactsyncCallSurfacesTheServersObjection(t *testing.T) {
	for name, call := range factsyncCalls {
		t.Run(name, func(t *testing.T) {
			srv := statusServer(t, http.StatusBadRequest, humaCapBody)
			h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
			err := call(context.Background(), h)
			if err == nil {
				t.Fatalf("%s: a 400 produced no error", name)
			}
			if !strings.Contains(err.Error(), serverDetail) {
				t.Fatalf("%s: error = %q; want it to carry the server's own explanation %q", name, err, serverDetail)
			}
		})
	}
}

// A 5xx is read the same way — the surfacing is keyed on "not the success status",
// not on a hand-listed set of 4xx codes.
func TestFactsyncSurfacesA5xxObjection(t *testing.T) {
	for name, call := range factsyncCalls {
		t.Run(name, func(t *testing.T) {
			srv := statusServer(t, http.StatusInternalServerError, `{"title":"Internal Server Error","detail":"fact-set store is read-only"}`)
			h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
			err := call(context.Background(), h)
			if err == nil || !strings.Contains(err.Error(), "fact-set store is read-only") {
				t.Fatalf("%s: error = %v; want the server's 500 explanation", name, err)
			}
		})
	}
}

// An ingress in front of entire-api answers with an HTML page, not JSON. The member
// must get its title, and the whole error must stay short enough to read.
func TestFactsyncNonJSONErrorPageIsBoundedAndReadable(t *testing.T) {
	page := "<!DOCTYPE html>\n<html><head><title>504 Gateway Time-out</title></head><body>\n" +
		strings.Repeat("<div>x</div>\n", 200000) + "</body></html>\n"
	for name, call := range factsyncCalls {
		t.Run(name, func(t *testing.T) {
			srv := statusServer(t, http.StatusGatewayTimeout, page)
			h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
			err := call(context.Background(), h)
			if err == nil {
				t.Fatalf("%s: a 504 produced no error", name)
			}
			if !strings.Contains(err.Error(), "504 Gateway Time-out") {
				t.Fatalf("%s: error = %q; want the page title", name, err)
			}
			if len(err.Error()) > 1024 {
				t.Fatalf("%s: error is %d bytes; the HTML page was dumped at the member", name, len(err.Error()))
			}
			if strings.Contains(err.Error(), "<div>") {
				t.Fatalf("%s: error = %q; raw markup reached the member", name, err)
			}
		})
	}
}

// SEMANTIC PRESERVATION 1/3 — 412 and 409 stay the CAS-loss signal.
//
// sync.go and proposals.go re-read and re-merge on errors.Is(err, ErrConflict). If the
// surfacing replaced that sentinel with a plain formatted error, a lost race would
// stop looking like a lost race and a converging sync would start failing instead.
func TestConflictStatusesStayErrConflict(t *testing.T) {
	for _, status := range []int{http.StatusPreconditionFailed, http.StatusConflict} {
		for _, body := range []string{"", `{"title":"Conflict","detail":"head moved to facts-abc"}`} {
			name := fmt.Sprintf("%d/body=%d", status, len(body))
			t.Run(name, func(t *testing.T) {
				srv := statusServer(t, status, body)
				h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}

				if _, err := h.Advance(context.Background(), "repo", "main", "old", []byte("fact:a\n")); !errors.Is(err, ErrConflict) {
					t.Fatalf("Advance(%d) = %v; want ErrConflict", status, err)
				}
				if _, err := h.PublishProposals(context.Background(), "repo", "main", "old", nil); !errors.Is(err, ErrConflict) {
					t.Fatalf("PublishProposals(%d) = %v; want ErrConflict", status, err)
				}
				_, err := h.ResolveProposal(context.Background(), ResolveProposalRequest{
					RepoID: "repo", Branch: "main", ProposalID: "p1", Decision: Reject, FactsUnchanged: true,
				})
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("ResolveProposal(%d) = %v; want ErrConflict", status, err)
				}
				if body != "" && !strings.Contains(err.Error(), "head moved to facts-abc") {
					t.Fatalf("ResolveProposal(%d) = %q; the server's explanation was dropped", status, err)
				}
			})
		}
	}
}

// SEMANTIC PRESERVATION 2/3 — 404/501/503 on the proposal endpoints stay
// "this deployment has no queue", which facts_sync_cmd.go degrades on rather than
// failing a sync whose head-advance already succeeded.
func TestQueueUnsupportedStatusesStayUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := statusServer(t, status, `{"title":"Not Found","detail":"hosted brain is disabled for this repo"}`)
			h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}

			_, listErr := h.ListProposals(context.Background(), "repo", "main")
			if !errors.Is(listErr, ErrProposalQueueUnsupported) {
				t.Fatalf("ListProposals(%d) = %v; want ErrProposalQueueUnsupported", status, listErr)
			}
			if !strings.Contains(listErr.Error(), "hosted brain is disabled for this repo") {
				t.Fatalf("ListProposals(%d) = %q; the server's explanation was dropped", status, listErr)
			}
			_, pubErr := h.PublishProposals(context.Background(), "repo", "main", "old", nil)
			if !errors.Is(pubErr, ErrProposalQueueUnsupported) {
				t.Fatalf("PublishProposals(%d) = %v; want ErrProposalQueueUnsupported", status, pubErr)
			}
			if !strings.Contains(pubErr.Error(), "hosted brain is disabled for this repo") {
				t.Fatalf("PublishProposals(%d) = %q; the server's explanation was dropped", status, pubErr)
			}
		})
	}
}

// SEMANTIC PRESERVATION 3/3 — a 404 on a single proposal stays "already settled",
// which is a normal outcome the resolve loop skips past, not a failure.
func TestProposalNotFoundStaysNotFound(t *testing.T) {
	srv := statusServer(t, http.StatusNotFound, `{"title":"Not Found","detail":"proposal p1 was resolved by another member"}`)
	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}

	_, getErr := h.GetProposal(context.Background(), "repo", "main", "p1")
	if !errors.Is(getErr, ErrProposalNotFound) {
		t.Fatalf("GetProposal(404) = %v; want ErrProposalNotFound", getErr)
	}
	if !strings.Contains(getErr.Error(), "resolved by another member") {
		t.Fatalf("GetProposal(404) = %q; the server's explanation was dropped", getErr)
	}
	_, resErr := h.ResolveProposal(context.Background(), ResolveProposalRequest{
		RepoID: "repo", Branch: "main", ProposalID: "p1", Decision: Reject, FactsUnchanged: true,
	})
	if !errors.Is(resErr, ErrProposalNotFound) {
		t.Fatalf("ResolveProposal(404) = %v; want ErrProposalNotFound", resErr)
	}
	if !strings.Contains(resErr.Error(), "resolved by another member") {
		t.Fatalf("ResolveProposal(404) = %q; the server's explanation was dropped", resErr)
	}
}

// SEMANTIC PRESERVATION, transport half — the dial-retry classifier reads a transport
// error, which exists BEFORE any response does. Reading error bodies must leave it
// untouched: a local port-table blip is still retried, and a refused connection is
// still not.
func TestErrorBodyReadingLeavesTheDialClassifierAlone(t *testing.T) {
	ctx := context.Background()
	ts := statusServer(t, http.StatusOK, `{"found":false}`)
	flaky := &flakyDialTransport{failFirst: transientDialRetries, real: http.DefaultTransport}
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok", Client: &http.Client{Transport: flaky}}
	if _, _, _, err := h.Current(ctx, "repo", "main"); err != nil {
		t.Fatalf("Current with transient dial failures = %v; want the retry to still absorb them", err)
	}
	if got, want := flaky.callCount(), transientDialRetries+1; got != want {
		t.Fatalf("round trips = %d, want %d", got, want)
	}

	refused := &nonTransientDialTransport{}
	h2 := &HTTPServer{BaseURL: "http://127.0.0.1:1", Token: "tok", Client: &http.Client{Transport: refused}}
	if _, _, _, err := h2.Current(ctx, "repo", "main"); err == nil {
		t.Fatal("Current succeeded against an always-refusing transport")
	}
	if refused.calls != 1 {
		t.Fatalf("round trips = %d, want exactly 1 (a real connectivity error is not retried)", refused.calls)
	}
}

// A refusal with nothing to add must not grow a dangling separator.
func TestSilentRefusalKeepsACleanMessage(t *testing.T) {
	srv := statusServer(t, http.StatusBadRequest, "")
	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
	_, err := h.Advance(context.Background(), "repo", "main", "old", []byte("fact:a\n"))
	if err == nil {
		t.Fatal("Advance(400, empty body) produced no error")
	}
	if strings.HasSuffix(err.Error(), ": ") || strings.HasSuffix(err.Error(), ":") {
		t.Fatalf("Advance(400, empty body) = %q; an empty body left a dangling separator", err)
	}
}
