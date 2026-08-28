package httpx

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDefaultClientIsBoundedAtEveryLayer(t *testing.T) {
	client := Default()
	if client == http.DefaultClient {
		t.Fatal("Default() returned http.DefaultClient, which has no timeout")
	}
	if client.Timeout <= 0 {
		t.Fatalf("client Timeout = %v, want a positive bound", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client Transport = %T, want *http.Transport", client.Transport)
	}
	if transport == http.DefaultTransport {
		t.Fatal("client shares http.DefaultTransport; tuning would leak to unrelated callers")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Fatal("TLSHandshakeTimeout is unbounded")
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("ResponseHeaderTimeout is unbounded: a server that never replies would hang the caller")
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil: the dial falls back to an unbounded default")
	}
}

func TestDefaultIsShared(t *testing.T) {
	if Default() != Default() {
		t.Fatal("Default() must return one shared client so connections pool")
	}
}

// TestNewClientTimesOutAgainstUnresponsiveServer proves the bounds actually
// fire: the peer accepts the connection and never writes a byte.
func TestNewClientTimesOutAgainstUnresponsiveServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
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

	cfg := DefaultConfig()
	cfg.ResponseHeader = 500 * time.Millisecond
	cfg.Request = 5 * time.Second
	client := NewClient(cfg)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from an unresponsive server")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("bounded client did not give up on an unresponsive server")
	}
}

// TestDialIsBounded covers the connect layer: a non-routable address must fail
// on the dial timeout instead of parking for the OS default.
func TestDialIsBounded(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Dial = 300 * time.Millisecond
	cfg.Request = 5 * time.Second
	client := NewClient(cfg)

	// 203.0.113.0/24 is TEST-NET-3: reserved for documentation, never routed.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://203.0.113.1:81", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Skip("TEST-NET-3 unexpectedly reachable in this environment")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("dial took %v; the dial timeout is not being applied", elapsed)
	}
}
