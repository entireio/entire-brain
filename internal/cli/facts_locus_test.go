package cli

import (
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

func TestFactLocus(t *testing.T) {
	text := "Use `MirrorCommittedMetadataRef` from internal/cli/strategy when reading entire/checkpoints/v1; settings.Load honors WithWorktreeRoot."
	locus := base.FactLocus(text)
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
	if got := base.FactLocus("This sentence has only ordinary words in it."); len(got) != 0 {
		t.Errorf("prose should yield no locus, got %v", got)
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

func TestFactRecordIDIgnoresKind(t *testing.T) {
	// Kind is additive metadata, not identity: the same text+paths must hash to
	// the same id regardless of kind, so backfill never forks a fact.
	paths := normalizeFactPaths([]string{"constraints.invariants.general"})
	id := factRecordID("the index is derived from the ndjson truth", paths)
	// A literal, not a second call to factRecordID: comparing the function to
	// itself holds for every implementation, including one that has silently
	// re-keyed every fact already on disk. See the note on recordIDNDJSONTruth in
	// factmerge. This id must equal the merge-core id byte for byte —
	// the alias exists so the CLI and the fact-set head agree on identity.
	if id != "fact:6bc25f8fd4b22fcbae32d329" {
		t.Fatalf("factRecordID = %s, want fact:6bc25f8fd4b22fcbae32d329", id)
	}
}

func TestFactsRelevantToChange(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		// Names a symbol defined in a changed file → relevant.
		{ID: "a", Text: "`MirrorCommittedMetadataRef` must stay best-effort.", Status: factStatusActive, UpdatedAt: now},
		// Names the changed file path → relevant.
		{ID: "b", Text: "internal/cli/strategy.go owns the mirror budget.", Status: factStatusActive, UpdatedAt: now},
		// Unrelated → not relevant.
		{ID: "c", Text: "The CLI prefers table-driven tests.", Status: factStatusActive, UpdatedAt: now},
		// Relevant but superseded → excluded.
		{ID: "d", Text: "`MirrorCommittedMetadataRef` was renamed.", Status: factStatusSuperseded, UpdatedAt: now},
	}
	files := []string{"internal/cli/strategy.go"}
	symbols := []semanticRecord{{Name: "MirrorCommittedMetadataRef", QualifiedName: "strategy.MirrorCommittedMetadataRef", FilePath: "internal/cli/strategy.go"}}
	got := factsRelevantToChange(files, symbols, facts, 10)
	ids := map[string]bool{}
	for _, f := range got {
		ids[f.ID] = true
	}
	if !ids["a"] || !ids["b"] {
		t.Fatalf("expected facts a and b relevant, got %v", ids)
	}
	if ids["c"] {
		t.Fatalf("unrelated fact c should not be relevant")
	}
	if ids["d"] {
		t.Fatalf("superseded fact d must be excluded")
	}
}

// TestFactLocusKeepsHyphenatedPaths pins the bug ashtom reported: the
// extractor used to cut a hyphenated path into fragments, so a fact about
// `website/scripts/pre-build.ts` was anchored to "website/scripts/pre" and
// "build.ts" — two tokens that name nothing in the repo — while the real path
// never appeared at all. Quoting the path in backticks did not help: the
// backtick span added the correct token, but the regex sweep still produced
// the fragments beside it, which is exactly what he saw.
func TestFactLocusKeepsHyphenatedPaths(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"bare", "the website/scripts/pre-build.ts hook runs first", "website/scripts/pre-build.ts"},
		{"backticked", "see `website/scripts/pre-build.ts` for the hook", "website/scripts/pre-build.ts"},
		{"no directory", "pre-build.ts runs before the bundle step", "pre-build.ts"},
		{"hyphenated directory", "entire-brain/internal/cli/facts_locus.go owns this", "entire-brain/internal/cli/facts_locus.go"},
		{"leading dot", "edit .github/workflows/ci-windows.yml to add the shard", ".github/workflows/ci-windows.yml"},
		{"dotted repo config", "the flag lives in .entire/settings.json", ".entire/settings.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := base.FactLocus(c.text)
			loc := map[string]bool{}
			for _, l := range got {
				loc[l] = true
			}
			if !loc[c.want] {
				t.Errorf("expected locus token %q in %v", c.want, got)
			}
			// The fragments are the defect, not merely a missing token: a
			// fragment is a plausible-looking path that resolves to nothing,
			// which is worse than no anchor at all.
			for _, frag := range []string{"website/scripts/pre", "build.ts", "brain/internal/cli/facts_locus.go", "github/workflows/ci", "windows.yml", "entire/settings.json"} {
				if frag == c.want {
					continue
				}
				if loc[frag] {
					t.Errorf("fragment %q leaked into locus %v", frag, got)
				}
			}
		})
	}
}

// TestFactLocusHyphenWideningStaysOffProse guards the other side of the fix:
// admitting "-" must not turn ordinary hyphenated English into code anchors.
// A segment separator (. or /) is still required, so prose stays empty.
func TestFactLocusHyphenWideningStaysOffProse(t *testing.T) {
	for _, text := range []string{
		"a state-of-the-art approach with no code named in it",
		"we did a before-and-after comparison last week",
		"This sentence has only ordinary words in it.",
	} {
		if got := base.FactLocus(text); len(got) != 0 {
			t.Errorf("prose %q should yield no locus, got %v", text, got)
		}
	}
}
