package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The first-parent contiguity regression needs REAL git: the bug is in what
// `git log --first-parent <tip>..<head>` omits when the tip is only reachable
// through a merge's second parent, which no scripted history can honestly
// model. gitRunner therefore shells out to git for real and synthesizes a
// provider response from the real tree diff — one function entity per changed
// file, which is all this test needs to tell "indexed" from "not indexed".
type gitRunner struct{}

func (gitRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" {
		return runGit(ctx, dir, args...)
	}
	var base, head, repo string
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--base":
			base = args[i+1]
		case "--head":
			head = args[i+1]
		case "--repo":
			repo = args[i+1]
		}
	}
	if repo == "" {
		repo = dir
	}
	stdout, stderr, err := runGit(ctx, repo, "diff", "--name-status", base, head)
	if err != nil {
		return nil, stderr, fmt.Errorf("provider diff %s..%s: %w", short(base), short(head), err)
	}
	result := graphResult{Base: base, Head: head}
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		path := fields[len(fields)-1]
		result.Files = append(result.Files, graphFileChange{Path: path, Status: fields[0], Changes: []graphEntityChange{{
			Type: "body_changed", Kind: "function", Name: "fn_" + strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), AfterStartLine: 1,
		}}})
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	return encoded, nil, nil
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2026-07-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-07-01T00:00:00Z",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

type gitFixture struct {
	t   *testing.T
	dir string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	f := &gitFixture{t: t, dir: t.TempDir()}
	f.run("init", "-q", "-b", "main")
	f.run("config", "user.email", "index@test.invalid")
	f.run("config", "user.name", "Index Test")
	f.run("config", "commit.gpgsign", "false")
	return f
}

func (f *gitFixture) run(args ...string) string {
	f.t.Helper()
	stdout, stderr, err := runGit(context.Background(), f.dir, args...)
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, stderr)
	}
	return strings.TrimSpace(string(stdout))
}

func (f *gitFixture) commit(file, content, message string) string {
	f.t.Helper()
	full := filepath.Join(f.dir, file)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.run("add", "-A")
	f.run("commit", "-q", "-m", message)
	return f.run("rev-parse", "HEAD")
}

// assertWindowCovered checks the window's ACTUAL claim: every first-parent
// commit from Floor to Tip inclusive carries a forward delta document.
func assertWindowCovered(t *testing.T, f *gitFixture, snap *Snapshot, w IndexWindow) {
	t.Helper()
	if w.Empty() {
		t.Fatal("window is empty; nothing was claimed")
	}
	var chain []string
	for _, sha := range strings.Fields(f.run("rev-list", "--first-parent", w.Tip)) {
		chain = append(chain, sha)
		if sha == w.Floor {
			break
		}
	}
	if len(chain) == 0 || chain[len(chain)-1] != w.Floor {
		t.Fatalf("floor %s is not on the first-parent chain of tip %s", short(w.Floor), short(w.Tip))
	}
	for _, sha := range chain {
		if _, ok := snap.RawDelta(sha); !ok {
			t.Fatalf("window %s..%s claims coverage but %s has no forward delta document (first-parent chain %v)",
				short(w.Floor), short(w.Tip), short(sha), chain)
		}
	}
}

// TestBuildDoesNotClaimCoverageOverASecondParentTip is the honesty invariant
// under the most ordinary merge topology there is: a feature branch that merged
// main into itself, after which main fast-forwards onto it.
//
// The stored tip (main's old head) is then only a merge's SECOND parent, so it
// is still an ancestor of the new head — but the head's FIRST-PARENT chain runs
// through the feature commits, which `<tip>..<head>` excludes because they are
// reachable from the tip. Advancing the tip over a range that never listed them
// leaves the window claiming a first-parent run it never indexed, and every
// later query answers "that entity was never touched" for the commits in the
// hole.
func TestBuildDoesNotClaimCoverageOverASecondParentTip(t *testing.T) {
	t.Parallel()
	f := newGitFixture(t)
	runner := gitRunner{}
	store := newTestStore(t)

	root := f.commit("root.go", "1", "root")
	f.run("checkout", "-q", "-b", "feature", root)
	feature1 := f.commit("feature1.go", "1", "feature one")
	feature2 := f.commit("feature2.go", "1", "feature two")
	f.run("checkout", "-q", "main")
	f.commit("main1.go", "1", "main one")
	f.run("merge", "-q", "--no-ff", "-m", "merge feature into main", "feature")
	mainMerge := f.run("rev-parse", "HEAD")

	first, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: f.dir, Now: fixedNow()})
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if first.Window.Tip != mainMerge {
		t.Fatalf("first window = %+v, want tip %s", first.Window, short(mainMerge))
	}
	assertWindowCovered(t, f, loadSnapshot(t, store), first.Window)

	// The feature branch merges main into itself, then main fast-forwards onto
	// it: main's recorded tip is now only the SECOND parent of the new head.
	f.run("checkout", "-q", "feature")
	f.run("merge", "-q", "--no-ff", "-m", "merge main into feature", "main")
	f.run("checkout", "-q", "main")
	f.run("merge", "-q", "--ff-only", "feature")
	head := f.run("rev-parse", "HEAD")
	if head == mainMerge {
		t.Fatal("main did not fast-forward onto the feature branch")
	}

	second, err := Build(context.Background(), runner, store, BuildOptions{RepoDir: f.dir, Now: fixedNow()})
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	snap := loadSnapshot(t, store)
	assertWindowCovered(t, f, snap, second.Window)

	// And the entities that only ever changed on the feature line are actually
	// answerable, rather than silently absent behind a window that claims them.
	for _, spec := range []struct {
		commit string
		key    string
	}{
		{feature1, EntityKey("feature1.go", "function", "fn_feature1")},
		{feature2, EntityKey("feature2.go", "function", "fn_feature2")},
	} {
		if _, ok := snap.RawDelta(spec.commit); !ok {
			t.Fatalf("commit %s inside the claimed window has no delta document", short(spec.commit))
		}
		if len(snap.Commits(spec.key)) == 0 {
			t.Fatalf("entity %s changed by a commit inside the claimed window has no recorded history", spec.key)
		}
	}
}
