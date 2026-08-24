package cli

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

func TestBuildDistillRelationshipsForBranchV2IsNeutralAndOwned(t *testing.T) {
	now := time.Date(2026, time.August, 23, 18, 0, 0, 0, time.UTC)
	paths := []string{"architecture.data.flow"}
	candidateID := distillCandidateStableIDV1(distillCandidateIDPrefixV1, "relationship-owner")
	left := factRecord{
		ID: factRecordID("The graph writer validates GraphNodeID before storage.", paths), Paths: paths,
		Kind: factKindInvariant, Locus: []string{"graphnodeid"}, Text: "The graph writer validates GraphNodeID before storage.",
		Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "s1", DistillTurnID: distillCandidateApplicationAnchorIDV2(candidateID, true), Transcript: "sessions/main/s1.jsonl", Line: 1}},
		CreatedAt:  now, UpdatedAt: now,
	}
	right := factRecord{
		ID: factRecordID("GraphNodeID remains stable across graph compaction.", paths), Paths: paths,
		Kind: factKindInvariant, Locus: []string{"graphnodeid"}, Text: "GraphNodeID remains stable across graph compaction.",
		Branch: "main", Origin: factOriginAuthored, Status: factStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}
	original := []factRecord{left, right}
	facts := append([]factRecord(nil), original...)
	got, err := buildDistillRelationshipsForBranchV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	wantFactIDs := []string{left.ID, right.ID}
	sort.Strings(wantFactIDs)
	if len(got) != 1 || !reflect.DeepEqual(got[0].FactIDs, wantFactIDs) {
		t.Fatalf("relationships = %+v", got)
	}
	if got[0].Subject.StrongLocus != "graphnodeid" || len(got[0].Owners) != 1 || got[0].Owners[0].CandidateID != candidateID || got[0].Owners[0].SourceSessionID != "s1" {
		t.Fatalf("relationship evidence = %+v", got[0])
	}
	if !reflect.DeepEqual(facts, original) {
		t.Fatalf("relationship build mutated facts:\nwant=%+v\ngot=%+v", original, facts)
	}
	for _, fact := range facts {
		if len(fact.RelatedIDs) != 0 || fact.Status != factStatusActive {
			t.Fatalf("neutral relationship changed fact state: %+v", fact)
		}
	}
}

func TestBuildDistillRelationshipsForBranchV2RejectsBadReconcilePair(t *testing.T) {
	paths := []string{"constraints.invariants.general"}
	candidateID := distillCandidateStableIDV1(distillCandidateIDPrefixV1, "bad-reconcile-owner")
	facts := []factRecord{
		{
			ID: factRecordID("The graph snapshot runs with `--no-network`.", paths), Paths: paths,
			Kind: factKindInvariant, Locus: []string{"--no-network"}, Text: "The graph snapshot runs with `--no-network`.",
			Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
			Provenance: []factAnchor{{SessionID: "s1", DistillTurnID: distillCandidateApplicationAnchorIDV2(candidateID, true)}},
		},
		{
			ID: factRecordID("RepositoryKey and symbol IDs form the repository identity.", paths), Paths: paths,
			Kind: factKindInvariant, Locus: []string{"repositorykey"}, Text: "RepositoryKey and symbol IDs form the repository identity.",
			Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		},
	}
	got, err := buildDistillRelationshipsForBranchV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unrelated same-taxonomy facts produced relationships: %+v", got)
	}
}

func TestReplaceDistillRelationshipBranchV2PreservesOtherBranches(t *testing.T) {
	store := newDistillRelationshipStoreV2()
	main := relationshipStoreFixture(t)
	feature := main
	feature.Branch = "feature"
	feature.ID, _ = factmerge.RelationshipProposalID(feature.Branch, feature.FactIDs, feature.Subject)
	if err := store.Put(main); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(feature); err != nil {
		t.Fatal(err)
	}
	if err := replaceDistillRelationshipBranchV2(&store, "main", nil); err != nil {
		t.Fatal(err)
	}
	if len(store.entries) != 1 || len(distillRelationshipsForBranchV2(store, "feature")) != 1 {
		t.Fatalf("branch replacement changed unrelated state: %+v", store.entries)
	}
}

func TestBuildDistillRelationshipsForBranchV2SkipsDenseBlockWithoutFailure(t *testing.T) {
	facts := relationshipBlockFactsV2("DenseSubject", 92)
	got, stats, err := buildDistillRelationshipsForBranchWithStatsV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("dense block produced partial relationships: %d", len(got))
	}
	if stats.BlocksConsidered != 1 || stats.BlocksBuilt != 0 || stats.SkippedBlocks != 1 || stats.SkippedDenseBlocks != 1 || stats.SkippedCapacityBlocks != 0 || stats.SkippedByteBlocks != 0 {
		t.Fatalf("dense stats = %+v", stats)
	}
}

func TestBuildDistillRelationshipsForBranchV2AdmitsLargestBoundedBlock(t *testing.T) {
	got, stats, err := buildDistillRelationshipsForBranchWithStatsV2(relationshipBlockFactsV2("BoundedSubject", 91))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 91*90/2 {
		t.Fatalf("relationships = %d, want %d", len(got), 91*90/2)
	}
	if stats.BlocksBuilt != 1 || stats.SkippedBlocks != 0 {
		t.Fatalf("bounded block stats = %+v", stats)
	}
}

func TestDistillRelationshipPairCountExceedsV2Boundary(t *testing.T) {
	if distillRelationshipPairCountExceedsV2(91, distillRelationshipStoreV2MaxEntries) {
		t.Fatal("91-member block (4095 pairs) should fit the 4096-pair bound")
	}
	if !distillRelationshipPairCountExceedsV2(92, distillRelationshipStoreV2MaxEntries) {
		t.Fatal("92-member block (4186 pairs) should exceed the 4096-pair bound")
	}
}

func TestBuildDistillRelationshipsForBranchV2GlobalBudgetIsDeterministic(t *testing.T) {
	facts := append(relationshipBlockFactsV2("AlphaSubject", 65), relationshipBlockFactsV2("BetaSubject", 65)...)
	got, stats, err := buildDistillRelationshipsForBranchWithStatsV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 65*64/2 {
		t.Fatalf("relationships = %d, want %d", len(got), 65*64/2)
	}
	if stats.BlocksConsidered != 2 || stats.BlocksBuilt != 1 || stats.SkippedBlocks != 1 || stats.SkippedCapacityBlocks != 1 {
		t.Fatalf("global capacity stats = %+v", stats)
	}
	reversed := append([]factRecord(nil), facts...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	again, againStats, err := buildDistillRelationshipsForBranchWithStatsV2(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, got) || againStats != stats {
		t.Fatalf("bounded discovery is not deterministic:\nwant=%+v %+v\ngot=%+v %+v", got, stats, again, againStats)
	}
}

func TestBuildDistillRelationshipsForBranchV2ByteBudgetSkipsWholeBlock(t *testing.T) {
	facts := relationshipBlockFactsV2("ByteSubject", 3)
	full, _, err := buildDistillRelationshipsForBranchWithStatsV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 3 {
		t.Fatalf("unbounded relationships = %d, want 3", len(full))
	}
	oneLine, err := distillRelationshipProposalLineBytesV2(full[0])
	if err != nil {
		t.Fatal(err)
	}
	got, stats, err := buildDistillRelationshipsForBranchWithBudgetV2(facts, distillRelationshipBuildBudgetV2{
		RemainingEntries: distillRelationshipStoreV2MaxEntries,
		RemainingBytes:   oneLine,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || stats.SkippedBlocks != 1 || stats.SkippedByteBlocks != 1 || stats.BlocksBuilt != 0 {
		t.Fatalf("byte-budget output=%d stats=%+v", len(got), stats)
	}
}

func TestBuildDistillRelationshipsForBranchV2OwnerBudgetSkipsWholeBlock(t *testing.T) {
	facts := relationshipBlockFactsV2("OwnerSubject", 2)
	anchors := make([]factAnchor, 0, factmerge.RelationshipMaxOwners+1)
	for index := 0; index <= factmerge.RelationshipMaxOwners; index++ {
		candidateID := distillCandidateStableIDV1(distillCandidateIDPrefixV1, fmt.Sprintf("owner-budget-%03d", index))
		anchors = append(anchors, factAnchor{
			SessionID:     fmt.Sprintf("owner-session-%03d", index),
			DistillTurnID: distillCandidateApplicationAnchorIDV2(candidateID, true),
		})
	}
	facts[0].Provenance = anchors
	facts[1].Provenance = anchors
	got, stats, err := buildDistillRelationshipsForBranchWithStatsV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || stats.SkippedBlocks != 1 || stats.SkippedOwnerBlocks != 1 || stats.BlocksBuilt != 0 {
		t.Fatalf("owner-budget output=%d stats=%+v", len(got), stats)
	}
}

func TestBuildDistillRelationshipsForBranchV2WrapperParity(t *testing.T) {
	facts := relationshipBlockFactsV2("ParitySubject", 3)
	want, stats, err := buildDistillRelationshipsForBranchWithStatsV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := buildDistillRelationshipsForBranchV2(facts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || stats.SkippedBlocks != 0 || stats.BlocksBuilt != 1 {
		t.Fatalf("wrapper output=%+v stats=%+v want=%+v", got, stats, want)
	}
}

func TestReplaceDistillRelationshipBranchV2IsTransactional(t *testing.T) {
	store := newDistillRelationshipStoreV2()
	main := relationshipStoreFixture(t)
	feature := main
	feature.Branch = "feature"
	feature.ID, _ = factmerge.RelationshipProposalID(feature.Branch, feature.FactIDs, feature.Subject)
	if err := store.Put(main); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(feature); err != nil {
		t.Fatal(err)
	}
	before := distillRelationshipStoreV2{entries: make(map[string]factmerge.RelationshipProposal, len(store.entries))}
	for id, proposal := range store.entries {
		before.entries[id] = cloneDistillRelationshipProposalV2(proposal)
	}
	if err := replaceDistillRelationshipBranchV2(&store, "main", []factmerge.RelationshipProposal{feature}); err == nil {
		t.Fatal("replacement with another branch proposal unexpectedly succeeded")
	}
	if !reflect.DeepEqual(store, before) {
		t.Fatalf("failed replacement mutated store:\nwant=%+v\ngot=%+v", before, store)
	}
}

func relationshipBlockFactsV2(locus string, count int) []factRecord {
	paths := []string{"architecture.data.flow"}
	candidateID := distillCandidateStableIDV1(distillCandidateIDPrefixV1, "relationship-block-"+locus)
	facts := make([]factRecord, 0, count)
	for index := 0; index < count; index++ {
		text := "Relationship block " + locus + " fact " + string(rune('A'+index%26)) + " uses " + locus + "."
		// The record ID must remain unique after the short alphabetic suffix wraps.
		text += " ordinal " + fmt.Sprintf("%03d", index)
		facts = append(facts, factRecord{
			ID:         factRecordID(text, paths),
			Paths:      paths,
			Kind:       factKindInvariant,
			Locus:      []string{locus},
			Text:       text,
			Branch:     "main",
			Origin:     factOriginDistilled,
			Status:     factStatusActive,
			Provenance: []factAnchor{{SessionID: "session-relationship-block", DistillTurnID: distillCandidateApplicationAnchorIDV2(candidateID, true)}},
		})
	}
	return facts
}
