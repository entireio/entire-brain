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

// shallowRunner shells out to real git and synthesizes a provider response from
// the real tree diff. A grafted boundary is a property of an actual shallow
// clone — no scripted history can produce one — so this test drives real git.
type shallowRunner struct{}

func (shallowRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" {
		return runGitCmd(ctx, dir, args...)
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
	stdout, stderr, err := runGitCmd(ctx, repo, "diff", "--name-status", base, head)
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
		change := "body_changed"
		if fields[0] == "A" {
			change = "added"
		}
		result.Files = append(result.Files, graphFileChange{Path: path, Status: fields[0], Changes: []graphEntityChange{{
			Type: change, Kind: "function",
			Name:           "fn_" + strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
			AfterStartLine: 1,
		}}})
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	return encoded, nil, nil
}

func runGitCmd(ctx context.Context, dir string, args ...string) ([]byte, []byte, error) {
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

// TestBuildDoesNotTreatAShallowBoundaryAsARootCommit pins the difference
// between a commit that HAS no parent and one whose parent is merely not in
// this clone. A graft truncates `git log --format=%P`, so the boundary looks
// parentless to the walk and is diffed against the empty tree — which records
// every entity in the whole tree as ADDED by that commit and answers "which
// commit introduced this symbol" with a fabricated one, durably and with no
// warning anywhere.
func TestBuildDoesNotTreatAShallowBoundaryAsARootCommit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	origin := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		stdout, stderr, err := runGitCmd(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, stderr)
		}
		return strings.TrimSpace(string(stdout))
	}
	commit := func(dir, file, content, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		run(dir, "add", "-A")
		run(dir, "commit", "-q", "-m", message)
	}
	run(origin, "init", "-q", "-b", "main")
	run(origin, "config", "user.email", "index@test.invalid")
	run(origin, "config", "user.name", "Index Test")
	run(origin, "config", "commit.gpgsign", "false")
	commit(origin, "first.go", "1", "add first")
	commit(origin, "second.go", "1", "add second")
	commit(origin, "third.go", "1", "add third")

	clone := filepath.Join(t.TempDir(), "shallow")
	if _, stderr, err := runGitCmd(context.Background(), t.TempDir(), "clone", "-q", "--depth=2", "file://"+origin, clone); err != nil {
		t.Skipf("shallow clone unavailable: %v\n%s", err, stderr)
	}
	if run(clone, "rev-parse", "--is-shallow-repository") != "true" {
		t.Skip("clone is not shallow")
	}
	run(clone, "config", "user.email", "index@test.invalid")
	run(clone, "config", "user.name", "Index Test")
	head := run(clone, "rev-parse", "HEAD")
	boundary := run(clone, "rev-parse", "HEAD~1")
	if got := run(clone, "log", "-1", "--format=%P", boundary); got != "" {
		t.Fatalf("the boundary commit still reports parents (%q); the fixture is not grafted", got)
	}

	store := newTestStore(t)
	result, err := Build(context.Background(), shallowRunner{}, store, BuildOptions{RepoDir: clone, Now: fixedNow()})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	snap := loadSnapshot(t, store)

	if _, ok := snap.RawDelta(boundary); ok {
		delta, _ := snap.Delta(boundary)
		t.Fatalf("the shallow boundary %s was indexed against the empty tree: base=%s entities=%+v",
			short(boundary), short(delta.Base), delta.Entities)
	}
	if result.Window.Floor != head || result.Window.Tip != head {
		t.Fatalf("window = %+v, want the boundary excluded (floor=tip=%s)", result.Window, short(head))
	}
	// first.go exists in the clone's tree but no VISIBLE commit introduced it.
	// Claiming the boundary added it would be a fabricated answer.
	if commits := snap.Commits(EntityKey("first.go", "function", "fn_first")); len(commits) != 0 {
		t.Fatalf("an entity no visible commit touched is recorded against %v", commits)
	}
	// The commit that really is inside the visible range is still indexed.
	if _, ok := snap.RawDelta(head); !ok {
		t.Fatalf("head %s was not indexed", short(head))
	}
	var warned bool
	for _, w := range result.Warnings {
		if strings.Contains(w, "shallow") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the skipped boundary was not reported: %v", result.Warnings)
	}
}
