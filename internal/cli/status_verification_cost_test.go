package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// status_verification_cost_test.go pins issue #325: `brain status` / `brain
// status --json` took ~a minute once a repo had facts, because it always ran
// fact verification, and verification resolves every distinct anchor commit
// with `git for-each-ref --contains <commit>` over refs/heads, refs/remotes
// and refs/tags — a full reachability walk whose cost scales with refs x
// history, not with fact count. Status is the command docs tell people to put
// in a hook, so the walk must be opt-in (--details), never the default.

// statusVerificationFixture builds a brain whose facts carry commit anchors,
// so verification — if it runs at all — has no choice but to shell out to the
// reachability walk. Without a commit on the anchor there is nothing to walk
// and the test could pass with the bug present.
func statusVerificationFixture(t *testing.T) (Options, string, *fakeCommandRunner) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	// The anchor commit must EXIST locally, otherwise verification stops at
	// `cat-file -e` and never reaches the walk this test is about — the test
	// would then pass with the bug fully present.
	anchorCommit := strings.Repeat("ab", 20)
	runner.responses[fakeCommandKey("git", "cat-file", "-e", anchorCommit+"^{commit}")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "for-each-ref", "--format=%(refname)", "--contains", anchorCommit, "refs/heads", "refs/remotes", "refs/tags")] = fakeCommandResponse{stdout: "refs/heads/feature\n"}
	// "feature" is the branch semanticFixtureRunner reports as checked out, so
	// facts written here are the ones status would verify.
	texts := []string{
		"checkpoints are condensed onto the v1 branch before push",
		"the ref store performs compare-and-swap on (repo_id, ref_name)",
		"status is wired into the session-start hook",
	}
	facts := make([]factRecord, 0, len(texts))
	for i, text := range texts {
		facts = append(facts, factRecord{
			ID:     factRecordID(text, []string{"project.tooling.stack"}),
			Paths:  []string{"project.tooling.stack"},
			Text:   text,
			Branch: "feature",
			Origin: factOriginAuthored,
			Status: factStatusActive,
			Provenance: []factAnchor{{
				SessionID: "session-1",
				Commit:    anchorCommit,
			}},
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
			UpdatedAt: now.Add(time.Duration(i) * time.Minute),
		})
	}
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, runner
}

// reachabilityWalks counts the `git for-each-ref --contains <commit>` calls the
// runner was asked to make. That command, not elapsed time, is the cost being
// regressed on, so the assertion cannot flake on a slow machine.
func reachabilityWalks(runner *fakeCommandRunner) int {
	walks := 0
	for _, call := range runner.calls {
		if call.name != "git" || len(call.args) == 0 || call.args[0] != "for-each-ref" {
			continue
		}
		for _, arg := range call.args {
			if arg == "--contains" {
				walks++
				break
			}
		}
	}
	return walks
}

func statusFactsPayload(t *testing.T, out string) map[string]any {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("status --json is not JSON: %v\n%s", err, out)
	}
	facts, _ := report["facts"].(map[string]any)
	return facts
}

func TestBrainStatusDefaultSkipsFactVerificationReachabilityWalk(t *testing.T) {
	t.Parallel()
	opts, _, runner := statusVerificationFixture(t)

	out, err := execute(t, NewRootCommand(opts), "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}

	if walks := reachabilityWalks(runner); walks != 0 {
		t.Fatalf("default status ran %d `git for-each-ref --contains` reachability walk(s); "+
			"this is the per-anchor history walk that made status unusable in a hook (#325)", walks)
	}
	facts := statusFactsPayload(t, out)
	if facts == nil {
		t.Fatalf("status --json carries no facts block:\n%s", out)
	}
	if _, present := facts["verification"]; present {
		t.Fatalf("default status payload still carries the verification block: %v", facts)
	}
}

func TestBrainStatusDetailsStillVerifiesFacts(t *testing.T) {
	t.Parallel()
	opts, _, runner := statusVerificationFixture(t)

	out, err := execute(t, NewRootCommand(opts), "status", "--json", "--details")
	if err != nil {
		t.Fatalf("status --json --details: %v\n%s", err, out)
	}

	if walks := reachabilityWalks(runner); walks == 0 {
		t.Fatalf("status --details did not verify anchors: no `git for-each-ref --contains` call was made")
	}
	facts := statusFactsPayload(t, out)
	if facts == nil {
		t.Fatalf("status --json --details carries no facts block:\n%s", out)
	}
	if _, present := facts["verification"]; !present {
		t.Fatalf("status --details dropped the verification block: %v", facts)
	}
}

// The MCP brain_status tool calls the same code path with compact=true and
// details defaulted to false. It is the surface an agent hits on every session
// start, so it must get the cheap report, not the walk.
func TestMCPBrainStatusSkipsFactVerificationReachabilityWalk(t *testing.T) {
	t.Parallel()
	opts, repoDir, runner := statusVerificationFixture(t)

	cmd := NewRootCommand(opts)
	cmd.SetArgs(nil)
	statusOpts := agentStatusOptions{json: true, compact: true, failOn: semanticAuditFailOnNone}
	if err := runAgentStatus(context.Background(), cmd, opts, statusOpts, repoDir); err != nil {
		t.Fatalf("mcp brain_status: %v", err)
	}

	if walks := reachabilityWalks(runner); walks != 0 {
		t.Fatalf("brain_status ran %d `git for-each-ref --contains` reachability walk(s)", walks)
	}
}
