package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// #568: a scheduled worker syncs branches it has not checked out. Before the
// fix `memories sync --branch` only changed the label in the output: the pull
// still judged teammate memories against the checkout's HEAD. These tests pin
// that the named branch now decides, and that it is resolved before anything
// leaves the machine.

const syncBranchTestTip = "f00df00df00df00df00df00df00df00df00df00d"

// scriptSyncBranch makes refs/heads/<branch> resolve to tip in the fake
// runner, and anchor reachable from tip only (HEAD stays unscripted, so the
// same anchor is unreachable from the checkout).
func scriptSyncBranch(f verifyFixture, branch, tip, anchor string) {
	ref := "refs/heads/" + branch
	f.runner.responses[fakeCommandKey("git", "check-ref-format", ref)] = fakeCommandResponse{}
	f.runner.responses[fakeCommandKey("git", "show-ref", "--verify", "--hash", ref)] = fakeCommandResponse{stdout: tip + "\n"}
	f.runner.responses[fakeCommandKey("git", "merge-base", "--is-ancestor", anchor, tip)] = fakeCommandResponse{}
}

func storeHas(t *testing.T, brainDir, id string) bool {
	t.Helper()
	local, err := factstore.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range local {
		if rec.ID == id {
			return true
		}
	}
	return false
}

func TestMemoriesSyncBranchJudgesReachabilityFromThatBranch(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	const anchor = "c0ffeec0ffeec0ffeec0ffeec0ffeec0ffeec0ff"
	onFeature := hostedSyncAnchoredFact("teammate fact anchored on feature", now, factmerge.Anchor{SessionID: "s1", Commit: anchor})
	fake := &hostedFake{}
	fake.seed(onFeature, "", "acct-teammate", "teammate")
	f, _ := newHostedSyncCmdFixture(t, fake)
	scriptSyncBranch(f, "feature", syncBranchTestTip, anchor)

	// The checkout (main) cannot reach the anchor, so a plain sync leaves the
	// teammate's memory out: unchanged behavior.
	if out, err := execute(t, NewRootCommand(f.opts), "memories", "sync"); err != nil {
		t.Fatalf("memories sync: %v\n%s", err, out)
	}
	if storeHas(t, f.brainDir, onFeature.ID) {
		t.Fatal("a plain sync must judge reachability from HEAD, which does not contain the anchor")
	}

	before := len(f.runner.calls)
	out, err := execute(t, NewRootCommand(f.opts), "memories", "sync", "--branch", "feature")
	if err != nil {
		t.Fatalf("memories sync --branch feature: %v\n%s", err, out)
	}
	if !storeHas(t, f.brainDir, onFeature.ID) {
		t.Fatal("--branch feature must adopt a teammate memory anchored on feature's history")
	}
	if !strings.Contains(out, "synced feature via") {
		t.Fatalf("output must name the synced branch, got %q", out)
	}
	assertBranchSyncGitCalls(t, f.runner.calls[before:], syncBranchTestTip)
}

// assertBranchSyncGitCalls checks the git calls of one `memories sync
// --branch` run: reachability is judged from the branch tip only, and the
// branch lookup carries the local-only policy, because it runs before the
// no-egress gate and a partial clone would otherwise lazy-fetch the tip.
func assertBranchSyncGitCalls(t *testing.T, calls []fakeCommandCall, tip string) {
	t.Helper()
	lookedUp := false
	for _, call := range calls {
		if call.name != "git" || len(call.args) == 0 {
			continue
		}
		switch {
		case call.args[0] == "merge-base" && len(call.args) == 4 && call.args[3] != tip:
			t.Fatalf("--branch sync must judge reachability from the branch tip only: %v", call.args)
		case call.args[0] == "show-ref":
			lookedUp = true
			if call.env[gitNoLazyFetchEnv] != "1" {
				t.Fatalf("branch lookup must run with %s=1, got env %v", gitNoLazyFetchEnv, call.env)
			}
		}
	}
	if !lookedUp {
		t.Fatal("--branch sync must look the branch up with show-ref")
	}
}

func TestMemoriesSyncBranchFailsBeforeUploadWhenNotALocalBranch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		branch string
		script func(f verifyFixture)
		want   string
	}{
		{
			name:   "missing branch",
			branch: "gone",
			script: func(f verifyFixture) {
				f.runner.responses[fakeCommandKey("git", "check-ref-format", "refs/heads/gone")] = fakeCommandResponse{}
				// show-ref stays unscripted: the ref does not exist.
			},
			want: "has no local branch",
		},
		{
			name:   "revision syntax is not a branch",
			branch: "main~1",
			script: func(verifyFixture) {
				// check-ref-format stays unscripted: git rejects the name.
			},
			want: "is not a valid branch name",
		},
		{
			name:   "explicitly empty branch",
			branch: "  ",
			script: func(verifyFixture) {},
			want:   "--branch is empty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertSyncBranchRefused(t, tc.branch, tc.script, tc.want) })
	}
}

// assertSyncBranchRefused runs `memories sync --branch <branch>` on a repo
// holding one local memory and checks the sync fails with want, uploads
// nothing and makes no Entire call (so no token mint) first.
func assertSyncBranchRefused(t *testing.T, branch string, script func(verifyFixture), want string) {
	t.Helper()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("local fact A", now)
	fake := &hostedFake{}
	f, _ := newHostedSyncCmdFixture(t, fake)
	f.writeFacts(t, "main", []factRecord{localFact})
	script(f)

	out, err := execute(t, NewRootCommand(f.opts), "memories", "sync", "--branch", branch)
	if err == nil {
		t.Fatalf("--branch %q must fail the sync:\n%s", branch, out)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error must say %q, got %v", want, err)
	}
	if fake.has(localFact.ID) {
		t.Fatal("nothing may be uploaded when the branch cannot be resolved")
	}
	for _, call := range f.runner.calls {
		if call.name == "entire" {
			t.Fatalf("no token mint or other Entire call may run before the branch resolves: %v", call.args)
		}
	}
}

// The fake runner above proves the wiring; this proves the git commands
// themselves, on a real repository whose checkout cannot see a commit that
// another local branch holds.
func TestSyncBranchTipAndReachabilityOnRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("checkout", "-q", "-b", "feature")
	git("commit", "-q", "--allow-empty", "-m", "only on feature")
	onFeature := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")

	ctx := context.Background()
	runner := ExecRunner{}
	tip, err := resolveSyncBranchTip(ctx, runner, repo, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if tip != onFeature {
		t.Fatalf("feature tip = %q, want %q", tip, onFeature)
	}
	if !newCommitReachability(ctx, runner, repo, tip)(onFeature) {
		t.Fatal("feature's own commit must be reachable from its tip")
	}
	if newCommitReachability(ctx, runner, repo, "HEAD")(onFeature) {
		t.Fatal("the main checkout must not reach a feature-only commit")
	}
	for _, bad := range []string{"gone", "main~1", "-x", "feature^{tree}"} {
		if _, err := resolveSyncBranchTip(ctx, runner, repo, bad); err == nil {
			t.Fatalf("--branch %q must not resolve", bad)
		}
	}
}

// A partial clone can hold a branch whose tip commit was never downloaded.
// Resolving that branch runs before the no-egress gate, so it must fail
// without asking the promisor remote for the commit.
func TestSyncBranchTipNeverLazyFetchesInAPartialClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git := func(dir string, env []string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := git(dir, nil, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return out
	}
	server := filepath.Join(root, "server")
	must(root, "init", "-q", "-b", "main", server)
	must(server, "config", "uploadpack.allowfilter", "true")
	must(server, "config", "uploadpack.allowanysha1inwant", "true")
	must(server, "commit", "-q", "--allow-empty", "-m", "base")
	must(server, "checkout", "-q", "-b", "other")
	must(server, "commit", "-q", "--allow-empty", "-m", "only on the server")
	remoteOnly := must(server, "rev-parse", "HEAD")
	must(server, "checkout", "-q", "main")

	client := filepath.Join(root, "client")
	must(root, "clone", "-q", "--filter=blob:none", "--single-branch", "--branch", "main", "file://"+server, client)
	// A local branch whose tip object is absent: written as a loose ref,
	// because update-ref refuses (or lazy-fetches) a missing object.
	if err := os.WriteFile(filepath.Join(client, ".git", "refs", "heads", "away"), []byte(remoteOnly+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := git(client, nil, "rev-parse", "--verify", "--quiet", "refs/heads/away"); err != nil || out != remoteOnly {
		t.Skipf("this git does not read a loose ref file (%v, %q); cannot stage the partial-clone case", err, out)
	}
	objectIsLocal := func() bool {
		_, err := git(client, []string{gitNoLazyFetchEnv + "=1"}, "cat-file", "-e", remoteOnly)
		return err == nil
	}
	if objectIsLocal() {
		t.Fatal("setup: the branch tip must start missing from the partial clone")
	}

	_, err := resolveSyncBranchTip(context.Background(), ExecRunner{}, client, "away")
	if objectIsLocal() {
		t.Fatal("resolving --branch must not lazy-fetch the tip from the promisor remote")
	}
	if err == nil || !strings.Contains(err.Error(), "has no local branch with its tip commit") {
		t.Fatalf("a branch whose tip is not local must fail the lookup, got %v", err)
	}
}
