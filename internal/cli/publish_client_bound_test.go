package cli

import (
	"net/http"
	"testing"

	"github.com/ashtom/entire-brain/internal/httpx"
)

// TestPublishClientIsFullyBounded closes the gap in the original "bound every
// outbound client" change: hostedbrain and factsync were moved off
// http.DefaultClient, but `brain publish` — which uploads the whole brain artifact
// bundle with the member's bearer token attached — kept building its client on
// http.DefaultTransport. http.Client.Timeout alone leaves the silent-server case
// costing the FULL five-minute request budget, because http.DefaultTransport sets
// no ResponseHeaderTimeout. It also means a dependency mutating the process-global
// http.DefaultTransport silently retunes the publish path.
func TestPublishClientIsFullyBounded(t *testing.T) {
	c := publishHTTPClient()
	if c.Timeout != publishRequestTimeout {
		t.Errorf("publish client Timeout = %v, want %v", c.Timeout, publishRequestTimeout)
	}
	if c.Transport == nil {
		t.Fatal("publish client has no Transport, so it falls back to the unbounded http.DefaultTransport")
	}
	if c.Transport == http.DefaultTransport {
		t.Fatal("publish client rides http.DefaultTransport: no response-header bound, and process-global tuning leaks in")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("publish client Transport is %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("publish client has no ResponseHeaderTimeout: a server that accepts the bundle and never replies costs the full request budget")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("publish client has no TLSHandshakeTimeout")
	}
	if tr != httpx.Shared() {
		t.Error("publish client does not share the pooled bounded transport, so it fragments the connection pool")
	}
}
