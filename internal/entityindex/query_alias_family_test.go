package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// renameRunner shells out to real git and turns the real, rename-detected tree
// diff into a provider response: one function entity per changed file, named
// after the file, so a `git mv` shows up as the move of an entity and the
// index's alias record is produced by an actual repository history rather than
// by a hand-written provider fixture.
type renameRunner struct{}

func (renameRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" {
		return runGitIn(ctx, dir, args...)
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
	stdout, stderr, err := runGitIn(ctx, repo, "diff", "--name-status", "-M", base, head)
	if err != nil {
		return nil, stderr, fmt.Errorf("provider diff %s..%s: %w", short(base), short(head), err)
	}
	result := graphResult{Base: base, Head: head}
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 || fields[0] == "" {
			continue
		}
		if strings.HasPrefix(fields[0], "R") && len(fields) == 3 {
			oldPath, newPath := fields[1], fields[2]
			result.Files = append(result.Files, graphFileChange{Path: newPath, OldPath: oldPath, Status: "R", Changes: []graphEntityChange{{
				Type: "moved", Kind: "function", Name: fileEntityName(newPath), OldName: fileEntityName(oldPath),
				OldPath: oldPath, NewPath: newPath, AfterStartLine: 1,
			}}})
			continue
		}
		path := fields[len(fields)-1]
		result.Files = append(result.Files, graphFileChange{Path: path, Status: fields[0], Changes: []graphEntityChange{{
			Type: "body_changed", Kind: "function", Name: fileEntityName(path), AfterStartLine: 1,
		}}})
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	return encoded, nil, nil
}

func fileEntityName(path string) string {
	base := filepath.Base(path)
	return "fn_" + strings.TrimSuffix(base, filepath.Ext(base))
}

func runGitIn(ctx context.Context, dir string, args ...string) ([]byte, []byte, error) {
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

// TestCommitsByCurrentKeyKeepThePreRenameHistory is the honesty invariant for a
// renamed or moved symbol: the answer must not depend on which of its spellings
// the caller asked by. The reverse index records a commit under the entity's
// POST-change key, so everything before a move lives under the OLD key —
// resolving aliases only forward makes a query by the symbol's CURRENT name
// (the only one that still exists in the tree) silently answer with the move
// commit alone.
func TestCommitsByCurrentKeyKeepThePreRenameHistory(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, stderr, err := runGitIn(context.Background(), dir, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, stderr)
		}
	}
	write := func(path, content string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "index@test.invalid")
	git("config", "user.name", "Index Test")
	git("config", "commit.gpgsign", "false")

	write("internal/pay/charge.go", "one")
	git("add", "-A")
	git("commit", "-q", "-m", "add charge")
	write("internal/pay/charge.go", "two")
	git("add", "-A")
	git("commit", "-q", "-m", "edit charge")
	if err := os.MkdirAll(filepath.Join(dir, "internal/billing"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("mv", "internal/pay/charge.go", "internal/billing/charge.go")
	git("commit", "-q", "-m", "move charge into billing")

	store := newTestStore(t)
	if _, err := Build(context.Background(), renameRunner{}, store, BuildOptions{RepoDir: dir, Now: fixedNow()}); err != nil {
		t.Fatalf("build: %v", err)
	}
	snap := loadSnapshot(t, store)

	oldKey := EntityKey("internal/pay/charge.go", "function", "fn_charge")
	currentKey := EntityKey("internal/billing/charge.go", "function", "fn_charge")
	if snap.Resolve(oldKey) != currentKey {
		t.Fatalf("the move produced no alias: Resolve(%s) = %s", oldKey, snap.Resolve(oldKey))
	}

	byOldKey := snap.Commits(oldKey)
	if len(byOldKey) != 3 {
		t.Fatalf("asking by the pre-move key returned %d commits, want the whole 3-commit history: %v", len(byOldKey), byOldKey)
	}
	byCurrentKey := snap.Commits(currentKey)
	if !reflect.DeepEqual(byCurrentKey, byOldKey) {
		t.Fatalf("history depends on the spelling asked by:\n  current key %s -> %v\n  old key     %s -> %v",
			currentKey, byCurrentKey, oldKey, byOldKey)
	}

	// The same must hold through the ranked search surface, which is what the
	// CLI and the MCP tool actually call: querying the symbol's current path
	// must not answer with a truncated history.
	matches := snap.Search("internal/billing/charge.go", 5)
	if len(matches) == 0 {
		t.Fatal("searching the current path matched nothing")
	}
	if !reflect.DeepEqual(matches[0].Commits, byOldKey) {
		t.Fatalf("search by the current path returned %v, want the full history %v", matches[0].Commits, byOldKey)
	}
}

func TestAliasFamilyIsTotalAndDeterministic(t *testing.T) {
	t.Parallel()
	a, b, c := "p.go#function#A", "p.go#function#B", "p.go#function#C"

	// A -> B -> C: every member sees the whole family, whichever end is asked.
	chain := map[string]string{a: b, b: c}
	want := []string{a, b, c}
	for _, key := range want {
		if got := AliasFamily(chain, key); !reflect.DeepEqual(got, want) {
			t.Fatalf("AliasFamily(%s) = %v, want %v", key, got, want)
		}
	}

	// A -> B -> A (renamed and renamed back) must terminate, not truncate.
	cycle := map[string]string{a: b, b: a}
	for _, key := range []string{a, b} {
		got := AliasFamily(cycle, key)
		if len(got) != 2 {
			t.Fatalf("AliasFamily(%s) over a cycle = %v, want both spellings exactly once", key, got)
		}
		seen := map[string]bool{}
		for _, k := range got {
			if seen[k] {
				t.Fatalf("AliasFamily(%s) repeated %s: %v", key, k, got)
			}
			seen[k] = true
		}
		if !seen[a] || !seen[b] {
			t.Fatalf("AliasFamily(%s) = %v, want both %s and %s", key, got, a, b)
		}
	}

	// Two old spellings folded into one current key: deterministic order.
	merged := map[string]string{a: c, b: c}
	first := AliasFamily(merged, c)
	if !reflect.DeepEqual(first, []string{a, b, c}) {
		t.Fatalf("AliasFamily(%s) = %v, want [%s %s %s]", c, first, a, b, c)
	}
	for i := 0; i < 25; i++ {
		if got := AliasFamily(merged, c); !reflect.DeepEqual(got, first) {
			t.Fatalf("AliasFamily is not deterministic: %v then %v", first, got)
		}
	}

	if got := AliasFamily(nil, a); !reflect.DeepEqual(got, []string{a}) {
		t.Fatalf("AliasFamily with no aliases = %v, want [%s]", got, a)
	}
}
