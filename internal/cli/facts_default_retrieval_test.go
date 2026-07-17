package cli

import "testing"

func TestRankFactsFusedPublicOraclePreservesTargetAndHardDistractor(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	got := rankFactsFused(facts, factsBM25PublicOracleQuery, len(facts), false, rr)
	const target = "fact:30c6c2e4a031d91df16e4d27"
	const hardDistractor = "fact:32a34f711bb107845dc407c3"
	targetRank := rankOfFactID(got, target)
	distractorRank := rankOfFactID(got, hardDistractor)
	if targetRank != 1 {
		t.Fatalf("public oracle target rank=%d, want 1", targetRank)
	}
	if distractorRank == 0 || distractorRank <= targetRank {
		t.Fatalf("public hard distractor rank=%d, target rank=%d", distractorRank, targetRank)
	}
	if !rr.lastRun.QueryVectorValid || rr.lastRun.ValidCandidateVectors != len(facts) {
		t.Fatalf("public oracle unexpectedly degraded semantic coverage: %+v", rr.lastRun)
	}
}

func TestRankFactsFusedPublicSyntheticNullGolden(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	// This is a clearly synthetic corpus-closed null, not a relevance label.
	// The semantic arm currently returns its nearest six rather than an empty
	// list; freeze that behavior here so failure-vector hardening cannot silently
	// change null-query packet content.
	got := rankFactsFused(facts, "xylophone zebra quokka", 6, false, rr)
	want := []string{
		"fact:e3e7d5ba3d7767d06ea91f5e",
		"fact:8c7cbd18b936edc149a64ec1",
		"fact:2b0e4f409de31a6aa95ce0a0",
		"fact:63485ff4d3e7da4d742dacda",
		"fact:0d1104c5493d9d6febcb7600",
		"fact:d6d013154da56499fbf92453",
	}
	if len(got) != len(want) {
		t.Fatalf("null result count=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("null rank %d=%s, want %s", i+1, got[i].ID, want[i])
		}
	}
}

func rankOfFactID(facts []factRecord, id string) int {
	for i, fact := range facts {
		if fact.ID == id {
			return i + 1
		}
	}
	return 0
}
