package factsync

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type redirectProbeTransport func(*http.Request) (*http.Response, error)

func (f redirectProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetryDoesNotReplayRedirectedPost(t *testing.T) {
	for _, status := range []int{303, 307} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			applied, followed := 0, 0
			h := &HTTPServer{Client: &http.Client{Transport: redirectProbeTransport(func(r *http.Request) (*http.Response, error) {
				if r.Body != nil {
					r.Body.Close()
				}
				if r.URL.Path == "/after" {
					followed++
					return nil, &net.OpError{Op: "dial", Err: errors.New("cannot assign requested address")}
				}
				applied++
				return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"http://localhost/after"}}, Body: io.NopCloser(strings.NewReader("redirect")), Request: r}, nil
			})}}
			req, _ := http.NewRequest(http.MethodPost, "http://localhost/apply", strings.NewReader("mutation"))
			resp, err := h.do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if applied != 1 || followed != 0 || resp.StatusCode != status {
				t.Fatalf("applied=%d followed=%d status=%d", applied, followed, resp.StatusCode)
			}
		})
	}
}

func TestRetryCancellationReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	h := &HTTPServer{Client: &http.Client{Transport: redirectProbeTransport(func(r *http.Request) (*http.Response, error) {
		attempts++
		cancel()
		return nil, &net.OpError{Op: "dial", Err: errors.New("cannot assign requested address")}
	})}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/facts", nil)
	_, err := h.do(req)
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}
