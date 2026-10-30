package cli

import (
	"path/filepath"
	"strings"
)

// retrievalHintsForResults stays in cli with the brain-brief path classifiers
// it binds (cleanBrainBriefHistoryFile, brainBriefRepoFileExists,
// brainBriefLikelyTestFile, extractBrainBriefPaths); the extracted retrieve
// commands inject it as Deps.TaskHints.
func retrievalHintsForResults(repoRoot string, results []unifiedResult, query string) retrievalTaskHints {
	_ = query
	seen := map[string]struct{}{}
	var editFiles, testFiles []string
	addPath := func(path string) {
		clean, ok := cleanBrainBriefHistoryFile(repoRoot, path)
		if !ok || !brainBriefRepoFileExists(repoRoot, clean) {
			return
		}
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		if brainBriefLikelyTestFile(clean) {
			testFiles = append(testFiles, clean)
		} else {
			editFiles = append(editFiles, clean)
		}
	}
	for _, result := range results {
		if result.Source != "history" && result.Source != "doc" {
			continue
		}
		addPath(result.Path)
		for _, path := range extractBrainBriefPaths(result.Text) {
			addPath(path)
		}
	}

	// Pair an indexed test locator with its conventional implementation path.
	// This uses only the paths contained in indexed results plus bounded stat
	// calls; ordinary retrieval never scans or reads the live source tree.
	var siblingImplementations []string
	for _, testFile := range testFiles {
		for _, candidate := range retrievalSiblingImplementationCandidates(testFile) {
			if _, exists := seen[candidate]; exists || !brainBriefRepoFileExists(repoRoot, candidate) {
				continue
			}
			seen[candidate] = struct{}{}
			siblingImplementations = append(siblingImplementations, candidate)
		}
	}
	editFiles = append(siblingImplementations, editFiles...)
	if len(editFiles) > 3 {
		editFiles = editFiles[:3]
	}
	if len(testFiles) > 3 {
		testFiles = testFiles[:3]
	}
	return retrievalTaskHints{LikelyEditFiles: editFiles, LikelyTestFiles: testFiles}
}

func retrievalSiblingImplementationCandidates(testFile string) []string {
	ext := filepath.Ext(testFile)
	switch {
	case strings.HasSuffix(testFile, "_test.go"):
		return []string{strings.TrimSuffix(testFile, "_test.go") + ".go"}
	case strings.HasSuffix(testFile, "_test.py"):
		return []string{strings.TrimSuffix(testFile, "_test.py") + ".py"}
	case strings.Contains(testFile, ".test"+ext):
		return []string{strings.Replace(testFile, ".test"+ext, ext, 1)}
	case strings.Contains(testFile, ".spec"+ext):
		return []string{strings.Replace(testFile, ".spec"+ext, ext, 1)}
	default:
		return nil
	}
}
