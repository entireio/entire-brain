package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

// Webhooks are the only outbound network traffic this binary makes, so most of
// what follows tests the conditions under which it must make NONE. Each of
// these asserts a promise in SECURITY.md or the command's own help; a test that
// cannot fail when the corresponding guard is deleted is not testing anything,
// so each one was checked against a weakened build.

// --- the guards -------------------------------------------------------------

// The default build makes no network calls. With no URL configured there must
// be no request, and emit must not report that as an error either: "webhooks
// are not set up" is the normal state, not a failure.
func TestWebhookSendsNothingWhenNoURLIsConfigured(t *testing.T) {
	rec := &clitest.WebhookRecorder{}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	t.Setenv(WebhookURLEnv, "") // explicitly unset, not merely absent

	if err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest}); err != nil {
		t.Fatalf("an unconfigured webhook reported an error: %v", err)
	}
	if rec.Count() != 0 {
		t.Fatalf("a request was made with no %s set", WebhookURLEnv)
	}
}

// The egress kill switch outranks the webhook configuration. If a configured
// URL could win, a local-only declaration would mean nothing.
func TestEgressToggleSilencesAConfiguredWebhook(t *testing.T) {
	for _, toggle := range []string{"ENTIRE_BRAIN_NO_EGRESS", "ENTIRE_BRAIN_LOCAL_ONLY"} {
		t.Run(toggle, func(t *testing.T) {
			rec := clitest.WebhookEndpointServer(t, WebhookURLEnv)
			t.Setenv(toggle, "1")

			if err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest}); err != nil {
				t.Fatalf("emit: %v", err)
			}
			if rec.Count() != 0 {
				t.Fatalf("%s did not stop the webhook: %d request(s) delivered", toggle, rec.Count())
			}
			if _, ok, reason := webhookEndpoint(); ok || !strings.Contains(reason, toggle) {
				t.Fatalf("webhookEndpoint should name %s as the reason, got ok=%v reason=%q", toggle, ok, reason)
			}
		})
	}
}

// The toggles are fail-closed: a value nobody recognises must disable egress,
// not enable it. A typo'd "ture" that silently left webhooks firing is exactly
// the failure securityToggleEnabled exists to prevent, and webhooks inherit it
// only because they route through brainNoEgressMode rather than reading the
// variable themselves.
func TestUnrecognizedEgressToggleValueStillSilencesWebhooks(t *testing.T) {
	rec := clitest.WebhookEndpointServer(t, WebhookURLEnv)
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "ture")

	if err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if rec.Count() != 0 {
		t.Fatalf("a misspelled toggle value left webhooks enabled: %d request(s)", rec.Count())
	}
}

// A file:// "webhook" is not a webhook. Admitting non-HTTP schemes would turn
// one environment variable into a write primitive.
func TestWebhookRefusesNonHTTPSchemes(t *testing.T) {
	// The message matters as much as the refusal. file:///etc/passwd parses
	// with an empty host, so a host check that runs first reports "not a valid
	// URL" — which sends somebody off to fix a URL that is perfectly valid and
	// simply not allowed. Each case pins the objection the user should read.
	for raw, wantInReason := range map[string]string{
		"file:///etc/passwd":          "must be http or https, not file",
		"unix:///var/run/docker.sock": "must be http or https, not unix",
		"ftp://example.com/hook":      "must be http or https, not ftp",
		"not-a-url":                   "must be http or https, not no scheme",
		"/just/a/path":                "must be http or https, not no scheme",
		"https://":                    "no host",
	} {
		t.Setenv(WebhookURLEnv, raw)
		endpoint, ok, reason := webhookEndpoint()
		if ok {
			t.Fatalf("%s was accepted as a webhook endpoint (%q)", raw, endpoint)
		}
		if !strings.Contains(reason, wantInReason) {
			t.Fatalf("%s was rejected with %q, which does not say %q", raw, reason, wantInReason)
		}
	}
}

// Fact text stays on the machine by default. This is the difference between a
// notification and a feed of everything the brain knows.
func TestFactWebhookWithholdsTextByDefault(t *testing.T) {
	t.Setenv(WebhookIncludeTextEnv, "")
	fact := clitest.SampleWebhookFact()
	event := newFactWebhookEvent(WebhookFactRecorded, webhookRepoName("/home/someone/work/myrepo"), "main", fact, clitest.WebhookTestNow())

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), fact.Text) {
		t.Fatalf("the fact text was in the payload: %s", encoded)
	}
	if event.Fact.Text != "" {
		t.Fatalf("Text should be empty, got %q", event.Fact.Text)
	}
	// Redaction must not cost the subscriber the ability to act: the id, kind
	// and paths are what make the event useful, and they must survive.
	if event.Fact.ID != fact.ID || event.Fact.Kind != fact.Kind || len(event.Fact.Paths) != 1 {
		t.Fatalf("redaction removed more than the text: %+v", event.Fact)
	}
	// The repository is identified by name, not by a path that discloses the
	// layout of somebody's home directory.
	if event.Repo != "myrepo" {
		t.Fatalf("Repo = %q, want the directory name", event.Repo)
	}
	if strings.Contains(string(encoded), "/home/someone") {
		t.Fatalf("the payload leaked a filesystem path: %s", encoded)
	}
}

// Opting in is the whole point of the default being safe: a user who wants the
// text in their channel can have it.
func TestFactWebhookIncludesTextWhenOptedIn(t *testing.T) {
	t.Setenv(WebhookIncludeTextEnv, "1")
	fact := clitest.SampleWebhookFact()
	event := newFactWebhookEvent(WebhookFactRecorded, webhookRepoName("/repo"), "main", fact, clitest.WebhookTestNow())
	if event.Fact.Text != fact.Text {
		t.Fatalf("opt-in did not include the text: %q", event.Fact.Text)
	}
}

// A redirect is refused, not followed. The payload and its signature must not
// reach a host the user never configured — including a downgrade to plaintext
// http, which is how a redirect-following client leaks credentials.
func TestWebhookRefusesToFollowRedirects(t *testing.T) {
	final := &clitest.WebhookRecorder{}
	finalServer := httptest.NewServer(final)
	t.Cleanup(finalServer.Close)

	rec := clitest.WebhookEndpointServer(t, WebhookURLEnv)
	rec.Mu.Lock()
	rec.Redirect = finalServer.URL
	rec.Mu.Unlock()
	t.Setenv(webhookSecretEnv, "topsecret")

	err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest})
	if err == nil {
		t.Fatal("a redirecting endpoint was reported as a successful delivery")
	}
	if final.Count() != 0 {
		t.Fatalf("the payload was delivered to the redirect target (%d request(s))", final.Count())
	}
	// The error has to say what to do about it, or the user is left with a
	// delivery that fails for no visible reason.
	if !strings.Contains(err.Error(), WebhookURLEnv) {
		t.Fatalf("the redirect error should name %s: %v", WebhookURLEnv, err)
	}
}

// A receiver can only trust an event it can attribute. The signature has to be
// over the exact bytes sent, or verification fails for everyone who implements
// it correctly.
func TestWebhookSignsThePayloadItActuallySends(t *testing.T) {
	rec := clitest.WebhookEndpointServer(t, WebhookURLEnv)
	const secret = "shared-secret"
	t.Setenv(webhookSecretEnv, secret)

	if err := emitWebhook(context.Background(), newFactWebhookEvent(
		WebhookFactRecorded, webhookRepoName("/repo"), "main", clitest.SampleWebhookFact(), clitest.WebhookTestNow())); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := rec.Last(t)
	header := got.Headers.Get("X-Entire-Signature-256")
	if header == "" {
		t.Fatal("no signature header was sent")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(got.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(header), []byte(want)) {
		t.Fatalf("signature %s does not verify over the delivered body (want %s)", header, want)
	}
	if event := got.Headers.Get("X-Entire-Event"); event != WebhookFactRecorded {
		t.Fatalf("X-Entire-Event = %q, want %q", event, WebhookFactRecorded)
	}
}

// Without a secret there is no signature header at all, rather than one
// computed over an empty key — which would verify for anybody who guessed that
// the key was empty and give a false sense of authentication.
func TestWebhookIsUnsignedWithoutASecret(t *testing.T) {
	rec := clitest.WebhookEndpointServer(t, WebhookURLEnv)
	t.Setenv(webhookSecretEnv, "")

	if err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if header := rec.Last(t).Headers.Get("X-Entire-Signature-256"); header != "" {
		t.Fatalf("a signature was sent with no secret configured: %q", header)
	}
}

// An endpoint that rejects the POST is a delivery failure, not a success. A
// webhook that silently swallows a 500 is indistinguishable from one that works.
func TestWebhookTreatsAnErrorStatusAsAFailure(t *testing.T) {
	rec := clitest.WebhookEndpointServer(t, WebhookURLEnv)
	rec.Mu.Lock()
	rec.Status = http.StatusInternalServerError
	rec.Mu.Unlock()

	err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest})
	if err == nil {
		t.Fatal("a 500 from the endpoint was reported as a successful delivery")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("the error should carry the status: %v", err)
	}
}

// notifyWebhook is the write paths' entry point and must never propagate a
// failure, only report it.
func TestNotifyWebhookWarnsAndDoesNotPropagate(t *testing.T) {
	t.Setenv(WebhookURLEnv, "http://127.0.0.1:1/definitely-not-listening")
	errOut := &bytes.Buffer{}
	notifyWebhook(context.Background(), errOut, WebhookEvent{Event: WebhookTest})
	if !strings.Contains(errOut.String(), "not delivered") {
		t.Fatalf("a failed delivery produced no warning: %q", errOut.String())
	}
}

// `webhook events` is the documentation people will actually read. A new event
// with no description ships a blank line next to its name.
func TestWebhookEventsCommandDescribesEveryEvent(t *testing.T) {
	cmd := newWebhookEventsCommand()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		name, description, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || strings.TrimSpace(description) == "" {
			t.Fatalf("event %q was listed with no description", name)
		}
	}
	for _, event := range WebhookEventNames() {
		if !strings.Contains(out.String(), event) {
			t.Fatalf("%s is emitted but `webhook events` does not list it", event)
		}
	}
}

// The endpoint has exactly one source: an environment variable. This is the
// property the decision not to filter private addresses rests on, so it is
// worth holding — if a config file or any second-party input ever reaches this,
// the address filtering belongs back on the list.
func TestWebhookEndpointComesOnlyFromTheEnvironment(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".", "webhooks.go"))
	if err != nil {
		t.Fatalf("read webhooks.go: %v", err)
	}
	source := string(data)
	for _, forbidden := range []string{"os.ReadFile", "os.Open", "internal/cli/internal/manifest", "settings"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("webhooks.go references %q; the endpoint may no longer be operator-only input, "+
				"which is what the absence of private-address filtering depends on", forbidden)
		}
	}
	if strings.Count(source, "os.Getenv(WebhookURLEnv)") != 1 {
		t.Fatal("the endpoint is read from somewhere other than the one environment variable")
	}
}

// Delivery is on the critical path of every command that triggers it, and
// `remember` is called by agents often. The timeout is therefore a latency
// budget, and a slow endpoint must cost about that and not more.
func TestASlowEndpointCostsAtMostTheTimeout(t *testing.T) {
	stall := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stall // accept the connection, then never respond
	}))
	// Cleanups run last-registered-first, so the stall must be released AFTER
	// this is registered: Close waits for outstanding handlers, and releasing
	// second would deadlock the test rather than the product.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(stall) })
	t.Setenv(WebhookURLEnv, server.URL)
	t.Setenv(webhookTimeoutEnv, "300ms")

	start := time.Now()
	err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a stalled endpoint was reported as a successful delivery")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("a stalled endpoint blocked for %s; the timeout is not bounding it", elapsed)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("delivery gave up after %s, well before the configured timeout", elapsed)
	}
}

func TestWebhookTimeoutIsConfigurableAndFailsSafe(t *testing.T) {
	t.Setenv(webhookTimeoutEnv, "")
	if got := webhookTimeout(); got != defaultWebhookTimeout {
		t.Fatalf("default = %s, want %s", got, defaultWebhookTimeout)
	}
	t.Setenv(webhookTimeoutEnv, "7s")
	if got := webhookTimeout(); got != 7*time.Second {
		t.Fatalf("configured = %s, want 7s", got)
	}
	// Nonsense and non-positive values fall back rather than disabling the
	// bound — a zero or negative deadline would make every delivery fail
	// instantly, which looks exactly like an unreachable endpoint.
	for _, bad := range []string{"soon", "0", "-5s", "5"} {
		t.Setenv(webhookTimeoutEnv, bad)
		if got := webhookTimeout(); got != defaultWebhookTimeout {
			t.Fatalf("%q gave %s, want the default", bad, got)
		}
	}
}

// The default has to stay a budget. Raising it silently would put seconds back
// onto every `remember` that has a webhook configured.
func TestDefaultWebhookTimeoutStaysABudget(t *testing.T) {
	if defaultWebhookTimeout > 2*time.Second {
		t.Fatalf("the default timeout is %s; delivery is synchronous, so this is charged to every triggering command",
			defaultWebhookTimeout)
	}
}

// For many receivers the URL is the credential — a Slack or Discord incoming
// webhook is a secret path. The transport error embeds the whole URL and is
// printed to stderr on every transient failure, into CI logs, agent transcripts
// and error monitoring. The host is enough to diagnose; the rest is a secret.
func TestDeliveryFailureDoesNotLeakTheEndpoint(t *testing.T) {
	const secretPath = "/services/T00000000/B00000000/XXXXXXXXXXXXsecrettoken"
	// A port nothing is listening on, so the POST fails at the transport.
	t.Setenv(WebhookURLEnv, "http://127.0.0.1:1"+secretPath+"?token=alsosecret")

	err := emitWebhook(context.Background(), WebhookEvent{Event: WebhookTest})
	if err == nil {
		t.Fatal("delivery to a dead port reported success")
	}
	message := err.Error()
	for _, secret := range []string{secretPath, "secrettoken", "alsosecret"} {
		if strings.Contains(message, secret) {
			t.Fatalf("the delivery error leaks the endpoint: %q", message)
		}
	}
	// And the same through the path the write commands actually use.
	errOut := &bytes.Buffer{}
	notifyWebhook(context.Background(), errOut, WebhookEvent{Event: WebhookTest})
	for _, secret := range []string{secretPath, "secrettoken", "alsosecret"} {
		if strings.Contains(errOut.String(), secret) {
			t.Fatalf("the stderr warning leaks the endpoint: %q", errOut.String())
		}
	}
	// It still has to be diagnosable: the host and the reason must survive.
	if !strings.Contains(message, "127.0.0.1") {
		t.Fatalf("redaction removed the host, leaving nothing to diagnose: %q", message)
	}
	if !strings.Contains(strings.ToLower(message), "refused") && !strings.Contains(strings.ToLower(message), "connect") {
		t.Fatalf("redaction removed the reason: %q", message)
	}
}
