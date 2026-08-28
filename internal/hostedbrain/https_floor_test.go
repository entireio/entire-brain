package hostedbrain

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return nil, http.ErrUseLastResponse
}

// TestClientRefusesPlaintextNonLoopbackBaseURL: the hosted MCP query path is an
// egress chokepoint too — a query (and the bearer token authorizing it) must not
// cross plaintext to a non-loopback host.
func TestClientRefusesPlaintextNonLoopbackBaseURL(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_BRAIN_ALLOW_INSECURE_API_URL", "")
	rt := &countingTransport{}
	c := &Client{BaseURL: "http://evil.example", Token: "tok", HTTP: &http.Client{Transport: rt}}

	if _, err := c.CallTool(context.Background(), "repo", "brain_search", map[string]any{"query": "secret"}); err == nil || !strings.Contains(err.Error(), "insecure_api_url") {
		t.Fatalf("CallTool err = %v; want an insecure_api_url refusal", err)
	}
	if rt.n != 0 {
		t.Fatalf("%d request(s) reached the transport under a refusal; want 0", rt.n)
	}
}
