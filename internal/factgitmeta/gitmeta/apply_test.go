// Vendored git-meta exchange engine. Originally copied verbatim from
// github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153) — do not edit here.
//
// PROVENANCE HAS MOVED: git-meta-service was a proof of concept, and the shipping
// implementation now lives in github.com/entirehq/entire-api internal/gitmeta. Re-vendor
// from THERE, not from the PoC repo, whose PR was superseded rather than merged.
//
// The two copies have since diverged in both directions — entire-api grew path-target
// support (PathTargetSubtree, ValidatePathTargetValue) and typed accessors, while this
// copy carries helpers of its own and the older ValidateTargetValue name. That is inert
// today because this engine only ever drives the BARE LOCAL store at
// refs/meta/local/main: no remote, no push or fetch, and no ref that entire-api also
// writes, so no record crosses between the two implementations. It stops being inert the
// moment brain exchanges git-meta records with entire-api over a shared ref, which is
// what makes deduplicating this the right follow-up: both are internal packages, so
// neither can import the other and a real fix means extracting the engine into a shared
// module.

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
