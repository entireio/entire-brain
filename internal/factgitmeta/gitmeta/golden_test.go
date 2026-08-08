// Copied verbatim from github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153); do not edit; de-internalize
// upstream to dedupe (follow-up).

package gitmeta

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"
)

// TestGoldenRoundTrip drives the real `git meta` Rust CLI to produce a metadata
// tree, then materializes and re-serializes it in Go and asserts the resulting
// tree object ID is byte-for-byte identical. This proves both Materialize and
// Serialize match the reference exchange format. Skips if the CLI is absent.
func TestGoldenRoundTrip(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	metaBin := lookGitMeta(t)
	if metaBin == "" {
		t.Skip("git-meta CLI not found")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.io",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.io",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	meta := func(args ...string) {
		t.Helper()
		full := append([]string{"-c", "commit.gpgsign=false", "meta"}, args...)
		run(full...)
	}

	run("init", "-q")
	run("-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "root")
	sha := strings.TrimSpace(gitOut(t, gitBin, dir, "rev-parse", "HEAD"))

	// A representative mix: strings, a multi-entry list, a multi-member set,
	// and non-commit targets (path, project, branch).
	meta("set", "commit:"+sha, "agent:model", "claude-opus-4.6")
	meta("set", "commit:"+sha, "agent:provider", "anthropic")
	meta("list:push", "commit:"+sha, "agent:chat", "hello world")
	meta("list:push", "commit:"+sha, "agent:chat", "second line")
	meta("set:add", "commit:"+sha, "review:approvers", "schacon")
	meta("set:add", "commit:"+sha, "review:approvers", "bob")
	meta("set", "path:src/metrics", "owner", "schacon")
	meta("set", "branch:feature-x", "status", "ready")
	meta("set", "project", "meta:name", "demo")
	meta("serialize")

	// Read the CLI's serialized tree via go-git.
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	ref, err := repo.Reference(plumbing.ReferenceName("refs/meta/local/main"), true)
	if err != nil {
		t.Fatalf("resolve refs/meta/local/main: %v", err)
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	cliTree, err := commit.Tree()
	if err != nil {
		t.Fatalf("commit tree: %v", err)
	}
	cliTreeHash := cliTree.Hash

	// Materialize the CLI tree, then re-serialize it in Go.
	state, err := Materialize(cliTree)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	goHash, err := Serialize(state, memory.NewStorage())
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	if goHash != cliTreeHash {
		t.Fatalf("tree OID mismatch:\n  cli = %s\n  go  = %s\n  state = %+v", cliTreeHash, goHash, state)
	}
	t.Logf("round-trip tree OID matches: %s", goHash)

	// Spot-check that materialize recovered the expected logical values.
	if got := findString(state, "commit:"+sha, "agent:model"); got != "claude-opus-4.6" {
		t.Errorf("agent:model = %q", got)
	}
	if n := len(findList(state, "commit:"+sha, "agent:chat")); n != 2 {
		t.Errorf("agent:chat entries = %d, want 2", n)
	}
	if m := findSet(state, "commit:"+sha, "review:approvers"); len(m) != 2 {
		t.Errorf("review:approvers members = %v", m)
	}
}

func lookGitMeta(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("git-meta"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	cand := filepath.Join(home, ".local", "bin", "git-meta")
	if _, err := os.Stat(cand); err == nil {
		// Ensure `git meta` resolves to it by prepending to PATH for child procs.
		_ = os.Setenv("PATH", filepath.Dir(cand)+string(os.PathListSeparator)+os.Getenv("PATH"))
		return cand
	}
	return ""
}

func gitOut(t *testing.T, gitBin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func findString(s State, target, key string) string {
	for _, sv := range s.Strings {
		if sv.Target.String() == target && sv.Key == key {
			return sv.Value
		}
	}
	return ""
}

func findList(s State, target, key string) []ListEntry {
	for _, lv := range s.Lists {
		if lv.Target.String() == target && lv.Key == key {
			return lv.Entries
		}
	}
	return nil
}

func findSet(s State, target, key string) []string {
	for _, sv := range s.Sets {
		if sv.Target.String() == target && sv.Key == key {
			return sv.Members
		}
	}
	return nil
}
