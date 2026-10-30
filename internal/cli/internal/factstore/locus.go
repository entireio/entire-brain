package factstore

import (
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// ScopeCrossCutting marks facts about how work is done (preferences,
// workflow) — they apply repo-wide and are noise for a specific technical
// lookup. ScopeLocal marks facts tied to code/subsystems (architecture,
// constraints) and standing project facts.
const (
	ScopeCrossCutting = "cross-cutting"
	ScopeLocal        = "local"
)

// crossCuttingTopLevels are the taxonomy categories that describe ways of
// working rather than the code itself.
var crossCuttingTopLevels = map[string]struct{}{
	"preferences": {},
	"workflow":    {},
}

// ScopeOf classifies a fact as cross-cutting or local from its taxonomy
// paths. A fact is cross-cutting only when every path is a cross-cutting
// category; any code/constraint/project path makes it local (the technical
// substance wins). This is the scope-tiering signal: separate "how we work"
// from "what this code is", so a technical lookup is not buried under generic
// preference and workflow facts.
func ScopeOf(paths []string) string {
	if len(paths) == 0 {
		return ScopeLocal
	}
	for _, p := range paths {
		if _, ok := crossCuttingTopLevels[TopLevel(p)]; !ok {
			return ScopeLocal
		}
	}
	return ScopeCrossCutting
}

// The fixed KIND set (Appendix D) — factKind* consts, the validFactKinds set,
// and factmerge.ValidFactKind — moved to factmerge (Kind* / ValidFactKind). The
// CLI names are aliases/wrappers in facts_aliases.go, so the inference and
// filtering below (which stay here because they depend on semanticRecord and
// other CLI-only surfaces) reference them unchanged.

// gotchaCues / invariantCues are high-precision phrases that override the
// taxonomy prior in InferKind. They are deliberately narrow: a wrong
// override is worse than falling back to the topic prior, and the agent path is
// the accurate source going forward.
var gotchaCues = []string{"gotcha", "footgun", "pitfall", "careful", "watch out", "easy to miss", "easy to forget", "don't forget", "beware", "subtle bug", "surprising", "counterintuitive", "fails silently", "silently fails", "breaks if", "only works if"}

// closedNegativeCues mark a settled-negative: something tried/considered and
// rejected with evidence. Checked before gotchaCues — a dead end phrased as a
// trap is still a dead end, and the closed-negative framing carries the
// revisit-when trigger a future session needs.
var closedNegativeCues = []string{"do not re-litigate", "dead end", "ruled out", "decided against", "rejected because", "was rejected", "measured as noise", "not a lever", "within noise", "abandoned because", "didn't work because", "did not work because", "tried and failed", "made no difference", "no measurable", "revisit only if", "revisit when"}

var invariantCues = []string{"must not", "must always", "must ", "never ", "always ", "is required", "are required", "invariant", "guaranteed", "may not ", "cannot ", "has to "}

// topLevelToKind maps a taxonomy top-level category to its default KIND, or ""
// for an unknown category.
func topLevelToKind(topLevel string) string {
	switch topLevel {
	case "preferences":
		return factmerge.KindPreference
	case "workflow":
		return factmerge.KindConvention
	case "constraints":
		return factmerge.KindInvariant
	case "architecture", "project":
		return factmerge.KindDecision
	}
	return ""
}

// KindPriority breaks ties when a multi-path fact maps to several kinds: the
// more specific/actionable kind wins, so a constraint co-tagged with
// architecture infers invariant rather than the weaker decision default.
var KindPriority = map[string]int{
	factmerge.KindClosedNegative: 6,
	factmerge.KindGotcha:         5,
	factmerge.KindInvariant:      4,
	factmerge.KindPreference:     3,
	factmerge.KindConvention:     2,
	factmerge.KindDecision:       1,
}

// InferKind deterministically classifies a fact's KIND from its taxonomy
// paths (the prior) refined by a few high-precision text cues. This is the
// no-agent backfill path and the fallback when the agent omits/violates the
// kind; it is heuristic by design — B4's eval is the guardrail. A gotcha cue
// wins over everything (a constraint phrased as a trap is a gotcha); otherwise
// the highest-priority kind across the fact's paths wins, so the invariant
// signal of a `constraints.*` path is never lost to an alphabetically-earlier
// co-tag. Allocates nothing — the hot read path calls this per fact.
func InferKind(paths []string, text string) string {
	lower := strings.ToLower(text)
	for _, cue := range closedNegativeCues {
		if strings.Contains(lower, cue) {
			return factmerge.KindClosedNegative
		}
	}
	for _, cue := range gotchaCues {
		if strings.Contains(lower, cue) {
			return factmerge.KindGotcha
		}
	}
	best := ""
	for _, p := range paths {
		if k := topLevelToKind(TopLevel(p)); k != "" && (best == "" || KindPriority[k] > KindPriority[best]) {
			best = k
		}
	}
	if best != "" {
		return best
	}
	// No (or unknown) taxonomy signal: let an invariant cue pull it off the
	// decision default.
	for _, cue := range invariantCues {
		if strings.Contains(lower, cue) {
			return factmerge.KindInvariant
		}
	}
	return factmerge.KindDecision
}

// KindOrInferred returns a fact's stored Kind when present and valid, else
// the deterministic inference. Read surfaces use this so a not-yet-backfilled
// fact still classifies consistently.
func KindOrInferred(f factmerge.Record) string {
	if factmerge.ValidFactKind(f.Kind) {
		return strings.ToLower(strings.TrimSpace(f.Kind))
	}
	return InferKind(f.Paths, f.Text)
}

// Reclassify backfills the deterministic derived fields — KIND and LOCUS —
// onto facts in place and returns the count of facts changed. KIND is filled
// only when missing/invalid (force recomputes it, e.g. after the inference rules
// change); LOCUS is purely a function of the text, so it is always reconciled to
// the computed set. It deliberately does NOT bump UpdatedAt — these are additive
// metadata, not a content edit, so the backfill stays invisible to recency
// ranking and the gc retention window.
func Reclassify(facts []factmerge.Record, force bool) int {
	changed := 0
	for i := range facts {
		dirty := false
		if force || !factmerge.ValidFactKind(facts[i].Kind) {
			if want := InferKind(facts[i].Paths, facts[i].Text); facts[i].Kind != want {
				facts[i].Kind = want
				dirty = true
			}
		}
		if want := nilIfEmpty(base.FactLocus(facts[i].Text)); !equalStrings(facts[i].Locus, want) {
			facts[i].Locus = want
			dirty = true
		}
		if dirty {
			changed++
		}
	}
	return changed
}

// nilIfEmpty normalizes an empty slice to nil so an absent locus is omitted from
// JSON (omitempty) and compares equal across runs.
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// equalStrings reports whether two string slices are element-wise equal. Both
// loci are produced by factLocus (sorted, deduped), so order is canonical.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FilterByKind keeps only facts of the requested KIND (matching on the
// stored-or-inferred kind); an empty kind returns all.
func FilterByKind(facts []factmerge.Record, kind string) []factmerge.Record {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return facts
	}
	out := facts[:0:0]
	for _, f := range facts {
		if KindOrInferred(f) == kind {
			out = append(out, f)
		}
	}
	return out
}

// FilterByLocus keeps facts whose locus intersects the query's locus (a
// path/glob/symbol). An empty query returns all. The query is run through
// factLocus so a path or CamelCase symbol normalizes the same way the stored
// loci did.
func FilterByLocus(facts []factmerge.Record, locus string) []factmerge.Record {
	locus = strings.TrimSpace(locus)
	if locus == "" {
		return facts
	}
	want := map[string]struct{}{strings.ToLower(locus): {}}
	for _, t := range base.FactLocus(locus) {
		want[t] = struct{}{}
	}
	out := facts[:0:0]
	for _, f := range facts {
		for _, t := range base.FactLocusOf(f) {
			if _, ok := want[t]; ok {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// LocusOverlap counts identifiers shared between a query's locus and a fact's
// locus derived from its text — the strength of the "they name the same code"
// signal. Prefer LocusOverlapTokens when the fact's locus is already stored.
func LocusOverlap(queryLocus []string, factText string) int {
	return LocusOverlapTokens(queryLocus, base.FactLocus(factText))
}

// LocusOverlapTokens counts identifiers shared between two locus token sets.
func LocusOverlapTokens(queryLocus, factLocus []string) int {
	if len(queryLocus) == 0 || len(factLocus) == 0 {
		return 0
	}
	want := make(map[string]struct{}, len(queryLocus))
	for _, q := range queryLocus {
		want[q] = struct{}{}
	}
	n := 0
	for _, f := range factLocus {
		if _, ok := want[f]; ok {
			n++
		}
	}
	return n
}

// FilterByScope returns facts matching the requested scope
// (ScopeLocal/ScopeCrossCutting); an empty scope returns all.
func FilterByScope(facts []factmerge.Record, scope string) []factmerge.Record {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return facts
	}
	out := facts[:0:0]
	for _, f := range facts {
		if ScopeOf(f.Paths) == scope {
			out = append(out, f)
		}
	}
	return out
}
