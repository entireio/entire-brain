// Copied verbatim from github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153) — do not edit; de-internalize
// upstream to dedupe (follow-up).

package gitmeta

import (
	"testing"

	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

func TestApplyMutationsRoundTrip(t *testing.T) {
	commit := mustTarget(t, "commit:13a7d29cde8f8557b54fd6474f547a56822180ae")

	st := State{}
	st = st.Apply(Mutation{Op: OpSetString, Target: commit, Key: "agent:model", Value: "claude"})
	st = st.Apply(Mutation{Op: OpSetAdd, Target: commit, Key: "review:approvers", Value: "alice"})
	st = st.Apply(Mutation{Op: OpSetAdd, Target: commit, Key: "review:approvers", Value: "bob"})
	st = st.Apply(Mutation{Op: OpListPush, Target: commit, Key: "agent:chat", Value: "one", NowMS: 1000})
	st = st.Apply(Mutation{Op: OpListPush, Target: commit, Key: "agent:chat", Value: "two", NowMS: 1000}) // same ms -> bumped

	if got := findString(st, commit.String(), "agent:model"); got != "claude" {
		t.Fatalf("agent:model=%q", got)
	}
	if m := findSet(st, commit.String(), "review:approvers"); len(m) != 2 {
		t.Fatalf("approvers=%v", m)
	}
	entries := findList(st, commit.String(), "agent:chat")
	if len(entries) != 2 || entries[0].Timestamp == entries[1].Timestamp {
		t.Fatalf("list timestamps not distinct: %+v", entries)
	}

	// Serialize then materialize: state survives a Git round-trip.
	store := memory.NewStorage()
	h, err := Serialize(st, store)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := object.GetTree(store, h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Materialize(tree)
	if err != nil {
		t.Fatal(err)
	}
	if findString(got, commit.String(), "agent:model") != "claude" {
		t.Fatal("string lost in round-trip")
	}
	if len(findList(got, commit.String(), "agent:chat")) != 2 {
		t.Fatal("list lost in round-trip")
	}
	if len(findSet(got, commit.String(), "review:approvers")) != 2 {
		t.Fatal("set lost in round-trip")
	}

	// Removing the string adds a whole-key tombstone and clears the value.
	st = st.Apply(Mutation{Op: OpSetString, Target: commit, Key: "agent:model", Value: "x"})
	st = st.Apply(Mutation{Op: OpRemoveKey, Target: commit, Key: "agent:model"})
	if findString(st, commit.String(), "agent:model") != "" {
		t.Fatal("expected value cleared after rm")
	}
	var hasTomb bool
	for _, ts := range st.Tombstones {
		if ts.Key == "agent:model" && ts.Entry == "" && ts.Member == "" {
			hasTomb = true
		}
	}
	if !hasTomb {
		t.Fatal("expected whole-key tombstone after rm")
	}
}
