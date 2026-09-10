package entityindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
	"reflect"
	"strings"
	"testing"
)

func TestMigrateIdentityPreservesHistoryAndUnrelatedMemory(t *testing.T) {
	for _, failure := range []string{"", "provider", "empty", "wrong-head", "revision", "missing-commit", "warnings", "missing-files", "cancelled", "concurrent-write", "publish-busy"} {
		t.Run(fmt.Sprint("failure=", failure), func(t *testing.T) {
			ctx := context.Background()
			r := newFakeRunner()
			store := newTestStore(t)
			cs := []commitInfo{{SHA: strings.Repeat("a", 40), CommittedAt: commitAt(0), Message: "root\nEntire-Checkpoint: abc123\n"}, {SHA: strings.Repeat("b", 40), Parent: strings.Repeat("a", 40), CommittedAt: commitAt(1), Message: "edit"}}
			scriptRepo(r, "main", cs)
			for _, c := range cs {
				base := c.Parent
				if base == "" {
					base = EmptyTreeSHA
				}
				r.set(graphDiffKey(base, c.SHA), graphOutput(base, c.SHA, graphFileChange{Path: "a.js", Changes: []graphEntityChange{{Type: "added", Kind: "method", Name: "A.helper", AfterStartLine: 3}}}))
				r.set("git log -1 "+gitLogFormat+" "+c.SHA, fmt.Sprintf("%s\x00%s\x00%s\x00%s\x1e", c.SHA, c.Parent, c.CommittedAt.Format("2006-01-02T15:04:05Z07:00"), c.Message))
			}
			if _, err := Build(ctx, r, store, BuildOptions{RepoDir: testRepoDir, Now: fixedNow()}); err != nil {
				t.Fatal(err)
			}
			_, err := store.Update(1, func(st gitmeta.State) (gitmeta.State, error) {
				return st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: "brain:authored-fact", Value: "keep me"}), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, _ := store.Tip()
			priorState, err := store.State()
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range cs {
				base := c.Parent
				if base == "" {
					base = EmptyTreeSHA
				}
				r.set(graphDiffKey(base, c.SHA), graphOutput(base, c.SHA, graphFileChange{Path: "a.js", Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "A.m.helper", AfterStartLine: 3}}}))
			}
			for _, c := range cs {
				base := c.Parent
				if base == "" {
					base = EmptyTreeSHA
				}
				key := graphDiffKey(base, c.SHA)
				response := r.responses[key]
				response.stdout = strings.Replace(response.stdout, "{", `{"identity_revision":"scope-1",`, 1)
				r.responses[key] = response
			}
			r.set("entire graph version --json", `{"identity_revision":"scope-1"}`)
			if failure == "provider" {
				r.fail(graphDiffKey(cs[0].SHA, cs[1].SHA), errors.New("provider stopped"))
			}
			if failure == "empty" {
				r.set(graphDiffKey(cs[0].SHA, cs[1].SHA), "")
			}
			if failure == "wrong-head" {
				r.set(graphDiffKey(cs[0].SHA, cs[1].SHA), `{"identity_revision":"scope-1","base":"wrong","head":"wrong","files":[]}`)
			}
			if failure == "revision" {
				r.set("entire graph version --json", `{"identity_revision":"changed-again"}`)
			}
			if failure == "missing-commit" {
				r.fail("git log -1 "+gitLogFormat+" "+cs[1].SHA, errors.New("missing object"))
			}
			if failure == "warnings" {
				key := graphDiffKey(cs[0].SHA, cs[1].SHA)
				response := r.responses[key]
				response.stdout = strings.Replace(response.stdout, "{", `{"warnings":[{"code":"W_PARSE_ERROR"}],`, 1)
				r.responses[key] = response
			}
			if failure == "missing-files" {
				r.set(graphDiffKey(cs[0].SHA, cs[1].SHA), fmt.Sprintf(`{"identity_revision":"scope-1","base":%q,"head":%q}`, cs[0].SHA, cs[1].SHA))
			}
			if failure == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var concurrentTip string
			var heldUnlock func()
			defer func() {
				if heldUnlock != nil {
					heldUnlock()
				}
			}()
			progress := func(done int) {
				// Every provider invocation must run outside the shared lock.
				unlock, err := store.TryLock()
				if err != nil {
					t.Fatalf("migration holds lock during parsing: %v", err)
				}
				unlock()
				if failure == "publish-busy" && done == 2 {
					heldUnlock, err = store.TryLock()
					if err != nil {
						t.Fatal(err)
					}
				}
				if failure == "concurrent-write" && done == 1 {
					concurrentTip, err = store.Update(1, func(st gitmeta.State) (gitmeta.State, error) {
						return st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: "brain:concurrent-fact", Value: "preserve"}), nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := Migrate(ctx, r, store, BuildOptions{RepoDir: testRepoDir, IdentityRevision: "scope-1", Now: fixedNow(), Progress: progress})
			if failure != "" {
				if err == nil {
					t.Fatal("accepted partial migration")
				}
				after, _ := store.Tip()
				if failure == "concurrent-write" {
					before = concurrentTip
				}
				if after != before {
					t.Fatal("failed migration changed store")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Indexed != 2 {
				t.Fatal(result)
			}
			state, err := store.State()
			if err != nil {
				t.Fatal(err)
			}
			snap := LoadRevision(state, "scope-1")
			legacy := Load(state)
			preserved := gitmeta.State{Sets: state.Sets, Tombstones: state.Tombstones}
			for _, v := range state.Strings {
				if _, ok := originalRevisionKey(v.Key, ""); ok {
					preserved.Strings = append(preserved.Strings, v)
				}
			}
			for _, v := range state.Lists {
				if _, ok := originalRevisionKey(v.Key, ""); ok {
					preserved.Lists = append(preserved.Lists, v)
				}
			}
			if !reflect.DeepEqual(normalizeState(priorState), normalizeState(preserved)) {
				t.Fatal("migration modified older or unrelated records")
			}

			if got := legacy.Commits("a.js#method#A.helper"); len(got) != 2 {
				t.Fatalf("legacy history lost: %v", got)
			}
			if len(state.Tombstones) != 0 {
				t.Fatal("migration introduced synced deletions")
			}
			if got := snap.Commits("a.js#function#A.m.helper"); !reflect.DeepEqual(got, []string{cs[0].SHA, cs[1].SHA}) {
				t.Fatalf("history=%v", got)
			}
			if got := snap.Commits("a.js#method#A.helper"); len(got) != 0 {
				t.Fatalf("stale reverse entries: %v", got)
			}
			if value, ok, err := store.ReadString(projectTarget, "brain:authored-fact"); err != nil || !ok || value != "keep me" {
				t.Fatal("authored memory changed")
			}
			w, ok := snap.Window("main")
			if !ok || w.Floor != cs[0].SHA || w.Tip != cs[1].SHA {
				t.Fatal("coverage changed", w)
			}
			// Simulate delivery of the serialized git-meta generation to an
			// older peer. Its legacy writer must preserve the new namespace.
			peer := newTestStore(t)
			if _, err := peer.Update(1, func(gitmeta.State) (gitmeta.State, error) { return state, nil }); err != nil {
				t.Fatal(err)
			}
			legacyRunner := newFakeRunner()
			third := commitInfo{SHA: strings.Repeat("c", 40), Parent: cs[1].SHA, CommittedAt: commitAt(2)}
			scriptRepo(legacyRunner, "main", append(append([]commitInfo(nil), cs...), third))
			legacyRunner.set(graphDiffKey(third.Parent, third.SHA), graphOutput(third.Parent, third.SHA, graphFileChange{Path: "a.js", Changes: []graphEntityChange{{Type: "modified", Kind: "method", Name: "A.helper"}}}))
			if _, err := Build(ctx, legacyRunner, peer, BuildOptions{RepoDir: testRepoDir}); err != nil {
				t.Fatal(err)
			}
			peerState, err := peer.State()
			if err != nil {
				t.Fatal(err)
			}
			if len(Load(peerState).Commits("a.js#method#A.helper")) != 3 {
				t.Fatal("legacy peer could not extend history")
			}
			if len(LoadRevision(peerState, "scope-1").Commits("a.js#function#A.m.helper")) != 2 {
				t.Fatal("legacy peer corrupted revised history")
			}
			if !HasOtherHistory(peerState, "scope-1") || HasOtherHistory(peerState, "") {
				t.Fatal("cross-revision coverage hint incorrect")
			}
			if len(peerState.Tombstones) != len(state.Tombstones) {
				t.Fatal("peer exchange introduced tombstones")
			}
			// A second migration is a no-op, without duplicate reverse entries.
			tip, _ := store.Tip()
			again, err := Migrate(ctx, r, store, BuildOptions{RepoDir: testRepoDir, IdentityRevision: "scope-1", Now: fixedNow()})
			if err != nil || again.MetaTip != tip || again.Indexed != 0 {
				t.Fatalf("non-idempotent migration: %+v %v", again, err)
			}
			for _, c := range cs {
				d, ok := snap.Delta(c.SHA)
				if !ok || d.Head != c.SHA || d.IdentityRevision != "scope-1" {
					t.Fatal("lost forward provenance", d)
				}
			}
		})
	}
}

// Regression for the high finding: an advisory global marker cannot invalidate
// legacy documents or force an unchanged backfill to parse historical JSON.
func TestLegacyHistoryIgnoresGlobalIdentityMarker(t *testing.T) {
	ctx := context.Background()
	r := newFakeRunner()
	store := newTestStore(t)
	c := commitInfo{SHA: strings.Repeat("a", 40), CommittedAt: commitAt(0)}
	scriptRepo(r, "main", []commitInfo{c})
	r.set(graphDiffKey(EmptyTreeSHA, c.SHA), graphOutput(EmptyTreeSHA, c.SHA, graphFileChange{Path: "a.js", Changes: []graphEntityChange{{Type: "added", Kind: "function", Name: "old"}}}))
	if _, err := Build(ctx, r, store, BuildOptions{RepoDir: testRepoDir}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update(2, func(st gitmeta.State) (gitmeta.State, error) {
		st = st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: projectTarget, Key: IdentityKey, Value: "scope-1"})
		// Unrelated older data is intentionally malformed. A steady-state tick
		// must not deserialize it while reading its own valid coverage window.
		st = st.Apply(gitmeta.Mutation{Op: gitmeta.OpSetString, Target: CommitTarget(strings.Repeat("b", 40)), Key: ForwardKey, Value: "not-json"})
		return st, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.Tip()
	calls := r.called("graph diff")
	got, err := Build(ctx, r, store, BuildOptions{RepoDir: testRepoDir})
	if err != nil || got.Indexed != 0 || got.MetaTip != before || r.called("graph diff") != calls {
		t.Fatalf("legacy tick regressed: %+v %v", got, err)
	}
	if len(loadSnapshot(t, store).Commits("a.js#function#old")) != 1 {
		t.Fatal("legacy query blocked")
	}
}

// Migration must preserve the established ParseDiff mapping policy: an
// entity-less file change or an unkeyable record must not block other history.
func TestMigrateUsesBackfillEntityMapping(t *testing.T) {
	ctx := context.Background()
	r := newFakeRunner()
	store := newTestStore(t)
	c := commitInfo{SHA: strings.Repeat("a", 40), CommittedAt: commitAt(0)}
	scriptRepo(r, "main", []commitInfo{c})
	raw := graphOutput(EmptyTreeSHA, c.SHA,
		graphFileChange{Path: "renamed.txt", OldPath: "old.txt", Status: "renamed"},
		graphFileChange{Path: "a.go", Changes: []graphEntityChange{
			{Type: "modified", Kind: "function", Name: "keep"},
			{Type: "modified", Kind: "function"},    // no name: skipped by the mapper
			{Type: "modified", Name: "legacy-kind"}, // absent kind: retained as before
		}},
		graphFileChange{Changes: []graphEntityChange{{Type: "modified", Kind: "function", Name: "no-path"}}},
	)
	r.set(graphDiffKey(EmptyTreeSHA, c.SHA), raw)
	if _, err := Build(ctx, r, store, BuildOptions{RepoDir: testRepoDir, Now: fixedNow()}); err != nil {
		t.Fatal(err)
	}
	legacy, ok := loadSnapshot(t, store).Delta(c.SHA)
	if !ok || len(legacy.Entities) != 2 {
		t.Fatalf("legacy mapping: %+v", legacy)
	}
	r.set(graphDiffKey(EmptyTreeSHA, c.SHA), strings.Replace(raw, "{", `{"identity_revision":"scope-1",`, 1))
	r.set("git log -1 "+gitLogFormat+" "+c.SHA, fmt.Sprintf("%s\x00\x00%s\x00root\x1e", c.SHA, c.CommittedAt.Format("2006-01-02T15:04:05Z07:00")))
	r.set("entire graph version --json", `{"identity_revision":"scope-1"}`)
	result, err := Migrate(ctx, r, store, BuildOptions{RepoDir: testRepoDir, IdentityRevision: "scope-1", Now: fixedNow()})
	if err != nil || result.Indexed != 1 {
		t.Fatalf("migration rejected supported mapping: %+v %v", result, err)
	}
	state, err := store.State()
	if err != nil {
		t.Fatal(err)
	}
	migrated, ok := LoadRevision(state, "scope-1").Delta(c.SHA)
	if !ok || !reflect.DeepEqual(migrated.Entities, legacy.Entities) {
		t.Fatalf("mapping diverged: %+v vs %+v", migrated, legacy)
	}
}

func TestMigrateLimitCountsOnlyMissingCommits(t *testing.T) {
	for _, alreadyPresent := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyPresent), func(t *testing.T) {
			r := newFakeRunner()
			store := newTestStore(t)
			ctx := context.Background()
			shas := []string{strings.Repeat("a", 40), strings.Repeat("b", 40)}
			state := gitmeta.State{}
			for _, sha := range shas {
				d := Delta{SchemaVersion: SchemaVersion, Head: sha, Base: EmptyTreeSHA, Entities: []EntityDelta{}}
				raw, _ := json.Marshal(d)
				state.Strings = append(state.Strings, gitmeta.StringVal{Target: CommitTarget(sha), Key: ForwardKey, Value: string(raw)})
				r.set("git log -1 "+gitLogFormat+" "+sha, fmt.Sprintf("%s\x00\x00%s\x00root\x1e", sha, commitAt(0).Format("2006-01-02T15:04:05Z07:00")))
				r.set(graphDiffKey(EmptyTreeSHA, sha), fmt.Sprintf(`{"base":%q,"head":%q,"identity_revision":"scope-1","files":[]}`, EmptyTreeSHA, sha))
			}
			if alreadyPresent {
				// A corpus larger than the cap is allowed when only one commit is missing.
				d := state.Strings[0]
				var delta Delta
				if err := json.Unmarshal([]byte(d.Value), &delta); err != nil {
					t.Fatal(err)
				}
				delta.IdentityRevision = "scope-1"
				raw, _ := json.Marshal(delta)
				d.Value = string(raw)
				d.Key = RevisionKey(ForwardKey, "scope-1")
				state.Strings = append(state.Strings, d)
			}
			if _, err := store.Update(1, func(gitmeta.State) (gitmeta.State, error) { return state, nil }); err != nil {
				t.Fatal(err)
			}
			before, _ := store.Tip()
			r.set("entire graph version --json", `{"identity_revision":"scope-1"}`)
			opts := BuildOptions{RepoDir: testRepoDir, IdentityRevision: "scope-1", Now: fixedNow()}
			// Scale the production cap down to one to exercise its boundary cheaply.
			result, err := migrate(ctx, r, store, opts, 1)
			if !alreadyPresent {
				after, _ := store.Tip()
				if err == nil || after != before || r.called("graph diff") != 0 {
					t.Fatalf("excess pending work published: %+v %v", result, err)
				}
				return
			}
			if err != nil || result.Indexed != 1 || result.Skipped != 1 || r.called("graph diff") != 1 {
				t.Fatalf("counted old history against cap: %+v %v", result, err)
			}
			again, err := migrate(ctx, r, store, opts, 1)
			if err != nil || again.Indexed != 0 || again.MetaTip != result.MetaTip {
				t.Fatalf("large no-op migration rejected: %+v %v", again, err)
			}
		})
	}
}
