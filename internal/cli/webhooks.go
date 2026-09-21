package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Webhooks: telling something else that a brain changed.
//
// What a brain learns stays inside it until somebody runs a command. That is
// fine for a tool you sit in front of and useless for everything downstream — a
// CI job that should re-run when an invariant is recorded, a channel that should
// see a decision, an index that needs rebuilding. Nothing could subscribe.
//
// It is also outbound network traffic from a product whose default build makes
// none, so the rules are strict and configuration cannot loosen them:
//
//   - Off unless ENTIRE_BRAIN_WEBHOOK_URL is set. No config file, no discovery,
//     no default endpoint. A URL has to be typed.
//   - brainNoEgressMode() wins. ENTIRE_BRAIN_NO_EGRESS / LOCAL_ONLY silences
//     webhooks even with a URL configured, and the check happens here rather
//     than at each call site. A local-only promise one subsystem can opt out of
//     is not a promise.
//   - Fact text is withheld unless ENTIRE_BRAIN_WEBHOOK_INCLUDE_TEXT says
//     otherwise, and that decision is made in newFactWebhookEvent so no caller
//     can forget it. The default payload is ids, kinds and taxonomy paths:
//     enough to trigger work, not enough to turn one env var into a feed of
//     everything the brain knows.
//   - Redirects are refused rather than followed. A webhook POST carries the
//     payload and its signature; following a 307 to somewhere else delivers
//     both to a host nobody configured. An endpoint that redirects is a
//     misconfiguration and is reported as one.
//   - Delivery never fails the command that triggered it. Recording a fact
//     succeeds or fails on its own terms; an unreachable endpoint is a warning
//     on stderr. The alternative is a brain that cannot write anything because
//     something unrelated is down.

const (
	webhookURLEnv         = "ENTIRE_BRAIN_WEBHOOK_URL"
	webhookSecretEnv      = "ENTIRE_BRAIN_WEBHOOK_SECRET"
	webhookIncludeTextEnv = "ENTIRE_BRAIN_WEBHOOK_INCLUDE_TEXT"
	webhookTimeout        = 5 * time.Second
)

// The events that exist. Every name here is emitted by real code in this
// package; the list is deliberately not padded with events a consumer could
// subscribe to and never receive.
const (
	WebhookFactRecorded   = "fact.recorded"
	WebhookFactRetracted  = "fact.retracted"
	WebhookBrainRefreshed = "brain.refreshed"
	WebhookTest           = "webhook.test"
)

// webhookCatalogue is the single list of what this brain sends and what each
// event means. `webhook events` prints it and the README quotes it, so neither
// can drift from the code; and because the descriptions live here rather than
// beside the command, a constant's only mention outside this file is a real
// emission — which is what TestEveryAdvertisedEventIsActuallyEmitted checks.
func webhookCatalogue() []struct{ Name, Description string } {
	return []struct{ Name, Description string }{
		{WebhookFactRecorded, "a durable fact was recorded (`remember`)"},
		{WebhookFactRetracted, "a fact was marked no longer true (`facts retract`)"},
		{WebhookBrainRefreshed, "the brain finished a refresh"},
		{WebhookTest, "sent only by `webhook test`"},
	}
}

func webhookEventNames() []string {
	names := make([]string, 0, 4)
	for _, event := range webhookCatalogue() {
		names = append(names, event.Name)
	}
	return names
}

type webhookFact struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind,omitempty"`
	Paths []string `json:"paths,omitempty"`
	// Text is present only when ENTIRE_BRAIN_WEBHOOK_INCLUDE_TEXT is set.
	Text string `json:"text,omitempty"`
}

type webhookEvent struct {
	Event     string         `json:"event"`
	Timestamp string         `json:"timestamp"`
	Repo      string         `json:"repo,omitempty"`
	Branch    string         `json:"branch,omitempty"`
	Fact      *webhookFact   `json:"fact,omitempty"`
	Counts    map[string]int `json:"counts,omitempty"`
}

// webhookRepoName is the repository's directory name, not its path. A consumer
// needs to tell two repos apart; it does not need to learn the layout of
// somebody's home directory.
func webhookRepoName(repoDir string) string {
	repoDir = strings.TrimSpace(repoDir)
	if repoDir == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(repoDir))
}

// newFactWebhookEvent builds a fact event and is the ONLY place that decides
// whether fact text leaves the machine. Callers pass the whole record and get
// back a payload already redacted, so forgetting to redact is not something a
// call site can do.
func newFactWebhookEvent(name, repoDir, branch string, record factRecord, now time.Time) webhookEvent {
	fact := &webhookFact{ID: record.ID, Kind: record.Kind, Paths: record.Paths}
	if envBool(webhookIncludeTextEnv) {
		fact.Text = record.Text
	}
	return webhookEvent{
		Event:     name,
		Timestamp: now.UTC().Format(time.RFC3339),
		Repo:      webhookRepoName(repoDir),
		Branch:    branch,
		Fact:      fact,
	}
}

// webhookEndpoint resolves the configured endpoint, and explains itself when
// there is none — `webhook status` prints the reason, so a webhook that is off
// because of a typo is distinguishable from one that was never configured.
func webhookEndpoint() (endpoint string, ok bool, reason string) {
	if brainNoEgressMode() {
		return "", false, "egress is disabled by ENTIRE_BRAIN_NO_EGRESS/ENTIRE_BRAIN_LOCAL_ONLY"
	}
	raw := strings.TrimSpace(os.Getenv(webhookURLEnv))
	if raw == "" {
		return "", false, webhookURLEnv + " is not set"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Sprintf("%s is not a valid URL", webhookURLEnv)
	}
	// Scheme before host, deliberately: file:///tmp/x parses with an empty host,
	// so checking the host first reports it as a malformed URL when the real
	// objection — and the one the user needs to read — is the scheme.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// A file:// or unix:// "webhook" is not a webhook, and admitting one
		// turns an environment variable into a write primitive.
		scheme := parsed.Scheme
		if scheme == "" {
			scheme = "no scheme"
		}
		return "", false, fmt.Sprintf("%s must be http or https, not %s", webhookURLEnv, scheme)
	}
	if parsed.Host == "" {
		return "", false, fmt.Sprintf("%s has no host", webhookURLEnv)
	}
	return raw, true, ""
}

// signWebhook returns the hex HMAC-SHA256 of the body, or "" when no secret is
// configured. Without it a receiver cannot tell a notification from this brain
// apart from a POST by anything else that learned the URL.
func signWebhook(body []byte) string {
	secret := strings.TrimSpace(os.Getenv(webhookSecretEnv))
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// errWebhookRedirect is returned rather than followed. See the header comment:
// the payload and its signature must not be delivered to a host the user did
// not configure.
var errWebhookRedirect = errors.New("webhook endpoint redirected; point " + webhookURLEnv + " at the final URL instead")

func newWebhookClient() *http.Client {
	return &http.Client{
		Timeout: webhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errWebhookRedirect
		},
	}
}

// emitWebhook delivers one event, or does nothing when webhooks are off. It
// returns an error so tests and `webhook test` can assert on delivery; the
// write paths use notifyWebhook instead and do not fail on it.
func emitWebhook(ctx context.Context, event webhookEvent) error {
	endpoint, ok, _ := webhookEndpoint()
	if !ok {
		return nil // not configured is the normal case, not a failure
	}
	return deliverWebhook(ctx, endpoint, event)
}

func deliverWebhook(ctx context.Context, endpoint string, event webhookEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode webhook: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, webhookTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "entire-brain")
	request.Header.Set("X-Entire-Event", event.Event)
	if signature := signWebhook(body); signature != "" {
		request.Header.Set("X-Entire-Signature-256", "sha256="+signature)
	}

	response, err := newWebhookClient().Do(request)
	if err != nil {
		if errors.Is(err, errWebhookRedirect) {
			return errWebhookRedirect
		}
		return fmt.Errorf("deliver webhook: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		_ = response.Body.Close()
	}()
	if response.StatusCode >= 400 {
		return fmt.Errorf("webhook endpoint returned %s", response.Status)
	}
	return nil
}

// notifyWebhook is what the write paths call: fire and forget, with a warning
// so a silently broken endpoint is still discoverable. It must never return an
// error, because no fact should fail to be recorded over this.
func notifyWebhook(ctx context.Context, errOut io.Writer, event webhookEvent) {
	if err := emitWebhook(ctx, event); err != nil && errOut != nil {
		fmt.Fprintf(errOut, "warning: webhook %s not delivered: %v\n", event.Event, err)
	}
}
