package hostedbrain

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The hosted MCP surface reads bodies already, but it read them RAW: up to 4 KiB of
// whatever the peer sent, pasted straight into the error. That is fine for the JSON
// envelope the endpoint documents and wrong for what an ingress in front of it sends
// — an HTML page, arriving with markup, newlines, and (from a hostile or broken peer)
// terminal escape sequences. These pin the rendered form without weakening the typed
// sentinels callers match on.

func errorPageServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The drift, on this surface: a cap violation must reach the member as the server's
// own sentence.
func TestHostedBrainSurfacesTheServersObjection(t *testing.T) {
	ts := errorPageServer(t, http.StatusBadRequest, `{"title":"Bad Request","detail":"branch is 600 bytes, limit is 512"}`)
	c := &Client{BaseURL: ts.URL, Token: "tok"}
	_, _, err := c.Initialize(context.Background(), "repo1")
	if err == nil || !strings.Contains(err.Error(), "branch is 600 bytes, limit is 512") {
		t.Fatalf("Initialize(400) = %v; want the server's explanation", err)
	}
}

// An HTML gateway page must arrive as its title, not as markup, and must stay short.
func TestHostedBrainHTMLErrorPageIsBoundedAndReadable(t *testing.T) {
	page := "<!DOCTYPE html>\n<html><head><title>502 Bad Gateway</title></head><body>\n" +
		strings.Repeat("<p>filler</p>\n", 100000) + "</body></html>\n"
	ts := errorPageServer(t, http.StatusBadGateway, page)
	c := &Client{BaseURL: ts.URL, Token: "tok"}
	_, _, err := c.Initialize(context.Background(), "repo1")
	if err == nil {
		t.Fatal("Initialize(502) produced no error")
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("Initialize(502) = %q; want the page title", err)
	}
	if len(err.Error()) > 1024 {
		t.Fatalf("Initialize(502) error is %d bytes; the page was dumped at the member", len(err.Error()))
	}
	if strings.Contains(err.Error(), "<p>") {
		t.Fatalf("Initialize(502) = %q; raw markup reached the member", err)
	}
}

// Terminal escapes and stray newlines from the peer must not be printed verbatim.
func TestHostedBrainErrorIsTerminalSafe(t *testing.T) {
	ts := errorPageServer(t, http.StatusInternalServerError, "boom\x1b[2J\x00\nsecond line")
	c := &Client{BaseURL: ts.URL, Token: "tok"}
	_, _, err := c.Initialize(context.Background(), "repo1")
	if err == nil {
		t.Fatal("Initialize(500) produced no error")
	}
	if strings.ContainsAny(err.Error(), "\x00\x1b\n\r") {
		t.Fatalf("Initialize(500) = %q; control bytes reached the terminal", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Initialize(500) = %q; the server's text was lost", err)
	}
}

// SEMANTIC PRESERVATION — the typed sentinels are what callers branch on (an expired
// token, a repo without pull access, a deployment with no hosted brain). Rendering the
// body better must not change which sentinel a status maps to.
func TestHostedBrainTypedSentinelsSurviveTheRendering(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusServiceUnavailable, ErrNotConfigured},
	} {
		ts := errorPageServer(t, tc.code, `{"detail":"token expired at 2026-08-01"}`)
		c := &Client{BaseURL: ts.URL, Token: "tok"}
		_, _, err := c.Initialize(context.Background(), "repo1")
		if !errors.Is(err, tc.want) {
			t.Fatalf("status %d → %v; want %v", tc.code, err, tc.want)
		}
		if !strings.Contains(err.Error(), "token expired at 2026-08-01") {
			t.Fatalf("status %d → %q; the server's explanation was dropped", tc.code, err)
		}
	}
}

// An empty error body must not leave a dangling separator.
func TestHostedBrainSilentRefusalKeepsACleanMessage(t *testing.T) {
	ts := errorPageServer(t, http.StatusUnauthorized, "")
	c := &Client{BaseURL: ts.URL, Token: "tok"}
	_, _, err := c.Initialize(context.Background(), "repo1")
	if err == nil {
		t.Fatal("Initialize(401, empty body) produced no error")
	}
	if strings.HasSuffix(err.Error(), ": ") || strings.HasSuffix(err.Error(), ":") {
		t.Fatalf("Initialize(401, empty body) = %q; an empty body left a dangling separator", err)
	}
}
