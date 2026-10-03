package factmerge

import (
	"fmt"
	"testing"
	"time"
)

// Regression cover for issue #324: `facts promote --keep-both` queued a
// Proposal{Confidence: 0} literal — a number the engine never computed, sent to
// a human review queue — and emitted one proposal per conflicting PAIR, so n
// facts on one taxonomy path produced O(n^2) proposals.

func sameFactPathSet(t *testing.T, n int, now time.Time) []Record {
	t.Helper()
	facts := make([]Record, 0, n)
	for i := range n {
		// Distinct statements, one shared taxonomy path: the exact shape that
		// makes activeConflictIndexes return a growing clique.
		facts = append(facts, factFor(t,
			fmt.Sprintf("the service listens on port %d", 8000+i),
			[]string{"architecture.api.ports"}, now))
	}
	return facts
}

func TestPromoteKeepBothNeverQueuesAnUncomputedConfidence(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	source := sameFactPathSet(t, 4, now)

	_, proposals, _ := Promote(source, nil, "keep-both", "main", now)
	if len(proposals) == 0 {
		t.Fatal("conflicting same-path facts queued no proposal at all; the conflict must still reach review")
	}
	for _, p := range proposals {
		if p.Confidence <= 0 {
			t.Errorf("proposal %s -> %s carries confidence %v: a human review queue must never receive a confidence the engine did not compute",
				p.CandidateID, p.TargetID, p.Confidence)
		}
		if p.Confidence >= DefaultConfidenceThreshold {
			t.Errorf("proposal %s -> %s carries confidence %v, at or above the auto-apply threshold %v: a lexical score is not evidence the supersede is correct",
				p.CandidateID, p.TargetID, p.Confidence, DefaultConfidenceThreshold)
		}
	}
}

func TestPromoteKeepBothProposalsDoNotGrowQuadratically(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// n facts on one path form a clique of n(n-1)/2 pairs. Keeping every fact in
	// one reviewable component needs only the n-1 spanning edges.
	for _, tc := range []struct{ facts, want, quadratic int }{
		{facts: 2, want: 1, quadratic: 1},
		{facts: 4, want: 3, quadratic: 6},
		{facts: 8, want: 7, quadratic: 28},
	} {
		t.Run(fmt.Sprintf("%d-same-path-facts", tc.facts), func(t *testing.T) {
			source := sameFactPathSet(t, tc.facts, now)
			merged, proposals, promoted := Promote(source, nil, "keep-both", "main", now)
			if promoted != tc.facts {
				t.Fatalf("promoted %d facts, want %d (keep-both drops nothing)", promoted, tc.facts)
			}
			if len(proposals) != tc.want {
				t.Fatalf("%d same-path facts queued %d proposals, want %d (one per PAIR would be %d)",
					tc.facts, len(proposals), tc.want, tc.quadratic)
			}
			// The saving must not come from dropping facts out of review: every
			// conflicting fact stays an endpoint of some queued proposal, so the
			// review surface still groups the whole clique together.
			endpoints := map[string]struct{}{}
			for _, p := range proposals {
				endpoints[p.CandidateID] = struct{}{}
				endpoints[p.TargetID] = struct{}{}
			}
			for _, f := range merged {
				if _, ok := endpoints[f.ID]; !ok {
					t.Errorf("fact %q (%q) conflicts but is in no queued proposal: it would never reach review", f.ID, f.Text)
				}
			}
		})
	}
}

// A pre-existing target clique is what separates a correct spanning fix from
// the cheap one ("queue a single proposal per candidate"). The target's facts
// conflict with each other but Promote never proposed between them, so the
// candidate's edges are the only thing that can pull them into one review
// group; dropping them is a silent loss of review coverage, not a saving.
func TestPromoteKeepBothKeepsAPreExistingTargetCliqueReviewable(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	all := sameFactPathSet(t, 4, now)
	target, source := all[:3], all[3:]

	merged, proposals, _ := Promote(source, target, "keep-both", "main", now)
	if len(proposals) != 3 {
		t.Fatalf("one candidate against a 3-fact target clique queued %d proposals, want 3 (the edges that join the components)", len(proposals))
	}
	endpoints := map[string]struct{}{}
	for _, p := range proposals {
		endpoints[p.CandidateID] = struct{}{}
		endpoints[p.TargetID] = struct{}{}
	}
	for _, f := range merged {
		if _, ok := endpoints[f.ID]; !ok {
			t.Errorf("fact %q (%q) conflicts but is in no queued proposal: it would never reach review", f.ID, f.Text)
		}
	}
}

func TestConflictConfidenceTracksTheActualStatements(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	path := []string{"architecture.api.ports"}
	base := factFor(t, "the service listens on port 8080", path, now)
	near := factFor(t, "the service listens on port 9090", path, now)
	far := factFor(t, "deployments are gated on a manual approval from the release owner", path, now)

	nearScore := ConflictConfidence(base, near)
	farScore := ConflictConfidence(base, far)
	if !(nearScore > farScore) {
		t.Fatalf("near-duplicate scored %v, unrelated statement scored %v: the confidence is not derived from the facts", nearScore, farScore)
	}
	if got := ConflictConfidence(base, near); got != nearScore {
		t.Fatalf("ConflictConfidence is not deterministic: %v then %v", nearScore, got)
	}
	if ConflictConfidence(base, base) > PromoteConfidenceCeiling {
		t.Fatalf("identical records scored above the ceiling %v", PromoteConfidenceCeiling)
	}
}
