package httpx

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func respWith(body string) *http.Response {
	return &http.Response{Body: io.NopCloser(strings.NewReader(body))}
}

// The drift this package exists to close: entire-api answers a violated cap with an
// actionable RFC 9457 body, and the caller must be able to put that sentence in front
// of the user instead of "unexpected status 400 Bad Request".
func TestErrorDetailSurfacesHumaDetail(t *testing.T) {
	got := ErrorDetail(respWith(`{"title":"Bad Request","status":400,"detail":"branch is 600 bytes, limit is 512"}`))
	if got != "branch is 600 bytes, limit is 512" {
		t.Fatalf("ErrorDetail = %q; want the server's detail", got)
	}
	if s := SuffixFromBody([]byte(`{"detail":"branch is 600 bytes, limit is 512"}`)); s != ": branch is 600 bytes, limit is 512" {
		t.Fatalf("SuffixFromBody = %q", s)
	}
}

// huma puts request-validation specifics in errors[], leaving detail generic, so both
// have to survive into the rendered line.
func TestErrorDetailKeepsValidationSpecifics(t *testing.T) {
	got := ErrorDetail(respWith(`{"title":"Unprocessable Entity","detail":"validation failed","errors":[{"message":"expected length <= 512","location":"body.branch"}]}`))
	if !strings.Contains(got, "validation failed") || !strings.Contains(got, "expected length <= 512") || !strings.Contains(got, "body.branch") {
		t.Fatalf("ErrorDetail = %q; want detail plus the errors[] specifics", got)
	}
}

func TestErrorDetailFallsBackToTitleThenAlternateKeys(t *testing.T) {
	if got := ErrorDetail(respWith(`{"title":"Service Unavailable","status":503}`)); got != "Service Unavailable" {
		t.Fatalf("title fallback = %q", got)
	}
	if got := ErrorDetail(respWith(`{"error":"repo not enabled for brain"}`)); got != "repo not enabled for brain" {
		t.Fatalf("error-key fallback = %q", got)
	}
	if got := ErrorDetail(respWith(`{"message":"quota exceeded"}`)); got != "quota exceeded" {
		t.Fatalf("message-key fallback = %q", got)
	}
}

// An ingress HTML error page is not JSON. It must neither panic nor reach the user as
// a wall of markup: the page title is the useful part, and the whole line stays short.
func TestErrorDetailHTMLPageIsTitleNotAWallOfMarkup(t *testing.T) {
	page := "<!DOCTYPE html>\n<html>\n<head><title>502 Bad Gateway</title></head>\n<body>\n<center><h1>502 Bad Gateway</h1></center>\n<hr><center>nginx</center>\n" +
		strings.Repeat("<div>padding padding padding</div>\n", 40000) + "</body>\n</html>\n"
	got := ErrorDetail(respWith(page))
	if got != "502 Bad Gateway" {
		t.Fatalf("ErrorDetail(html) = %q; want the page title", got)
	}
}

// A body with no title, no JSON, and no end: bounded, single-line, no panic.
func TestErrorDetailUntitledGiantBodyIsBounded(t *testing.T) {
	got := ErrorDetail(respWith(strings.Repeat("A", 4<<20)))
	if len([]rune(got)) > maxErrorDetailRunes+1 {
		t.Fatalf("ErrorDetail length = %d runes; want <= %d", len([]rune(got)), maxErrorDetailRunes+1)
	}
	if got == "" {
		t.Fatalf("ErrorDetail dropped a non-empty body entirely")
	}
}

// Control characters and invalid UTF-8 from a hostile or broken server must not be
// pasted into a terminal verbatim.
func TestErrorDetailSanitizesControlBytesAndInvalidUTF8(t *testing.T) {
	got := ErrorDetail(respWith("bad\x00\x1b[31mred\x07\nrequest\xff\xfe here"))
	if strings.ContainsAny(got, "\x00\x1b\x07\n\r\t") {
		t.Fatalf("ErrorDetail = %q; control bytes survived", got)
	}
	if !strings.Contains(got, "bad") || !strings.Contains(got, "request") {
		t.Fatalf("ErrorDetail = %q; text was lost", got)
	}
	if strings.Contains(got, "���") {
		t.Fatalf("ErrorDetail = %q; invalid UTF-8 was not cleaned", got)
	}
}

// Nothing to say stays nothing: an empty body must not append a bare ": ".
func TestErrorDetailEmptyBodyAddsNothing(t *testing.T) {
	if got := ErrorDetail(respWith("")); got != "" {
		t.Fatalf("ErrorDetail(empty) = %q", got)
	}
	if got := ErrorDetail(respWith("   \n\t ")); got != "" {
		t.Fatalf("ErrorDetail(blank) = %q", got)
	}
	if got := ErrorSuffix(respWith("")); got != "" {
		t.Fatalf("ErrorSuffix(empty) = %q; want no separator", got)
	}
	if got := ErrorDetail(nil); got != "" {
		t.Fatalf("ErrorDetail(nil) = %q", got)
	}
	if got := ErrorDetail(&http.Response{}); got != "" {
		t.Fatalf("ErrorDetail(no body) = %q", got)
	}
}

// The read is capped, so a server streaming forever cannot be used to exhaust the
// client's memory through its own error path.
func TestErrorDetailReadIsCapped(t *testing.T) {
	counting := &countingReader{}
	if got := ErrorDetail(&http.Response{Body: io.NopCloser(counting)}); got == "" {
		t.Fatalf("ErrorDetail read nothing from an endless body")
	}
	if counting.n > maxErrorBodyBytes+1024 {
		t.Fatalf("read %d bytes from an endless body; cap is %d", counting.n, maxErrorBodyBytes)
	}
}

type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	c.n += len(p)
	return len(p), nil
}

func TestErrorDetailHTMLUnicodeOffsets(t *testing.T) {
	for _, body := range []string{
		"<div>KKK</div><TITLE>upstream unavailable</TITLE>",
		"<TITLE data-note=\"KKKKKKKKKK\">upstream unavailable</TITLE>",
	} {
		if got := ErrorDetailFromBody([]byte(body)); got != "upstream unavailable" {
			t.Fatalf("got %q", got)
		}
	}
}
