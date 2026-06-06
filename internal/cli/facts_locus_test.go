package cli

import (
	"testing"
	"time"
)

func TestFactScope(t *testing.T) {
	cases := []struct {
		paths []string
		want  string
	}{
		{[]string{"preferences.coding.style"}, factScopeCrossCutting},
		{[]string{"workflow.testing.rules"}, factScopeCrossCutting},
		{[]string{"preferences.coding.style", "workflow.review.rules"}, factScopeCrossCutting},
		{[]string{"architecture.data.flow"}, factScopeLocal},
		{[]string{"constraints.invariants.general"}, factScopeLocal},
		{[]string{"project.tooling.stack"}, factScopeLocal},
		// Mixed: any local path makes it local (the technical substance wins).
		{[]string{"preferences.coding.style", "architecture.data.flow"}, factScopeLocal},
		{nil, factScopeLocal},
	}
	for _, c := range cases {
		if got := factScope(c.paths); got != c.want {
			t.Errorf("factScope(%v) = %q, want %q", c.paths, got, c.want)
		}
	}
}

func TestFactLocus(t *testing.T) {
	text := "Use `MirrorCommittedMetadataRef` from internal/cli/strategy when reading entire/checkpoints/v1; settings.Load honors WithWorktreeRoot."
	locus := factLocus(text)
	loc := map[string]bool{}
	for _, l := range locus {
		loc[l] = true
	}
	for _, want := range []string{"mirrorcommittedmetadataref", "internal/cli/strategy", "settings.load", "withworktreeroot", "entire/checkpoints/v1"} {
		if !loc[want] {
			t.Errorf("expected locus token %q in %v", want, locus)
		}
	}
	// Plain prose yields no identifiers.
	if got := factLocus("This sentence has only ordinary words in it."); len(got) != 0 {
		t.Errorf("prose should yield no locus, got %v", got)
	}
}

func TestLocusOverlap(t *testing.T) {
	q := factLocus("where is MirrorCommittedMetadataRef defined")
	if locusOverlap(q, "`MirrorCommittedMetadataRef` is best-effort") < 1 {
		t.Errorf("expected overlap on the shared symbol")
	}
	if locusOverlap(q, "an unrelated fact about caching") != 0 {
		t.Errorf("expected no overlap")
	}
	if locusOverlap(nil, "anything") != 0 {
		t.Errorf("empty query locus should overlap nothing")
	}
}

func TestFilterFactsByScope(t *testing.T) {
	facts := []factRecord{
		{ID: "a", Paths: []string{"preferences.coding.style"}},
		{ID: "b", Paths: []string{"architecture.data.flow"}},
		{ID: "c", Paths: []string{"workflow.testing.rules"}},
	}
	if got := filterFactsByScope(facts, factScopeLocal); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("local filter wrong: %+v", got)
	}
	if got := filterFactsByScope(facts, factScopeCrossCutting); len(got) != 2 {
		t.Fatalf("cross-cutting filter should keep 2, got %d", len(got))
	}
	if got := filterFactsByScope(facts, ""); len(got) != 3 {
		t.Fatalf("empty scope should keep all")
	}
}

func TestRankFactsLocusBoost(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		// Mentions the symbol by name but little prose overlap.
		{ID: "sym", Paths: []string{"architecture.data.flow"}, Text: "`MirrorCommittedMetadataRef` uses context.WithoutCancel for the fetch budget.", Status: factStatusActive, UpdatedAt: now},
		// Generic fact, no symbol.
		{ID: "generic", Paths: []string{"architecture.data.flow"}, Text: "The mirror operation should be best effort and not fail the primary.", Status: factStatusActive, UpdatedAt: now},
	}
	got := rankFacts(facts, "MirrorCommittedMetadataRef", 5, false)
	if len(got) == 0 || got[0].ID != "sym" {
		t.Fatalf("locus boost should rank the symbol-naming fact first, got %+v", got)
	}
}
