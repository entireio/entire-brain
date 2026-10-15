package cli

import (
	"context"
	"errors"
	"sync"
	"time"
)

// hostedPlacementNegativeTTL bounds how long one failed placement resolution
// suppresses the next shell-out for the same repository.
//
// The cost this exists to cap is real and user-visible: the session-end hook
// runs the auto-connect path once for the current branch and once for the
// default branch, and resolveHostedPlacement bounds each shell-out at
// hostedShellOutTimeout. Against a host CLI that does not provide `entire repo
// view` (COR-2029 is unshipped), that is two failing shell-outs — up to 60s —
// on every session end, forever, because nothing was remembered between them.
//
// The TTL, rather than a permanent negative, is what keeps the daemon honest:
// a long-lived watch process must still notice that the user logged in, the
// repo was registered, or the host CLI was upgraded. One retry per repository
// per TTL is cheap; a stall per session end is not.
const hostedPlacementNegativeTTL = 10 * time.Minute

// errHostedPlacementSuppressed is returned instead of a shell-out when a
// recent failure for the same repository is still inside the negative TTL.
// Callers report an ATTEMPTED failure and stay quiet about a suppressed one,
// so the stderr note appears once per window rather than once per branch.
var errHostedPlacementSuppressed = errors.New("hosted placement resolution recently failed; not retried yet")

var hostedPlacementFailures = struct {
	mu sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// cachedHostedPlacement is the auto-connect path's resolver:
// resolveHostedPlacement behind a per-repository negative cache. The second
// return reports whether the host CLI was actually invoked — false means a
// cached failure suppressed it, and the error is errHostedPlacementSuppressed.
//
// Deliberately NOT used by `entire brain connect`: an explicit connect is a
// user asking right now, and must always try the real thing. That is also what
// keeps `entire brain disconnect` honest — the opt-out gate in
// hostedFactsSyncAndPull returns before this cache is ever consulted, and
// re-joining goes through connect, so a cached negative can neither be set by
// a disconnected repo nor mask its reconnect.
func cachedHostedPlacement(ctx context.Context, runner CommandRunner, repoDir string, now time.Time) (hostedPlacement, bool, error) {
	if hostedPlacementRecentlyFailed(repoDir, now) {
		return hostedPlacement{}, false, errHostedPlacementSuppressed
	}
	resolved, err := resolveHostedPlacement(ctx, runner, repoDir)
	recordHostedPlacementOutcome(repoDir, now, err)
	return resolved, true, err
}

func hostedPlacementRecentlyFailed(repoDir string, now time.Time) bool {
	hostedPlacementFailures.mu.Lock()
	defer hostedPlacementFailures.mu.Unlock()
	failedAt, ok := hostedPlacementFailures.at[repoDir]
	if !ok {
		return false
	}
	if now.Sub(failedAt) >= hostedPlacementNegativeTTL {
		delete(hostedPlacementFailures.at, repoDir)
		return false
	}
	return true
}

func recordHostedPlacementOutcome(repoDir string, now time.Time, err error) {
	hostedPlacementFailures.mu.Lock()
	defer hostedPlacementFailures.mu.Unlock()
	if err == nil {
		delete(hostedPlacementFailures.at, repoDir)
		return
	}
	hostedPlacementFailures.at[repoDir] = now
}
