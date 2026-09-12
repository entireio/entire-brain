package factsync

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// compile-time: HTTPServer satisfies the Server seam Sync/Resolve are written against.
var _ Server = (*HTTPServer)(nil)

// contractServer stands up an httptest server that speaks entire-api's fact-set sync
// wire contract (brain_facts.go) backed by an in-memory fakeServer. It lets the REAL
// HTTPServer adapter be exercised over a real HTTP round-trip — JSON encoding, base64
// data, query/body branch, and the success/no-op/conflict/bad-request status
// mapping — validating the adapter against the documented contract. (End-to-end
// validation against the live entire-api handler is the deploy-time step; the two
// Go modules can't share types.)
func contractServer(t *testing.T, fake *fakeServer) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/brain/facts"):
			ref, blob, found, _ := fake.Current(ctx, "repo", r.URL.Query().Get("branch"))
			_ = json.NewEncoder(w).Encode(map[string]any{"found": found, "ref": ref, "version": 1, "data": blob})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/brain/facts/advance"):
			var body struct {
				Branch string `json:"branch"`
				OldRef string `json:"oldRef"`
				Data   []byte `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(body.Data) == 0 { // brain_facts.go: empty data → 400
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			newRef, err := fake.Advance(ctx, "repo", body.Branch, body.OldRef, body.Data)
			switch {
			case err == ErrNoChange:
				cur, _, _, _ := fake.Current(ctx, "repo", body.Branch)
				// no-op: changed omitted (false), mirroring the real server.
				_ = json.NewEncoder(w).Encode(map[string]any{"newRef": cur, "version": 1})
			case err == ErrConflict:
				w.WriteHeader(http.StatusPreconditionFailed)
			case err != nil:
				w.WriteHeader(http.StatusBadRequest)
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"newRef": newRef, "version": 1, "changed": true})
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestHTTPServerContract exercises the adapter's every status→sentinel mapping over a real
// HTTP round-trip: empty-head read, create, read-back, no-op (ErrNoChange), and
// stale-old-ref (412/409→ErrConflict).
func TestHTTPServerContract(t *testing.T) {
	ctx := context.Background()
	ts := contractServer(t, &fakeServer{})
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "test-token"}

	// Empty head.
	if ref, blob, found, err := h.Current(ctx, "repo", "main"); err != nil || found || ref != "" || blob != nil {
		t.Fatalf("Current(empty) = %q,%v,%v,%v", ref, blob, found, err)
	}
	// Create.
	ref1, err := h.Advance(ctx, "repo", "main", "", []byte("fact:a\n"))
	if err != nil || ref1 != contentRef([]byte("fact:a\n")) {
		t.Fatalf("Advance(create) = %q, %v", ref1, err)
	}
	// Read back the decrypted blob + ref over the wire (base64 round-trip).
	ref, blob, found, err := h.Current(ctx, "repo", "main")
	if err != nil || !found || ref != ref1 || string(blob) != "fact:a\n" {
		t.Fatalf("Current(after create) = %q,%q,%v,%v", ref, blob, found, err)
	}
	// No-op (identical content, current ref) → ErrNoChange.
	if _, err := h.Advance(ctx, "repo", "main", ref1, []byte("fact:a\n")); err != ErrNoChange {
		t.Fatalf("Advance(no-op) = %v; want ErrNoChange", err)
	}
	// Stale oldRef → ErrConflict.
	if _, err := h.Advance(ctx, "repo", "main", "facts-stale", []byte("fact:a\nfact:b\n")); err != ErrConflict {
		t.Fatalf("Advance(stale) = %v; want ErrConflict", err)
	}
}

func TestHTTPServerLegacyAdvanceWireCompatibility(t *testing.T) {
	ctx := context.Background()
	fake := &fakeServer{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/brain/facts/advance") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Branch string `json:"branch"`
			OldRef string `json:"old_ref"`
			Data   []byte `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		newRef, err := fake.Advance(ctx, "repo", body.Branch, body.OldRef, body.Data)
		switch {
		case err == ErrNoChange:
			cur, _, _, _ := fake.Current(ctx, "repo", body.Branch)
			_ = json.NewEncoder(w).Encode(map[string]any{"new_ref": cur, "version": 1, "unchanged": true})
		case err == ErrConflict:
			w.WriteHeader(http.StatusConflict)
		case err != nil:
			w.WriteHeader(http.StatusBadRequest)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"new_ref": newRef, "version": 1})
		}
	}))
	defer ts.Close()

	h := &HTTPServer{BaseURL: ts.URL, Token: "test-token"}
	ref1, err := h.Advance(ctx, "repo", "main", "", []byte("fact:a\n"))
	if err != nil || ref1 != contentRef([]byte("fact:a\n")) {
		t.Fatalf("Advance(create legacy) = %q, %v", ref1, err)
	}
	if _, err := h.Advance(ctx, "repo", "main", ref1, []byte("fact:a\n")); err != ErrNoChange {
		t.Fatalf("Advance(no-op legacy) = %v; want ErrNoChange", err)
	}
	if _, err := h.Advance(ctx, "repo", "main", "facts-stale", []byte("fact:a\nfact:b\n")); err != ErrConflict {
		t.Fatalf("Advance(stale legacy) = %v; want ErrConflict", err)
	}
}

// TestHTTPServerConvergenceOverWire runs the whole read-merge-CAS loop for TWO members
// through the real HTTPServer adapter over HTTP — proving the runner (Sync + factmerge)
// converges against the actual wire contract, not just the in-process fake.
func TestHTTPServerConvergenceOverWire(t *testing.T) {
	ctx := context.Background()
	ts := contractServer(t, &fakeServer{})
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	a := []factmerge.Record{fact("uses postgres", []string{"data.store.engine"}, "sess-A", now)}
	b := []factmerge.Record{fact("deploys via argo", []string{"ops.deploy.tool"}, "sess-B", now)}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = Sync(ctx, h, "repo", "main", "member-A", a, now) }()
	go func() { defer wg.Done(); _, errs[1] = Sync(ctx, h, "repo", "main", "member-B", b, now) }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("member %d sync over HTTP: %v", i, err)
		}
	}

	_, blob, found, err := h.Current(ctx, "repo", "main")
	if err != nil || !found {
		t.Fatalf("Current after wire convergence: found=%v err=%v", found, err)
	}
	recs, err := factmerge.ParseNDJSON(strings.NewReader(string(blob)))
	if err != nil {
		t.Fatal(err)
	}
	texts := map[string]bool{}
	for _, r := range recs {
		texts[r.Text] = true
	}
	if !texts["uses postgres"] || !texts["deploys via argo"] || len(recs) != 2 {
		t.Fatalf("converged-over-HTTP set = %v; want both members' facts", recs)
	}
}

// flakyDialTransport fails the first failFirst round trips with the exact
// error shape net/http produces for a LOCAL dial failure (ephemeral port /
// connection table exhaustion under host load), then delegates to a real
// transport. It is what a momentarily overloaded host looks like from the
// client's side: nothing about the remote server is wrong.
type flakyDialTransport struct {
	mu        sync.Mutex
	failFirst int
	calls     int
	real      http.RoundTripper
}

func (f *flakyDialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n <= f.failFirst {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: cannot assign requested address")}
	}
	return f.real.RoundTrip(req)
}

func (f *flakyDialTransport) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// nonTransientDialTransport always fails with a dial error that is NOT the
// local-exhaustion signature (e.g. a real "connection refused" from a server
// that is actually down), so do() must return it on the very first attempt.
type nonTransientDialTransport struct {
	calls int
}

func (f *nonTransientDialTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls++
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
}

// TestHTTPServerRetriesTransientLocalDialFailure pins the flake fix: a GET
// that hits transientDialRetries-worth of local
// dial failures (the "cannot assign requested address" signature a busy host
// or a shared CI runner produces under ephemeral-port pressure) must still
// succeed, because nothing ever reached the peer on the failed attempts.
// Before this fix, the FIRST such blip failed the whole call.
func TestHTTPServerRetriesTransientLocalDialFailure(t *testing.T) {
	ctx := context.Background()
	ts := contractServer(t, &fakeServer{})
	defer ts.Close()
	flaky := &flakyDialTransport{failFirst: transientDialRetries, real: http.DefaultTransport}
	h := &HTTPServer{BaseURL: ts.URL, Token: "test-token", Client: &http.Client{Transport: flaky}}

	if ref, blob, found, err := h.Current(ctx, "repo", "main"); err != nil || found || ref != "" || blob != nil {
		t.Fatalf("Current with %d transient dial failures = %q,%v,%v,%v (err should be nil)", transientDialRetries, ref, blob, found, err)
	}
	if got, want := flaky.callCount(), transientDialRetries+1; got != want {
		t.Fatalf("round trips attempted = %d, want %d (the failures plus the one that reached the server)", got, want)
	}
}

// TestHTTPServerGivesUpAfterTheRetryBudget proves do() is bounded: one MORE
// transient failure than the budget must still surface as an error rather
// than retrying forever or hanging.
func TestHTTPServerGivesUpAfterTheRetryBudget(t *testing.T) {
	ctx := context.Background()
	ts := contractServer(t, &fakeServer{})
	defer ts.Close()
	flaky := &flakyDialTransport{failFirst: transientDialRetries + 1, real: http.DefaultTransport}
	h := &HTTPServer{BaseURL: ts.URL, Token: "test-token", Client: &http.Client{Transport: flaky}}

	if _, _, _, err := h.Current(ctx, "repo", "main"); err == nil {
		t.Fatal("Current succeeded despite exhausting the retry budget")
	}
	if got, want := flaky.callCount(), transientDialRetries+1; got != want {
		t.Fatalf("round trips attempted = %d, want exactly %d (the retry budget, no more)", got, want)
	}
}

// TestHTTPServerDoesNotRetryARealConnectivityFailure proves do() is narrow: a
// dial error that is NOT the local-exhaustion signature (a server that is
// genuinely down) must fail on the first attempt, not eat three retries'
// worth of latency pretending a real outage might clear itself in
// milliseconds.
func TestHTTPServerDoesNotRetryARealConnectivityFailure(t *testing.T) {
	ctx := context.Background()
	refused := &nonTransientDialTransport{}
	h := &HTTPServer{BaseURL: "http://127.0.0.1:1", Token: "test-token", Client: &http.Client{Transport: refused}}

	if _, _, _, err := h.Current(ctx, "repo", "main"); err == nil {
		t.Fatal("Current succeeded against a transport that always refuses")
	}
	if refused.calls != 1 {
		t.Fatalf("round trips attempted = %d, want exactly 1 (a real connectivity error must not be retried)", refused.calls)
	}
}

// TestHTTPServerRetriesTransientDialFailureOnPOSTWithoutDoublingTheMutation
// proves the retry is safe for a mutating call: a dial failure never reaches
// the peer, so the server must observe the Advance exactly once even though
// the client retried, and the resulting state must be exactly what a single
// successful Advance would produce.
func TestHTTPServerRetriesTransientDialFailureOnPOSTWithoutDoublingTheMutation(t *testing.T) {
	ctx := context.Background()
	fake := &fakeServer{}
	ts := contractServer(t, fake)
	defer ts.Close()
	flaky := &flakyDialTransport{failFirst: transientDialRetries, real: http.DefaultTransport}
	h := &HTTPServer{BaseURL: ts.URL, Token: "test-token", Client: &http.Client{Transport: flaky}}

	ref, err := h.Advance(ctx, "repo", "main", "", []byte("fact:a\n"))
	if err != nil || ref != contentRef([]byte("fact:a\n")) {
		t.Fatalf("Advance through transient dial failures = %q, %v", ref, err)
	}
	// A second Advance with the SAME oldRef must be a stale-CAS conflict, not
	// a "no-op against a doubled ref" or any other sign the retried POST body
	// landed twice.
	if _, err := h.Advance(ctx, "repo", "main", "", []byte("fact:a\nfact:b\n")); err != ErrConflict {
		t.Fatalf("Advance(stale oldRef after the retried create) = %v; want ErrConflict", err)
	}
}
