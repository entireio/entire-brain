package factsync

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// countingTransport proves a refusal is round-trip-free: any request that reaches it
// is a request that would have left the machine.
type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return nil, http.ErrUseLastResponse
}

// TestHTTPServerRefusesPlaintextNonLoopbackBaseURL is the transport-level floor: even
// if a caller hands HTTPServer an http:// origin, no fact-set read, no fact-set
// advance (which carries the merged fact-set), and no proposal call may leave the
// machine in the clear — and the bearer token must never be put on the wire.
func TestHTTPServerRefusesPlaintextNonLoopbackBaseURL(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_ALLOW_INSECURE_API_URL", "")
	rt := &countingTransport{}
	h := &HTTPServer{BaseURL: "http://evil.example", Token: "tok", Client: &http.Client{Transport: rt}}
	ctx := context.Background()

	if _, _, _, err := h.Current(ctx, "repo", "main"); err == nil || !strings.Contains(err.Error(), "insecure_api_url") {
		t.Fatalf("Current err = %v; want an insecure_api_url refusal", err)
	}
	if _, err := h.Advance(ctx, "repo", "main", "", []byte("{\"text\":\"secret\"}\n")); err == nil || !strings.Contains(err.Error(), "insecure_api_url") {
		t.Fatalf("Advance err = %v; want an insecure_api_url refusal", err)
	}
	if _, err := h.ListProposals(ctx, "repo", "main"); err == nil || !strings.Contains(err.Error(), "insecure_api_url") {
		t.Fatalf("ListProposals err = %v; want an insecure_api_url refusal", err)
	}
	if _, err := h.PublishProposals(ctx, "repo", "main", "", []OpenProposal{{ID: "p1"}}); err == nil || !strings.Contains(err.Error(), "insecure_api_url") {
		t.Fatalf("PublishProposals err = %v; want an insecure_api_url refusal", err)
	}
	if rt.n != 0 {
		t.Fatalf("%d request(s) reached the transport under a refusal; want 0", rt.n)
	}
}

// TestHTTPServerAllowsLoopbackPlaintext keeps local dev (and every httptest-based
// test in this repo, which binds 127.0.0.1) working unchanged.
func TestHTTPServerAllowsLoopbackPlaintext(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_ALLOW_INSECURE_API_URL", "")
	rt := &countingTransport{}
	h := &HTTPServer{BaseURL: "http://127.0.0.1:9", Token: "tok", Client: &http.Client{Transport: rt}}
	if _, _, _, err := h.Current(context.Background(), "repo", "main"); err != nil && strings.Contains(err.Error(), "insecure_api_url") {
		t.Fatalf("loopback http refused: %v", err)
	}
	if rt.n == 0 {
		t.Fatal("loopback request never reached the transport; the carve-out is broken")
	}
}
