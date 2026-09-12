package httpx

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSharedTransportIsNotTheDefault: the whole point of this package is that
// hosted-API traffic does NOT ride http.DefaultTransport, which has no
// ResponseHeaderTimeout and whose settings any dependency can mutate globally.
func TestSharedTransportIsNotTheDefault(t *testing.T) {
	tr := Shared()
	if any(tr) == any(http.DefaultTransport) {
		t.Fatal("Shared() returned http.DefaultTransport; tuning here would leak into unrelated callers")
	}
	if tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 {
		t.Fatalf("Shared() transport is unbounded: ResponseHeaderTimeout=%v TLSHandshakeTimeout=%v", tr.ResponseHeaderTimeout, tr.TLSHandshakeTimeout)
	}
	if Shared() != tr {
		t.Error("Shared() must return one pooled transport, not a fresh one per call")
	}
}

// TestClientCarriesBothBounds: a caller picks its own end-to-end request bound,
// but always inherits the shared transport's narrower phase bounds.
func TestClientCarriesBothBounds(t *testing.T) {
	c := Client(90 * time.Second)
	if c.Timeout != 90*time.Second {
		t.Errorf("Client(90s).Timeout = %v, want 90s", c.Timeout)
	}
	if c.Transport != http.RoundTripper(Shared()) {
		t.Error("Client did not get the shared bounded transport")
	}
}

// TestResponseHeaderTimeoutFiresBeforeTheRequestBound is the reason the phase
// bounds exist at all. A peer that completes the TCP handshake and then never
// writes a response header is the classic silent hang: with only
// http.Client.Timeout, the caller waits the FULL request budget (five minutes on
// the publish and fact-sync paths) before learning anything is wrong. The
// ResponseHeaderTimeout cuts it short while still allowing a slow-but-progressing
// body transfer to run to the longer bound.
func TestResponseHeaderTimeoutFiresBeforeTheRequestBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- conn // hold it open, never answer
		}
	}()
	t.Cleanup(func() {
		close(accepted)
		for c := range accepted {
			_ = c.Close()
		}
	})

	cfg := DefaultConfig()
	cfg.ResponseHeader = 200 * time.Millisecond
	cfg.Request = time.Hour // deliberately far larger than the phase bound
	client := NewClient(cfg)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a server that never answered reported success")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a server that accepted the connection and never answered hung the client past its response-header bound")
	}
}

func TestUploadClientAllowsProcessingWithinRequestBudget(t *testing.T) {
	query := Client(time.Minute)
	upload := UploadClient(5 * time.Minute)
	tr := upload.Transport.(*http.Transport)
	if upload.Timeout != 5*time.Minute || tr.ResponseHeaderTimeout != upload.Timeout {
		t.Fatalf("upload budget mismatch: request=%v headers=%v", upload.Timeout, tr.ResponseHeaderTimeout)
	}
	if upload.Transport == query.Transport || upload.Transport != UploadClient(5*time.Minute).Transport {
		t.Fatal("uploads need their own reused connection pool")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Fatal("upload TLS phase is unbounded")
	}

	// Scale the same policies down for a server that processes a completed body
	// longer than the query header budget, but within the upload request budget.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	cfg := uploadConfig()
	cfg.Request = 2 * time.Second
	cfg.ResponseHeader = cfg.Request
	client := NewClient(cfg)
	defer client.CloseIdleConnections()
	resp, err := client.Post(server.URL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cfg.ResponseHeader = 20 * time.Millisecond
	queryProbe := NewClient(cfg)
	defer queryProbe.CloseIdleConnections()
	resp, err = queryProbe.Get(server.URL)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("query header timeout did not fire")
	}
}
