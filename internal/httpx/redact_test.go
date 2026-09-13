package httpx

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A hosted endpoint that echoes the request's Authorization header into its error
// envelope gets the caller's own bearer token rendered into the member's stderr,
// from where it reaches terminal scrollback, a CI job log, and any pasted bug
// report. The peer already has the token; what it must not get to decide is that
// the token ends up somewhere durable and shared.
func TestRedactRemovesTheCallersToken(t *testing.T) {
	const token = "entire_pat_0123456789abcdef"
	body := `{"detail":"bad credential: Bearer ` + token + ` is not valid"}`
	resp := &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(body))}

	got := Redact(ErrorSuffix(resp), token)
	if strings.Contains(got, token) {
		t.Fatalf("the rendered detail still contains the bearer token: %q", got)
	}
	if !strings.Contains(got, redactedPlaceholder) {
		t.Errorf("the redaction left no placeholder behind: %q", got)
	}
	// The rest of the server's objection must survive, or the member loses the
	// explanation along with the token.
	if !strings.Contains(got, "bad credential") || !strings.Contains(got, "is not valid") {
		t.Errorf("redaction destroyed the server's message: %q", got)
	}
}

func TestRedactLeavesUnrelatedTextAlone(t *testing.T) {
	const token = "entire_pat_0123456789abcdef"
	if got := Redact("branch is 600 bytes, limit is 512", token); got != "branch is 600 bytes, limit is 512" {
		t.Errorf("Redact rewrote a detail that never mentioned the token: %q", got)
	}
}

// An unauthenticated client has no token to protect, and a short or accidental value
// must not turn coincidental substrings into placeholders.
func TestRedactIgnoresAnEmptyOrTinySecret(t *testing.T) {
	for _, secret := range []string{"", "a", "abc", "1234567"} {
		const detail = "abc is fine and 1234567 too"
		if got := Redact(detail, secret); got != detail {
			t.Errorf("Redact(%q) with secret %q rewrote the text: %q", detail, secret, got)
		}
	}
}
