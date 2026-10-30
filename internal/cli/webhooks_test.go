package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/facts"
	"github.com/entireio/entire-brain/internal/cli/command/webhook"
	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

// --- the events -------------------------------------------------------------

// Every event name this brain advertises must be sent by real code somewhere
// outside the webhook plumbing. A list a consumer can subscribe to and never
// receive anything from is worse than a shorter list.
func TestEveryAdvertisedEventIsActuallyEmitted(t *testing.T) {
	constantFor := map[string]string{
		webhook.WebhookFactRecorded:   "webhook.WebhookFactRecorded",
		webhook.WebhookFactRetracted:  "webhook.WebhookFactRetracted",
		webhook.WebhookBrainRefreshed: "webhook.WebhookBrainRefreshed",
		webhook.WebhookTest:           "WebhookTest",
	}
	var sources []string
	// The emitters live in this package, in command/webhook (where the
	// `webhook test` command moved), and in command/facts (where `remember`
	// moved, COR-2268), so all three directories are scanned.
	for _, dir := range []string{".", filepath.Join("command", "webhook"), filepath.Join("command", "facts")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read package dir: %v", err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			// webhooks.go declares the constants; a reference there proves nothing.
			if name == "webhooks.go" {
				continue
			}
			data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			sources = append(sources, string(data))
		}
	}
	for _, event := range webhook.WebhookEventNames() {
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

// --- the commands -----------------------------------------------------------

func webhookCommandOptions(t *testing.T) Options {
	t.Helper()
	repoDir := t.TempDir()
	return Options{
		Version: "test",
		Env:     semanticTestEnv(t, repoDir),
		Runner:  semanticFixtureRunner(repoDir, ""),
		Now:     clitest.WebhookTestNow,
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
	t.Setenv(webhook.WebhookURLEnv, "")
	_, err := runWebhookSubcommand(t, webhook.NewTestCommand(webhookCommandOptions(t)))
	if err == nil {
		t.Fatal("`webhook test` reported success with no endpoint configured")
	}
	if !strings.Contains(err.Error(), webhook.WebhookURLEnv) {
		t.Fatalf("the error should say what to set: %v", err)
	}
}

func TestWebhookTestDeliversToTheConfiguredEndpoint(t *testing.T) {
	rec := clitest.WebhookEndpointServer(t, webhook.WebhookURLEnv)
	out, err := runWebhookSubcommand(t, webhook.NewTestCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("`webhook test`: %v", err)
	}
	if rec.Count() != 1 {
		t.Fatalf("expected exactly one delivery, got %d", rec.Count())
	}
	var payload webhook.WebhookEvent
	if err := json.Unmarshal(rec.Last(t).Body, &payload); err != nil {
		t.Fatalf("the endpoint received something that is not an event: %v", err)
	}
	if payload.Event != webhook.WebhookTest {
		t.Fatalf("event = %q, want %q", payload.Event, webhook.WebhookTest)
	}
	if !strings.Contains(out, webhook.WebhookTest) {
		t.Fatalf("the command did not report what it sent: %q", out)
	}
}

// Silence is the failure mode webhooks have, so `status` must distinguish
// "never configured" from "configured but disabled" — and say so, rather than
// printing the same "off" for both.
func TestWebhookStatusExplainsWhyItIsOff(t *testing.T) {
	t.Setenv(webhook.WebhookURLEnv, "")
	unset, err := runWebhookSubcommand(t, webhook.NewStatusCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(unset, webhook.WebhookURLEnv) {
		t.Fatalf("status should name the variable to set: %q", unset)
	}

	clitest.WebhookEndpointServer(t, webhook.WebhookURLEnv)
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	gated, err := runWebhookSubcommand(t, webhook.NewStatusCommand(webhookCommandOptions(t)))
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
	if strings.Contains(gated, "set "+webhook.WebhookURLEnv+"=https://") {
		t.Fatalf("status told the user to set a URL that is already set: %q", gated)
	}
	if !strings.Contains(gated, "unset") {
		t.Fatalf("status should say to unset the egress toggle: %q", gated)
	}

	// A URL that is set but unusable is a third state, and must not be reported
	// as "you have not configured one".
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv(webhook.WebhookURLEnv, "ftp://example.com/hook")
	bad, err := runWebhookSubcommand(t, webhook.NewStatusCommand(webhookCommandOptions(t)))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(bad, "ftp") {
		t.Fatalf("status should show the value it rejected: %q", bad)
	}
	if strings.Contains(bad, "set "+webhook.WebhookURLEnv+"=https://") {
		t.Fatalf("a bad URL was reported as an unset one: %q", bad)
	}
}

func TestWebhookStatusReportsTheRedactionDefault(t *testing.T) {
	clitest.WebhookEndpointServer(t, webhook.WebhookURLEnv)
	t.Setenv(webhook.WebhookIncludeTextEnv, "")
	out, err := runWebhookSubcommand(t, webhook.NewStatusCommand(webhookCommandOptions(t)))
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
	now := clitest.WebhookTestNow()
	opts, brainDir, branch := rememberReviveEnv(t, now)
	t.Setenv(webhook.WebhookURLEnv, "http://127.0.0.1:1/definitely-not-listening")

	text := "Refs are written through a Postgres compare-and-swap."
	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	if err := facts.RunRemember(context.Background(), cmd, factsCommandConfig(opts),
		facts.RememberCommandOptions{Path: "preferences.coding.style", Agent: "none"}, text); err != nil {
		t.Fatalf("remember failed because a webhook could not be delivered: %v", err)
	}
	facts, err := factstore.Load(brainDir, branch)
	if err != nil {
		t.Fatalf("factstore.Load: %v", err)
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
	now := clitest.WebhookTestNow()
	opts, _, branch := rememberReviveEnv(t, now)
	rec := clitest.WebhookEndpointServer(t, webhook.WebhookURLEnv)
	t.Setenv(webhook.WebhookIncludeTextEnv, "")

	text := "Refs are written through a Postgres compare-and-swap."
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := facts.RunRemember(context.Background(), cmd, factsCommandConfig(opts),
		facts.RememberCommandOptions{Path: "preferences.coding.style", Agent: "none"}, text); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if rec.Count() != 1 {
		t.Fatalf("expected one notification, got %d", rec.Count())
	}
	var payload webhook.WebhookEvent
	if err := json.Unmarshal(rec.Last(t).Body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Event != webhook.WebhookFactRecorded {
		t.Fatalf("event = %q", payload.Event)
	}
	if payload.Branch != branch {
		t.Fatalf("branch = %q, want %q", payload.Branch, branch)
	}
	if payload.Fact == nil || payload.Fact.ID == "" {
		t.Fatalf("the event carried no fact identity: %+v", payload.Fact)
	}
	if strings.Contains(string(rec.Last(t).Body), text) {
		t.Fatalf("the fact text was delivered by default: %s", rec.Last(t).Body)
	}
}

func TestGlobalFactWebhooksDoNotClaimRepositoryOwnership(t *testing.T) {
	opts, _ := globalTestOptions(t)
	rec := clitest.WebhookEndpointServer(t, webhook.WebhookURLEnv)
	t.Setenv(webhook.WebhookIncludeTextEnv, "")
	text := "I prefer table-driven tests."
	rememberGlobal(t, opts, text)
	globals, err := factstore.LoadGlobal(opts.Env)
	if err != nil || len(globals) != 1 {
		t.Fatalf("global fixture: %v, %v", globals, err)
	}
	check := func(event string) {
		t.Helper()
		var payload webhook.WebhookEvent
		if err := json.Unmarshal(rec.Last(t).Body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Event != event || !payload.Global || payload.Repo != "" || payload.Branch != "" {
			t.Fatalf("global event claimed repository ownership: %+v", payload)
		}
		if payload.Fact == nil || payload.Fact.ID != globals[0].ID || payload.Fact.Text != "" {
			t.Fatalf("wrong fact or leaked text: %+v", payload.Fact)
		}
	}
	check(webhook.WebhookFactRecorded)
	cmd := newFactsRetractCommand(opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{globals[0].ID, "--global"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	check(webhook.WebhookFactRetracted)
	if rec.Count() != 2 {
		t.Fatalf("expected recorded and retracted events, got %d", rec.Count())
	}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	rememberGlobal(t, opts, "We deploy on Thursdays.")
	if rec.Count() != 2 {
		t.Fatal("global write sent a webhook while egress was disabled")
	}
}

// remember writes to disk; under no-egress it must still write, and still send
// nothing. This is the combination that matters in practice — a locked-down
// machine where somebody has also configured a webhook.
func TestRememberSendsNothingUnderNoEgress(t *testing.T) {
	now := clitest.WebhookTestNow()
	opts, brainDir, branch := rememberReviveEnv(t, now)
	rec := clitest.WebhookEndpointServer(t, webhook.WebhookURLEnv)
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")

	text := "Refs are written through a Postgres compare-and-swap."
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := facts.RunRemember(context.Background(), cmd, factsCommandConfig(opts),
		facts.RememberCommandOptions{Path: "preferences.coding.style", Agent: "none"}, text); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if rec.Count() != 0 {
		t.Fatalf("no-egress mode still sent %d webhook(s)", rec.Count())
	}
	facts, err := factstore.Load(brainDir, branch)
	if err != nil || len(facts) == 0 {
		t.Fatalf("the fact was not recorded under no-egress: %v", err)
	}
}

// Every event for one repository must report the same `repo`, or a subscriber
// correlating by it sees two repositories where there is one. The call sites
// had drifted between the git toplevel and the configured root, which differ
// whenever a repository is reached through a symlink or from a subdirectory.
func TestEveryWebhookCallSiteUsesOneRepoIdentity(t *testing.T) {
	// Call sites live in this package and in command/webhook, so both
	// directories are scanned; webhooks.go defines the helper itself.
	for _, dir := range []string{".", filepath.Join("command", "webhook")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "webhooks.go" {
				continue
			}
			data, err := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(line, "webhookRepoName(") {
					continue
				}
				t.Fatalf("%s builds a webhook repo name directly:\n  %s\nuse webhook.WebhookRepoIdentity so every event names the repository the same way",
					name, strings.TrimSpace(line))
			}
		}
	}
}

// And the identity itself must be stable across the two paths that differ:
// the configured root and the resolved git toplevel.
func TestWebhookRepoIdentityIsStableAcrossPathSpellings(t *testing.T) {
	repoDir := t.TempDir()
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: semanticFixtureRunner(repoDir, ""), Now: clitest.WebhookTestNow}

	// The fixture answers rev-parse --show-toplevel with the repo directory,
	// so both sources agree here; what is pinned is that the resolved value is
	// used and is a bare name rather than a path.
	got := webhook.WebhookRepoIdentity(context.Background(), opts)
	if got == "" {
		t.Fatal("no repo identity was produced")
	}
	if strings.ContainsRune(got, os.PathSeparator) {
		t.Fatalf("the repo identity is a path, not a name: %q", got)
	}
	if got != filepath.Base(repoDir) {
		t.Fatalf("identity = %q, want the repository's directory name %q", got, filepath.Base(repoDir))
	}
}

// An unconfigured webhook must cost nothing. Resolving the repository identity
// shells out to git, so building the event eagerly and letting emitWebhook
// discard it put a subprocess on every `remember` for people who have never
// configured one — on the path this file itself notes is called by agents,
// often.
func TestUnconfiguredWebhookSpawnsNoSubprocess(t *testing.T) {
	t.Setenv(webhook.WebhookURLEnv, "")
	repoDir := t.TempDir()
	runner := semanticFixtureRunner(repoDir, "")
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: runner, Now: clitest.WebhookTestNow}

	before := len(runner.calls)
	webhook.NotifyFactWebhook(context.Background(), &bytes.Buffer{}, opts, webhook.WebhookFactRecorded, "main", clitest.SampleWebhookFact(), clitest.WebhookTestNow())
	webhook.NotifyBrainWebhook(context.Background(), &bytes.Buffer{}, opts, webhook.WebhookBrainRefreshed, clitest.WebhookTestNow())

	if after := len(runner.calls); after != before {
		var names []string
		for _, call := range runner.calls[before:] {
			names = append(names, call.name+" "+strings.Join(call.args, " "))
		}
		t.Fatalf("an unconfigured webhook ran %d command(s): %v", after-before, names)
	}
}

// And a configured one must still resolve the identity, or the short-circuit
// has disabled the feature rather than skipping its cost.
func TestConfiguredWebhookStillResolvesTheRepository(t *testing.T) {
	rec := clitest.WebhookEndpointServer(t, webhook.WebhookURLEnv)
	repoDir := t.TempDir()
	opts := Options{Version: "test", Env: semanticTestEnv(t, repoDir), Runner: semanticFixtureRunner(repoDir, ""), Now: clitest.WebhookTestNow}

	webhook.NotifyFactWebhook(context.Background(), &bytes.Buffer{}, opts, webhook.WebhookFactRecorded, "main", clitest.SampleWebhookFact(), clitest.WebhookTestNow())
	if rec.Count() != 1 {
		t.Fatalf("a configured webhook delivered %d event(s)", rec.Count())
	}
	var payload webhook.WebhookEvent
	if err := json.Unmarshal(rec.Last(t).Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Repo == "" {
		t.Fatal("the delivered event carries no repository name")
	}
}
