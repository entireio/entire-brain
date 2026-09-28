package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unrunnable_remedy_test.go covers two reports of one defect: a command told
// the user what to run next, and the thing it named could not work.
//
// status, on a dirty working tree:
//
//	- freshness degraded -- run `entire-brain refresh --agent none`
//	  worktree=dirty-unindexed (semantic index is based on committed HEAD)
//	$ entire-brain refresh --agent none
//	refresh: seed baseline failed
//	dirty_worktree: refusing to seed uncommitted content without --worktree
//	EXIT=1
//	$ entire-brain status
//	- freshness degraded -- run `entire-brain refresh --agent none`   <- identical
//
// refresh --worktree, which is the flag that state seems to point at:
//
//	stdout: refreshed brain: /…      EXIT=0
//	stderr: refresh: semantic index skipped
//
// so the stage was dropped, nothing on stdout said so, and the status verdict
// underneath went on naming the same unrunnable command. Between the two, a
// dirty worktree was a state NO command cleared and none explained.

// TestStatusRemediationNeverNamesACommandThatCannotRun pins the mapping from
// "what is wrong" to "what the reader does", which is a strictly wider question
// than Remedy's "which subcommand". Both overrides are scoped, because the ONLY
// thing wrong with the old line was that it was unconditional.
func TestStatusRemediationNeverNamesACommandThatCannotRun(t *testing.T) {
	t.Parallel()
	const refresh = "run `entire-brain " + statusRemedyRefresh + "`"
	for name, testCase := range map[string]struct {
		health statusHealth
		want   string
	}{
		// Unchanged: every state this tool can still repair itself.
		"stale index, clean tree": {health: statusHealth{Severity: "degraded"}, want: refresh},
		"no semantic index":       {health: statusHealth{SemanticMissing: true}, want: refresh},
		"a health issue":          {health: statusHealth{Issues: 1}, want: "run `entire-brain doctor`"},

		// A dirty tree invalidates exactly one remedy: the one that refuses to
		// index uncommitted content.
		"dirty tree, stale index": {
			health: statusHealth{Severity: "degraded", DirtyWorktree: true},
			want:   setupDirtyWorktreeHint("entire-brain"),
		},
		"dirty tree, no semantic index": {
			health: statusHealth{SemanticMissing: true, DirtyWorktree: true},
			want:   setupDirtyWorktreeHint("entire-brain"),
		},
		// ...and only that one. `doctor` reads a corrupt store perfectly well on
		// a dirty tree, and sending the reader to git instead would be a second
		// wrong answer in place of the first.
		"dirty tree, a health issue": {
			health: statusHealth{Issues: 1, DirtyWorktree: true},
			want:   "run `entire-brain doctor`",
		},
		// `setup` builds a degraded brain with no host CLI and on a dirty tree,
		// so a repository with no brain keeps the answer it always had.
		"no brain, dirty tree, no host CLI": {
			health: statusHealth{BrainMissing: true, DirtyWorktree: true, HostCLIMissing: true},
			want:   "run `entire-brain setup`",
		},

		// The host CLI builds sessions, semantic and entities. While it is
		// absent, no refresh brings any of them back.
		"no host CLI, components failed": {
			health: statusHealth{HostCLIMissing: true, Failed: []string{"sessions", "semantic"}},
			want:   "`entire` is not on PATH; install the Entire CLI, then run `entire-brain setup`",
		},
		// A machine with no host CLI whose brain is otherwise fine is not told
		// to go installing things: nothing it can see has failed.
		"no host CLI, nothing failed": {
			health: statusHealth{HostCLIMissing: true, Severity: "degraded"},
			want:   refresh,
		},

		// PRECEDENCE. A missing host CLI is a background condition that
		// coexists with everything, so it is the floor: a diagnosed fault takes
		// the line first, or the verdict answers a corrupt store with advice
		// that does not touch it.
		"no host CLI AND a corrupt store": {
			health: statusHealth{
				HostCLIMissing: true,
				Failed:         []string{"semantic"},
				Severity:       "unsafe",
			},
			want: refresh,
		},
		// Not `refresh`: a health issue with no freshness problem is Remedy's
		// `doctor` case and stays that way. What matters here is only that the
		// environmental note did not displace the diagnosis.
		"no host CLI AND a health issue": {
			health: statusHealth{HostCLIMissing: true, Failed: []string{"semantic"}, Issues: 1},
			want:   "run `entire-brain doctor`",
		},
		// A brain that has simply never had an index built is an ABSENCE, not a
		// failure to blame on the environment. #236 fixed this state to name the
		// command that builds it, on any machine.
		"no host CLI AND no semantic index ever built": {
			health: statusHealth{HostCLIMissing: true, SemanticMissing: true},
			want:   refresh,
		},
		// The blocker outranks the fault it blocks: `refresh --agent none` exits
		// 1 on dirty_worktree before it reads a single index, so the rebuild the
		// corrupt store needs cannot start until the tree is clean.
		"corrupt store AND a dirty tree": {
			health: statusHealth{Severity: "unsafe", DirtyWorktree: true},
			want:   setupDirtyWorktreeHint("entire-brain"),
		},
		// ...and all three at once still resolves top-down, without ambiguity.
		"no brain AND a dirty tree AND a corrupt store AND no host CLI": {
			health: statusHealth{
				BrainMissing: true, DirtyWorktree: true, Severity: "unsafe",
				HostCLIMissing: true, Failed: []string{"semantic"},
			},
			want: "run `entire-brain setup`",
		},
	} {
		if got := testCase.health.nextStepMessage("entire-brain"); got != testCase.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, testCase.want)
		}
	}
}

// TestStatusRemediationReadsTheLiveWorktree pins WHERE the dirty signal comes
// from. The `dirty-unindexed` freshness axis was the obvious source and it is
// not the condition: after `refresh --worktree` the seed axis reads
// `dirty-indexed`, freshness aggregates to "ok", and `refresh --agent none`
// goes on refusing to reseed the same uncommitted content.
func TestStatusRemediationReadsTheLiveWorktree(t *testing.T) {
	t.Parallel()
	report := brainStatusReport{
		Live:      brainLiveState{Dirty: true},
		Retrieval: &brainStatusRetrieval{Freshness: &staleReport{Severity: "ok", Axes: map[string]staleAxis{"seed": {State: "dirty-indexed"}}}},
	}
	if !buildStatusHealth(report).DirtyWorktree {
		t.Fatal("a dirty worktree with an `ok` freshness report is still a dirty worktree")
	}
	report.Live.Dirty = false
	if buildStatusHealth(report).DirtyWorktree {
		t.Fatal("a clean worktree must not be reported as dirty")
	}
}

// TestRefreshWorktreeSaysWhichStageItSkipped is the other report. `--worktree`
// turning the semantic stage off is deliberate and stays, and the exit code
// stays 0 with it: `setup` sets the precedent for exit-0-on-partial, and an
// exit code cannot carry the sentence a reader needs here. But setup EARNS
// that by naming every component it skipped, and this named none.
func TestRefreshWorktreeSaysWhichStageItSkipped(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all")] = fakeCommandResponse{stdout: "[]"}
	runner.responses[fakeCommandKey("entire-test", "checkpoint", "explain", "--json")] = fakeCommandResponse{stdout: "[]"}
	opts := Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	}
	out, err := execute(t, NewRootCommand(opts), "refresh", "--worktree", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("refresh --worktree: %v\n%s", err, out)
	}
	for _, want := range []string{
		"semantic index not rebuilt",
		"--worktree refreshes seed and docs only",
		"commit or stash the working tree",
		"refreshed brain:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("refresh --worktree does not say %q:\n%s", want, out)
		}
	}

	// The note belongs to the IMPLICIT skip only. A reader who typed
	// --semantic=false asked for it and does not need telling.
	explicit, err := execute(t, NewRootCommand(opts), "refresh", "--worktree", "--semantic=false", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("refresh --worktree --semantic=false: %v\n%s", err, explicit)
	}
	if strings.Contains(explicit, "semantic index not rebuilt") {
		t.Fatalf("an explicitly requested skip does not need explaining:\n%s", explicit)
	}
	// And an ordinary refresh, which builds it, says nothing at all.
	ordinary, err := execute(t, NewRootCommand(opts), "refresh", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, ordinary)
	}
	if strings.Contains(ordinary, "semantic index not rebuilt") {
		t.Fatalf("a refresh that DID build the semantic index must not claim otherwise:\n%s", ordinary)
	}
}
