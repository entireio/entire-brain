package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

func TestDistillRelationshipStoreV2RoundTripAndOwnerLifecycle(t *testing.T) {
	brainDir := t.TempDir()
	proposal := relationshipStoreFixture(t)
	store := newDistillRelationshipStoreV2()
	if err := store.Put(proposal); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(proposal); err != nil { // idempotently union, never duplicate
		t.Fatal(err)
	}
	if err := saveDistillRelationshipStoreV2(brainDir, store); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(distillRelationshipStoreV2Path))
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("relationship store mode = %v, err=%v", info.Mode(), err)
	}
	loaded, err := loadDistillRelationshipStoreV2(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.entries, store.entries) {
		t.Fatalf("round trip = %#v, want %#v", loaded.entries, store.entries)
	}
	removed, err := loaded.RemoveOwner(factmerge.RelationshipOwner{CandidateID: "candidate:a", SourceSessionID: "session:1"})
	if err != nil || removed != 1 {
		t.Fatalf("RemoveOwner() = %d, %v", removed, err)
	}
	if len(loaded.entries) != 1 || len(loaded.entries[proposal.ID].Owners) != 1 {
		t.Fatalf("remaining ownership = %#v", loaded.entries)
	}
	if _, err := loaded.RemoveOwner(factmerge.RelationshipOwner{CandidateID: "candidate:b", SourceSessionID: "session:2"}); err != nil {
		t.Fatal(err)
	}
	if len(loaded.entries) != 0 {
		t.Fatalf("relationship without owners was retained: %#v", loaded.entries)
	}
}

func TestDistillRelationshipStoreV2RejectsCorruptAndOversize(t *testing.T) {
	proposal := relationshipStoreFixture(t)
	store := newDistillRelationshipStoreV2()
	if err := store.Put(proposal); err != nil {
		t.Fatal(err)
	}
	data, err := marshalDistillRelationshipStoreV2(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDistillRelationshipStoreV2([]byte(`{"type":"header","version":1}` + "\n" + `{"type":"relationship","relationship":{}}` + "\n")); err == nil {
		t.Fatal("accepted corrupt relationship")
	}
	if _, err := parseDistillRelationshipStoreV2(append(data, []byte("\n")...)); err == nil {
		t.Fatal("accepted blank NDJSON line")
	}
	if _, err := parseDistillRelationshipStoreV2(make([]byte, distillRelationshipStoreV2MaxBytes+1)); err == nil {
		t.Fatal("accepted oversized relationship store")
	}
	if _, err := parseDistillRelationshipStoreV2([]byte(strings.Repeat("x", distillRelationshipStoreV2MaxLine+1))); err == nil {
		t.Fatal("accepted oversized relationship line")
	}
}

func TestDistillRelationshipStoreV2RejectsOwnerUnionOverflowTransactionally(t *testing.T) {
	proposal := relationshipStoreFixture(t)
	owners := func(start, count int) []factmerge.RelationshipOwner {
		out := make([]factmerge.RelationshipOwner, 0, count)
		for index := start; index < start+count; index++ {
			out = append(out, factmerge.RelationshipOwner{
				CandidateID:     fmt.Sprintf("candidate:%03d", index),
				SourceSessionID: fmt.Sprintf("session:%03d", index),
			})
		}
		return factmerge.UnionRelationshipOwners(out)
	}
	proposal.Owners = owners(0, factmerge.RelationshipMaxOwners/2)
	store := newDistillRelationshipStoreV2()
	if err := store.Put(proposal); err != nil {
		t.Fatal(err)
	}
	before := cloneDistillRelationshipProposalV2(store.entries[proposal.ID])
	additional := proposal
	additional.Owners = owners(factmerge.RelationshipMaxOwners/2, factmerge.RelationshipMaxOwners/2+1)
	if err := store.Put(additional); err == nil {
		t.Fatal("owner-union overflow unexpectedly succeeded")
	}
	if got := store.entries[proposal.ID]; !reflect.DeepEqual(got, before) {
		t.Fatalf("failed owner union mutated store:\nwant=%+v\ngot=%+v", before, got)
	}
}

func relationshipStoreFixture(t *testing.T) factmerge.RelationshipProposal {
	t.Helper()
	left := factmerge.Record{ID: "fact:a", Branch: "main", Status: factmerge.StatusActive, Kind: "invariant", Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/graph.go"}}
	right := factmerge.Record{ID: "fact:b", Branch: "main", Status: factmerge.StatusActive, Kind: "invariant", Paths: []string{"architecture.cache.policy"}, Locus: []string{"internal/cli/graph.go"}}
	proposal, ok, err := factmerge.BuildPossibleSameSubject(left, right, []factmerge.RelationshipOwner{{CandidateID: "candidate:b", SourceSessionID: "session:2"}, {CandidateID: "candidate:a", SourceSessionID: "session:1"}})
	if err != nil || !ok {
		t.Fatalf("fixture relationship = %+v, %v, %v", proposal, ok, err)
	}
	return proposal
}
