package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

type gitlessRunner struct{}

func (gitlessRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New("no git here")
}

func captureNotifyHostedSync(t *testing.T) *[]string {
	t.Helper()
	original := notifyHostedFactsSync
	t.Cleanup(func() { notifyHostedFactsSync = original })
	var branches []string
	notifyHostedFactsSync = func(_ context.Context, _ io.Writer, _ Options, _ string, _ repoStorage, branch string) (hostedSyncOutcome, error) {
		branches = append(branches, branch)
		return hostedSyncOutcome{Attempted: true, PullSucceeded: true}, nil
	}
	return &branches
}

func notifySyncHarness(t *testing.T, now *time.Time) (Options, repoStorage) {
	t.Helper()
	brainDir := t.TempDir()
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{Version: "test", Now: func() time.Time { return *now }}
	return opts, repoStorage{Key: "test/repo", BrainDir: brainDir}
}

func TestPushHintSyncsBranchAndDefault(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)

	ran := syncHostedMemoriesOnEvent(context.Background(), io.Discard, opts, t.TempDir(), storage, "feat-x")
	if !ran {
		t.Fatal("first push hint must sync")
	}
	if len(*calls) != 2 || (*calls)[0] != "feat-x" || (*calls)[1] != distillDefaultBranch {
		t.Fatalf("expected [feat-x %s], got %v", distillDefaultBranch, *calls)
	}
}

func TestPushHintSyncsDefaultBranchOnce(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)

	if !syncHostedMemoriesOnEvent(context.Background(), io.Discard, opts, t.TempDir(), storage, distillDefaultBranch) {
		t.Fatal("hint on the default branch must sync")
	}
	if len(*calls) != 1 || (*calls)[0] != distillDefaultBranch {
		t.Fatalf("expected one %s sync, got %v", distillDefaultBranch, *calls)
	}
}

// A recorded no-access pause (COR-2279) bails before the throttle claim, like
// an opt-out: the push beat must not spend a sync slot — or a server round
// trip — on a repo the server already denied.
func TestPushHintSkipsPausedRepo(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{
		RepoID: "repo1", BaseURL: "https://cell.example", NoAccessUntil: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if syncHostedMemoriesOnEvent(context.Background(), io.Discard, opts, t.TempDir(), storage, "feat-x") {
		t.Fatal("a paused repo must not sync on a push hint")
	}
	if len(*calls) != 0 {
		t.Fatalf("expected no sync calls, got %v", *calls)
	}
}

func TestPushHintDoesNotSuppressCompletionAtSameTime(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)
	for i := 0; i < 2; i++ {
		if !syncHostedMemoriesOnEvent(context.Background(), io.Discard, opts, t.TempDir(), storage, "main") {
			t.Fatal("event was suppressed")
		}
	}
	if len(*calls) != 2 {
		t.Fatalf("calls=%v", *calls)
	}
}

func TestPushHintSyncEmptyBranchFallsBackToDefault(t *testing.T) {
	// No git repository behind repoDir, so branch resolution fails and the
	// sync covers the default branch only — never an empty branch name.
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	opts.Runner = gitlessRunner{}
	calls := captureNotifyHostedSync(t)

	if !syncHostedMemoriesOnEvent(context.Background(), io.Discard, opts, t.TempDir(), storage, "") {
		t.Fatal("hint without a branch must still sync")
	}
	if len(*calls) != 1 || (*calls)[0] != distillDefaultBranch {
		t.Fatalf("expected one %s sync, got %v", distillDefaultBranch, *calls)
	}
}

func TestNotifyCommandDispatchesPushWorkDetached(t *testing.T) {
	opts, _ := memoryAdminCommandFixture(t)
	originalLaunch := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { return nil }
	t.Cleanup(func() { memoryWorkerLaunch = originalLaunch })
	originalWork := memoryPushWorkLaunch
	t.Cleanup(func() { memoryPushWorkLaunch = originalWork })
	var launches []string
	memoryPushWorkLaunch = func(_ string, branch string) error {
		launches = append(launches, branch)
		return nil
	}

	for _, event := range []string{"session_start", "session_end"} {
		if _, err := execute(t, newMemoryNotifyCommand(opts), "--event", event, "--session", "s1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(launches) != 0 {
		t.Fatalf("session lifecycle events must not dispatch upload work, got %v", launches)
	}
	if _, err := execute(t, newMemoryNotifyCommand(opts), "--event", "checkpoint", "--session", "s1", "--branch", "feat-x"); err != nil {
		t.Fatal(err)
	}
	if len(launches) != 1 || launches[0] != "feat-x" {
		t.Fatalf("checkpoint launches=%v", launches)
	}
	launches = nil
	out, err := execute(t, newMemoryNotifyCommand(opts), "--event", "push", "--branch", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(launches) != 1 || launches[0] != "feat-x" {
		t.Fatalf("push must dispatch the hinted branch, got %v", launches)
	}
	if !strings.Contains(out, "dispatched push upload") {
		t.Fatalf("push output = %q", out)
	}
}

func TestPushWorkCommandRunsSync(t *testing.T) {
	opts, _ := memoryAdminCommandFixture(t)
	calls := captureNotifyHostedSync(t)

	if _, err := execute(t, newMemoryPushWorkCommand(opts), "--branch", "feat-x"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[0] != "feat-x" {
		t.Fatalf("push work must sync the hinted branch then default, got %v", *calls)
	}
}
