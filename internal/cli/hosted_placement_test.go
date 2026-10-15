package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// countingPlacementRunner answers `entire auth token` like the hosted harness
// does and fails `entire repo view` the way every stock host CLI does
// (COR-2029 is unshipped), counting how often the resolver reached for it.
func countingPlacementRunner(calls *int) commandRunnerFunc {
	return commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			*calls++
			return nil, nil, errors.New("unknown command \"view\" for \"entire repo\"")
		}
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
}

func stderrLines(errOut string) []string {
	trimmed := strings.TrimRight(errOut, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// ND-2: the auto-connect gate used to `return nil` with no stderr line — alone
// among the gates in hostedFactsSyncAndPull, all of which report. The hosted
// loop therefore looked installed and simply never engaged.
func TestHostedSyncAutoConnectFailureIsReported(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, false)
	placementCalls := 0
	opts.Runner = countingPlacementRunner(&placementCalls)

	var errOut bytes.Buffer
	if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("an unresolvable placement must stay a graceful local fallback, got %v", err)
	}
	if placementCalls != 1 {
		t.Fatalf("the gate must have actually attempted resolution, got %d calls", placementCalls)
	}
	lines := stderrLines(errOut.String())
	if len(lines) != 1 {
		t.Fatalf("auto-connect failure must print exactly one stderr line, got %d: %q", len(lines), errOut.String())
	}
	line := lines[0]
	for _, want := range []string{
		repoDir,                         // which repository stayed local
		"branch main",                   // and which branch
		"COR-2029",                      // why: the host contract is unshipped
		"--repo-id",                     // the runnable remedy
		"the local brain is unaffected", // and that nothing was lost
	} {
		// Errorf, not Fatalf: every missing element should be named in one
		// run, so a weakening proves each of these assertions fires rather
		// than only the first.
		if !strings.Contains(line, want) {
			t.Errorf("stderr line must contain %q, got %q", want, line)
		}
	}
	if _, present, _ := readHostedRepoBinding(storage.BrainDir); present {
		t.Fatal("no binding may be written when placement is unresolved")
	}
	if fake.requestCount() != 0 {
		t.Fatalf("local fallback must make zero hosted requests, got %d", fake.requestCount())
	}
}

// ND-3: the session-end hook runs this path for the current branch and then
// the default branch, and each resolveHostedPlacement attempt is bounded only
// by hostedShellOutTimeout — so an unshipped `repo view` cost up to 60s of
// shelling out per session end. One attempt per process (per TTL window) is the cap.
func TestHostedSyncResolvesPlacementAtMostOncePerProcess(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, false)
	placementCalls := 0
	opts.Runner = countingPlacementRunner(&placementCalls)

	var errOut bytes.Buffer
	// Exactly the session-end hook's pair: current branch, then the default.
	for _, branch := range []string{"feature", distillDefaultBranch} {
		if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, branch); err != nil {
			t.Fatalf("branch %s must stay a graceful local fallback, got %v", branch, err)
		}
	}
	if placementCalls != 1 {
		t.Fatalf("a failed resolution must not be retried in the same process: want 1 shell-out, got %d", placementCalls)
	}
	// The suppressed attempt must also stay quiet, or the fix for ND-2 would
	// reintroduce the per-branch noise the cache exists to remove.
	if lines := stderrLines(errOut.String()); len(lines) != 1 {
		t.Fatalf("a suppressed attempt must not print again: want 1 line, got %d: %q", len(lines), errOut.String())
	}
}

// The negative cache must not become a permanent negative: a long-lived watch
// daemon has to notice that the user logged in, the repo was registered, or
// the host CLI finally shipped `repo view`.
func TestHostedPlacementRetriesAfterTheNegativeTTL(t *testing.T) {
	repoDir := t.TempDir()
	placementCalls := 0
	runner := countingPlacementRunner(&placementCalls)
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	if _, attempted, err := cachedHostedPlacement(context.Background(), runner, repoDir, start); err == nil || !attempted {
		t.Fatalf("first call must attempt and fail: attempted=%v err=%v", attempted, err)
	}
	if _, attempted, err := cachedHostedPlacement(context.Background(), runner, repoDir, start.Add(hostedPlacementNegativeTTL-time.Second)); attempted || !errors.Is(err, errHostedPlacementSuppressed) {
		t.Fatalf("inside the TTL the shell-out must be suppressed: attempted=%v err=%v", attempted, err)
	}
	if _, attempted, _ := cachedHostedPlacement(context.Background(), runner, repoDir, start.Add(hostedPlacementNegativeTTL)); !attempted {
		t.Fatal("past the TTL the resolver must try again")
	}
	if placementCalls != 2 {
		t.Fatalf("want 2 shell-outs (first attempt, post-TTL retry), got %d", placementCalls)
	}
}

// ND-1: `connect` with no flags cannot succeed on a first run, and its error
// used to lead with `entire repo view` — a command that exists nowhere,
// so the reader goes looking for a local breakage that is not there. The
// runnable path has to come first.
func TestConnectErrorNamesTheRunnableFlagsBeforeTheUnshippedCommand(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	placementCalls := 0
	runner := countingPlacementRunner(&placementCalls)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	_, _, err := runConnectCommand(t, opts)
	if err == nil {
		t.Fatal("connect without flags must fail while repo view is unshipped")
	}
	msg := err.Error()
	repoIDAt := strings.Index(msg, "--repo-id")
	apiURLAt := strings.Index(msg, "--api-url")
	unshippedAt := strings.Index(msg, "repo view")
	if repoIDAt < 0 || apiURLAt < 0 {
		t.Fatalf("error must name both flags the user can actually run, got: %v", err)
	}
	if unshippedAt < 0 {
		t.Fatalf("error must still name the unshipped command it tried, got: %v", err)
	}
	if repoIDAt > unshippedAt {
		t.Fatalf("the runnable path must come before the unshipped command, got: %v", err)
	}
	if !strings.Contains(msg, "COR-2029") || !strings.Contains(msg, "not shipped") {
		t.Fatalf("error must say automatic binding is unshipped (COR-2029), got: %v", err)
	}
}

// The negative cache must not interact with `entire brain disconnect`.
//
// Two separate guarantees, both load-bearing once disconnect exists:
//
//   - A disconnected repository never reaches the resolver at all. The opt-out
//     gate in hostedFactsSyncAndPull precedes the auto-connect gate, so sync on
//     a disconnected repo neither shells out nor records a negative — there is
//     nothing cached that a later re-join could trip over.
//   - An explicit `connect` always tries the real thing. Re-joining is the user
//     asking right now, so it must bypass any cached negative; otherwise a
//     repository disconnected inside a live daemon's TTL window would refuse to
//     re-resolve for up to hostedPlacementNegativeTTL.
func TestHostedPlacementCacheDoesNotInterfereWithDisconnect(t *testing.T) {
	t.Run("a disconnected repo never populates the cache", func(t *testing.T) {
		fake := &hostedFake{}
		opts, storage, repoDir, _ := hostedSyncHarness(t, fake, false)
		placementCalls := 0
		opts.Runner = countingPlacementRunner(&placementCalls)
		if _, err := setHostedOptOut(storage.BrainDir); err != nil {
			t.Fatal(err)
		}

		var errOut bytes.Buffer
		for _, branch := range []string{"feature", distillDefaultBranch} {
			if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, branch); err != nil {
				t.Fatalf("branch %s: %v", branch, err)
			}
		}
		if placementCalls != 0 {
			t.Fatalf("a disconnected repo must not resolve placement at all, got %d shell-outs", placementCalls)
		}
		if errOut.Len() != 0 {
			t.Fatalf("the opt-out is what the user asked for; it must stay silent, got %q", errOut.String())
		}
		// The point of the first guarantee: nothing was remembered, so a
		// re-join cannot be masked by a stale negative.
		if hostedPlacementRecentlyFailed(repoDir, opts.Now().UTC()) {
			t.Fatal("a disconnected repo must leave no negative cache entry behind")
		}
	})

	t.Run("explicit connect bypasses a cached negative", func(t *testing.T) {
		repoDir := t.TempDir()
		env := connectTestEnv(t, repoDir)
		placementCalls := 0
		runner := countingPlacementRunner(&placementCalls)
		opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

		// Prime the cache against the very dir connect will key on, and prove
		// the priming took — otherwise this test would pass vacuously if the
		// resolved path ever stopped matching.
		resolvedDir, local, err := resolveLocalTargetRepoDir(context.Background(), runner, agentSurfaceTarget(opts, nil))
		if err != nil || !local {
			t.Fatalf("resolve repo dir: local=%v err=%v", local, err)
		}
		// Real clock, not a fixture date: opts.Now is time.Now, so a fixture
		// timestamp would already be outside the TTL and the cache would
		// re-attempt for that reason alone — the weakening would not fire.
		now := time.Now().UTC()
		if _, attempted, _ := cachedHostedPlacement(context.Background(), runner, resolvedDir, now); !attempted {
			t.Fatal("priming call must attempt")
		}
		if !hostedPlacementRecentlyFailed(resolvedDir, now) {
			t.Fatal("priming did not record a negative; the rest of this test would be vacuous")
		}
		primed := placementCalls

		if _, _, err := runConnectCommand(t, opts); err == nil {
			t.Fatal("connect without flags must still fail while repo view is unshipped")
		}
		if placementCalls != primed+1 {
			t.Fatalf("explicit connect must shell out despite the cached negative: want %d calls, got %d", primed+1, placementCalls)
		}
	})
}
