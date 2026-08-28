package factsync

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/httpx"
)

// unresponsiveAPI accepts TCP connections and never writes a byte back.
func unresponsiveAPI(t *testing.T) string {
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

func TestHTTPServerDefaultsToBoundedHTTPClient(t *testing.T) {
	client := (&HTTPServer{}).client()
	if client == http.DefaultClient {
		t.Fatal("fact-set sync falls back to http.DefaultClient, which has no timeout")
	}
	if client.Timeout <= 0 {
		t.Fatalf("default client Timeout = %v, want a positive bound", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout <= 0 {
		t.Fatalf("default client transport is unbounded: %#v", client.Transport)
	}
}

func TestHTTPServerRespectsInjectedHTTPClient(t *testing.T) {
	injected := &http.Client{}
	if got := (&HTTPServer{Client: injected}).client(); got != injected {
		t.Fatal("an explicitly injected client must win over the package default")
	}
}

// TestSyncGivesUpOnUnresponsiveAPI is the regression: the watch daemon calls
// this on a loop, and before the fix one wedged endpoint parked it forever.
func TestSyncGivesUpOnUnresponsiveAPI(t *testing.T) {
	cfg := httpx.DefaultConfig()
	cfg.ResponseHeader = 500 * time.Millisecond
	cfg.Request = 5 * time.Second
	restore := defaultHTTPClient
	defaultHTTPClient = httpx.NewClient(cfg)
	t.Cleanup(func() { defaultHTTPClient = restore })

	server := &HTTPServer{BaseURL: unresponsiveAPI(t), Token: "token"}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := server.Current(context.Background(), "repo-1", "main")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from an unresponsive API")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("fact-set sync never returned against an unresponsive API")
	}
}
