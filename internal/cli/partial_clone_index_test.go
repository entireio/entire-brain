package cli

import "testing"

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
// and verifySemanticWorktreeStable has already confirmed the tree did not move
// while the provider ran -- so the snapshot describes exactly that commit.
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
