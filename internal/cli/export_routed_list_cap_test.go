package cli

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// "All" is not the absence of a flag. The host CLI's --limit defaults to 100
// and rejects a non-positive value, so omitting it asks for the first hundred
// — which is how an unlimited default silently became a 100-checkpoint
// catalog on every repository larger than that.
func TestRoutedCheckpointListAsksForAnExplicitUnlimitedLimit(t *testing.T) {
	t.Parallel()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeRoutedCheckpointListKey("entire-test", true): {stdout: `[]`},
	}}
	if _, _, err := listRoutedCheckpoints(context.Background(), runner, "/repo", "entire-test", 0, true); err != nil {
		t.Fatalf("unlimited routed list failed: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected one list call, got %d", len(runner.calls))
	}
	args := runner.calls[0].args
	limit, ok := flagValue(args, "--limit")
	if !ok {
		t.Fatalf("an unlimited list asked for no --limit at all, which the host reads as 100: %v", args)
	}
	n, err := strconv.Atoi(limit)
	if err != nil || n <= 0 {
		t.Fatalf("--limit %q is not a positive number the host accepts: %v", limit, err)
	}
	// Any real catalog has to fit under it; the host's own default is 100.
	if n < 1_000_000 {
		t.Fatalf("--limit %d is too small to mean \"all\"", n)
	}
}

// A caller's explicit bound still has to be honoured exactly: the fix must not
// quietly turn --checkpoint-limit 10 into an unlimited scan.
func TestRoutedCheckpointListKeepsAnExplicitLimit(t *testing.T) {
	t.Parallel()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all", "--limit", "10"): {stdout: `[]`},
	}}
	if _, _, err := listRoutedCheckpoints(context.Background(), runner, "/repo", "entire-test", 10, true); err != nil {
		t.Fatalf("bounded routed list failed: %v", err)
	}
	if got, _ := flagValue(runner.calls[0].args, "--limit"); got != "10" {
		t.Fatalf("explicit limit was rewritten to %q", got)
	}
}

// Second line of defence. The host reports a cap it applied in prose only —
// there is no scope record for it — so an unclassified cap notice would let a
// truncated inventory be described as complete and replace the persistent
// brain with it.
func TestHostCheckpointListCapIsClassifiedAsIncompleteScope(t *testing.T) {
	t.Parallel()
	const capNotice = "note: list capped at 100 checkpoints; rerun with --limit <N> to see more\n"

	warnings := routedCheckpointWarnings([]byte(capNotice))
	if !hasCheckpointScopeIncompleteWarning(warnings) {
		t.Fatalf("a capped list was not classified as an incomplete scope: %v", warnings)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "capped at 100 checkpoints") {
		t.Fatalf("the classification dropped what the host actually said: %v", warnings)
	}
}

// And the consequence that matters: a capped list must never be described as
// Entire's complete routed catalog.
func TestDiscoveryNeverCallsACappedRoutedListComplete(t *testing.T) {
	t.Parallel()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeRoutedCheckpointListKey("entire-test", true): {
			stdout: `[{"checkpoint_id":"aaa111aaa111","is_logs_only":true}]`,
			stderr: "note: list capped at 100 checkpoints; rerun with --limit <N> to see more\n",
		},
	}}
	checkpoints, warnings, err := discoverCheckpoints(context.Background(), runner, "/repo", "entire-test", exportScopeAll, 0)
	if err != nil {
		t.Fatalf("capped discovery failed: %v", err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected the one entry the capped list returned, got %d", len(checkpoints))
	}
	joined := strings.Join(warnings, "\n")
	if !hasCheckpointScopeIncompleteWarning(warnings) {
		t.Fatalf("a capped discovery reported a complete scope: %v", warnings)
	}
	if strings.Contains(joined, "complete routed checkpoint list") {
		t.Fatalf("a truncated catalog was described as complete: %v", warnings)
	}
}

func flagValue(args []string, name string) (string, bool) {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
