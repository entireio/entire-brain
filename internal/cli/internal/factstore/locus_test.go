package factstore_test

import (
	"testing"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestFactScope(t *testing.T) {
	cases := []struct {
		paths []string
		want  string
	}{
		{[]string{"preferences.coding.style"}, factstore.ScopeCrossCutting},
		{[]string{"workflow.testing.rules"}, factstore.ScopeCrossCutting},
		{[]string{"preferences.coding.style", "workflow.review.rules"}, factstore.ScopeCrossCutting},
		{[]string{"architecture.data.flow"}, factstore.ScopeLocal},
		{[]string{"constraints.invariants.general"}, factstore.ScopeLocal},
		{[]string{"project.tooling.stack"}, factstore.ScopeLocal},
		// Mixed: any local path makes it local (the technical substance wins).
		{[]string{"preferences.coding.style", "architecture.data.flow"}, factstore.ScopeLocal},
		{nil, factstore.ScopeLocal},
	}
	for _, c := range cases {
		if got := factstore.ScopeOf(c.paths); got != c.want {
			t.Errorf("factstore.ScopeOf(%v) = %q, want %q", c.paths, got, c.want)
		}
	}
}

func TestLocusOverlap(t *testing.T) {
	q := base.FactLocus("where is MirrorCommittedMetadataRef defined")
	if factstore.LocusOverlap(q, "`MirrorCommittedMetadataRef` is best-effort") < 1 {
		t.Errorf("expected overlap on the shared symbol")
	}
	if factstore.LocusOverlap(q, "an unrelated fact about caching") != 0 {
		t.Errorf("expected no overlap")
	}
	if factstore.LocusOverlap(nil, "anything") != 0 {
		t.Errorf("empty query locus should overlap nothing")
	}
}

func TestFilterFactsByScope(t *testing.T) {
	facts := []factmerge.Record{
		{ID: "a", Paths: []string{"preferences.coding.style"}},
		{ID: "b", Paths: []string{"architecture.data.flow"}},
		{ID: "c", Paths: []string{"workflow.testing.rules"}},
	}
	if got := factstore.FilterByScope(facts, factstore.ScopeLocal); len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("local filter wrong: %+v", got)
	}
	if got := factstore.FilterByScope(facts, factstore.ScopeCrossCutting); len(got) != 2 {
		t.Fatalf("cross-cutting filter should keep 2, got %d", len(got))
	}
	if got := factstore.FilterByScope(facts, ""); len(got) != 3 {
		t.Fatalf("empty scope should keep all")
	}
}

func TestInferFactKind(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		text  string
		want  string
	}{
		{"preference from taxonomy", []string{"preferences.coding.style"}, "uses tabs", factmerge.KindPreference},
		{"convention from workflow", []string{"workflow.testing.rules"}, "tests run on CI", factmerge.KindConvention},
		{"decision from architecture", []string{"architecture.boundaries.rationale"}, "the store is split in two", factmerge.KindDecision},
		{"decision from project", []string{"project.tooling.stack"}, "the project uses Go", factmerge.KindDecision},
		{"invariant from constraints", []string{"constraints.invariants.general"}, "the index is derived", factmerge.KindInvariant},
		{"gotcha cue overrides taxonomy", []string{"constraints.invariants.general"}, "easy to miss: the lock must be released", factmerge.KindGotcha},
		{"gotcha cue overrides preference", []string{"preferences.coding.style"}, "footgun: never embed the prod URL", factmerge.KindGotcha},
		{"closed-negative from rejection cue", []string{"architecture.boundaries.rationale"}, "a per-file truth layout was ruled out; NDJSON stays the system of record", factmerge.KindClosedNegative},
		{"closed-negative from measurement cue", []string{"workflow.testing.rules"}, "RRF fusion-weight tuning measured as noise across all swept cells", factmerge.KindClosedNegative},
		{"closed-negative from dead-end cue", nil, "the scoped-floor recall variant was a dead end; backfill facts do not help", factmerge.KindClosedNegative},
		// A dead end phrased as a trap is still a dead end: the closed-negative
		// cue must win over a co-present gotcha cue.
		{"closed-negative beats gotcha cue", nil, "beware: query expansion was tried and failed at larger n", factmerge.KindClosedNegative},
		{"invariant cue with no taxonomy", nil, "the cursor must always be monotonic", factmerge.KindInvariant},
		{"default decision with no signal", nil, "the team shipped the feature", factmerge.KindDecision},
		// Multi-path tie-break: the invariant signal of constraints.* must win over
		// an alphabetically-earlier architecture co-tag (not be dropped), in both
		// path orderings.
		{"co-tag picks higher-priority kind", []string{"architecture.boundaries.rationale", "constraints.invariants.general"}, "the boundary holds", factmerge.KindInvariant},
		{"co-tag order-independent", []string{"constraints.invariants.general", "architecture.boundaries.rationale"}, "the boundary holds", factmerge.KindInvariant},
	}
	for _, c := range cases {
		if got := factstore.InferKind(c.paths, c.text); got != c.want {
			t.Errorf("%s: factstore.InferKind(%v, %q) = %q, want %q", c.name, c.paths, c.text, got, c.want)
		}
	}
}

func TestFactKindOrInferredPrefersStored(t *testing.T) {
	// A valid stored kind wins over inference even when they disagree.
	f := factmerge.Record{Kind: factmerge.KindGotcha, Paths: []string{"preferences.coding.style"}, Text: "uses tabs"}
	if got := factstore.KindOrInferred(f); got != factmerge.KindGotcha {
		t.Fatalf("stored kind should win, got %q", got)
	}
	// An invalid stored kind falls back to inference.
	f.Kind = "bogus"
	if got := factstore.KindOrInferred(f); got != factmerge.KindPreference {
		t.Fatalf("invalid stored kind should fall back to inference, got %q", got)
	}
}

func TestFilterFactsByKind(t *testing.T) {
	facts := []factmerge.Record{
		{ID: "a", Kind: factmerge.KindInvariant, Paths: []string{"constraints.invariants.general"}, Text: "x"},
		{ID: "b", Kind: factmerge.KindPreference, Paths: []string{"preferences.coding.style"}, Text: "y"},
		{ID: "c", Paths: []string{"workflow.testing.rules"}, Text: "z"}, // unset → inferred convention
	}
	if got := factstore.FilterByKind(facts, factmerge.KindInvariant); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("invariant filter wrong: %+v", got)
	}
	if got := factstore.FilterByKind(facts, factmerge.KindConvention); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("convention filter should match the inferred fact: %+v", got)
	}
	if got := factstore.FilterByKind(facts, ""); len(got) != 3 {
		t.Fatalf("empty kind should keep all")
	}
}

func TestReclassifyFacts(t *testing.T) {
	facts := []factmerge.Record{
		{ID: "missing", Paths: []string{"constraints.invariants.general"}, Text: "x"},                     // no kind → fill
		{ID: "valid", Kind: factmerge.KindGotcha, Paths: []string{"preferences.coding.style"}, Text: "y"}, // keep
		{ID: "bogus", Kind: "nonsense", Paths: []string{"preferences.coding.style"}, Text: "z"},           // invalid → fill
	}
	changed := factstore.Reclassify(facts, false)
	if changed != 2 {
		t.Fatalf("expected 2 changed (missing+bogus), got %d", changed)
	}
	if facts[0].Kind != factmerge.KindInvariant || facts[2].Kind != factmerge.KindPreference {
		t.Fatalf("backfill wrong: %+v", facts)
	}
	if facts[1].Kind != factmerge.KindGotcha {
		t.Fatalf("valid kind must be preserved without --force, got %q", facts[1].Kind)
	}
	// Force recomputes the agent-labeled one to the inferred value.
	if changed := factstore.Reclassify(facts, true); changed != 1 {
		t.Fatalf("force should recompute the one disagreeing fact, got %d", changed)
	}
	if facts[1].Kind != factmerge.KindPreference {
		t.Fatalf("force should overwrite to inferred preference, got %q", facts[1].Kind)
	}
}

func TestFilterFactsByLocus(t *testing.T) {
	facts := []factmerge.Record{
		{ID: "sym", Text: "`ValidateToken` returns an error.", Status: factmerge.StatusActive},
		{ID: "path", Text: "internal/auth/token.go holds the validator.", Status: factmerge.StatusActive},
		{ID: "other", Text: "Caching is best-effort.", Status: factmerge.StatusActive},
	}
	if got := factstore.FilterByLocus(facts, "ValidateToken"); len(got) != 1 || got[0].ID != "sym" {
		t.Fatalf("symbol locus filter wrong: %+v", got)
	}
	if got := factstore.FilterByLocus(facts, "internal/auth/token.go"); len(got) != 1 || got[0].ID != "path" {
		t.Fatalf("path locus filter wrong: %+v", got)
	}
	if got := factstore.FilterByLocus(facts, ""); len(got) != 3 {
		t.Fatalf("empty locus should keep all")
	}
}

func TestReclassifyFactsFillsLocus(t *testing.T) {
	facts := []factmerge.Record{
		{ID: "a", Paths: []string{"architecture.data.flow"}, Text: "`MirrorCommittedMetadataRef` is best-effort."},
	}
	if changed := factstore.Reclassify(facts, false); changed != 1 {
		t.Fatalf("expected the fact to be backfilled, changed=%d", changed)
	}
	if len(facts[0].Locus) == 0 || facts[0].Locus[0] != "mirrorcommittedmetadataref" {
		t.Fatalf("locus not backfilled: %v", facts[0].Locus)
	}
	// Idempotent: a second pass changes nothing (kind + locus already set).
	if changed := factstore.Reclassify(facts, false); changed != 0 {
		t.Fatalf("reclassify should be idempotent, changed=%d", changed)
	}
}
