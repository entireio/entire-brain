package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `brain publish` already lifted "detail"/"title" out of the server's error envelope,
// which is why it never showed the bare-status symptom the fact-set transport did.
// What it did NOT do is defend the OTHER shapes an error body arrives in: a proxy's
// HTML page (pasted as a 200-character slice of markup) and a peer's control bytes
// (pasted into the member's terminal). It also dropped the body entirely on 503, the
// one status where the server's own sentence says which of several reasons applies.
//
// The publish caps the server grew alongside the branch cap — the artifact count and
// the manifest reference count — are surfaced, not mirrored: unlike the body-size
// ceiling, which the client must project anyway to avoid building a multi-GB buffer,
// the client has no independent basis for those counts, and a guessed constant would
// refuse bundles the server accepts.

const publishTestRepoID = "01JABCDEFGHJKMNPQRSTVWXYZ0"

func publishErrorServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func publishErr(t *testing.T, status int, body string) string {
	t.Helper()
	srv := publishErrorServer(t, status, body)
	_, err := postBrainArtifacts(context.Background(), srv.URL, publishTestRepoID, "secret", publishRequestBody{})
	if err == nil {
		t.Fatalf("postBrainArtifacts(%d) produced no error", status)
	}
	return err.Error()
}

// A cap the client does not mirror still has to reach the member in the server's own
// words, whichever 4xx carries it.
func TestPublishSurfacesAServerCapItDoesNotMirror(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge} {
		got := publishErr(t, status, `{"title":"Bad Request","detail":"manifest references 4096 artifacts, limit is 512"}`)
		if !strings.Contains(got, "manifest references 4096 artifacts, limit is 512") {
			t.Fatalf("publish(%d) = %q; want the server's own explanation", status, got)
		}
	}
}

// 503 had its body thrown away, so "publishing is not configured" was the client's
// guess at a status the server uses for more than one reason.
func TestPublishSurfacesWhyTheServerIsUnavailable(t *testing.T) {
	got := publishErr(t, http.StatusServiceUnavailable, `{"title":"Service Unavailable","detail":"brain publishing is disabled for this jurisdiction"}`)
	if !strings.Contains(got, "brain publishing is disabled for this jurisdiction") {
		t.Fatalf("publish(503) = %q; want the server's reason", got)
	}
	if !strings.Contains(got, "publish_unavailable") {
		t.Fatalf("publish(503) = %q; the error class was lost", got)
	}
}

// A gateway's HTML page must arrive as its title, not as a slice of markup.
func TestPublishHTMLErrorPageIsBoundedAndReadable(t *testing.T) {
	page := "<!DOCTYPE html>\n<html><head><title>413 Request Entity Too Large</title></head><body>\n" +
		strings.Repeat("<span>x</span>\n", 100000) + "</body></html>\n"
	got := publishErr(t, http.StatusRequestEntityTooLarge, page)
	if !strings.Contains(got, "413 Request Entity Too Large") {
		t.Fatalf("publish(html) = %q; want the page title", got)
	}
	if strings.Contains(got, "<span>") || strings.Contains(got, "<!DOCTYPE") {
		t.Fatalf("publish(html) = %q; raw markup reached the member", got)
	}
	if len(got) > 1024 {
		t.Fatalf("publish(html) error is %d bytes; the page was dumped at the member", len(got))
	}
}

// Control bytes from the peer must not be printed verbatim.
func TestPublishErrorIsTerminalSafe(t *testing.T) {
	got := publishErr(t, http.StatusBadGateway, "upstream\x1b[2Jconnect\x00 error\nsecond line")
	if strings.ContainsAny(got, "\x00\x1b\n\r") {
		t.Fatalf("publish(502) = %q; control bytes reached the terminal", got)
	}
	if !strings.Contains(got, "upstream") {
		t.Fatalf("publish(502) = %q; the server's text was lost", got)
	}
}

// SEMANTIC PRESERVATION — the error CLASS prefixes are what the command and its tests
// branch on. Rendering the body better must not renumber them.
func TestPublishStatusClassesAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		status int
		class  string
	}{
		{http.StatusUnauthorized, "publish_auth:"},
		{http.StatusForbidden, "publish_auth:"},
		{http.StatusUnprocessableEntity, "publish_rejected:"},
		{http.StatusServiceUnavailable, "publish_unavailable:"},
		{http.StatusRequestEntityTooLarge, "publish_too_large:"},
		{http.StatusInternalServerError, "publish_failed:"},
	} {
		got := publishErr(t, tc.status, `{"detail":"nope"}`)
		if !strings.HasPrefix(got, tc.class) {
			t.Fatalf("publish(%d) = %q; want the %q class", tc.status, got, tc.class)
		}
	}
}

// An empty error body must not leave a dangling separator.
func TestPublishSilentRefusalKeepsACleanMessage(t *testing.T) {
	got := publishErr(t, http.StatusBadRequest, "")
	if strings.HasSuffix(got, ": ") || strings.HasSuffix(got, ":") {
		t.Fatalf("publish(400, empty body) = %q; an empty body left a dangling separator", got)
	}
}
