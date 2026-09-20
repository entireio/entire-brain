package entityindex

import (
	"context"
	"github.com/entireio/entire-brain/internal/factgitmeta/gitmeta"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestThreeNameRenameCycle(t *testing.T) {
	r := newFakeRunner()
	var cs []commitInfo
	names := []string{"A", "B", "C", "A"}
	for i, name := range names {
		sha := strings.Repeat(strconv.Itoa(i+1), 40)
		c := commitInfo{SHA: sha, CommittedAt: commitAt(i)}
		base := EmptyTreeSHA
		old := ""
		typ := "added"
		if i > 0 {
			c.Parent = cs[i-1].SHA
			base = c.Parent
			old = names[i-1]
			typ = "renamed"
		}
		cs = append(cs, c)
		r.set(graphDiffKey(base, sha), graphOutput(base, sha, graphFileChange{Path: "p.go", Changes: []graphEntityChange{{Type: typ, Kind: "function", Name: name, NewName: name, OldName: old}}}))
	}
	scriptRepo(r, "main", cs)
	store := newTestStore(t)
	if _, e := Build(context.Background(), r, store, BuildOptions{RepoDir: testRepoDir, Now: fixedNow()}); e != nil {
		t.Fatal(e)
	}
	snap := loadSnapshot(t, store)
	want := EntityKey("p.go", "function", "A")
	for _, name := range names[:3] {
		key := EntityKey("p.go", "function", name)
		if got := snap.Resolve(key); got != want {
			t.Errorf("Resolve(%s)=%s; want live spelling %s", key, got, want)
		}
	}
}
func BenchmarkBatchOneEntity(b *testing.B) {
	for _, n := range []int{1000, 10000, 20000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			muts := make([]gitmeta.Mutation, n)
			for i := range muts {
				muts[i] = gitmeta.Mutation{Op: gitmeta.OpListPush, Target: projectTarget, Key: "brain:entity:61", Value: "commit", NowMS: int64(i)}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = applyBatch(gitmeta.State{}, muts)
			}
		})
	}
}

func TestResolveLongAliasChainAndCycleEntry(t *testing.T) {
	aliases := map[string]string{}
	for i := 0; i < 100; i++ {
		aliases[strconv.Itoa(i)] = strconv.Itoa(i + 1)
	}
	if got := resolveAlias(aliases, nil, "0"); got != "100" {
		t.Fatalf("long chain truncated at %s", got)
	}
	aliases["100"] = "98"
	times := map[string]int64{"0": 999, "98": 2, "99": 3, "100": 1}
	for _, start := range []string{"0", "98", "99", "100"} {
		if got := resolveAlias(aliases, times, start); got != "99" {
			t.Errorf("cycle from %s resolved %s", start, got)
		}
	}
}

func TestBatchTimestampCacheSurvivesFallbackAndOverflow(t *testing.T) {
	key := "brain:entity:61"
	seed := gitmeta.State{Lists: []gitmeta.ListVal{{Target: projectTarget, Key: key, Entries: []gitmeta.ListEntry{{Value: "max", Timestamp: math.MaxInt64}}}}}
	muts := []gitmeta.Mutation{
		{Op: gitmeta.OpListPush, Target: projectTarget, Key: key, Value: "overflow", NowMS: 1},
		{Op: gitmeta.OpListPush, Target: projectTarget, Key: key, Value: "overflow-again", NowMS: 2},
		{Op: gitmeta.OpListRemove, Target: projectTarget, Key: key, Value: "max"},
		{Op: gitmeta.OpListPush, Target: projectTarget, Key: key, Value: "after-remove", NowMS: 10},
	}
	reference := seed
	for _, m := range muts {
		reference = reference.Apply(m)
	}
	if got := applyBatch(seed, muts); !reflect.DeepEqual(got, reference) {
		t.Fatalf("timestamp cache diverged: got %+v want %+v", got, reference)
	}
}
