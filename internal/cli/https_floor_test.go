package cli

import (
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// The scheme floor: a hosted base URL that is not https:// (and not loopback) must
// be refused BEFORE any disk read or network call, for every command that egresses
// brain content. These tests are the regression fence for the plaintext-egress
// finding: ENTIRE_API_URL=http://evil.example used to ship brain content and the
// bearer token in the clear to whatever that name resolved to.

const evilPlaintextURL = "http://evil.example"

// assertNoNetworkAttempt fails when the error looks like the command actually tried
// to reach the host — a refusal must happen before the dial, so a DNS/dial error is
// proof the guard did not hold.
func assertNoNetworkAttempt(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, dialErr := range []string{"dial tcp", "no such host", "connection refused", "lookup "} {
		if strings.Contains(err.Error(), dialErr) {
			t.Fatalf("a network call was attempted before the URL was refused: %v", err)
		}
	}
}

// newHostedSyncFixture scripts one syncable fact so `facts sync` reaches the
// hosted-target URL checks rather than stopping at an empty store.
func newHostedSyncFixture(t *testing.T) verifyFixture {
	t.Helper()
	f := newVerifyFixture(t)
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	f.writeFacts(t, "main", []factRecord{{
		ID: factRecordID("deploys use a canary rollout", paths), Paths: paths,
		Text: "deploys use a canary rollout", Branch: "main",
		Origin: factstore.OriginDistilled, Status: factStatusActive,
		CreatedAt: f.now, UpdatedAt: f.now,
	}})
	return f
}

func TestFactsSyncRefusesPlaintextNonLoopbackAPIURL(t *testing.T) {
	f := newHostedSyncFixture(t)

	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_BRAIN_ALLOW_INSECURE_API_URL", "")
	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_API_URL", evilPlaintextURL)
	t.Setenv("ENTIRE_API_TOKEN", "tok")
	t.Setenv("ENTIRE_REPO_ID", "repo-01HZZ")

	out, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http")
	if err == nil {
		t.Fatalf("facts sync to %s succeeded; want refusal\n%s", evilPlaintextURL, out)
	}
	assertNoNetworkAttempt(t, err)
	if !strings.Contains(err.Error(), "insecure_api_url") {
		t.Fatalf("facts sync error = %v; want an insecure_api_url refusal", err)
	}
}

// TestHostedCommandsRejectMalformedAPIURL: a relative, schemeless, or non-HTTP base
// URL is a configuration error refused up front rather than a confusing later
// failure — and never a plaintext or non-HTTP egress.
func TestHostedCommandsRejectMalformedAPIURL(t *testing.T) {
	for _, raw := range []string{
		"api.entire.io",       // schemeless
		"//api.entire.io",     // protocol-relative
		"/api/v1",             // relative path
		"ftp://api.entire.io", // non-HTTP scheme
		"file:///etc/passwd",  // local file scheme
		"https://",            // no host
		"not a url at all",    // garbage
	} {
		t.Run(raw, func(t *testing.T) {
			f := newHostedSyncFixture(t)

			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
			t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
			// Even the plaintext opt-in must not rescue a malformed URL.
			t.Setenv("ENTIRE_BRAIN_ALLOW_INSECURE_API_URL", "1")
			t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
			t.Setenv("ENTIRE_API_URL", raw)
			t.Setenv("ENTIRE_API_TOKEN", "tok")
			t.Setenv("ENTIRE_REPO_ID", "repo-01HZZ")

			out, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http")
			if err == nil {
				t.Fatalf("facts sync with ENTIRE_API_URL=%q succeeded; want refusal\n%s", raw, out)
			}
			assertNoNetworkAttempt(t, err)
			if !strings.Contains(err.Error(), "invalid_api_url") {
				t.Fatalf("facts sync error = %v; want an invalid_api_url refusal", err)
			}
		})
	}
}

// TestFactsSyncAllowsPlaintextLoopbackAndOptIn is the carve-out fence: loopback http
// stays usable for local dev (and every existing httptest-based test), and a
// documented explicit opt-in re-enables non-loopback plaintext for the operators who
// knowingly need it. An accepted URL proceeds to the actual sync (which then fails
// on the unreachable port), so the assertion is only on the refusal class.
func TestFactsSyncAllowsPlaintextLoopbackAndOptIn(t *testing.T) {
	for _, tc := range []struct {
		name        string
		url         string
		optIn       string
		wantRefusal bool
	}{
		{name: "loopback ip", url: "http://127.0.0.1:9", optIn: ""},
		{name: "loopback name", url: "http://localhost:9", optIn: ""},
		{name: "loopback v6", url: "http://[::1]:9", optIn: ""},
		// host.invalid: non-loopback, and DNS fails instantly instead of a
		// 10s TEST-NET dial stall when the URL is accepted.
		{name: "opt-in", url: "http://host.invalid:9", optIn: "1"},
		{name: "opt-in garbage fails closed", url: "http://host.invalid:9", optIn: "ture", wantRefusal: true},
		{name: "opt-in false", url: "http://host.invalid:9", optIn: "0", wantRefusal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostedSyncFixture(t)

			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
			t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
			t.Setenv("ENTIRE_BRAIN_ALLOW_INSECURE_API_URL", tc.optIn)
			t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
			t.Setenv("ENTIRE_API_URL", tc.url)
			t.Setenv("ENTIRE_API_TOKEN", "tok")
			t.Setenv("ENTIRE_REPO_ID", "repo-01HZZ")

			_, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http")
			if err == nil {
				t.Fatalf("facts sync unexpectedly succeeded against an unreachable port")
			}
			refused := strings.Contains(err.Error(), "insecure_api_url")
			if refused != tc.wantRefusal {
				t.Fatalf("refused = %v (%v); want refused = %v", refused, err, tc.wantRefusal)
			}
		})
	}
}
