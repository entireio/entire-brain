package hostedbrain

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fuzzTransport answers every request in-process from a fixed status and body.
//
// A real httptest server is socket-bound here: the client abandons a connection
// whenever a fuzzed body does not decode cleanly to EOF, so every request costs a
// fresh TCP connection. That exhausts the machine's ephemeral ports within
// seconds, collapses the exec rate to double digits, and breaks every other test
// on the box. A RoundTripper drives the identical client code — status mapping,
// JSON-RPC envelope decode, tool-content decode — with no sockets at all.
type fuzzTransport struct {
	status int
	body   []byte
}

func (t fuzzTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode:    t.status,
		Status:        fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		Proto:         "HTTP/1.1",
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(t.body)),
		ContentLength: int64(len(t.body)),
		Request:       req,
	}, nil
}

func fuzzClient(status int, body []byte) *http.Client {
	return &http.Client{Transport: fuzzTransport{status: status, body: body}}
}

// The hosted brain client is the runner-side consumer of a REMOTE JSON-RPC
// surface: every byte it decodes is chosen by the server. These harnesses point
// the real client at a hostile server and assert the only outcomes are a value
// or an error — never a panic, never an unbounded read, never a hang.
//
//	go test ./internal/hostedbrain -run xxx -fuzz FuzzClientResponses

func FuzzClientResponses(f *testing.F) {
	f.Add(200, []byte(`{"result":{"serverInfo":{"name":"n","version":"v","brainSchemaVersion":"1.0"}}}`))
	f.Add(200, []byte(`{"result":{"serverInfo":{"brainSchemaVersion":"9.9"}}}`))
	f.Add(200, []byte(`{"result":{"serverInfo":{"brainSchemaVersion":"not-a-version"}}}`))
	f.Add(200, []byte(`{"error":{"code":-32000,"message":"boom"}}`))
	f.Add(200, []byte(`{"result":{"tools":[{"name":"t","inputSchema":{"a":1}}]}}`))
	f.Add(200, []byte(`{"result":{"content":[{"type":"text","text":"[{\"fact\":{\"id\":\"f\"},\"score\":1}]"}]}}`))
	f.Add(200, []byte(`{"result":{"content":[]}}`))
	f.Add(200, []byte(`{"result":`))
	f.Add(401, []byte(`nope`))
	f.Add(503, []byte(``))

	f.Fuzz(func(t *testing.T, status int, body []byte) {
		if status < 100 || status > 599 {
			return
		}
		// The egress gate must be off for this harness to reach the decoders.
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
		t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")

		c := &Client{BaseURL: "http://fuzz.invalid", Token: "t", HTTP: fuzzClient(status, body)}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, _, _ = c.Initialize(ctx, "repo")
		_, _ = c.ListTools(ctx, "repo")
		_, _ = c.CallTool(ctx, "repo", "brain_search", map[string]any{"query": "q"})
		_, _ = c.Search(ctx, "repo", "main", "q", 5)
		_, _ = c.Get(ctx, "repo", "main", "fact:aa")
		_, _ = c.MultiGet(ctx, "repo", "main", []string{"fact:aa"})
		_, _ = c.Status(ctx, "repo", "main")
		_, _ = c.ListProposals(ctx, "repo", "main")
		_, _ = c.GetProposal(ctx, "repo", "main", "prop-0123456789abcdef")
	})
}

// FuzzEgressGate pins the fail-closed semantics of the no-egress toggle: any
// value the harness cannot recognize must DISABLE egress, never enable it.
func FuzzEgressGate(f *testing.F) {
	f.Add("")
	f.Add("1")
	f.Add("false")
	f.Add("  TRUE  ")
	f.Add("maybe")
	f.Add("0\n")
	f.Fuzz(func(t *testing.T, v string) {
		if strings.ContainsRune(v, 0) {
			return // the OS refuses a NUL in an environment value; not a product path
		}
		t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", v)
		on := noEgress()
		switch strings.ToLower(strings.TrimSpace(v)) { // the same normalization toggleOn applies
		case "", "0", "false", "no", "off", "disable", "disabled":
			if on {
				t.Fatalf("noEgress() reported ON for the off-value %q", v)
			}
		case "1", "true", "yes", "on", "enable", "enabled":
			if !on {
				t.Fatalf("noEgress() reported OFF for the on-value %q", v)
			}
		default:
			if !on {
				t.Fatalf("noEgress() failed OPEN for the unrecognized value %q", v)
			}
		}
	})
}
