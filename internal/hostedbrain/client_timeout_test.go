package hostedbrain

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDefaultClientHasABound is the direct assertion: the client this package uses
// when the caller supplies none must carry a timeout. http.DefaultClient does not,
// which is what made a wedged hosted endpoint hang the CLI forever.
func TestDefaultClientHasABound(t *testing.T) {
	c := &Client{BaseURL: "https://example.invalid"}
	if got := c.httpClient().Timeout; got == 0 {
		t.Fatal("default hosted-brain client has no Timeout; a wedged endpoint hangs the caller forever")
	}
	custom := &http.Client{Timeout: time.Second}
	if c2 := (&Client{HTTP: custom}); c2.httpClient() != custom {
		t.Error("caller-supplied client must still win over the package default")
	}
}

// TestDefaultClientBoundsAWedgedEndpoint is the behavioural proof: a server that
// accepts the request and never responds, called with context.Background() (the
// real CLI/daemon case — no caller deadline), must still return.
func TestDefaultClientBoundsAWedgedEndpoint(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	withShortHostedTimeouts(t)

	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-done }))
	t.Cleanup(func() { close(done); srv.Close() })

	assertReturns(t, func() error {
		_, err := (&Client{BaseURL: srv.URL}).Status(context.Background(), "repo", "main")
		return err
	})
}

// withShortHostedTimeouts shrinks the package bound for the duration of one test, so
// the behavioural test proves the bound exists without waiting out the production
// minute.
func withShortHostedTimeouts(t *testing.T) {
	t.Helper()
	req := hostedRequestTimeout
	hostedRequestTimeout = 250 * time.Millisecond
	t.Cleanup(func() { hostedRequestTimeout = req })
}

// assertReturns fails if call has not returned an error within a generous window.
// Without a client bound it never returns at all.
func assertReturns(t *testing.T, call func() error) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- call() }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("wedged endpoint reported success")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("call against a wedged endpoint never returned: the client has no bound")
	}
}
