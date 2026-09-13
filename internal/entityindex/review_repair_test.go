package entityindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
)

type unavailableGraphRunner struct{ gitRunner }

func (r unavailableGraphRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name != "git" {
		return nil, nil, fmt.Errorf("provider unavailable")
	}
	return r.gitRunner.Run(ctx, dir, name, args...)
}

func TestInvalidWindowIsRetractedWhenRecoveryFails(t *testing.T) {
	f := newGitFixture(t)
	root := f.commit("root.go", "1", "root")
	f.run("checkout", "-q", "-b", "feature", root)
	f.commit("feature.go", "1", "feature")
	f.run("checkout", "-q", "main")
	f.commit("main.go", "1", "main")
	f.run("merge", "-q", "--no-ff", "-m", "feature into main", "feature")
	store := newTestStore(t)
	if _, err := Build(context.Background(), gitRunner{}, store, BuildOptions{RepoDir: f.dir, Now: fixedNow()}); err != nil {
		t.Fatal(err)
	}
	f.run("checkout", "-q", "feature")
	f.run("merge", "-q", "--no-ff", "-m", "main into feature", "main")
	f.run("checkout", "-q", "main")
	f.run("merge", "-q", "--ff-only", "feature")
	result, err := Build(context.Background(), unavailableGraphRunner{}, store, BuildOptions{RepoDir: f.dir, Limit: 1, Now: fixedNow()})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Window.Empty() {
		t.Fatalf("window = %+v", result.Window)
	}
	if raw, ok, err := store.ReadString(projectTarget, WindowKey("main")); err != nil || ok {
		t.Fatalf("invalid window persisted: %s %v", raw, err)
	}
}

func TestBoundaryRepairAndDetection(t *testing.T) {
	for _, mode := range []string{"depth1", "graft", "replace", "unshallowed"} {
		t.Run(mode, func(t *testing.T) {
			f := newGitFixture(t)
			f.commit("first.go", "1", "first")
			head := f.commit("second.go", "1", "second\n\nEntire-Checkpoint: abcdef123456")
			repo := f.dir
			if mode == "depth1" || mode == "replace" {
				repo = filepath.Join(t.TempDir(), "clone")
				f.run("clone", "-q", "--depth=1", "file://"+f.dir, repo)
			}
			if mode == "graft" {
				if err := os.WriteFile(filepath.Join(repo, ".git", "info", "grafts"), []byte(head+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "replace" {
				if _, stderr, err := runGit(context.Background(), repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "replace", "--graft", head); err != nil {
					t.Fatalf("replace: %v %s", err, stderr)
				}
			}
			store := newTestStore(t)
			entity := EntityDelta{Change: ChangeAdded, Kind: "function", Name: "fabricated", Path: "first.go"}
			raw, _ := json.Marshal(Delta{SchemaVersion: SchemaVersion, Base: EmptyTreeSHA, Head: head, Entities: []EntityDelta{entity}})
			muts := []gitmeta.Mutation{
				{Op: gitmeta.OpSetString, Target: CommitTarget(head), Key: ForwardKey, Value: string(raw)},
				{Op: gitmeta.OpListPush, Target: projectTarget, Key: EntityRecordKey(entity.Key()), Value: head, NowMS: 123},
				{Op: gitmeta.OpSetString, Target: projectTarget, Key: WindowKey("main"), Value: EncodeWindow(IndexWindow{Floor: head, Tip: head})},
			}
			if _, err := store.Update(len(muts), func(s gitmeta.State) (gitmeta.State, error) { return applyBatch(s, muts), nil }); err != nil {
				t.Fatal(err)
			}
			before, err := store.Tip()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Build(context.Background(), shallowRunner{}, store, BuildOptions{RepoDir: repo, Full: true, CheckpointsOnly: true, Now: fixedNow()}); err != nil {
				t.Fatal(err)
			}
			if after, err := store.Tip(); err != nil || after != before {
				t.Fatalf("checkpoint-only pass changed persisted coverage: %s -> %s (%v)", before, after, err)
			}
			result, err := Build(context.Background(), shallowRunner{}, store, BuildOptions{RepoDir: repo, Full: true, Now: fixedNow()})
			if err != nil {
				t.Fatal(err)
			}
			snap := loadSnapshot(t, store)
			if mode == "unshallowed" {
				delta, ok := snap.Delta(head)
				if !ok || delta.Base == EmptyTreeSHA {
					t.Fatalf("true parent delta was not recovered: %+v", delta)
				}
				if len(snap.Commits(entity.Key())) != 0 {
					t.Fatal("fabricated entity survives recovery")
				}
				assertWindowCovered(t, f, snap, result.Window)
				return
			}
			b := builder{ctx: context.Background(), runner: shallowRunner{}, opts: BuildOptions{RepoDir: repo}, result: &BuildResult{}, consecutiveFailures: 4}
			if ok, stop := b.indexCommit(commitInfo{SHA: head}); ok || stop || b.consecutiveFailures != 0 {
				t.Fatalf("boundary counted as a provider failure: indexed=%v stop=%v failures=%d", ok, stop, b.consecutiveFailures)
			}

			if _, ok := snap.RawDelta(head); ok {
				t.Fatal("fabricated boundary delta survives full repair")
			}
			if got := snap.Commits(entity.Key()); len(got) != 0 {
				t.Fatalf("fabricated reverse entries: %v", got)
			}
			if !result.Window.Empty() || result.Failed != 1 || !strings.Contains(strings.Join(result.Warnings, " "), "fetch complete history") {
				t.Fatalf("unusable history not clearly reported: %+v", result)
			}
		})
	}
}

func TestLegacyCappedDeltaRequiresOneVerification(t *testing.T) {
	d := Delta{Entities: make([]EntityDelta, 2000)}
	raw, _ := json.Marshal(d)
	if !deltaNeedsRepair(string(raw)) {
		t.Fatal("legacy silently capped delta treated as complete")
	}
	d.EntityCount = 2000
	raw, _ = json.Marshal(d)
	if deltaNeedsRepair(string(raw)) {
		t.Fatal("verified complete delta would repeat forever")
	}
}
