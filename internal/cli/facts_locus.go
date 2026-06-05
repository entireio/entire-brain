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
