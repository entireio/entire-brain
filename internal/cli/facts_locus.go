package cli

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// factScopeCrossCutting marks facts about how work is done (preferences,
// workflow) — they apply repo-wide and are noise for a specific technical
// lookup. factScopeLocal marks facts tied to code/subsystems (architecture,
// constraints) and standing project facts.
const (
	factScopeCrossCutting = "cross-cutting"
	factScopeLocal        = "local"
)

// crossCuttingTopLevels are the taxonomy categories that describe ways of
// working rather than the code itself.
var crossCuttingTopLevels = map[string]struct{}{
	"preferences": {},
	"workflow":    {},
}

// factScope classifies a fact as cross-cutting or local from its taxonomy
// paths. A fact is cross-cutting only when every path is a cross-cutting
// category; any code/constraint/project path makes it local (the technical
// substance wins). This is the scope-tiering signal: separate "how we work"
// from "what this code is", so a technical lookup is not buried under generic
// preference and workflow facts.
func factScope(paths []string) string {
	if len(paths) == 0 {
		return factScopeLocal
	}
	for _, p := range paths {
		if _, ok := crossCuttingTopLevels[factTopLevel(p)]; !ok {
			return factScopeLocal
		}
	}
	return factScopeCrossCutting
}

// The fixed KIND set (Appendix D): the *what shape of claim* axis, orthogonal to
// WHERE (locus) and the topic taxonomy. Kept small and closed so it is a usable
// retrieval filter rather than another sprawling free-text dimension.
const (
	factKindDecision   = "decision"   // a resolved choice + its rationale
	factKindInvariant  = "invariant"  // a must-hold rule/constraint
	factKindGotcha     = "gotcha"     // a non-obvious trap/footgun
	factKindPreference = "preference" // how the user likes work done
	factKindConvention = "convention" // a standing process/style norm
)

// validFactKinds is the closed set; an agent-emitted kind outside it is rejected
// and the deterministic inference is used instead.
var validFactKinds = map[string]struct{}{
	factKindDecision:   {},
	factKindInvariant:  {},
	factKindGotcha:     {},
	factKindPreference: {},
	factKindConvention: {},
}

func validFactKind(kind string) bool {
	_, ok := validFactKinds[strings.ToLower(strings.TrimSpace(kind))]
	return ok
}

// gotchaCues / invariantCues are high-precision phrases that override the
// taxonomy prior in inferFactKind. They are deliberately narrow: a wrong
// override is worse than falling back to the topic prior, and the agent path is
// the accurate source going forward.
var gotchaCues = []string{"gotcha", "footgun", "pitfall", "careful", "watch out", "easy to miss", "easy to forget", "don't forget", "beware", "subtle bug", "surprising", "counterintuitive"}

var invariantCues = []string{"must not", "must always", "must ", "never ", "always ", "is required", "are required", "invariant", "guaranteed", "may not ", "cannot ", "has to "}

// topLevelToKind maps a taxonomy top-level category to its default KIND, or ""
// for an unknown category.
func topLevelToKind(topLevel string) string {
	switch topLevel {
	case "preferences":
		return factKindPreference
	case "workflow":
		return factKindConvention
	case "constraints":
		return factKindInvariant
	case "architecture", "project":
		return factKindDecision
	}
	return ""
}

// factKindPriority breaks ties when a multi-path fact maps to several kinds: the
// more specific/actionable kind wins, so a constraint co-tagged with
// architecture infers invariant rather than the weaker decision default.
var factKindPriority = map[string]int{
	factKindGotcha:     5,
	factKindInvariant:  4,
	factKindPreference: 3,
	factKindConvention: 2,
	factKindDecision:   1,
}

// inferFactKind deterministically classifies a fact's KIND from its taxonomy
// paths (the prior) refined by a few high-precision text cues. This is the
// no-agent backfill path and the fallback when the agent omits/violates the
// kind; it is heuristic by design — B4's eval is the guardrail. A gotcha cue
// wins over everything (a constraint phrased as a trap is a gotcha); otherwise
// the highest-priority kind across the fact's paths wins, so the invariant
// signal of a `constraints.*` path is never lost to an alphabetically-earlier
// co-tag. Allocates nothing — the hot read path calls this per fact.
func inferFactKind(paths []string, text string) string {
	lower := strings.ToLower(text)
	for _, cue := range gotchaCues {
		if strings.Contains(lower, cue) {
			return factKindGotcha
		}
	}
	best := ""
	for _, p := range paths {
		if k := topLevelToKind(factTopLevel(p)); k != "" && (best == "" || factKindPriority[k] > factKindPriority[best]) {
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
			return factKindInvariant
		}
	}
	return factKindDecision
}

// factKindOrInferred returns a fact's stored Kind when present and valid, else
// the deterministic inference. Read surfaces use this so a not-yet-backfilled
// fact still classifies consistently.
func factKindOrInferred(f factRecord) string {
	if validFactKind(f.Kind) {
		return strings.ToLower(strings.TrimSpace(f.Kind))
	}
	return inferFactKind(f.Paths, f.Text)
}

// reclassifyFacts backfills the deterministic derived fields — KIND and LOCUS —
// onto facts in place and returns the count of facts changed. KIND is filled
// only when missing/invalid (force recomputes it, e.g. after the inference rules
// change); LOCUS is purely a function of the text, so it is always reconciled to
// the computed set. It deliberately does NOT bump UpdatedAt — these are additive
// metadata, not a content edit, so the backfill stays invisible to recency
// ranking and the gc retention window.
func reclassifyFacts(facts []factRecord, force bool) int {
	changed := 0
	for i := range facts {
		dirty := false
		if force || !validFactKind(facts[i].Kind) {
			if want := inferFactKind(facts[i].Paths, facts[i].Text); facts[i].Kind != want {
				facts[i].Kind = want
				dirty = true
			}
		}
		if want := nilIfEmpty(factLocus(facts[i].Text)); !equalStrings(facts[i].Locus, want) {
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

// filterFactsByKind keeps only facts of the requested KIND (matching on the
// stored-or-inferred kind); an empty kind returns all.
func filterFactsByKind(facts []factRecord, kind string) []factRecord {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return facts
	}
	out := facts[:0:0]
	for _, f := range facts {
		if factKindOrInferred(f) == kind {
			out = append(out, f)
		}
	}
	return out
}

// locusIdentifierPattern matches code-identifier-like tokens: backtick spans are
// handled separately; this catches dotted paths (settings.Load), file paths
// (internal/cli/facts.go), CamelCase, and snake_case identifiers.
var locusIdentifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:[./][A-Za-z0-9_]+)+|[A-Za-z_]*[a-z][A-Za-z0-9]*[A-Z][A-Za-z0-9_]*|[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+`)

// factLocus extracts the set of code identifiers a fact is about — the symbols,
// packages, file paths, and refs named in its text (including backtick-quoted
// spans). Returned lowercased and deduped. This is the locus signal: it lets
// retrieval boost facts that name the same code the query names, and is the
// hook for tying facts to the semantic index in a later slice.
func factLocus(text string) []string {
	set := map[string]struct{}{}
	add := func(tok string) {
		tok = strings.ToLower(strings.Trim(tok, "`.,;:()[]{}'\""))
		if len(tok) >= 3 {
			set[tok] = struct{}{}
		}
	}
	// Backtick-quoted spans are almost always code; take each whitespace token.
	for _, span := range backtickSpanPattern.FindAllStringSubmatch(text, -1) {
		for _, tok := range strings.Fields(span[1]) {
			add(tok)
		}
	}
	for _, tok := range locusIdentifierPattern.FindAllString(text, -1) {
		add(tok)
	}
	out := make([]string, 0, len(set))
	for tok := range set {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

var backtickSpanPattern = regexp.MustCompile("`([^`]+)`")

// factLocusOf returns a fact's stored locus, falling back to deriving it from
// the text for facts authored before the locus field existed (or never
// backfilled). Read surfaces use this so locus matching works regardless of
// whether `reclassify` has run.
func factLocusOf(f factRecord) []string {
	if len(f.Locus) > 0 {
		return f.Locus
	}
	return factLocus(f.Text)
}

// factsRelevantToChange ranks active facts whose locus names a changed file or a
// symbol defined in those files — the "changed files → relevant facts" tie-in
// between the durable-facts locus and the semantic graph. files are the changed
// paths; symbols are the semantic symbols in those files (already resolved by
// the caller). Ranked by overlap count, then recency, capped at limit.
func factsRelevantToChange(files []string, symbols []semanticRecord, facts []factRecord, limit int) []factRecord {
	if limit <= 0 {
		limit = 10
	}
	target := map[string]struct{}{}
	add := func(s string) {
		if s = strings.ToLower(strings.TrimSpace(s)); len(s) >= 3 {
			target[s] = struct{}{}
		}
	}
	for _, f := range files {
		add(f)
		add(filepath.Base(f))
	}
	for _, s := range symbols {
		add(s.Name)
		add(s.QualifiedName)
	}
	if len(target) == 0 {
		return nil
	}
	type scored struct {
		rec factRecord
		n   int
	}
	var hits []scored
	for _, f := range facts {
		if f.Status != factStatusActive {
			continue
		}
		n := 0
		for _, tok := range factLocusOf(f) {
			if _, ok := target[tok]; ok {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{f, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].rec.UpdatedAt.After(hits[j].rec.UpdatedAt)
	})
	out := make([]factRecord, 0, min(limit, len(hits)))
	for _, h := range hits {
		out = append(out, h.rec)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// filterFactsByLocus keeps facts whose locus intersects the query's locus (a
// path/glob/symbol). An empty query returns all. The query is run through
// factLocus so a path or CamelCase symbol normalizes the same way the stored
// loci did.
func filterFactsByLocus(facts []factRecord, locus string) []factRecord {
	locus = strings.TrimSpace(locus)
	if locus == "" {
		return facts
	}
	want := map[string]struct{}{strings.ToLower(locus): {}}
	for _, t := range factLocus(locus) {
		want[t] = struct{}{}
	}
	out := facts[:0:0]
	for _, f := range facts {
		for _, t := range factLocusOf(f) {
			if _, ok := want[t]; ok {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// locusOverlap counts identifiers shared between a query's locus and a fact's
// locus derived from its text — the strength of the "they name the same code"
// signal. Prefer locusOverlapTokens when the fact's locus is already stored.
func locusOverlap(queryLocus []string, factText string) int {
	return locusOverlapTokens(queryLocus, factLocus(factText))
}

// locusOverlapTokens counts identifiers shared between two locus token sets.
func locusOverlapTokens(queryLocus, factLocus []string) int {
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

// filterFactsByScope returns facts matching the requested scope
// (factScopeLocal/factScopeCrossCutting); an empty scope returns all.
func filterFactsByScope(facts []factRecord, scope string) []factRecord {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return facts
	}
	out := facts[:0:0]
	for _, f := range facts {
		if factScope(f.Paths) == scope {
			out = append(out, f)
		}
	}
	return out
}
