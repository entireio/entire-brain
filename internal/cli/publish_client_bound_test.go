package cli

import (
	"net/http"
	"testing"

	"github.com/entireio/entire-brain/internal/httpx"
)

// TestPublishClientIsFullyBounded verifies that uploads share an isolated
// transport while retaining their full processing budget and TLS phase bound.
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
	if tr.ResponseHeaderTimeout != publishRequestTimeout {
		t.Error("publish header bound must preserve the full processing budget")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("publish client has no TLSHandshakeTimeout")
	}
	if tr != httpx.UploadClient(publishRequestTimeout).Transport {
		t.Error("publish client does not share the pooled bounded transport, so it fragments the connection pool")
	}
}
