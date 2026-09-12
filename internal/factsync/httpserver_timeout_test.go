package factsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDefaultSyncClientHasABound is the direct assertion: the client the sync
// adapter uses when the caller supplies none must carry a timeout. http.DefaultClient
// does not, and `entire brain facts sync --backend http` constructs an HTTPServer
// with no Client, so a wedged hosted endpoint hung the sync forever.
func TestDefaultSyncClientHasABound(t *testing.T) {
	h := &HTTPServer{BaseURL: "https://example.invalid"}
	if got := h.client().Timeout; got == 0 {
		t.Fatal("default factsync client has no Timeout; a wedged endpoint hangs the caller forever")
	}
	custom := &http.Client{Timeout: time.Second}
	if (&HTTPServer{Client: custom}).client().Timeout != custom.Timeout {
		t.Error("caller-supplied client must still win over the package default")
	}
}

// TestDefaultSyncClientBoundsAWedgedEndpoint is the behavioural proof, on both the
// read (Current) and the write (Advance) path, called with context.Background() —
// the real CLI/daemon case, where there is no caller deadline.
func TestDefaultSyncClientBoundsAWedgedEndpoint(t *testing.T) {
	withShortSyncTimeouts(t)

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-done }))
	t.Cleanup(func() { close(done); srv.Close() })

	h := &HTTPServer{BaseURL: srv.URL, Token: "t"}
	assertReturns(t, "Current", func() error {
		_, _, _, err := h.Current(context.Background(), "repo", "main")
		return err
	})
	assertReturns(t, "Advance", func() error {
		_, err := h.Advance(context.Background(), "repo", "main", "", []byte("x"))
		return err
	})
}

func withShortSyncTimeouts(t *testing.T) {
	t.Helper()
	req := syncRequestTimeout
	syncRequestTimeout = 250 * time.Millisecond
	t.Cleanup(func() { syncRequestTimeout = req })
}

func assertReturns(t *testing.T, name string, call func() error) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- call() }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatalf("%s: wedged endpoint reported success", name)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s against a wedged endpoint never returned: the client has no bound", name)
	}
}
