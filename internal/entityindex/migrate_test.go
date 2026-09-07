package entityindex

import (
	"context"
	"errors"
	"fmt"
	"github.com/ashtom/entire-brain/internal/factgitmeta/gitmeta"
	"reflect"
	"strings"
	"testing"
)

func TestMigrateIdentityPreservesHistoryAndUnrelatedMemory(t *testing.T) {
	for _, failure := range []string{"", "provider", "empty", "wrong-head", "revision", "missing-commit", "warnings", "missing-files", "cancelled"} {
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
			if err := CheckIdentity(store, "scope-1"); err == nil {
				t.Fatal("upgrade accepted without migration")
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
			result, err := Migrate(ctx, r, store, BuildOptions{RepoDir: testRepoDir, IdentityRevision: "scope-1", Now: fixedNow()})
			if failure != "" {
				if err == nil {
					t.Fatal("accepted partial migration")
				}
				after, _ := store.Tip()
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
			if err := CheckIdentity(store, "scope-1"); err != nil {
				t.Fatal(err)
			}
			snap := loadSnapshot(t, store)
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
			for _, c := range cs {
				d, ok := snap.Delta(c.SHA)
				if !ok || d.Head != c.SHA || d.IdentityRevision != "scope-1" {
					t.Fatal("lost forward provenance", d)
				}
			}
		})
	}
}
