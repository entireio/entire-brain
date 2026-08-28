package hostedbrain

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// requestLineRecorder accepts one HTTP request per connection, records its
// request line verbatim, and answers 503 so the client gives up immediately.
// It reads the raw bytes on purpose: net/http's server would normalize the
// path before a handler could observe what was actually sent.
func requestLineRecorder(t *testing.T) (baseURL string, lines <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	out := make(chan string, 16)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			out <- strings.TrimSpace(line)
			_, _ = conn.Write([]byte("HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"))
			_ = conn.Close()
		}
	}()
	return "http://" + ln.Addr().String(), out
}

// TestRPCEscapesRepoID pins the request target: a repo id is one path segment,
// whatever it contains. Before the fix it was interpolated raw, so a repo id
// could add segments, traverse out of /api/v1/repos/, or start a query string
// and drop the /brain/mcp suffix entirely.
func TestRPCEscapesRepoID(t *testing.T) {
	base, lines := requestLineRecorder(t)
	for name, repoID := range map[string]string{
		"plain":         "01JABCDEF",
		"traversal":     "../../../admin/wipe",
		"extra segment": "repo-1/brain/mcp/../../danger",
		"query":         "repo-1?evil=1",
		"fragment":      "repo-1#frag",
		"space":         "repo 1",
	} {
		t.Run(name, func(t *testing.T) {
			c := &Client{BaseURL: base, Token: "token"}
			_, _, _ = c.Initialize(context.Background(), repoID)
			var line string
			select {
			case line = <-lines:
			case <-time.After(5 * time.Second):
				t.Fatal("no request observed")
			}
			target := strings.Fields(line)
			if len(target) < 2 {
				t.Fatalf("malformed request line %q", line)
			}
			want := "/api/v1/repos/" + url.PathEscape(repoID) + "/brain/mcp"
			if target[1] != want {
				t.Fatalf("request target = %q, want %q", target[1], want)
			}
		})
	}
}

// TestRPCCannotReachAnotherHandler is the concrete consequence: with the raw
// interpolation, a repo id of "../../../admin/wipe" made the client POST the
// caller's bearer token to /admin/wipe/brain/mcp — any hop that normalizes dot
// segments (net/http's own ServeMux, nginx, a CDN) resolves the traversal.
func TestRPCCannotReachAnotherHandler(t *testing.T) {
	reached := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "brain:" + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"n","version":"v","brainSchemaVersion":"1.0"},"protocolVersion":"2024-11-05"}}`))
	})
	mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		reached <- "admin:" + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "token"}
	_, _, _ = c.Initialize(context.Background(), "../../../admin/wipe")
	close(reached)
	for hit := range reached {
		if strings.HasPrefix(hit, "admin:") {
			t.Fatalf("hosted brain request escaped its route and reached %s", hit)
		}
	}
}

// TestRPCRejectsEmptyRepoID keeps the degenerate case honest: an empty id
// collapses "/api/v1/repos//brain/mcp" onto a different route once a hop
// cleans the doubled slash, so it is refused before any egress.
func TestRPCRejectsEmptyRepoID(t *testing.T) {
	base, lines := requestLineRecorder(t)
	c := &Client{BaseURL: base, Token: "token"}
	_, _, err := c.Initialize(context.Background(), "  ")
	if err == nil {
		t.Fatal("expected an empty repo id to be refused")
	}
	select {
	case line := <-lines:
		t.Fatalf("a request was sent for an empty repo id: %s", line)
	case <-time.After(300 * time.Millisecond):
	}
}
