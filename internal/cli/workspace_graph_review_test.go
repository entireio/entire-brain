package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestWorkspaceRouteTopKDeduplicatesCanonicalPair(t *testing.T) {
	caller := workspaceGraphSymbolRef{RepoKey: "a", ID: "caller", Count: 5}
	lower := caller
	lower.Count = 3
	other := workspaceGraphSymbolRef{RepoKey: "a", ID: "other", Count: 1}
	handler := workspaceGraphSymbolRef{RepoKey: "b", ID: "handler", Count: 1}
	index := map[string]*workspaceExternalContractAggregate{
		"one":     {Endpoint: "external:route:/users/:id", Type: "HTTP_CALLS", Participants: []workspaceGraphSymbolRef{caller, other}},
		"two":     {Endpoint: "external:route:/users/{user}", Type: "HTTP_CALLS", Participants: []workspaceGraphSymbolRef{lower}},
		"handler": {Endpoint: "external:route:/users/{name}", Type: "HANDLES_ROUTE", Participants: []workspaceGraphSymbolRef{handler}},
	}
	got := workspaceGraphRouteCallCrossEdges(index, 2)
	if len(got) != 2 {
		t.Fatalf("got %d edges, want two unique pairs", len(got))
	}
	if workspaceGraphCrossEdgeID(got[0]) == workspaceGraphCrossEdgeID(got[1]) {
		t.Fatalf("duplicate pair displaced another edge: %+v", got)
	}
	if got[0].FromSymbol.ID != "caller" || got[0].SharedCount != 6 || got[1].FromSymbol.ID != "other" {
		t.Fatalf("unexpected best distinct pairs: %+v", got)
	}
}

func TestWorkspaceChannelTopKDeduplicatesPair(t *testing.T) {
	for _, counts := range [][]int{{3, 5}, {5, 3}} {
		index := map[string]*workspaceExternalContractAggregate{
			"emits": {Endpoint: "external:channel:updates", Type: "EMITS", Participants: []workspaceGraphSymbolRef{
				{RepoKey: "a", ID: "caller", Count: counts[0]},
				{RepoKey: "a", ID: "caller", Count: counts[1]},
				{RepoKey: "a", ID: "other", Count: 1},
			}},
			"listens": {Endpoint: "external:channel:updates", Type: "LISTENS_ON", Participants: []workspaceGraphSymbolRef{{RepoKey: "b", ID: "listener", Count: 1}}},
		}
		got := workspaceGraphChannelCrossEdges(index, 2)
		if len(got) != 2 || got[0].FromSymbol.ID != "caller" || got[0].SharedCount != 6 || got[1].FromSymbol.ID != "other" {
			t.Fatalf("counts %v: best distinct pairs lost: %+v", counts, got)
		}
	}
}

func TestWorkspaceGraphDegradedMemberKeepsHealthyEdges(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC) }}
	manifest := workspaceManifest{SchemaVersion: workspaceSchemaVersion, Name: "degraded"}
	var brokenPath string
	var original []byte
	for _, name := range []string{"A", "B", "Broken"} {
		repo := t.TempDir()
		key := testLocalRepoStorageKey(t, repo)
		indexWorkspaceGraphRepo(t, &cobra.Command{Use: "index"}, opts, runner, repo, key, "HandleShared"+name)
		manifest.Repos = append(manifest.Repos, workspaceRepo{RepoKey: key, LocalPathHint: repo})
		if name == "Broken" {
			brainDir, err := brainDirForKey(env, key)
			if err != nil {
				t.Fatal(err)
			}
			brokenPath = filepath.Join(brainDir, "manifest.json")
			original, err = os.ReadFile(brokenPath)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(brokenPath, []byte("{malformed"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatal(err)
	}
	payload, err := buildWorkspaceGraphPayload(context.Background(), opts, manifest, 20)
	if err != nil {
		t.Fatalf("one broken member aborted healthy graph: %v", err)
	}
	if len(payload.Results) != 3 || payload.Results[0].Error != "" || payload.Results[1].Error != "" || payload.Results[2].Error == "" || len(payload.CrossEdges) == 0 {
		t.Fatalf("degraded member diagnostics or healthy graph lost: %+v", payload)
	}
	if _, err = writeWorkspaceGraphPayload(env, payload); err != nil {
		t.Fatal(err)
	}
	cached, err := loadWorkspaceGraphPayload(env, manifest.Name)
	if err != nil || len(cached.CrossEdges) == 0 {
		t.Fatalf("degraded cache could not serve healthy edges: %+v %v", cached, err)
	}
	if err = os.WriteFile(brokenPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	cached, err = loadWorkspaceGraphPayload(env, manifest.Name)
	if err != nil || len(cached.CrossEdges) != 0 {
		t.Fatalf("healed member did not invalidate degraded generation: %+v %v", cached, err)
	}
}
