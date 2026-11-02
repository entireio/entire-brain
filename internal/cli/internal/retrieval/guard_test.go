package retrieval

import (
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

func TestAnnotateUnifiedFactResultsFlagsLocusDriftAndEnforcesLimit(t *testing.T) {
	repoDir := t.TempDir()
	facts := []factmerge.Record{
		{ID: "fact:departed", Text: "Use the new cache.", Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cache.go"}, Status: factmerge.StatusActive},
		{ID: "fact:current", Text: "Requests are idempotent.", Paths: []string{"constraints.invariants.general"}, Status: factmerge.StatusActive},
	}
	ranked := []Result{
		{Source: "fact", ID: "fact:departed", Text: facts[0].Text, Score: 0.9},
		{Source: "fact", ID: "fact:current", Text: facts[1].Text, Score: 0.8},
		{Source: "doc", ID: "doc:runbook", Text: "Runbook", Score: 0.7},
	}

	got := AnnotateUnifiedFactResults(repoDir, facts, ranked, 2)
	if len(got) != 2 {
		t.Fatalf("limit not enforced, got %d results: %+v", len(got), got)
	}
	if !got[0].VerificationRequired || !hasRetrievalCaveat(got[0], CaveatStaleLocus) {
		t.Fatalf("departed locus must be flagged for verification: %+v", got[0])
	}
	if got[1].ID != "fact:current" || got[1].VerificationRequired || len(got[1].Caveats) != 0 {
		t.Fatalf("fact without drift must pass through unannotated: %+v", got[1])
	}
}

func TestAnnotateUnifiedFactResultsPassesNonFactResultsThrough(t *testing.T) {
	ranked := []Result{
		{Source: "doc", ID: "doc:runbook", Text: "Runbook", Score: 0.9},
		{Source: "fact", ID: "fact:unknown", Text: "Not in the store.", Score: 0.8},
	}
	got := AnnotateUnifiedFactResults(t.TempDir(), nil, ranked, 10)
	if len(got) != 2 || got[0].ID != "doc:runbook" || got[1].ID != "fact:unknown" {
		t.Fatalf("results should pass through in rank order: %+v", got)
	}
	if got[1].VerificationRequired || len(got[1].Caveats) != 0 {
		t.Fatalf("a fact missing from the store must not be annotated: %+v", got[1])
	}
}

func TestEmptyAnnotatePreservesArrayShape(t *testing.T) {
	ranked := make([]Result, 0)
	got := AnnotateUnifiedFactResults("", nil, ranked, 10)
	if got == nil {
		t.Fatal("empty ranked results must remain a non-nil slice for JSON array output")
	}
}

func hasRetrievalCaveat(result Result, kind string) bool {
	for _, caveat := range result.Caveats {
		if caveat.Kind == kind {
			return true
		}
	}
	return false
}
