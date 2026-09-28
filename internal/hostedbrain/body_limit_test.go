package hostedbrain

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/httpx"
)

// The hosted MCP surface read its 200 with an unbounded json.NewDecoder, the same
// shape as the fact-set transport. A hosted brain that answers a tools/call with a
// gigabyte-scale content block allocates it whole on the member's machine. See
// internal/httpx/decode.go for the measurement and why the bound refuses rather
// than truncates.
func TestRPCRefusesAnOversizedResult(t *testing.T) {
	prev := httpx.MaxJSONBodyBytes
	httpx.MaxJSONBodyBytes = 64 << 10
	t.Cleanup(func() { httpx.MaxJSONBodyBytes = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"`)
		chunk := strings.Repeat("a", 4096)
		for written := 0; int64(written) <= httpx.MaxJSONBodyBytes; written += len(chunk) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}}}`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "tok"}
	info, _, err := c.Initialize(t.Context(), "01HZZPROBE0000000000000000")
	if err == nil {
		t.Fatalf("Initialize accepted an oversized result (serverInfo.name is %d bytes)", len(info.Name))
	}
	if !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Errorf("Initialize failed with %v; want httpx.ErrBodyTooLarge", err)
	}
}

func TestRPCAcceptsAResultWithinTheLimit(t *testing.T) {
	prev := httpx.MaxJSONBodyBytes
	httpx.MaxJSONBodyBytes = 1 << 20
	t.Cleanup(func() { httpx.MaxJSONBodyBytes = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"hosted","version":"1"}}}`)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "tok"}
	info, _, err := c.Initialize(t.Context(), "01HZZPROBE0000000000000000")
	if err != nil {
		t.Fatalf("Initialize refused a normal result: %v", err)
	}
	if info.Name != "hosted" {
		t.Errorf("serverInfo.name = %q, want %q", info.Name, "hosted")
	}
}
