package cli

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
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// Webhooks are the only outbound network traffic this binary makes, so most of
// what follows tests the conditions under which it must make NONE. Each of
// these asserts a promise in SECURITY.md or the command's own help; a test that
// cannot fail when the corresponding guard is deleted is not testing anything,
// so each one was checked against a weakened build.

// recorder is a fake endpoint that remembers what it was sent.
type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
	status   int
	redirect string
}

type recordedRequest struct {
	body    []byte
	headers http.Header
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body := &bytes.Buffer{}
	_, _ = body.ReadFrom(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, recordedRequest{body: body.Bytes(), headers: req.Header.Clone()})
	redirect, status := r.redirect, r.status
	r.mu.Unlock()
	if redirect != "" {
		// 307 is the interesting case: unlike 302 it preserves the method and
		// body, so a client that follows it re-delivers the payload.
		http.Redirect(w, req, redirect, http.StatusTemporaryRedirect)
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *recorder) last(t *testing.T) recordedRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("the endpoint was never called")
	}
	return r.requests[len(r.requests)-1]
}

// webhookEndpointServer starts a fake endpoint and points the brain at it.
func webhookEndpointServer(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	t.Setenv(webhookURLEnv, server.URL)
	return rec
}

func sampleFact() factRecord {
	return factRecord{
		ID:    "fact-1",
		Kind:  "invariant",
		Paths: []string{"architecture.storage.invariant"},
		Text:  "Refs are written through a Postgres compare-and-swap.",
	}
}

func testNow() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC) }

// --- the guards -------------------------------------------------------------

// The default build makes no network calls. With no URL configured there must
// be no request, and emit must not report that as an error either: "webhooks
// are not set up" is the normal state, not a failure.
func TestWebhookSendsNothingWhenNoURLIsConfigured(t *testing.T) {
	rec := &recorder{}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	t.Setenv(webhookURLEnv, "") // explicitly unset, not merely absent

	if err := emitWebhook(context.Background(), webhookEvent{Event: WebhookTest}); err != nil {
		t.Fatalf("an unconfigured webhook reported an error: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("a request was made with no %s set", webhookURLEnv)
	}
}

// The egress kill switch outranks the webhook configuration. If a configured
// URL could win, a local-only declaration would mean nothing.
func TestEgressToggleSilencesAConfiguredWebhook(t *testing.T) {
	for _, toggle := range []string{"ENTIRE_BRAIN_NO_EGRESS", "ENTIRE_BRAIN_LOCAL_ONLY"} {
		t.Run(toggle, func(t *testing.T) {
			rec := webhookEndpointServer(t)
			t.Setenv(toggle, "1")

			if err := emitWebhook(context.Background(), webhookEvent{Event: WebhookTest}); err != nil {
				t.Fatalf("emit: %v", err)
			}
			if rec.count() != 0 {
				t.Fatalf("%s did not stop the webhook: %d request(s) delivered", toggle, rec.count())
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
	rec := webhookEndpointServer(t)
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "ture")

	if err := emitWebhook(context.Background(), webhookEvent{Event: WebhookTest}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("a misspelled toggle value left webhooks enabled: %d request(s)", rec.count())
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
		t.Setenv(webhookURLEnv, raw)
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
	t.Setenv(webhookIncludeTextEnv, "")
	fact := sampleFact()
	event := newFactWebhookEvent(WebhookFactRecorded, "/home/someone/work/myrepo", "main", fact, testNow())

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
	t.Setenv(webhookIncludeTextEnv, "1")
	fact := sampleFact()
	event := newFactWebhookEvent(WebhookFactRecorded, "/repo", "main", fact, testNow())
	if event.Fact.Text != fact.Text {
		t.Fatalf("opt-in did not include the text: %q", event.Fact.Text)
	}
}

// A redirect is refused, not followed. The payload and its signature must not
// reach a host the user never configured — including a downgrade to plaintext
// http, which is how a redirect-following client leaks credentials.
func TestWebhookRefusesToFollowRedirects(t *testing.T) {
	final := &recorder{}
	finalServer := httptest.NewServer(final)
	t.Cleanup(finalServer.Close)

	rec := webhookEndpointServer(t)
	rec.mu.Lock()
	rec.redirect = finalServer.URL
	rec.mu.Unlock()
	t.Setenv(webhookSecretEnv, "topsecret")

	err := emitWebhook(context.Background(), webhookEvent{Event: WebhookTest})
	if err == nil {
		t.Fatal("a redirecting endpoint was reported as a successful delivery")
	}
	if final.count() != 0 {
		t.Fatalf("the payload was delivered to the redirect target (%d request(s))", final.count())
	}
	// The error has to say what to do about it, or the user is left with a
	// delivery that fails for no visible reason.
	if !strings.Contains(err.Error(), webhookURLEnv) {
		t.Fatalf("the redirect error should name %s: %v", webhookURLEnv, err)
	}
}

// A receiver can only trust an event it can attribute. The signature has to be
// over the exact bytes sent, or verification fails for everyone who implements
// it correctly.
func TestWebhookSignsThePayloadItActuallySends(t *testing.T) {
	rec := webhookEndpointServer(t)
	const secret = "shared-secret"
	t.Setenv(webhookSecretEnv, secret)

	if err := emitWebhook(context.Background(), newFactWebhookEvent(
		WebhookFactRecorded, "/repo", "main", sampleFact(), testNow())); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got := rec.last(t)
	header := got.headers.Get("X-Entire-Signature-256")
	if header == "" {
		t.Fatal("no signature header was sent")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(got.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(header), []byte(want)) {
		t.Fatalf("signature %s does not verify over the delivered body (want %s)", header, want)
	}
	if event := got.headers.Get("X-Entire-Event"); event != WebhookFactRecorded {
		t.Fatalf("X-Entire-Event = %q, want %q", event, WebhookFactRecorded)
	}
}

// Without a secret there is no signature header at all, rather than one
// computed over an empty key — which would verify for anybody who guessed that
// the key was empty and give a false sense of authentication.
func TestWebhookIsUnsignedWithoutASecret(t *testing.T) {
	rec := webhookEndpointServer(t)
	t.Setenv(webhookSecretEnv, "")

	if err := emitWebhook(context.Background(), webhookEvent{Event: WebhookTest}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if header := rec.last(t).headers.Get("X-Entire-Signature-256"); header != "" {
		t.Fatalf("a signature was sent with no secret configured: %q", header)
	}
}

// An endpoint that rejects the POST is a delivery failure, not a success. A
// webhook that silently swallows a 500 is indistinguishable from one that works.
func TestWebhookTreatsAnErrorStatusAsAFailure(t *testing.T) {
	rec := webhookEndpointServer(t)
	rec.mu.Lock()
	rec.status = http.StatusInternalServerError
	rec.mu.Unlock()

	err := emitWebhook(context.Background(), webhookEvent{Event: WebhookTest})
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
	t.Setenv(webhookURLEnv, "http://127.0.0.1:1/definitely-not-listening")
	errOut := &bytes.Buffer{}
	notifyWebhook(context.Background(), errOut, webhookEvent{Event: WebhookTest})
	if !strings.Contains(errOut.String(), "not delivered") {
		t.Fatalf("a failed delivery produced no warning: %q", errOut.String())
	}
}

// --- the events -------------------------------------------------------------

// Every event name this brain advertises must be sent by real code somewhere
// outside the webhook plumbing. A list a consumer can subscribe to and never
// receive anything from is worse than a shorter list.
func TestEveryAdvertisedEventIsActuallyEmitted(t *testing.T) {
	constantFor := map[string]string{
		WebhookFactRecorded:   "WebhookFactRecorded",
		WebhookFactRetracted:  "WebhookFactRetracted",
		WebhookBrainRefreshed: "WebhookBrainRefreshed",
		WebhookTest:           "WebhookTest",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var sources []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// webhooks.go declares the constants; a reference there proves nothing.
		if name == "webhooks.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sources = append(sources, string(data))
	}
	for _, event := range webhookEventNames() {
		identifier, known := constantFor[event]
		if !known {
			t.Fatalf("event %q has no constant in this test; add it and the code that emits it", event)
		}
		found := false
		for _, source := range sources {
			if strings.Contains(source, identifier) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s (%s) is advertised but nothing outside webhooks.go emits it", event, identifier)
		}
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
	for _, event := range webhookEventNames() {
		if !strings.Contains(out.String(), event) {
			t.Fatalf("%s is emitted but `webhook events` does not list it", event)
		}
	}
}

// --- the commands -----------------------------------------------------------

func webhookCommandOptions(t *testing.T) Options {
	t.Helper()
	repoDir := t.TempDir()
	return Options{
		Version: "test",
		Env:     semanticTestEnv(t, repoDir),
		Runner:  semanticFixtureRunner(repoDir, ""),
		Now:     testNow,
	}
}

func runWebhookSubcommand(t *testing.T, cmd *cobra.Command) (string, error) {
	t.Helper()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, nil)
	return out.String(), err
}

// `webhook test` exists to answer "is this thing on". Reporting success for a
// send that never happened would be the one answer worse than no command.
func TestWebhookTestRefusesWhenWebhooksAreOff(t *testing.T) {
	t.Setenv(webhookURLEnv, "")
	_, err := runWebhookSubcommand(t, newWebhookTestCommand(webhookCommandOptions(t)))
	if err == nil {
		t.Fatal("`webhook test` reported success with no endpoint configured")
	}
	if !strings.Contains(err.Error(), webhookURLEnv) {
		t.Fatalf("the error should say what to set: %v", err)
	}
}

func TestWebhookTestDeliversToTheConfiguredEndpoint(t *testing.T) {
	rec := webhookEndpointServer(t)
	out, err := runWebhookSubcommand(t, newWebhookTestCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("`webhook test`: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected exactly one delivery, got %d", rec.count())
	}
	var payload webhookEvent
	if err := json.Unmarshal(rec.last(t).body, &payload); err != nil {
		t.Fatalf("the endpoint received something that is not an event: %v", err)
	}
	if payload.Event != WebhookTest {
		t.Fatalf("event = %q, want %q", payload.Event, WebhookTest)
	}
	if !strings.Contains(out, WebhookTest) {
		t.Fatalf("the command did not report what it sent: %q", out)
	}
}

// Silence is the failure mode webhooks have, so `status` must distinguish
// "never configured" from "configured but disabled" — and say so, rather than
// printing the same "off" for both.
func TestWebhookStatusExplainsWhyItIsOff(t *testing.T) {
	t.Setenv(webhookURLEnv, "")
	unset, err := runWebhookSubcommand(t, newWebhookStatusCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(unset, webhookURLEnv) {
		t.Fatalf("status should name the variable to set: %q", unset)
	}

	webhookEndpointServer(t)
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	gated, err := runWebhookSubcommand(t, newWebhookStatusCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(gated, "ENTIRE_BRAIN_NO_EGRESS") {
		t.Fatalf("status should blame the egress toggle, not a missing URL: %q", gated)
	}
	if gated == unset {
		t.Fatal("a URL configured but gated reads identically to no URL at all")
	}
	// The hint has to point at the thing that is actually wrong. Telling
	// somebody to set a variable they already set sends them to fix the wrong
	// knob, and they will conclude the feature is broken.
	if strings.Contains(gated, "set "+webhookURLEnv+"=https://") {
		t.Fatalf("status told the user to set a URL that is already set: %q", gated)
	}
	if !strings.Contains(gated, "unset") {
		t.Fatalf("status should say to unset the egress toggle: %q", gated)
	}

	// A URL that is set but unusable is a third state, and must not be reported
	// as "you have not configured one".
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv(webhookURLEnv, "ftp://example.com/hook")
	bad, err := runWebhookSubcommand(t, newWebhookStatusCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(bad, "ftp") {
		t.Fatalf("status should show the value it rejected: %q", bad)
	}
	if strings.Contains(bad, "set "+webhookURLEnv+"=https://") {
		t.Fatalf("a bad URL was reported as an unset one: %q", bad)
	}
}

func TestWebhookStatusReportsTheRedactionDefault(t *testing.T) {
	webhookEndpointServer(t)
	t.Setenv(webhookIncludeTextEnv, "")
	out, err := runWebhookSubcommand(t, newWebhookStatusCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// Somebody deciding whether to point this at a shared channel needs to know
	// what will be in it without reading the source.
	if !strings.Contains(out, "not sent") {
		t.Fatalf("status did not say fact text is withheld: %q", out)
	}
}

// --- the write paths --------------------------------------------------------

// The fact is on disk before anybody is told about it, and a broken endpoint
// cannot undo the write. A brain that refuses to record because a chat webhook
// is down would be worse than one with no webhooks at all.
func TestRememberStillRecordsWhenTheWebhookEndpointIsDown(t *testing.T) {
	now := testNow()
	opts, brainDir, branch := rememberReviveEnv(t, now)
	t.Setenv(webhookURLEnv, "http://127.0.0.1:1/definitely-not-listening")

	text := "Refs are written through a Postgres compare-and-swap."
	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	if err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none"}, text); err != nil {
		t.Fatalf("remember failed because a webhook could not be delivered: %v", err)
	}
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatalf("loadFacts: %v", err)
	}
	found := false
	for _, fact := range facts {
		if fact.Text == text {
			found = true
		}
	}
	if !found {
		t.Fatal("the fact was not persisted")
	}
	// It failed silently or it warned; silent is not acceptable.
	if !strings.Contains(errOut.String(), "not delivered") {
		t.Fatalf("a failed delivery was not reported: %q", errOut.String())
	}
}

// The happy path: recording a fact notifies the endpoint, with the fact's
// identity and without its text.
func TestRememberNotifiesTheEndpoint(t *testing.T) {
	now := testNow()
	opts, _, branch := rememberReviveEnv(t, now)
	rec := webhookEndpointServer(t)
	t.Setenv(webhookIncludeTextEnv, "")

	text := "Refs are written through a Postgres compare-and-swap."
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none"}, text); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected one notification, got %d", rec.count())
	}
	var payload webhookEvent
	if err := json.Unmarshal(rec.last(t).body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Event != WebhookFactRecorded {
		t.Fatalf("event = %q", payload.Event)
	}
	if payload.Branch != branch {
		t.Fatalf("branch = %q, want %q", payload.Branch, branch)
	}
	if payload.Fact == nil || payload.Fact.ID == "" {
		t.Fatalf("the event carried no fact identity: %+v", payload.Fact)
	}
	if strings.Contains(string(rec.last(t).body), text) {
		t.Fatalf("the fact text was delivered by default: %s", rec.last(t).body)
	}
}

// remember writes to disk; under no-egress it must still write, and still send
// nothing. This is the combination that matters in practice — a locked-down
// machine where somebody has also configured a webhook.
func TestRememberSendsNothingUnderNoEgress(t *testing.T) {
	now := testNow()
	opts, brainDir, branch := rememberReviveEnv(t, now)
	rec := webhookEndpointServer(t)
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")

	text := "Refs are written through a Postgres compare-and-swap."
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := runRemember(context.Background(), cmd, opts,
		rememberCommandOptions{path: "preferences.coding.style", agent: "none"}, text); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("no-egress mode still sent %d webhook(s)", rec.count())
	}
	facts, err := loadFacts(brainDir, branch)
	if err != nil || len(facts) == 0 {
		t.Fatalf("the fact was not recorded under no-egress: %v", err)
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
	for _, forbidden := range []string{"os.ReadFile", "os.Open", "loadBrainManifest", "settings"} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("webhooks.go references %q; the endpoint may no longer be operator-only input, "+
				"which is what the absence of private-address filtering depends on", forbidden)
		}
	}
	if strings.Count(source, "os.Getenv(webhookURLEnv)") != 1 {
		t.Fatal("the endpoint is read from somewhere other than the one environment variable")
	}
}
