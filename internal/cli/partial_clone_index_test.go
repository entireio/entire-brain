package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// entire-graph refuses git metadata subprocesses in a PARTIAL CLONE -- rightly,
// since a promisor remote means an ordinary git command can fetch over the
// network and the provider runs --no-network. It then returns an empty commit
// and tree, and brain DISCARDED an index it had already parsed in full: 2,526
// files and 35,510 symbols on one real repository here. `git clone
// --filter=blob:none` is ordinary on a large repo, so the repos that most need
// an index were the ones silently losing it.
//
// Brain never needed the provider for that answer. head and tree come from its
// own `git rev-parse HEAD` and `HEAD^{tree}`, which succeed in a partial clone,
// and verifySemanticHeadStable re-resolves both after the provider ran -- so
// the snapshot describes exactly that commit.
//
// It is verifySemanticHeadStable and NOT verifySemanticWorktreeStable that
// establishes this, which this comment used to claim. See
// TestStampedProvenanceRefusesAMovedHead: a dirtiness check is measured
// against whatever HEAD is current, so a commit made while the provider ran
// leaves the worktree clean and passes it.
func TestBrainStampsAHeaderTheProviderCouldNotStamp(t *testing.T) {
	t.Parallel()

	const commit = "a80904c86091139e5451785c002d8fee1a9de85c"
	const tree = "5b66bbca628cc9ecbbc4c8d6a69f1cf3c648db6a"

	// The partial-clone shape: both blank.
	blank := semanticHeader{}
	note := stampSemanticHeaderFromLocalGit(&blank, commit, tree)
	if blank.Commit != commit || blank.Tree != tree {
		t.Fatalf("an unstamped header must be filled from local git, got %q/%q", blank.Commit, blank.Tree)
	}
	if note == "" {
		t.Error("stamping must be disclosed, not silent")
	}

	// A provider that DID answer is never overruled.
	theirs := semanticHeader{Commit: "theirs", Tree: "theirtree"}
	if got := stampSemanticHeaderFromLocalGit(&theirs, commit, tree); got != "" {
		t.Errorf("a stamped header must be left alone, got %q", got)
	}
	if theirs.Commit != "theirs" || theirs.Tree != "theirtree" {
		t.Error("the provider's own values were overwritten")
	}

	// HALF a header is not a declining provider -- it is an inconsistent one,
	// and must keep failing validation rather than be papered over.
	for name, h := range map[string]semanticHeader{
		"commit only": {Commit: "c"},
		"tree only":   {Tree: "t"},
	} {
		got := h
		if note := stampSemanticHeaderFromLocalGit(&got, commit, tree); note != "" {
			t.Errorf("%s: a half-filled header must not be stamped, got %q", name, note)
		}
	}

	// Nothing to stamp WITH is not a stamp.
	empty := semanticHeader{}
	if note := stampSemanticHeaderFromLocalGit(&empty, "", ""); note != "" {
		t.Errorf("no local commit means no stamp, got %q", note)
	}
	if note := stampSemanticHeaderFromLocalGit(&empty, commit, "  "); note != "" {
		t.Errorf("a blank tree means no stamp, got %q", note)
	}
	if note := stampSemanticHeaderFromLocalGit(nil, commit, tree); note != "" {
		t.Errorf("a nil header must be a no-op, got %q", note)
	}
}

// The values must be set where head and tree are resolved, NOT inside the
// already-current branch. That branch only runs when a semantic source exists,
// so a FIRST index -- exactly the case a partial clone fails on -- left them
// empty and the stamp never happened. That is how the first version of this
// fix passed its unit test and still failed on a real partial clone.
func TestLocalCommitIsSetOnTheFirstIndexPath(t *testing.T) {
	t.Parallel()

	src := readGoSourceForTest(t, "semantic.go")
	assign := "indexOpts.localCommit = head"
	guard := "if !indexOpts.force && existing.Sources != nil"
	ai, gi := indexOfForTest(src, assign), indexOfForTest(src, guard)
	if ai < 0 {
		t.Fatal("the local commit is never carried into the index options")
	}
	if gi < 0 {
		t.Fatal("the already-current guard is gone; this test needs rewriting")
	}
	if ai > gi {
		t.Error("localCommit is assigned at or after the already-current guard, so a FIRST index " +
			"leaves it empty and a partial clone still loses its index")
	}
}

func indexOfForTest(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// The stamp DISCLOSES that the commit was "verified unchanged across the index
// run". Nothing verified it.
//
// verifySemanticWorktreeStable, which the comment above credits with that
// guarantee, compares worktree dirtiness and a worktree fingerprint -- both
// measured against whatever HEAD is current. `git commit --allow-empty` or a
// checkout while the provider runs moves HEAD and leaves the worktree clean,
// so it passes. On every other path validateLiveSemanticHeader catches it,
// because the provider resolved the commit itself; on THIS path brain stamped
// its own `head` into the header, so that comparison is a value against itself
// and cannot fail. The one path that makes the claim was the one path with
// nothing behind it.
func TestStampedProvenanceRefusesAMovedHead(t *testing.T) {
	t.Parallel()

	const before = "a80904c86091139e5451785c002d8fee1a9de85c"
	const beforeTree = "5b66bbca628cc9ecbbc4c8d6a69f1cf3c648db6a"
	const after = "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1"
	const afterTree = "e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2e2"

	runner := func(commit, tree string) CommandRunner {
		return commandRunnerFunc(func(_ context.Context, _, name string, args ...string) ([]byte, []byte, error) {
			if name != "git" || len(args) != 2 || args[0] != "rev-parse" {
				return nil, nil, fmt.Errorf("unexpected command %s %v", name, args)
			}
			switch args[1] {
			case "HEAD":
				return []byte(commit + "\n"), nil, nil
			case "HEAD^{tree}":
				return []byte(tree + "\n"), nil, nil
			}
			return nil, nil, fmt.Errorf("unexpected rev-parse %q", args[1])
		})
	}

	// Unchanged HEAD: the claim holds and the index proceeds.
	if err := verifySemanticHeadStable(context.Background(), runner(before, beforeTree), t.TempDir(), before, beforeTree); err != nil {
		t.Fatalf("an unchanged HEAD must not be refused: %v", err)
	}

	// A new commit on the same tree -- the exact shape a dirtiness check
	// cannot see, because the worktree is clean before and after.
	err := verifySemanticHeadStable(context.Background(), runner(after, beforeTree), t.TempDir(), before, beforeTree)
	if err == nil {
		t.Fatal("a HEAD that moved during indexing was accepted, so the snapshot would be published " +
			"stamped with a commit it does not describe, claiming it was verified unchanged")
	}
	if !strings.Contains(err.Error(), "head_changed") {
		t.Errorf("the refusal must be classifiable, got %q", err)
	}

	// The tree alone moving is refused too.
	if err := verifySemanticHeadStable(context.Background(), runner(before, afterTree), t.TempDir(), before, beforeTree); err == nil {
		t.Error("a HEAD tree that moved during indexing was accepted")
	}

	// A git that fails is an error, never a silent pass.
	broken := commandRunnerFunc(func(_ context.Context, _, _ string, _ ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("git exploded")
	})
	if err := verifySemanticHeadStable(context.Background(), broken, t.TempDir(), before, beforeTree); err == nil {
		t.Error("a failed recheck must not be read as 'unchanged'")
	}
}

// The check has to be WIRED, not merely present: a helper nobody calls proves
// nothing, and the previous defect on this same code path was an assignment in
// the wrong branch rather than a missing function.
func TestStampedProvenanceIsVerifiedBeforeItIsDisclosed(t *testing.T) {
	t.Parallel()

	src := readGoSourceForTest(t, "semantic.go")
	stamp := indexOfForTest(src, `if res.stampedHeader != ""`)
	if stamp < 0 {
		t.Fatal("the stamped-header disclosure is gone; this test needs rewriting")
	}
	disclose := indexOfForTest(src[stamp:], `"provider_header_stamped_locally"`)
	if disclose < 0 {
		t.Fatal("the disclosure warning code is gone; this test needs rewriting")
	}
	call := indexOfForTest(src[stamp:stamp+disclose], "verifySemanticHeadStable(")
	if call < 0 {
		t.Error("the stamped path discloses that the commit was verified unchanged without calling " +
			"verifySemanticHeadStable first, so nothing verifies it")
	}
}
