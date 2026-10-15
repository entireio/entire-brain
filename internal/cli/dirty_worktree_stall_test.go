package cli

import (
	"strings"
	"testing"
)

// Issue #327: with a dirty worktree, indexing stalled permanently.
//
// The loop was: needsSemanticRebuild saw a dirty tree and returned true →
// runSemanticIndex refused with dirty_worktree because --worktree was absent →
// watch treated the refusal as a failure, aborted the tick and left the cursor
// unmoved → the next tick did exactly the same. Nothing advanced for as long as
// the tree stayed dirty, which for anyone working is most of the time.
//
// Both halves are pinned here, at the source level: the behaviour needs a real
// git repo plus a provider, and these two source facts are what actually
// closed the loop.

// Dirtiness alone must not force a rebuild. The index describes HEAD, and HEAD
// has not moved because the worktree has uncommitted edits.
func TestDirtyWorktreeDoesNotForceARebuildItCannotPerform(t *testing.T) {
	t.Parallel()

	src := readSourceForTest(t, "refresh.go")
	fn := functionBodyForTest(t, src, "semanticRefreshNeeded")
	if fn == "" {
		t.Fatal("semanticRefreshNeeded not found; this guard is now vacuous")
	}

	// The exact shape that caused the stall: an unconditional true on dirty.
	normalized := strings.Join(strings.Fields(fn), " ")
	if strings.Contains(normalized, "if dirty { return true, warning, nil }") {
		t.Error("a dirty worktree forces a rebuild that runSemanticIndex then refuses (#327); " +
			"let the HEAD-tree comparison below decide instead")
	}
}

// A refusal to index uncommitted content must not abort the whole tick.
func TestWatchContinuesWhenTheWorktreeIsMerelyDirty(t *testing.T) {
	t.Parallel()

	src := readSourceForTest(t, "watch.go")
	// The constant, not its value: watch.go refers to it by name.
	if !strings.Contains(src, "dirtyWorktreeErrorCode") {
		t.Fatal("watch.go does not distinguish the dirty-worktree refusal, so a dirty tree still aborts the tick (#327)")
	}
	if !strings.Contains(src, "continuing") {
		t.Error("watch.go recognises the refusal but does not say it continued past it")
	}
	// ...and it must still abort on everything else: spending tokens against
	// an unrefreshed brain is the reason the abort exists.
	if !strings.Contains(src, "skipping agent work this tick") {
		t.Error("the general refresh-failure abort was removed; a real failure must still stop the tick")
	}
}
