package httpx

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the other half of the outbound HTTP policy: what the caller is allowed
// to LEARN when the hosted API says no.
//
// The hosted brain endpoints answer a violated cap with an actionable RFC 9457 body —
//
//	400 {"title":"Bad Request","status":400,"detail":"branch is 600 bytes, limit is 512"}
//
// — and a client that reads only resp.Status turns that into "unexpected status 400
// Bad Request", which tells the member neither what was wrong nor what the limit is.
// Every hosted call site therefore renders the body through here.
//
// Reading an error body is not free of hazards, and all three are handled once, here,
// rather than at each of the dozen call sites:
//
//	SIZE       the body may not be the small JSON envelope the endpoint documents.
//	           An ingress or proxy in front of entire-api answers 502/504 with an
//	           HTML page, and a broken upstream can stream without end, so the read
//	           is bounded (maxErrorBodyBytes) and the rendered line is bounded again
//	           (maxErrorDetailRunes).
//	SHAPE      JSON when the endpoint answered, HTML when something in front of it
//	           did. An HTML page's <title> is the whole of its useful content
//	           ("502 Bad Gateway"); its markup is noise.
//	CONTENT    the bytes are attacker-influenced and are about to be printed to a
//	           terminal, so control characters (ANSI escapes, NUL, newlines that
//	           would forge a second log line) and invalid UTF-8 are stripped.
//
// It renders "" for a body with nothing to say, and the Suffix forms then add no
// separator — a caller's message is never left ending in a bare ": ".

// maxErrorBodyBytes bounds how much of a non-2xx body is read. The documented error
// envelopes are a few hundred bytes; 8 KiB is generous for one and small enough that
// an endless body costs nothing.
const maxErrorBodyBytes = 8 << 10

// maxErrorDetailRunes bounds the rendered line. It is measured in runes, not bytes,
// so truncation cannot split a multi-byte character.
const maxErrorDetailRunes = 200

// errorEnvelope is the union of the error shapes the hosted API and its front doors
// emit: huma/RFC 9457 (title/detail/errors) plus the two bare-key spellings that
// appear from middleware.
type errorEnvelope struct {
	Detail  string `json:"detail"`
	Title   string `json:"title"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Errors  []struct {
		Message  string `json:"message"`
		Location string `json:"location"`
	} `json:"errors"`
}

// ErrorDetail reads a bounded prefix of resp.Body and renders the server's objection
// as one short, terminal-safe line, or "" when the body says nothing useful.
//
// It is for NON-2xx responses only: it consumes from the body, so calling it on a
// success response would eat the payload. Safe on a nil response and on a response
// with no body.
func ErrorDetail(resp *http.Response) string {
	if resp == nil || resp.Body == nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return ErrorDetailFromBody(raw)
}

// ErrorSuffix is ErrorDetail formatted for appending to an error message: ": <detail>",
// or "" when there is no detail.
func ErrorSuffix(resp *http.Response) string { return suffix(ErrorDetail(resp)) }

// ErrorDetailFromBody is ErrorDetail for a caller that already holds the bytes (the
// publish path reads the body once and branches on status afterwards). The caller
// owns the read bound in that case; the rendered line is bounded here regardless.
func ErrorDetailFromBody(raw []byte) string {
	if len(raw) > maxErrorBodyBytes {
		raw = raw[:maxErrorBodyBytes]
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return ""
	}
	if detail := jsonDetail(trimmed); detail != "" {
		return clean(detail)
	}
	if title := htmlTitle(trimmed); title != "" {
		return clean(title)
	}
	return clean(trimmed)
}

// SuffixFromBody is ErrorDetailFromBody formatted for appending to an error message.
func SuffixFromBody(raw []byte) string { return suffix(ErrorDetailFromBody(raw)) }

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

// jsonDetail pulls the human-readable part out of a JSON error envelope.
//
// detail and errors[] are COMBINED rather than ranked, because huma splits them: a
// request-validation failure carries the generic detail "validation failed" and puts
// the specifics ("expected length <= 512" at "body.branch") in errors[]. Ranking
// either one first would throw away the half that names the problem.
func jsonDetail(body string) string {
	if !strings.HasPrefix(body, "{") {
		return ""
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return ""
	}
	lead := firstNonEmpty(env.Detail, env.Error, env.Message, env.Title)
	var specifics []string
	for _, e := range env.Errors {
		switch {
		case e.Message != "" && e.Location != "":
			specifics = append(specifics, e.Location+": "+e.Message)
		case e.Message != "":
			specifics = append(specifics, e.Message)
		}
	}
	switch {
	case lead != "" && len(specifics) > 0:
		return lead + " (" + strings.Join(specifics, "; ") + ")"
	case lead != "":
		return lead
	case len(specifics) > 0:
		return strings.Join(specifics, "; ")
	}
	return ""
}

// htmlTitle returns the <title> of an HTML error page. A proxy's 502 page says
// everything it has to say in its title; the surrounding markup is noise that would
// otherwise fill the member's whole line.
func htmlTitle(body string) string {
	lower := strings.ToLower(body)
	if !strings.HasPrefix(lower, "<") {
		return ""
	}
	open := strings.Index(lower, "<title")
	if open < 0 {
		return ""
	}
	gt := strings.IndexByte(body[open:], '>')
	if gt < 0 {
		return ""
	}
	start := open + gt + 1
	end := strings.Index(lower[start:], "</title>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(body[start : start+end])
}

// clean makes an arbitrary server string safe and short enough to print: valid UTF-8,
// no control characters, no internal line breaks, and at most maxErrorDetailRunes
// runes followed by an ellipsis.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) > maxErrorDetailRunes {
		out = string([]rune(out)[:maxErrorDetailRunes]) + "…"
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
