package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/connect"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func readSetupRecordForTest(t *testing.T, f *setupTestFixture) setupRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(f.storage.HeadPath), setupRecordFile))
	if err != nil {
		t.Fatalf("read setup record: %v", err)
	}
	var record setupRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("parse setup record: %v", err)
	}
	return record
}

func TestSetupHostedAndLocalOnlyConflict(t *testing.T) {
	f := newSetupTestFixture(t)
	opts := defaultSetupOptions()
	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, opts)
	for _, name := range []string{setupFlagHosted, setupFlagLocalOnly} {
		if err := cmd.Flags().Set(name, "true"); err != nil {
			t.Fatal(err)
		}
	}
	err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, (&recordedSetup{}).steps(f))
	if err == nil || !strings.Contains(err.Error(), "contradict") {
		t.Fatalf("--hosted with --local-only must be refused, got %v", err)
	}
}

// Automation safety: a bare run in CI (no TTY) must not prompt, must not
// connect, and must record no hosted choice.
func TestSetupNoTTYStaysLocalAndSilent(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	runSetupForTest(t, f, opts, rec)

	if rec.establishCalls != 0 {
		t.Fatalf("no TTY and no flag must never connect, got %d establish calls", rec.establishCalls)
	}
	if _, present, _ := brainstore.ReadHostedRepoBinding(f.storage.BrainDir); present {
		t.Fatal("no binding may exist after a silent local run")
	}
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "" {
		t.Fatalf("a defaulted run must record no hosted choice, got %q", record.HostedChoice)
	}
}

// An opted-out binding is an explicit decision; setup must not prompt, sync,
// or resurrect anything — it says how to re-enable and moves on.
func TestSetupOptedOutBindingIsRespected(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	if err := brainstore.WriteHostedRepoBinding(f.storage.BrainDir,
		brainstore.HostedRepoBinding{RepoID: "r", BaseURL: "https://cell.example.com", OptedOut: true}); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	opts.hosted = true // even an explicit flag must not override the opt-out silently
	out := runSetupForTest(t, f, opts, rec)

	if rec.establishCalls != 0 {
		t.Fatalf("an opted-out repo must not sync, got %d establish calls", rec.establishCalls)
	}
	if !strings.Contains(out, "disconnected") {
		t.Fatalf("the run must say the repo is disconnected and how to re-enable:\n%s", out)
	}
}

func TestSetupHostedPreBindFailureFallsBackLocal(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{agent: "codex", establishErr: errors.New("connect: `entire repo view --json`: exit status 1")}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	opts.hosted = true
	out := runSetupForTest(t, f, opts, rec)

	if rec.establishCalls != 1 {
		t.Fatalf("--hosted must attempt the connection once, got %d", rec.establishCalls)
	}
	if !strings.Contains(out, "continuing local-only") {
		t.Fatalf("a pre-bind failure must print the remedy and continue local-only:\n%s", out)
	}
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "" {
		t.Fatalf("a failed hosted attempt must record nothing, got %q", record.HostedChoice)
	}
}

func TestSetupHostedPostBindSyncFailureStaysBound(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{
		agent:           "codex",
		establishResult: connect.EstablishResult{Bound: true, StorageKey: "gh/acme/repo"},
		establishErr:    errors.New("connect: gh/acme/repo is bound, but the first sync failed"),
	}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	opts.hosted = true
	out := runSetupForTest(t, f, opts, rec)

	if !strings.Contains(out, "first sync") {
		t.Fatalf("a post-bind sync failure must be reported with its remedy:\n%s", out)
	}
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "hosted" {
		t.Fatalf("a bound repo records the hosted choice even when the first sync failed, got %q", record.HostedChoice)
	}
}

func TestSetupRemembersHostedChoice(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{agent: "codex", establishResult: connect.EstablishResult{Bound: true, StorageKey: "gh/acme/repo"}}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	opts.hosted = true
	runSetupForTest(t, f, opts, rec)
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "hosted" {
		t.Fatalf("the hosted choice must be recorded, got %q", record.HostedChoice)
	}

	// The bare re-run keeps the choice without prompting (no TTY here anyway)
	// and keeps syncing.
	bare := defaultSetupOptions()
	bare.agent = "codex"
	rerun := &recordedSetup{agent: "codex", establishResult: connect.EstablishResult{Bound: true, StorageKey: "gh/acme/repo"}}
	runSetupForTest(t, f, bare, rerun)
	if rerun.establishCalls != 1 {
		t.Fatalf("a remembered hosted choice must sync on re-runs, got %d establish calls", rerun.establishCalls)
	}
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "hosted" {
		t.Fatalf("the re-run must keep the recorded choice, got %q", record.HostedChoice)
	}
}

// --local-only on a bound repo is respected for the run: no network, binding
// untouched, remembered choice untouched.
func TestSetupLocalOnlySkipsSyncOnABoundRepo(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	if err := brainstore.WriteHostedRepoBinding(f.storage.BrainDir,
		brainstore.HostedRepoBinding{RepoID: "r", BaseURL: "https://cell.example.com"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{agent: "codex"}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	opts.localOnly = true
	out := runSetupForTest(t, f, opts, rec)

	if rec.establishCalls != 0 {
		t.Fatalf("--local-only must skip the sync, got %d establish calls", rec.establishCalls)
	}
	if !strings.Contains(out, "skipping hosted sync") {
		t.Fatalf("the run must say the sync was skipped and how to opt out for good:\n%s", out)
	}
	if _, present, _ := brainstore.ReadHostedRepoBinding(f.storage.BrainDir); !present {
		t.Fatal("--local-only must leave the binding in place")
	}
}

// A live binding syncs without re-resolving placement or re-binding: a custom
// --repo-id/--api-url binding (staging, another cell) must never be silently
// moved to the host CLI's resolved placement, and the COR-2279 no-access pause
// must survive a setup re-run.
func TestSetupBoundRepoSyncsWithoutRebinding(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	custom := brainstore.HostedRepoBinding{RepoID: "custom-ulid", BaseURL: "https://staging.example.com"}
	if err := brainstore.WriteHostedRepoBinding(f.storage.BrainDir, custom); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{agent: "codex", hostedSyncOutcome: hostedSyncOutcome{Attempted: true, PullSucceeded: true}}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	runSetupForTest(t, f, opts, rec)

	if rec.establishCalls != 0 {
		t.Fatalf("a bound repo must not re-resolve placement or re-bind, got %d establish calls", rec.establishCalls)
	}
	if rec.hostedSyncCalls != 1 {
		t.Fatalf("a bound repo must sync once per setup run, got %d", rec.hostedSyncCalls)
	}
	after, _, err := brainstore.ReadHostedRepoBinding(f.storage.BrainDir)
	if err != nil || after.RepoID != "custom-ulid" || after.BaseURL != "https://staging.example.com" {
		t.Fatalf("the binding must be byte-for-byte untouched, got %+v (err %v)", after, err)
	}
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "hosted" {
		t.Fatalf("a bound repo records hosted, got %q", record.HostedChoice)
	}
}

// The short form is the DEFAULT output; the hosted step's results must survive
// it, or the default run reports nothing about connecting at all.
func TestSetupTerseReportsHostedOutcome(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	if err := brainstore.WriteHostedRepoBinding(f.storage.BrainDir,
		brainstore.HostedRepoBinding{RepoID: "r", BaseURL: "https://cell.example.com"}); err != nil {
		t.Fatal(err)
	}
	rec := &recordedSetup{agent: "codex", hostedSyncOutcome: hostedSyncOutcome{Attempted: true, PullSucceeded: true}}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	out := runSetupTerseForTest(t, f, opts, rec)
	if !strings.Contains(out, "team memories pulled") {
		t.Fatalf("the default (terse) output must report the sync result:\n%s", out)
	}

	localOnly := defaultSetupOptions()
	localOnly.noDaemon, localOnly.noBackfill = true, true
	localOnly.localOnly = true
	out = runSetupTerseForTest(t, f, localOnly, &recordedSetup{agent: "codex"})
	if !strings.Contains(out, "skipping hosted sync") {
		t.Fatalf("the default (terse) output must say --local-only skipped the sync:\n%s", out)
	}
}

// A typed --local-only is a one-run override; it must not erase the remembered
// choice when the record is re-saved at the end of the run.
func TestSetupLocalOnlyRunKeepsTheRememberedChoice(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	first := defaultSetupOptions()
	first.noDaemon, first.noBackfill = true, true
	first.hosted = true
	runSetupForTest(t, f, first, &recordedSetup{agent: "codex", establishResult: connect.EstablishResult{Bound: true, StorageKey: "gh/acme/repo"}})
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "hosted" {
		t.Fatalf("precondition: first run must record hosted, got %q", record.HostedChoice)
	}

	second := defaultSetupOptions()
	second.noDaemon, second.noBackfill = true, true
	second.localOnly = true
	second.agent = "codex"
	second.verbose = true
	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, second)
	if err := cmd.Flags().Set(setupFlagLocalOnly, "true"); err != nil {
		t.Fatal(err)
	}
	if err := runSetup(context.Background(), cmd, f.opts, second, f.repoDir, (&recordedSetup{agent: "codex"}).steps(f)); err != nil {
		t.Fatalf("runSetup: %v\n%s", err, out.String())
	}
	if record := readSetupRecordForTest(t, f); record.HostedChoice != "hosted" {
		t.Fatalf("a --local-only run must keep the remembered choice, got %q", record.HostedChoice)
	}
}

// A failed connect attempt must own the closing line: printing the generic
// "to share them, run connect" offer right after a connection error tells the
// user to re-run the thing that just failed, with no hint why.
func TestSetupTerseClosesWithTheConnectErrorWhenHostedFails(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{agent: "codex", establishErr: errors.New(`connect: remote "origin" not found; pass --repo-id and --api-url explicitly`)}
	opts := defaultSetupOptions()
	opts.noDaemon, opts.noBackfill = true, true
	opts.hosted = true
	out := runSetupTerseForTest(t, f, opts, rec)

	if !strings.Contains(out, `remote "origin" not found`) {
		t.Fatalf("the closing line must carry the connection error:\n%s", out)
	}
	if strings.Contains(out, "To share them") {
		t.Fatalf("the generic connect offer must not print after a failed connect:\n%s", out)
	}
}
