package factsync

import (
	"context"
	"encoding/json"
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
// data, query/body branch, and the 200/200-unchanged/412/400 status mapping — validating
// the adapter against the documented contract. (End-to-end validation against the live
// entire-api handler is the deploy-time step; the two Go modules can't share types.)
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
				_ = json.NewEncoder(w).Encode(map[string]any{"newRef": cur, "version": 1, "unchanged": true})
			case err == ErrConflict:
				w.WriteHeader(http.StatusPreconditionFailed)
			case err != nil:
				w.WriteHeader(http.StatusBadRequest)
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"newRef": newRef, "version": 1})
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestHTTPServerContract exercises the adapter's every status→sentinel mapping over a real
// HTTP round-trip: empty-head read, create, read-back, no-op (unchanged→ErrNoChange), and
// stale-old-ref (412→ErrConflict).
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
