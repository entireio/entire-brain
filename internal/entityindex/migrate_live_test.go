package entityindex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Opt-in regression with real released Graph binaries. No installed plugin,
// user Git configuration, or customer metadata is used.
func TestGraphIdentityUpgradeLive(t *testing.T) {
	before, after := os.Getenv("BRAIN_TEST_GRAPH_BEFORE"), os.Getenv("BRAIN_TEST_GRAPH_AFTER")
	if before == "" || after == "" {
		t.Skip("set BRAIN_TEST_GRAPH_BEFORE and BRAIN_TEST_GRAPH_AFTER to released Graph binaries")
	}
	for _, binary := range []*string{&before, &after} {
		abs, err := filepath.Abs(*binary)
		if err != nil {
			t.Fatal(err)
		}
		*binary = abs
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &migrationLiveRunner{provider: before, env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root,
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"ENTIRE_BRAIN_NO_EGRESS=1", "ENTIRE_REPO_ROOT=" + repo,
	}}
	for _, key := range []string{"ENTIRE_PLUGIN_DATA_DIR", "ENTIRE_PLUGIN_CACHE_DIR", "ENTIRE_PLUGIN_STATE_DIR", "ENTIRE_PLUGIN_CONFIG_DIR", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		r.env = append(r.env, key+"="+filepath.Join(root, key))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	git := func(args ...string) string {
		t.Helper()
		out, stderr, err := r.Run(ctx, repo, "git", args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, stderr)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Migration Test")
	git("config", "user.email", "migration@example.invalid")
	write(".brainignore", "ignored.go\n")
	write("go.mod", "module example.invalid/migration\n\ngo 1.23\n")
	write("fixture.go", "package fixture\nfunc Render() string { return \"before\" }\n")
	write("fixture.ts", "export function outer() { function inner() { return 1; } return inner(); }\n")
	git("add", ".")
	git("commit", "-qm", "Initial fixture\n\nEntire-Checkpoint: aaaaaaaaaaaa")
	first := git("rev-parse", "HEAD")
	write("fixture.go", "package fixture\nfunc Render() string { return \"after\" }\n")
	write("fixture.ts", "export function outer() { function inner() { return 2; } return inner(); }\n")
	git("add", ".")
	git("commit", "-qm", "Update fixture\n\nEntire-Checkpoint: bbbbbbbbbbbb")
	head := git("rev-parse", "HEAD")

	oldRevision, err := ProviderIdentity(ctx, r, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	opts := BuildOptions{RepoDir: repo, Full: true, IdentityRevision: oldRevision}
	built, err := Build(ctx, r, store, opts)
	if err != nil || built.Indexed != 2 || built.Failed != 0 {
		t.Fatalf("legacy backfill: %+v, %v", built, err)
	}
	prior, err := store.State()
	if err != nil {
		t.Fatal(err)
	}
	legacy := LoadRevision(prior, oldRevision)
	if len(legacy.Commits("fixture.go#function#Render")) != 2 {
		t.Fatal("fixture did not produce legacy history")
	}

	r.provider = after
	newRevision, err := ProviderIdentity(ctx, r, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if newRevision == oldRevision {
		t.Fatal("select Graph releases with different identity revisions")
	}
	opts.IdentityRevision = newRevision
	result, err := Migrate(ctx, r, store, opts)
	if err != nil || result.Indexed != 2 {
		t.Fatalf("metadata files blocked migration: %+v, %v", result, err)
	}
	state, err := store.State()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy, LoadRevision(state, oldRevision)) {
		t.Fatal("migration changed legacy history")
	}
	current := LoadRevision(state, newRevision)
	if len(current.Commits("fixture.go#function#Render")) != 2 {
		t.Fatal("migration lost code history")
	}
	for _, sha := range []string{first, head} {
		delta, ok := current.Delta(sha)
		if !ok || delta.Head != sha || delta.IdentityRevision != newRevision {
			t.Fatalf("lost commit provenance: %+v", delta)
		}
	}
	again, err := Migrate(ctx, r, store, opts)
	if err != nil || again.Indexed != 0 || again.MetaTip != result.MetaTip {
		t.Fatalf("non-idempotent migration: %+v, %v", again, err)
	}

	// An older provider can extend its namespace after the migration. The
	// next migration must carry that commit forward without rewriting it.
	r.provider = before
	write("fixture.go", "package fixture\nfunc Render() string { return \"legacy follow-up\" }\n")
	git("add", ".")
	git("commit", "-qm", "Legacy follow-up\n\nEntire-Checkpoint: cccccccccccc")
	head = git("rev-parse", "HEAD")
	opts.IdentityRevision = oldRevision
	built, err = Build(ctx, r, store, opts)
	if err != nil || built.Indexed != 1 || built.Failed != 0 {
		t.Fatalf("legacy extension: %+v, %v", built, err)
	}
	r.provider = after
	opts.IdentityRevision = newRevision
	result, err = Migrate(ctx, r, store, opts)
	if err != nil || result.Indexed != 1 {
		t.Fatalf("incremental migration: %+v, %v", result, err)
	}
	state, err = store.State()
	if err != nil {
		t.Fatal(err)
	}
	for _, revision := range []string{oldRevision, newRevision} {
		if len(LoadRevision(state, revision).Commits("fixture.go#function#Render")) != 3 {
			t.Fatalf("missing history for revision %q", revision)
		}
	}
	if git("rev-parse", "HEAD") != head || git("status", "--porcelain") != "" {
		t.Fatal("migration changed source repository")
	}
}

type migrationLiveRunner struct {
	provider string
	env      []string
}

func (r *migrationLiveRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name == "entire" && len(args) > 0 && args[0] == "graph" {
		name, args = r.provider, args[1:]
	} else if name != "git" {
		return nil, nil, fmt.Errorf("unexpected live-test command: %s", name)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
