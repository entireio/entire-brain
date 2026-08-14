package cli

import (
	"path/filepath"
	"sort"
	"strings"
)

const semanticTestSuggestionPerFileLimit = 2

// semanticTestSuggestionCandidate carries ranking-only evidence. The public
// suggestion stays compact; weak marks reservoir-only fallback evidence which
// must not consume the emitted agent packet.
type semanticTestSuggestionCandidate struct {
	suggestion   semanticTestSuggestion
	tier         int
	identityHits int
	pathHits     int
	benchmark    bool
}

// legacySemanticTestSuggestions retains the exact pre-ranking evidence order
// solely for downstream likely-file/action synthesis. The agent-facing stream
// is independently ranked and filtered; keeping this private stream prevents a
// token-quality improvement from deleting an already-recalled file.
func legacySemanticTestSuggestions(
	symbolsByID map[string]semanticRecord,
	roots, relations []semanticRecord,
	limit int,
) []semanticTestSuggestion {
	if limit <= 0 {
		return nil
	}
	related := semanticTestRelatedIDs(roots, relations)
	rootDirs := make(map[string]struct{}, len(roots))
	rootNames := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.FilePath != "" {
			rootDirs[pathDirSlash(root.FilePath)] = struct{}{}
		}
		if name := strings.ToLower(strings.TrimSpace(root.Name)); name != "" {
			rootNames = append(rootNames, name)
		}
	}
	seen := make(map[string]struct{}, min(limit, len(symbolsByID)))
	out := make([]semanticTestSuggestion, 0, min(limit, len(symbolsByID)))
	for _, symbol := range sortedSemanticSymbols(symbolsByID) {
		if !isSemanticTestSymbol(symbol) {
			continue
		}
		reason := legacySemanticTestReason(symbol, related, rootDirs, rootNames)
		if reason == "" {
			continue
		}
		if _, ok := seen[symbol.ID]; ok {
			continue
		}
		seen[symbol.ID] = struct{}{}
		out = append(out, semanticTestSuggestion{Symbol: symbol, Reason: reason})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func legacySemanticTestReason(
	symbol semanticRecord,
	related, rootDirs map[string]struct{},
	rootNames []string,
) string {
	if _, ok := related[symbol.ID]; ok {
		return "semantic relation"
	}
	if _, ok := rootDirs[pathDirSlash(symbol.FilePath)]; ok {
		return "same directory"
	}
	if semanticTestRootNameMatch(symbol, rootNames) {
		return "name match"
	}
	return ""
}

func rankSemanticTestSuggestions(
	symbolsByID map[string]semanticRecord,
	roots, relations []semanticRecord,
	query string,
	limit int,
) []semanticTestSuggestion {
	if limit <= 0 || len(symbolsByID) == 0 || len(roots) == 0 {
		return nil
	}
	rootDirs := make(map[string]struct{}, len(roots))
	rootNames := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.FilePath != "" {
			rootDirs[semanticTestPathDirSlash(root.FilePath)] = struct{}{}
		}
		if name := strings.ToLower(strings.TrimSpace(root.Name)); name != "" {
			rootNames = append(rootNames, name)
		}
	}
	terms := semanticTestTaskTerms(query)
	related := semanticTestRelatedIDs(roots, relations)
	exactCompanions := semanticTestExactCompanionPaths(roots)
	alignedRoots := semanticTestTaskAlignedRoots(roots, terms)
	alignedRelated := semanticTestRelatedIDs(alignedRoots, relations)
	alignedExactCompanions := semanticTestExactCompanionPaths(alignedRoots)
	performanceIntent := brainBriefPerformanceIntent(query)
	// The graph is already resident in symbolsByID. Ranking does not need a
	// second O(graph size) sorted copy: the comparator is a total order, so a
	// bounded streaming reservoir is deterministic across map iteration order.
	reservoir := make([]semanticTestSuggestionCandidate, 0, min(limit, len(symbolsByID)))
	for _, symbol := range symbolsByID {
		if !isSemanticTestSymbol(symbol) {
			continue
		}
		candidate, ok := semanticTestRankCandidate(
			symbol,
			related,
			alignedRelated,
			rootDirs,
			exactCompanions,
			alignedExactCompanions,
			rootNames,
			terms,
			performanceIntent,
		)
		if !ok {
			continue
		}
		reservoir = insertSemanticTestSuggestionCandidate(reservoir, candidate, limit)
	}
	if len(reservoir) == 0 {
		return nil
	}
	out := make([]semanticTestSuggestion, 0, len(reservoir))
	for _, candidate := range reservoir {
		out = append(out, candidate.suggestion)
	}
	return out
}

func semanticTestRelatedIDs(roots, relations []semanticRecord) map[string]struct{} {
	related := make(map[string]struct{}, len(roots)+len(relations))
	for _, root := range roots {
		related[root.ID] = struct{}{}
	}
	for _, relation := range relations {
		if _, ok := related[relation.FromID]; ok {
			related[relation.ToID] = struct{}{}
		}
		if _, ok := related[relation.ToID]; ok {
			related[relation.FromID] = struct{}{}
		}
	}
	return related
}

func semanticTestRankCandidate(
	symbol semanticRecord,
	related, alignedRelated, rootDirs, exactCompanions, alignedExactCompanions map[string]struct{},
	rootNames, terms []string,
	performanceIntent bool,
) (semanticTestSuggestionCandidate, bool) {
	_, relation := related[symbol.ID]
	_, alignedRelation := alignedRelated[symbol.ID]
	_, sameDirectory := rootDirs[semanticTestPathDirSlash(symbol.FilePath)]
	nameMatch := semanticTestRootNameMatch(symbol, rootNames)
	_, exactCompanion := exactCompanions[filepath.ToSlash(symbol.FilePath)]
	_, alignedExactCompanion := alignedExactCompanions[filepath.ToSlash(symbol.FilePath)]
	identityHits, pathHits := semanticTestTaskTermHits(symbol, terms)
	benchmark := semanticTestBenchmarkPath(symbol.FilePath)
	benchmarkAligned := !benchmark || (performanceIntent && identityHits+pathHits >= 2)

	candidate := semanticTestSuggestionCandidate{
		suggestion:   semanticTestSuggestion{Symbol: symbol},
		identityHits: identityHits,
		pathHits:     pathHits,
		benchmark:    benchmark && !benchmarkAligned,
	}
	switch {
	case exactCompanion && benchmarkAligned && identityHits+pathHits >= 2:
		candidate.tier = 6
		candidate.suggestion.Reason = "exact source/test companion"
	case benchmarkAligned && (identityHits >= 2 || (identityHits >= 1 && pathHits >= 1)):
		candidate.tier = 5
		candidate.suggestion.Reason = "task term match"
	case relation && benchmarkAligned && identityHits+pathHits > 0:
		candidate.tier = 4
		candidate.suggestion.Reason = "semantic relation"
	case nameMatch && benchmarkAligned && identityHits+pathHits > 0:
		candidate.tier = 3
		candidate.suggestion.Reason = "name match"
	case identityHits >= 1 && sameDirectory && benchmarkAligned:
		// This is the genuinely task-aligned same-directory fallback: the
		// directory alone is insufficient, but a matching test symbol survives.
		candidate.tier = 2
		candidate.suggestion.Reason = "task term match"
	case alignedExactCompanion && benchmarkAligned:
		candidate.tier = 3
		candidate.suggestion.Reason = "exact source/test companion"
		candidate.suggestion.fallback = true
	case alignedRelation && benchmarkAligned:
		candidate.tier = 4
		candidate.suggestion.Reason = "semantic relation"
		candidate.suggestion.fallback = true
	case exactCompanion:
		candidate.tier = 6
		candidate.suggestion.Reason = "exact source/test companion"
		candidate.suggestion.weak = true
	case relation:
		// Relation-only evidence still leads directory-only fallback in the
		// bounded reservoir, but needs task alignment before it spends tokens.
		candidate.tier = 4
		candidate.suggestion.Reason = "semantic relation"
		candidate.suggestion.weak = true
	case sameDirectory:
		candidate.tier = 1
		candidate.suggestion.Reason = "same directory"
		candidate.suggestion.weak = true
	default:
		return semanticTestSuggestionCandidate{}, false
	}
	return candidate, true
}

func semanticTestTaskTerms(query string) []string {
	terms := brainBriefFileMatchTerms(query)
	out := terms[:0]
	for _, term := range terms {
		switch term {
		case "change", "changes", "code", "file", "files", "prioritize", "prioritized", "prioritise",
			"source", "sources", "test", "tests", "testing":
			continue
		}
		out = append(out, term)
	}
	return out
}

func semanticTestTaskAlignedRoots(roots []semanticRecord, terms []string) []semanticRecord {
	aligned := make([]semanticRecord, 0, len(roots))
	for _, root := range roots {
		identityHits, pathHits := semanticTestTaskTermHits(root, terms)
		if identityHits+pathHits > 0 {
			aligned = append(aligned, root)
		}
	}
	return aligned
}

func semanticTestTaskTermHits(symbol semanticRecord, terms []string) (identityHits, pathHits int) {
	base := filepath.Base(symbol.FilePath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	for _, term := range terms {
		switch {
		case semanticTestContainsFold(symbol.Name, term),
			semanticTestContainsFold(symbol.QualifiedName, term),
			semanticTestContainsFold(symbol.Signature, term):
			identityHits++
		case semanticTestContainsFold(base, term):
			pathHits++
		}
	}
	return identityHits, pathHits
}

func semanticTestRootNameMatch(symbol semanticRecord, rootNames []string) bool {
	for _, rootName := range rootNames {
		if rootName != "" && semanticTestContainsFold(symbol.Name, rootName) {
			return true
		}
	}
	return false
}

func semanticTestExactCompanionPaths(roots []semanticRecord) map[string]struct{} {
	paths := make(map[string]struct{}, len(roots)*2)
	for _, root := range roots {
		for _, candidate := range brainBriefSiblingTestCandidates(root.FilePath) {
			paths[filepath.ToSlash(candidate)] = struct{}{}
		}
		for _, candidate := range brainBriefNestedTestCandidates(root.FilePath) {
			paths[filepath.ToSlash(candidate)] = struct{}{}
		}
	}
	return paths
}

func semanticTestPathDirSlash(value string) string {
	value = filepath.ToSlash(value)
	if slash := strings.LastIndexByte(value, '/'); slash >= 0 {
		return value[:slash]
	}
	return "."
}

func semanticTestBenchmarkPath(value string) bool {
	value = filepath.ToSlash(value)
	base := filepath.Base(value)
	return semanticTestContainsFold(base, "_bench") || semanticTestContainsFold(base, "benchmark") ||
		semanticTestContainsFold(value, "/benchmarks/") || semanticTestHasPrefixFold(value, "benchmarks/")
}

// semanticTestContainsFold avoids allocating a lowercased copy for the common
// ASCII identifiers and paths in a large semantic graph. The uncommon Unicode
// task/root term keeps the prior Unicode lowercase behavior.
func semanticTestContainsFold(value, term string) bool {
	if term == "" {
		return true
	}
	if !semanticTestASCII(term) {
		return strings.Contains(strings.ToLower(value), strings.ToLower(term))
	}
	for start := 0; start+len(term) <= len(value); start++ {
		if semanticTestASCIIEqualFold(value[start:start+len(term)], term) {
			return true
		}
	}
	return false
}

func semanticTestHasPrefixFold(value, prefix string) bool {
	if len(value) < len(prefix) {
		return false
	}
	if !semanticTestASCII(prefix) {
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix))
	}
	return semanticTestASCIIEqualFold(value[:len(prefix)], prefix)
}

func semanticTestHasSuffixFold(value, suffix string) bool {
	if len(value) < len(suffix) {
		return false
	}
	if !semanticTestASCII(suffix) {
		return strings.HasSuffix(strings.ToLower(value), strings.ToLower(suffix))
	}
	return semanticTestASCIIEqualFold(value[len(value)-len(suffix):], suffix)
}

func semanticTestASCII(value string) bool {
	for i := range len(value) {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func semanticTestASCIIEqualFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range len(left) {
		l, r := left[i], right[i]
		if 'A' <= l && l <= 'Z' {
			l += 'a' - 'A'
		}
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		if l != r {
			return false
		}
	}
	return true
}

func insertSemanticTestSuggestionCandidate(
	reservoir []semanticTestSuggestionCandidate,
	candidate semanticTestSuggestionCandidate,
	limit int,
) []semanticTestSuggestionCandidate {
	if limit <= 0 {
		return reservoir
	}
	// Enforce diversity inside the reservoir. Applying it only after collection
	// can under-fill a packet when one giant test module owns every early row,
	// even though useful rows from later files exist.
	sameFileCount := 0
	worstSameFile := -1
	for i := range reservoir {
		if reservoir[i].suggestion.Symbol.FilePath != candidate.suggestion.Symbol.FilePath {
			continue
		}
		sameFileCount++
		worstSameFile = i
	}
	if sameFileCount >= semanticTestSuggestionPerFileLimit {
		if !semanticTestSuggestionCandidateLess(candidate, reservoir[worstSameFile]) {
			return reservoir
		}
		copy(reservoir[worstSameFile:], reservoir[worstSameFile+1:])
		reservoir = reservoir[:len(reservoir)-1]
	}
	position := sort.Search(len(reservoir), func(i int) bool {
		return semanticTestSuggestionCandidateLess(candidate, reservoir[i])
	})
	if position >= limit {
		return reservoir
	}
	if len(reservoir) < limit {
		reservoir = append(reservoir, semanticTestSuggestionCandidate{})
	} else {
		reservoir = reservoir[:limit]
	}
	copy(reservoir[position+1:], reservoir[position:len(reservoir)-1])
	reservoir[position] = candidate
	return reservoir
}

func semanticTestSuggestionCandidateLess(left, right semanticTestSuggestionCandidate) bool {
	if left.suggestion.weak != right.suggestion.weak {
		return !left.suggestion.weak
	}
	if left.suggestion.fallback != right.suggestion.fallback {
		return !left.suggestion.fallback
	}
	if left.tier != right.tier {
		return left.tier > right.tier
	}
	if left.identityHits != right.identityHits {
		return left.identityHits > right.identityHits
	}
	if left.pathHits != right.pathHits {
		return left.pathHits > right.pathHits
	}
	if left.benchmark != right.benchmark {
		return !left.benchmark
	}
	leftSymbol, rightSymbol := left.suggestion.Symbol, right.suggestion.Symbol
	if leftSymbol.FilePath != rightSymbol.FilePath {
		return leftSymbol.FilePath < rightSymbol.FilePath
	}
	if leftSymbol.StartLine != rightSymbol.StartLine {
		return leftSymbol.StartLine < rightSymbol.StartLine
	}
	if leftSymbol.QualifiedName != rightSymbol.QualifiedName {
		return leftSymbol.QualifiedName < rightSymbol.QualifiedName
	}
	return leftSymbol.ID < rightSymbol.ID
}

// visibleSemanticTestSuggestions removes weak rows and uses inherited evidence
// only as an abstention fallback when no candidate-local strong row survives.
// When nothing is removed and the cap is already satisfied, it returns the
// original slice so no-match and already-good packets remain byte-identical.
func visibleSemanticTestSuggestions(suggestions []semanticTestSuggestion, limit int) []semanticTestSuggestion {
	if limit <= 0 || len(suggestions) == 0 {
		return suggestions
	}
	needsProjection := len(suggestions) > limit
	hasPrimary, hasFallback := false, false
	for _, suggestion := range suggestions {
		switch {
		case suggestion.weak:
			needsProjection = true
		case suggestion.fallback:
			hasFallback = true
		default:
			hasPrimary = true
		}
	}
	needsProjection = needsProjection || (hasPrimary && hasFallback)
	if !needsProjection {
		return suggestions
	}
	out := make([]semanticTestSuggestion, 0, min(limit, len(suggestions)))
	for _, suggestion := range suggestions {
		if suggestion.weak || (hasPrimary && suggestion.fallback) {
			continue
		}
		out = append(out, suggestion)
		if len(out) >= limit {
			break
		}
	}
	return nonNil(out)
}

func semanticTestSuggestionsForSynthesis(tests semanticTestsResult) []semanticTestSuggestion {
	if tests.synthesisSuggestions != nil {
		return tests.synthesisSuggestions
	}
	return tests.Suggestions
}
