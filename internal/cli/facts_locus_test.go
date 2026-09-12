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

func TestInferFactKind(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		text  string
		want  string
	}{
		{"preference from taxonomy", []string{"preferences.coding.style"}, "uses tabs", factKindPreference},
		{"convention from workflow", []string{"workflow.testing.rules"}, "tests run on CI", factKindConvention},
		{"decision from architecture", []string{"architecture.boundaries.rationale"}, "the store is split in two", factKindDecision},
		{"decision from project", []string{"project.tooling.stack"}, "the project uses Go", factKindDecision},
		{"invariant from constraints", []string{"constraints.invariants.general"}, "the index is derived", factKindInvariant},
		{"gotcha cue overrides taxonomy", []string{"constraints.invariants.general"}, "easy to miss: the lock must be released", factKindGotcha},
		{"gotcha cue overrides preference", []string{"preferences.coding.style"}, "footgun: never embed the prod URL", factKindGotcha},
		{"closed-negative from rejection cue", []string{"architecture.boundaries.rationale"}, "a per-file truth layout was ruled out; NDJSON stays the system of record", factKindClosedNegative},
		{"closed-negative from measurement cue", []string{"workflow.testing.rules"}, "RRF fusion-weight tuning measured as noise across all swept cells", factKindClosedNegative},
		{"closed-negative from dead-end cue", nil, "the scoped-floor recall variant was a dead end; backfill facts do not help", factKindClosedNegative},
		// A dead end phrased as a trap is still a dead end: the closed-negative
		// cue must win over a co-present gotcha cue.
		{"closed-negative beats gotcha cue", nil, "beware: query expansion was tried and failed at larger n", factKindClosedNegative},
		{"invariant cue with no taxonomy", nil, "the cursor must always be monotonic", factKindInvariant},
		{"default decision with no signal", nil, "the team shipped the feature", factKindDecision},
		// Multi-path tie-break: the invariant signal of constraints.* must win over
		// an alphabetically-earlier architecture co-tag (not be dropped), in both
		// path orderings.
		{"co-tag picks higher-priority kind", []string{"architecture.boundaries.rationale", "constraints.invariants.general"}, "the boundary holds", factKindInvariant},
		{"co-tag order-independent", []string{"constraints.invariants.general", "architecture.boundaries.rationale"}, "the boundary holds", factKindInvariant},
	}
	for _, c := range cases {
		if got := inferFactKind(c.paths, c.text); got != c.want {
			t.Errorf("%s: inferFactKind(%v, %q) = %q, want %q", c.name, c.paths, c.text, got, c.want)
		}
	}
}

func TestFactKindOrInferredPrefersStored(t *testing.T) {
	// A valid stored kind wins over inference even when they disagree.
	f := factRecord{Kind: factKindGotcha, Paths: []string{"preferences.coding.style"}, Text: "uses tabs"}
	if got := factKindOrInferred(f); got != factKindGotcha {
		t.Fatalf("stored kind should win, got %q", got)
	}
	// An invalid stored kind falls back to inference.
	f.Kind = "bogus"
	if got := factKindOrInferred(f); got != factKindPreference {
		t.Fatalf("invalid stored kind should fall back to inference, got %q", got)
	}
}

func TestFilterFactsByKind(t *testing.T) {
	facts := []factRecord{
		{ID: "a", Kind: factKindInvariant, Paths: []string{"constraints.invariants.general"}, Text: "x"},
		{ID: "b", Kind: factKindPreference, Paths: []string{"preferences.coding.style"}, Text: "y"},
		{ID: "c", Paths: []string{"workflow.testing.rules"}, Text: "z"}, // unset → inferred convention
	}
	if got := filterFactsByKind(facts, factKindInvariant); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("invariant filter wrong: %+v", got)
	}
	if got := filterFactsByKind(facts, factKindConvention); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("convention filter should match the inferred fact: %+v", got)
	}
	if got := filterFactsByKind(facts, ""); len(got) != 3 {
		t.Fatalf("empty kind should keep all")
	}
}

func TestReclassifyFacts(t *testing.T) {
	facts := []factRecord{
		{ID: "missing", Paths: []string{"constraints.invariants.general"}, Text: "x"},               // no kind → fill
		{ID: "valid", Kind: factKindGotcha, Paths: []string{"preferences.coding.style"}, Text: "y"}, // keep
		{ID: "bogus", Kind: "nonsense", Paths: []string{"preferences.coding.style"}, Text: "z"},     // invalid → fill
	}
	changed := reclassifyFacts(facts, false)
	if changed != 2 {
		t.Fatalf("expected 2 changed (missing+bogus), got %d", changed)
	}
	if facts[0].Kind != factKindInvariant || facts[2].Kind != factKindPreference {
		t.Fatalf("backfill wrong: %+v", facts)
	}
	if facts[1].Kind != factKindGotcha {
		t.Fatalf("valid kind must be preserved without --force, got %q", facts[1].Kind)
	}
	// Force recomputes the agent-labeled one to the inferred value.
	if changed := reclassifyFacts(facts, true); changed != 1 {
		t.Fatalf("force should recompute the one disagreeing fact, got %d", changed)
	}
	if facts[1].Kind != factKindPreference {
		t.Fatalf("force should overwrite to inferred preference, got %q", facts[1].Kind)
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
	// internal/factmerge. This id must equal the merge-core id byte for byte —
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

func TestFilterFactsByLocus(t *testing.T) {
	facts := []factRecord{
		{ID: "sym", Text: "`ValidateToken` returns an error.", Status: factStatusActive},
		{ID: "path", Text: "internal/auth/token.go holds the validator.", Status: factStatusActive},
		{ID: "other", Text: "Caching is best-effort.", Status: factStatusActive},
	}
	if got := filterFactsByLocus(facts, "ValidateToken"); len(got) != 1 || got[0].ID != "sym" {
		t.Fatalf("symbol locus filter wrong: %+v", got)
	}
	if got := filterFactsByLocus(facts, "internal/auth/token.go"); len(got) != 1 || got[0].ID != "path" {
		t.Fatalf("path locus filter wrong: %+v", got)
	}
	if got := filterFactsByLocus(facts, ""); len(got) != 3 {
		t.Fatalf("empty locus should keep all")
	}
}

func TestReclassifyFactsFillsLocus(t *testing.T) {
	facts := []factRecord{
		{ID: "a", Paths: []string{"architecture.data.flow"}, Text: "`MirrorCommittedMetadataRef` is best-effort."},
	}
	if changed := reclassifyFacts(facts, false); changed != 1 {
		t.Fatalf("expected the fact to be backfilled, changed=%d", changed)
	}
	if len(facts[0].Locus) == 0 || facts[0].Locus[0] != "mirrorcommittedmetadataref" {
		t.Fatalf("locus not backfilled: %v", facts[0].Locus)
	}
	// Idempotent: a second pass changes nothing (kind + locus already set).
	if changed := reclassifyFacts(facts, false); changed != 0 {
		t.Fatalf("reclassify should be idempotent, changed=%d", changed)
	}
}
