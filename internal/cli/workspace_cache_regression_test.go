package cli

import (
	"fmt"
	"testing"
	"time"
)

func TestRemovedMemberCannotRemainInGraph(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	m := workspaceManifest{Name: "audit", Repos: []workspaceRepo{{RepoKey: "local/a"}, {RepoKey: "local/b"}}}
	if e := writeWorkspaceManifest(env, m); e != nil {
		t.Fatal(e)
	}
	edge := workspaceGraphCrossEdge{FromRepo: "local/a", ToRepo: "local/b", Type: "CALLS", RelationKind: "cross_repo_route_call", Endpoint: "removed-canary", FromSymbol: workspaceGraphSymbolRef{ID: "a"}, ToSymbol: workspaceGraphSymbolRef{ID: "b"}}
	if _, e := writeWorkspaceGraphPayload(env, workspaceGraphPayload{Generation: workspaceGraphTestGeneration(t, env, m), Workspace: "audit", GeneratedAt: time.Now(), CrossEdges: []workspaceGraphCrossEdge{edge}}); e != nil {
		t.Fatal(e)
	}
	if _, e := execute(t, newWorkspaceRemoveCommand(Options{Env: env}), "audit", "local/b"); e != nil {
		t.Fatal(e)
	}
	after, e := loadWorkspaceManifest(env, "audit")
	if e != nil || len(after.Repos) != 1 {
		t.Fatalf("remove failed: %v %v", after, e)
	}
	got, e := retrieveWorkspaceGraphCrossEdges(env, "audit", "removed-canary", 10)
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 0 {
		t.Fatalf("removed member still returned: %+v", got)
	}
}
func BenchmarkWorkspaceCrossEdgesBounded(b *testing.B) {
	for _, n := range []int{100, 300, 500} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			a := &workspaceExternalContractAggregate{Endpoint: "external:route:/shared", Type: "HTTP_CALLS", RepoCounts: map[string]int{"a": n, "b": n}}
			for _, r := range []string{"a", "b"} {
				for i := 0; i < n; i++ {
					a.Participants = append(a.Participants, workspaceGraphSymbolRef{RepoKey: r, ID: fmt.Sprintf("%s-%d", r, i), Count: 1})
				}
			}
			idx := map[string]*workspaceExternalContractAggregate{"shared": a}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got := workspaceGraphCrossEdges(idx, 1)
				if len(got) != 1 {
					b.Fatal(len(got))
				}
			}
		})
	}
}

func workspaceGraphTestGeneration(t *testing.T, env EntireEnv, manifest workspaceManifest) string {
	t.Helper()
	generation, err := workspaceGraphGeneration(env, manifest)
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestWorkspaceGraphRejectsChangedMemberGeneration(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	m := workspaceManifest{Name: "generations", Repos: []workspaceRepo{{RepoKey: "local/a"}, {RepoKey: "local/b"}}}
	if err := writeWorkspaceManifest(env, m); err != nil {
		t.Fatal(err)
	}
	edge := workspaceGraphCrossEdge{FromRepo: "local/a", ToRepo: "local/b", Endpoint: "canary", Type: "CALLS", FromSymbol: workspaceGraphSymbolRef{ID: "a"}, ToSymbol: workspaceGraphSymbolRef{ID: "b"}}
	payload := workspaceGraphPayload{Workspace: m.Name, Generation: workspaceGraphTestGeneration(t, env, m), CrossEdges: []workspaceGraphCrossEdge{edge}}
	if _, err := writeWorkspaceGraphPayload(env, payload); err != nil {
		t.Fatal(err)
	}
	if got, err := retrieveWorkspaceGraphCrossEdges(env, m.Name, "canary", 10); err != nil || len(got) != 1 {
		t.Fatalf("fresh cache missing: %+v %v", got, err)
	}
	dir, err := brainDirForKey(env, "local/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(dir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A concurrent builder can publish its old snapshot after the source changes.
	if _, err := writeWorkspaceGraphPayload(env, payload); err != nil {
		t.Fatal(err)
	}
	if got, err := retrieveWorkspaceGraphCrossEdges(env, m.Name, "canary", 10); err != nil || len(got) != 0 {
		t.Fatalf("stale cache served: %+v %v", got, err)
	}
	if _, found, err := getWorkspaceLevelRecord(env, m.Name, workspaceGraphCrossEdgeID(edge)); err != nil || found {
		t.Fatalf("get served stale edge: found=%v err=%v", found, err)
	}
}

func TestWorkspaceGraphBoundedSelectionMatchesRankedPairs(t *testing.T) {
	aggregate := &workspaceExternalContractAggregate{Endpoint: "external:test", Type: "CALLS", RepoCounts: map[string]int{"a": 8, "b": 8, "c": 8}}
	for _, repo := range []string{"c", "a", "b"} {
		for i := 7; i >= 0; i-- {
			aggregate.Participants = append(aggregate.Participants, workspaceGraphSymbolRef{RepoKey: repo, ID: fmt.Sprintf("%s-%d", repo, i), Count: (i * 7) % 11})
		}
	}
	var reference []workspaceGraphCrossEdge
	for _, from := range aggregate.Participants {
		for _, to := range aggregate.Participants {
			if from.RepoKey >= to.RepoKey {
				continue
			}
			reference = append(reference, workspaceGraphCrossEdge{Endpoint: aggregate.Endpoint, Type: aggregate.Type, FromRepo: from.RepoKey, ToRepo: to.RepoKey, FromSymbol: from, ToSymbol: to, SharedCount: from.Count + to.Count, RelationKind: "shared_external_contract"})
		}
	}
	sortWorkspaceGraphCrossEdges(reference)
	for _, limit := range []int{0, 1, 7, 50, 200} {
		got := workspaceGraphCrossEdges(map[string]*workspaceExternalContractAggregate{"test": aggregate}, limit)
		want := reference[:min(limit, len(reference))]
		if len(got) != len(want) {
			t.Fatalf("limit %d: length %d want %d", limit, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("limit %d rank %d got %+v want %+v", limit, i, got[i], want[i])
			}
		}
	}
}
