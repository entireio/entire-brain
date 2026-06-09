package cli

import (
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

// inferFactKind deterministically classifies a fact's KIND from its taxonomy
// paths (the prior) refined by a few high-precision text cues. This is the
// no-agent backfill path and the fallback when the agent omits/violates the
// kind; it is heuristic by design — B4's eval is the guardrail. Text cues win
// over the prior so a constraint phrased as a trap surfaces as a gotcha.
func inferFactKind(paths []string, text string) string {
	lower := strings.ToLower(text)
	for _, cue := range gotchaCues {
		if strings.Contains(lower, cue) {
			return factKindGotcha
		}
	}
	// The taxonomy prior, from the dominant top-level across the fact's paths.
	switch factPrimaryTopLevel(paths) {
	case "preferences":
		return factKindPreference
	case "workflow":
		return factKindConvention
	case "architecture":
		return factKindDecision
	case "project":
		return factKindDecision
	case "constraints":
		return factKindInvariant
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

// factPrimaryTopLevel returns the most common taxonomy top-level among paths
// (first wins on a tie via sorted iteration), or "" when there are none.
func factPrimaryTopLevel(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, p := range paths {
		if tl := factTopLevel(p); tl != "" {
			counts[tl]++
		}
	}
	best, bestN := "", 0
	tops := make([]string, 0, len(counts))
	for tl := range counts {
		tops = append(tops, tl)
	}
	sort.Strings(tops)
	for _, tl := range tops {
		if counts[tl] > bestN {
			best, bestN = tl, counts[tl]
		}
	}
	return best
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

// reclassifyFacts backfills the deterministic KIND onto facts in place and
// returns the count changed. By default it only fills facts lacking a valid
// kind (the migration case); force recomputes every fact's kind from scratch
// (e.g. after the inference rules change). It deliberately does NOT bump
// UpdatedAt — kind is additive metadata, not a content edit, so the backfill
// must stay invisible to recency ranking and the gc retention window.
func reclassifyFacts(facts []factRecord, force bool) int {
	changed := 0
	for i := range facts {
		if !force && validFactKind(facts[i].Kind) {
			continue
		}
		want := inferFactKind(facts[i].Paths, facts[i].Text)
		if facts[i].Kind != want {
			facts[i].Kind = want
			changed++
		}
	}
	return changed
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

// locusOverlap counts identifiers shared between a query's locus and a fact's
// locus — the strength of the "they name the same code" signal.
func locusOverlap(queryLocus []string, factText string) int {
	if len(queryLocus) == 0 {
		return 0
	}
	want := make(map[string]struct{}, len(queryLocus))
	for _, q := range queryLocus {
		want[q] = struct{}{}
	}
	n := 0
	for _, f := range factLocus(factText) {
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
