package cli

import (
	"os"
	"path/filepath"
	"strings"
)

// brainBriefSemanticFreshnessStats describes the packet-boundary projection,
// not the provider index. A record repeated in two packet sections counts once
// in each section because it consumes packet space (and agent attention) twice.
type brainBriefSemanticFreshnessStats struct {
	InputRecords    int
	KeptRecords     int
	RemovedRecords  int
	RemovedEvidence int
}

func brainBriefSemanticNeedsCurrentBoundary(status brainStatusReport) bool {
	return status.Semantic != nil && brainStatusFreshnessSeverity(status) != "ok"
}

// brainBriefSemanticCandidateLimit is deliberately bounded relative to the
// caller's packet cap. It restores current candidates hidden behind a small
// stale prefix without turning an unsafe index into an unbounded full scan.
func brainBriefSemanticCandidateLimit(requested, indexedSymbols int) int {
	if requested <= 0 {
		return requested
	}
	maxInt := int(^uint(0) >> 1)
	// The semantic stages derive internal scan limits up to candidate*8, so the
	// candidate itself must leave room for that multiplication.
	maxCandidate := maxInt / 8
	if requested >= maxCandidate {
		return maxCandidate
	}
	candidate := requested * 4
	if candidate > maxCandidate {
		candidate = maxCandidate
	}
	if indexedSymbols >= requested && indexedSymbols < candidate {
		candidate = indexedSymbols
	}
	return max(requested, candidate)
}

func brainBriefCapSemantic(semantic *brainBriefSemantic, limit int) {
	if semantic == nil || limit <= 0 {
		return
	}
	semantic.Context.Symbols = capBrainBriefSlice(semantic.Context.Symbols, limit)
	semantic.Context.Neighbors = capBrainBriefSlice(semantic.Context.Neighbors, limit)
	semantic.RuntimeTraces = capBrainBriefSlice(semantic.RuntimeTraces, limit)
	semantic.Tests.Roots = capBrainBriefSlice(semantic.Tests.Roots, limit)
	semantic.Tests.Suggestions = capBrainBriefSlice(semantic.Tests.Suggestions, limit)
	contextIDs := map[string]struct{}{}
	for _, records := range [][]semanticRecord{semantic.Context.Symbols, semantic.Context.Neighbors} {
		for _, record := range records {
			if record.ID != "" {
				contextIDs[record.ID] = struct{}{}
			}
		}
	}
	relations := make([]semanticRecord, 0, len(semantic.Context.Relations))
	for _, relation := range semantic.Context.Relations {
		if !brainBriefSemanticRelationEndpointsKnown(relation, contextIDs) {
			continue
		}
		relations = append(relations, relation)
	}
	semantic.Context.Relations = capBrainBriefSlice(nonNil(relations), brainBriefSemanticRelationLimit(limit))
}

func brainBriefSemanticRelationLimit(limit int) int {
	maxInt := int(^uint(0) >> 1)
	if limit > maxInt/4 {
		return maxInt
	}
	return limit * 4
}

func capBrainBriefSlice[T any](values []T, limit int) []T {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

// brainBriefFilterDepartedSemantic removes indexed semantic records whose
// declared repository file no longer exists in the current worktree. It is a
// brief-only trust boundary: the durable semantic index remains untouched.
//
// Live paths are authoritative. In particular, a changed path which is now
// absent remains useful deletion/restoration context. Records without a file
// path are also retained because external graph nodes and provider contracts
// legitimately have no repository locus.
func brainBriefFilterDepartedSemantic(repoRoot string, live brainLiveState, semantic *brainBriefSemantic) brainBriefSemanticFreshnessStats {
	if semantic == nil || repoRoot == "" {
		return brainBriefSemanticFreshnessStats{}
	}
	filter := newBrainBriefSemanticPathFilter(repoRoot, live)
	var stats brainBriefSemanticFreshnessStats
	keptIDs := map[string]struct{}{}
	keptContextIDs := map[string]struct{}{}
	droppedIDs := map[string]struct{}{}

	filterRecords := func(records []semanticRecord, contextRecord bool) []semanticRecord {
		out := make([]semanticRecord, 0, len(records))
		for _, record := range records {
			stats.InputRecords++
			if !filter.keepRecord(record) {
				stats.RemovedRecords++
				if record.ID != "" {
					droppedIDs[record.ID] = struct{}{}
				}
				continue
			}
			stats.KeptRecords++
			if record.ID != "" {
				keptIDs[record.ID] = struct{}{}
				if contextRecord {
					keptContextIDs[record.ID] = struct{}{}
				}
			}
			out = append(out, record)
		}
		return nonNil(out)
	}

	semantic.Context.Symbols = filterRecords(semantic.Context.Symbols, true)
	semantic.Context.Neighbors = filterRecords(semantic.Context.Neighbors, true)
	semantic.Tests.Roots = filterRecords(semantic.Tests.Roots, false)

	suggestions := make([]semanticTestSuggestion, 0, len(semantic.Tests.Suggestions))
	for _, suggestion := range semantic.Tests.Suggestions {
		stats.InputRecords++
		if !filter.keepRecord(suggestion.Symbol) {
			stats.RemovedRecords++
			if suggestion.Symbol.ID != "" {
				droppedIDs[suggestion.Symbol.ID] = struct{}{}
			}
			continue
		}
		stats.KeptRecords++
		if suggestion.Symbol.ID != "" {
			keptIDs[suggestion.Symbol.ID] = struct{}{}
		}
		suggestions = append(suggestions, suggestion)
	}
	semantic.Tests.Suggestions = nonNil(suggestions)

	for id := range keptIDs {
		delete(droppedIDs, id)
	}
	keepEdge := func(record semanticRecord, requireKnownEndpoints bool) bool {
		if _, dropped := droppedIDs[record.FromID]; dropped {
			return false
		}
		if _, dropped := droppedIDs[record.ToID]; dropped {
			return false
		}
		if requireKnownEndpoints {
			return brainBriefSemanticRelationEndpointsKnown(record, keptContextIDs) && filter.keepRecord(record)
		}
		return filter.keepRecord(record)
	}
	filterEdges := func(records []semanticRecord, requireKnownEndpoints bool) []semanticRecord {
		out := make([]semanticRecord, 0, len(records))
		for _, record := range records {
			stats.InputRecords++
			if !keepEdge(record, requireKnownEndpoints) {
				stats.RemovedRecords++
				continue
			}
			var removed int
			record.Evidence, removed = filter.filterEvidence(record.Evidence)
			stats.RemovedEvidence += removed
			stats.KeptRecords++
			out = append(out, record)
		}
		return nonNil(out)
	}

	semantic.Context.Relations = filterEdges(semantic.Context.Relations, true)
	// Runtime-trace endpoint values may be provider-free-form names rather than
	// semantic IDs. Their own current/live file locus remains the trust boundary.
	semantic.RuntimeTraces = filterEdges(semantic.RuntimeTraces, false)

	content := make([]semanticContent, 0, len(semantic.Context.Content))
	for _, item := range semantic.Context.Content {
		if strings.TrimSpace(item.Path) == "" || filter.pathIsCurrentOrLive(item.Path) {
			content = append(content, item)
		}
	}
	semantic.Context.Content = nonNil(content)
	return stats
}

func brainBriefSemanticRelationEndpointsKnown(record semanticRecord, contextIDs map[string]struct{}) bool {
	for _, id := range []string{record.FromID, record.ToID} {
		if _, known := contextIDs[id]; known {
			continue
		}
		// External graph endpoints deliberately have no repository file record.
		// Other missing endpoints cannot be checked against the current worktree.
		if strings.HasPrefix(id, "external:") {
			continue
		}
		return false
	}
	return true
}

type brainBriefSemanticPathFilter struct {
	repoRoot string
	live     map[string]struct{}
	current  map[string]bool
}

func newBrainBriefSemanticPathFilter(repoRoot string, live brainLiveState) *brainBriefSemanticPathFilter {
	filter := &brainBriefSemanticPathFilter{
		repoRoot: repoRoot,
		live:     map[string]struct{}{},
		current:  map[string]bool{},
	}
	for _, paths := range [][]string{live.ChangedFiles, live.Staged, live.Unstaged, live.Untracked} {
		for _, candidate := range paths {
			if clean, ok := cleanBrainBriefRepoRelativePath(strings.TrimSpace(candidate)); ok {
				filter.live[clean] = struct{}{}
			}
		}
	}
	return filter
}

func (filter *brainBriefSemanticPathFilter) keepRecord(record semanticRecord) bool {
	paths := []string{record.FilePath, record.Path}
	for _, candidate := range paths {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		// Retaining a record retains both fields, so every declared locus must
		// be current/live; one good alias must not smuggle a departed path.
		if !filter.pathIsCurrentOrLive(candidate) {
			return false
		}
	}
	return true
}

func (filter *brainBriefSemanticPathFilter) pathIsCurrentOrLive(candidate string) bool {
	clean, ok := cleanBrainBriefRepoRelativePath(strings.TrimSpace(candidate))
	if !ok {
		return false
	}
	if current, cached := filter.current[clean]; cached {
		return current
	}
	current := brainBriefRepoFileExists(filter.repoRoot, clean)
	if !current {
		if _, live := filter.live[clean]; live {
			// A missing live path is actionable deletion/restoration context. If
			// it still exists but fails the regular-file check, it is a symlink,
			// directory, or crosses a symlink component and is not trusted.
			nativeRel := filepath.FromSlash(clean)
			if err := rejectExistingSymlinkPathComponents(filter.repoRoot, nativeRel); err == nil {
				_, statErr := os.Lstat(filepath.Join(filter.repoRoot, nativeRel))
				current = os.IsNotExist(statErr)
			}
		}
	}
	filter.current[clean] = current
	return current
}

func (filter *brainBriefSemanticPathFilter) filterEvidence(evidence []semanticEvidence) ([]semanticEvidence, int) {
	if len(evidence) == 0 {
		return evidence, 0
	}
	out := make([]semanticEvidence, 0, len(evidence))
	removed := 0
	for _, item := range evidence {
		if strings.TrimSpace(item.FilePath) != "" && !filter.pathIsCurrentOrLive(item.FilePath) {
			removed++
			continue
		}
		out = append(out, item)
	}
	return nonNil(out), removed
}
