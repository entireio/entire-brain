package cli

import (
	"slices"
	"testing"
)

func TestRankFactsFusedPublicOraclePreservesTargetAndHardDistractor(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	got := rankFactsFused(facts, factsBM25PublicOracleQuery, 6, false, rr)
	const target = "fact:30c6c2e4a031d91df16e4d27"
	const hardDistractor = "fact:32a34f711bb107845dc407c3"
	targetRank := rankOfFactID(got, target)
	distractorRank := rankOfFactID(got, hardDistractor)
	if targetRank != 1 {
		t.Fatalf("public oracle target rank=%d, want 1", targetRank)
	}
	if distractorRank != 3 {
		t.Fatalf("public hard distractor rank=%d, want 3", distractorRank)
	}
	if !rr.lastRun.QueryVectorValid || rr.lastRun.ValidCandidateVectors != len(facts) {
		t.Fatalf("public oracle unexpectedly degraded semantic coverage: %+v", rr.lastRun)
	}
}

func TestRankFactsFusedPublicSyntheticNullFailsClosed(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	// This is a clearly synthetic corpus-closed null, not a relevance label.
	// Automatic semantic injection now fails closed when the nearest candidates
	// do not clear the corpus-relative calibration gate.
	got := rankFactsFused(facts, "xylophone zebra quokka", 6, false, rr)
	if len(got) != 0 {
		t.Fatalf("synthetic null returned semantic-only facts: %+v", got)
	}
}

func TestRankFactsFusedCalibrationSuppressesWeakSemanticTail(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	const query = "why does the summary endpoint provide aggregates ignored by the command line"
	const target = "fact:32203deb1036cba4d14b243c"

	fullDepth := rankFactsFusedWithSemanticDepthMultiplier(facts, query, 6, false, rr, 0)
	if rank := rankOfFactID(fullDepth, target); rank == 0 {
		t.Fatal("calibration removed a strong lexical target at full semantic depth")
	}
	got := rankFactsFused(facts, query, 6, false, rr)
	if rank := rankOfFactID(got, target); rank == 0 {
		t.Fatal("weak semantic tail displaced a strong lexical target from the top 6")
	}
}

func TestRankFactsFusedTwoTimesDepthPreservesCalibrationCases(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	for _, tc := range []struct {
		query  string
		target string
	}{
		{
			query:  "how do command line and terminal UI select multiple projects",
			target: "fact:2b0e4f409de31a6aa95ce0a0",
		},
		{
			query:  "why are token metering calculation failures swallowed",
			target: "fact:42e169e787462c51357851e2",
		},
	} {
		t.Run(tc.target, func(t *testing.T) {
			fullDepthRank := rankOfFactID(rankFactsFusedWithSemanticDepthMultiplier(facts, tc.query, 6, false, rr, 0), tc.target)
			if fullDepthRank == 0 {
				t.Fatal("calibration target absent from historical top 6")
			}
			oneTimesRank := rankOfFactID(rankFactsFusedWithSemanticDepthMultiplier(facts, tc.query, 6, false, rr, 1), tc.target)
			if oneTimesRank != 0 {
				t.Fatalf("1x semantic depth target rank=%d, want outside top 6", oneTimesRank)
			}
			twoTimesRank := rankOfFactID(rankFactsFused(facts, tc.query, 6, false, rr), tc.target)
			if twoTimesRank == 0 || twoTimesRank > fullDepthRank {
				t.Fatalf("2x semantic depth target rank=%d, want 1..%d", twoTimesRank, fullDepthRank)
			}
		})
	}
}

func TestRankFactsFusedRejectsUncalibratedSemanticOnlyFact(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	const query = "usage notifications are monotonically accumulated"
	const target = "fact:653adbd42aa9c222acb547d9"
	if rank := rankOfFactID(rankFacts(facts, query, len(facts), false), target); rank != 0 {
		t.Fatalf("semantic-only calibration target unexpectedly has lexical rank %d", rank)
	}
	if rank := rankOfFactID(rankFactsFused(facts, query, 6, false, rr), target); rank != 0 {
		t.Fatalf("uncalibrated semantic-only target reached automatic top 6 at rank %d", rank)
	}
}

func TestRankFactsFusedKeepsTemporalConstraintTerms(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
	facts := loadFactsBM25PublicSnapshot(t)
	rr := newSemanticReranker(defaultEmbedder())
	if rr == nil {
		t.Skip("bundled semantic backend unavailable")
	}
	const query = "validate the access credential recipient endpoint before transmitting the secret"
	const target = "fact:c9b44a5e5e1678e0a21270d3"

	// Unlike ordinary filler, "before" states an ordering constraint. Keeping it
	// in the lexical arm gives the semantically-near invariant a second,
	// independent retrieval signal instead of leaving it just outside the packet.
	if terms := historyQueryTerms(query); !slices.Contains(terms, "before") {
		t.Fatalf("temporal constraint term was discarded: %v", terms)
	}
	if rank := rankOfFactID(rankFacts(facts, query, len(facts), false), target); rank == 0 {
		t.Fatal("temporal constraint fact remained lexically unreachable")
	}
	if rank := rankOfFactID(rankFactsFused(facts, query, 6, false, rr), target); rank != 4 {
		t.Fatalf("temporal constraint fact rank=%d, want 4", rank)
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
