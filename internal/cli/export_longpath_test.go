package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exportRelPathBudget bounds the longest path the export tree can build BELOW
// the store root. It is a regression guard, not an aspiration: the number is
// what the current layout produces, and the point is that it must not grow
// without someone reading this comment.
//
// Why it matters, on Windows specifically. A classic Win32 path is capped at
// 260 characters INCLUDING the store root, and the root is not small — a real
// one looks like
//
//	C:\Users\<user>\AppData\Local\entire\plugins\data\brain\repos\<repo-key>
//
// which is comfortably 70+ characters before any of this budget is spent. The
// relative part measured here is already larger than the whole Win32 limit, so
// on paper every deep export path is over.
//
// It works anyway, for one reason that is easy to lose: Go's os package
// rewrites a long ABSOLUTE path to its extended-length \\?\ form before calling
// Win32 (os/path_windows.go fixLongPath), and the store root is always
// absolute. That covers the brain's own reads and writes and nothing else.
// Everything outside Go's os package still sees a path over the limit — git
// subprocesses without core.longpaths, the user's backup tool, Explorer, a zip
// of the store. So the margin here is not "277 of 260 used", it is zero, and
// the only thing standing between this layout and a hard failure is a Go
// implementation detail.
//
// If this test fails because the number went UP, the fix is to shorten a
// component cap, not to raise the budget.
const exportRelPathBudget = 277

// The caps that produce the worst case, from sessionFileName and
// stableBranchDirComponent. Spelled out so a cap change shows up as a diff here
// rather than as a silently larger budget.
func TestExportRelativePathStaysWithinItsBudget(t *testing.T) {
	t.Parallel()

	// Inputs long enough to saturate every cap, and legal under
	// SafePathComponent so nothing is trimmed for any other reason.
	saturate := func(n int) string { return strings.Repeat("a", n+40) }

	branchDir := filepath.Join(
		"sessions",
		exportBranchesDirectory,
		stableBranchDirComponent(saturate(80)),
	)
	name := sessionFileName(
		time.Date(2026, 11, 11, 12, 34, 56, 0, time.UTC),
		saturate(32), // agent
		saturate(80), // session id
		saturate(32), // checkpoint id
		".jsonl",
	)
	rel := filepath.Join(branchDir, name)

	if got := len(rel); got > exportRelPathBudget {
		t.Fatalf("worst-case export relative path is %d chars, over the %d budget:\n  %s\n"+
			"Shorten a component cap rather than raising the budget; see the comment above.",
			got, exportRelPathBudget, rel)
	}

	// Guard the guard: a budget that nothing approaches would pass whatever the
	// layout did. The worst case is expected to sit AT the budget, so a change
	// that quietly shortens the layout should also update the constant.
	if got := len(rel); got < exportRelPathBudget {
		t.Fatalf("worst-case export relative path is %d chars but the budget is %d; "+
			"the layout got shorter, so lower exportRelPathBudget to match", got, exportRelPathBudget)
	}
}

// The single fact the budget rests on: brain's own relative path already
// exceeds the whole Win32 limit, so nothing outside Go's os package can address
// a deep export path on a machine without LongPathsEnabled.
func TestExportRelativePathExceedsWin32MaxPath(t *testing.T) {
	t.Parallel()

	const win32MaxPath = 260
	if exportRelPathBudget <= win32MaxPath {
		t.Fatalf("exportRelPathBudget (%d) no longer exceeds MAX_PATH (%d) — if the layout "+
			"really did shrink below it, delete this test and say so, because the \\\\?\\ "+
			"dependency documented above would no longer be load-bearing",
			exportRelPathBudget, win32MaxPath)
	}
}
