package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// envRecordingRunner records the environment each spawn was given so a test
// can assert on it without a real git.
type envRecordingRunner struct {
	indexPath string
	calls     []recordedSpawn
}

type recordedSpawn struct {
	args []string
	env  map[string]string
	// indexModTime is the scratch index's mtime as the child would see it.
	indexModTime time.Time
	// indexSeen is the scratch index's contents AS THE CHILD WOULD SEE THEM.
	// It has to be captured here: the file is removed when the call returns,
	// which is the behaviour TestScratchIndexIsRemovedAfterTheCall asserts.
	indexSeen string
}

func (r *envRecordingRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, recordedSpawn{args: args})
	if len(args) >= 2 && args[0] == "rev-parse" {
		return []byte(r.indexPath + "\n"), nil, nil
	}
	return nil, nil, nil
}

func (r *envRecordingRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	seen := ""
	var mtime time.Time
	if p := env["GIT_INDEX_FILE"]; p != "" {
		if data, err := os.ReadFile(p); err == nil {
			seen = string(data)
		}
		if info, err := os.Stat(p); err == nil {
			mtime = info.ModTime()
		}
	}
	r.calls = append(r.calls, recordedSpawn{args: args, env: env, indexSeen: seen, indexModTime: mtime})
	return nil, nil, nil
}

// plainRunner has no environment capability.
type plainRunner struct{ calls int }

func (p *plainRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	p.calls++
	return nil, nil, nil
}

func writeFakeIndex(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index")
	if err := os.WriteFile(path, []byte("DIRC fake index contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --no-optional-locks does NOT gate the refresh a porcelain `git diff <commit>`
// performs; only the index it refreshes can be redirected. Measured on git
// 2.54.0 from an identical stat-dirty index each time.
func TestWorktreeDiffRunsAgainstAScratchIndex(t *testing.T) {
	t.Parallel()

	real := writeFakeIndex(t)
	runner := &envRecordingRunner{indexPath: real}

	if _, _, err := runGitWithScratchIndex(context.Background(), runner, t.TempDir(), "diff", "--name-status", "HEAD"); err != nil {
		t.Fatalf("run: %v", err)
	}

	var diff *recordedSpawn
	for i := range runner.calls {
		if len(runner.calls[i].args) > 0 && runner.calls[i].args[0] == "diff" {
			diff = &runner.calls[i]
		}
	}
	if diff == nil {
		t.Fatalf("the diff was never spawned; calls = %+v", runner.calls)
	}
	scratch := diff.env["GIT_INDEX_FILE"]
	if scratch == "" {
		t.Fatal("the diff ran without GIT_INDEX_FILE; it would refresh the repository's real index (#326)")
	}
	if scratch == real {
		t.Fatal("GIT_INDEX_FILE points at the REAL index; redirecting to itself protects nothing")
	}
	// The copy must carry the real contents: an empty index would make
	// `diff HEAD` report every tracked file as deleted.
	want, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if diff.indexSeen != string(want) {
		t.Fatalf("the child saw a scratch index that is not a copy of the real one:\n got %q\nwant %q", diff.indexSeen, want)
	}
}

// The scratch index holds a copy of repository metadata and must not outlive
// the call.
func TestScratchIndexIsRemovedAfterTheCall(t *testing.T) {
	t.Parallel()

	runner := &envRecordingRunner{indexPath: writeFakeIndex(t)}
	if _, _, err := runGitWithScratchIndex(context.Background(), runner, t.TempDir(), "diff", "HEAD"); err != nil {
		t.Fatal(err)
	}
	for _, c := range runner.calls {
		if p := c.env["GIT_INDEX_FILE"]; p != "" {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("scratch index %s still exists after the call (stat err=%v)", p, err)
			}
		}
	}
}

// A runner with no environment capability still runs the command, rather than
// failing the brief outright. Degraded, not broken.
func TestWorktreeDiffFallsBackWhenTheRunnerCannotSetEnv(t *testing.T) {
	t.Parallel()

	runner := &plainRunner{}
	if _, _, err := runGitWithScratchIndex(context.Background(), runner, t.TempDir(), "diff", "HEAD"); err != nil {
		t.Fatalf("fallback must still run the command: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("want exactly one spawn in the fallback path, got %d", runner.calls)
	}
}

// Issue #326 is about `brain brief` specifically. Pin the call path so a future
// edit cannot reintroduce a porcelain worktree diff there.
func TestNoPorcelainWorktreeDiffRemainsInTheBriefPath(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("semantic.go")
	if err != nil {
		t.Fatalf("read semantic.go: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, `"git", "diff"`) {
			continue
		}
		if strings.Contains(line, "--cached") || strings.Contains(line, "diff-index") {
			continue
		}
		t.Errorf("a porcelain worktree diff is spawned directly; it refreshes the real index:\n  %s", strings.TrimSpace(line))
	}
}

// The scratch index must be a faithful stand-in, and its MTIME is part of that.
//
// git decides whether a cached stat entry is trustworthy by comparing the
// file's recorded mtime against THE INDEX FILE'S OWN mtime. An entry whose
// file changed in the same moment the index was written is "racily clean" and
// git re-reads its content. A freshly written copy has a newer mtime than
// every tracked file, so git trusts the stale cache, and a same-moment edit --
// especially one that does not change the file's size -- comes back as
// UNCHANGED. Losing a worktree edit is worse than the lock contention this
// code exists to avoid.
//
// This asserts the MECHANISM, not the symptom, and deliberately so. The
// symptom needs a file whose mtime equals the one `git add` recorded while its
// content differs, which is a race this test loses on a fast machine: an
// earlier version of it passed with AND without the fix, making it no guard at
// all. The symptom is covered for real by
// TestWorktreeFingerprintDoesNotRunRepoLocalDiffExternal, whose fixture
// overwrites a 12-byte file with 12 different bytes immediately after
// committing it -- that is what caught this, on CI, after it passed locally.
func TestScratchIndexCopiesTheOriginalsModTime(t *testing.T) {
	t.Parallel()

	real := writeFakeIndex(t)
	// Make the original visibly older than anything created now, so a copy
	// that stamps its own time cannot coincidentally match.
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(real, old, old); err != nil {
		t.Fatal(err)
	}

	runner := &envRecordingRunner{indexPath: real}
	if _, _, err := runGitWithScratchIndex(context.Background(), runner, t.TempDir(), "diff", "HEAD"); err != nil {
		t.Fatal(err)
	}

	var seen time.Time
	for _, c := range runner.calls {
		if !c.indexModTime.IsZero() {
			seen = c.indexModTime
		}
	}
	if seen.IsZero() {
		t.Fatal("no scratch index was observed; the test proves nothing")
	}
	if !seen.Equal(old) {
		t.Fatalf("scratch index mtime is %s, want the original's %s; git would trust a stale stat cache "+
			"and report a racily-clean edit as unchanged", seen, old)
	}
}
