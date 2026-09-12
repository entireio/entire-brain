package cli

import (
	"context"
	"sort"
	"testing"
)

// TestLoadAggregateCheckpointSources_UnionsEveryCandidateRef pins that the
// git-branch backend's aggregate discovery reads every candidate ref rather
// than stopping at the first one that yielded anything.
//
// entire/checkpoints/v1 is an ordinary branch, so refs/heads/... and
// refs/remotes/origin/... diverge in both directions: the CLI advances the
// local ref only under its own confinement rules, and checkpoints pushed from
// another clone land on origin's tracking ref without touching this one. A
// first-ref-wins lookup hid every origin-only checkpoint behind a single stale
// local one, and returned success while doing it.
func TestLoadAggregateCheckpointSources_UnionsEveryCandidateRef(t *testing.T) {
	t.Parallel()

	const localID = "aaaaaaaaaaaa"
	const originID = "bbbbbbbbbbbb"

	localTree := "aa/aaaaaaaaaa/metadata.json\naa/aaaaaaaaaa/0/metadata.json\naa/aaaaaaaaaa/0/full.jsonl\n"
	originTree := "bb/bbbbbbbbbb/metadata.json\nbb/bbbbbbbbbb/0/metadata.json\nbb/bbbbbbbbbb/0/full.jsonl\n"

	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):   {stdout: localTree},
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {stdout: originTree},
		},
	}

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, t.TempDir(), true, nil)

	got := make([]string, 0, len(sources))
	for id := range sources {
		got = append(got, id)
	}
	sort.Strings(got)

	want := []string{localID, originID}
	if len(got) != len(want) {
		t.Fatalf("discovered checkpoints %v, want %v (an origin-only checkpoint was dropped)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("discovered checkpoints %v, want %v", got, want)
		}
	}

	if sources[localID].Ref != v1MainRef {
		t.Errorf("local checkpoint resolved to %s, want %s", sources[localID].Ref, v1MainRef)
	}
	if sources[originID].Ref != v1OriginRef {
		t.Errorf("origin checkpoint resolved to %s, want %s", sources[originID].Ref, v1OriginRef)
	}
}

// TestLoadAggregateCheckpointSources_LocalRefWinsAConflict keeps the existing
// precedence: a checkpoint present on both refs is served from the local ref.
func TestLoadAggregateCheckpointSources_LocalRefWinsAConflict(t *testing.T) {
	t.Parallel()

	const sharedID = "aaaaaaaaaaaa"
	tree := "aa/aaaaaaaaaa/metadata.json\naa/aaaaaaaaaa/0/metadata.json\naa/aaaaaaaaaa/0/full.jsonl\n"

	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):   {stdout: tree},
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {stdout: tree},
		},
	}

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, t.TempDir(), true, nil)

	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	if sources[sharedID].Ref != v1MainRef {
		t.Errorf("shared checkpoint resolved to %s, want the local ref %s", sources[sharedID].Ref, v1MainRef)
	}
}
