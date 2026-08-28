package hostedbrain

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/httpx"
)

// unresponsiveEndpoint accepts TCP connections and never writes a byte back —
// the shape of a wedged hosted endpoint (as opposed to one that is down, which
// fails the dial immediately).
func unresponsiveEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		var held []net.Conn
		for {
			conn, err := ln.Accept()
			if err != nil {
				for _, c := range held {
					_ = c.Close()
				}
				return
			}
			held = append(held, conn)
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestClientDefaultsToBoundedHTTPClient(t *testing.T) {
	client := (&Client{}).httpClient()
	if client == http.DefaultClient {
		t.Fatal("hosted brain client falls back to http.DefaultClient, which has no timeout")
	}
	if client.Timeout <= 0 {
		t.Fatalf("default client Timeout = %v, want a positive bound", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout <= 0 {
		t.Fatalf("default client transport is unbounded: %#v", client.Transport)
	}
}

func TestClientRespectsInjectedHTTPClient(t *testing.T) {
	injected := &http.Client{}
	if got := (&Client{HTTP: injected}).httpClient(); got != injected {
		t.Fatal("an explicitly injected client must win over the package default")
	}
}

// TestHostedCallGivesUpOnUnresponsiveServer is the regression: before the fix
// this call never returned, because http.DefaultClient has no timeout and no
// caller sets a request deadline.
func TestHostedCallGivesUpOnUnresponsiveServer(t *testing.T) {
	cfg := httpx.DefaultConfig()
	cfg.ResponseHeader = 500 * time.Millisecond
	cfg.Request = 5 * time.Second
	restore := defaultHTTPClient
	defaultHTTPClient = httpx.NewClient(cfg)
	t.Cleanup(func() { defaultHTTPClient = restore })

	c := &Client{BaseURL: unresponsiveEndpoint(t), Token: "token"}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Initialize(context.Background(), "repo-1")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from an unresponsive hosted endpoint")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("hosted brain call never returned against an unresponsive server")
	}
}
