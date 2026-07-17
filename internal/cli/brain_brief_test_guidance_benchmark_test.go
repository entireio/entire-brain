package cli

import (
	"os"
	"path/filepath"
	"testing"
)

var benchmarkBrainBriefTestGuidanceFiles []string

// BenchmarkBrainBriefNestedTestPostprocess isolates the bounded filesystem
// postprocess that turns an action/edit target into likely test files. The
// direct-only sub-benchmark is the pre-change behavior for this fixture; the
// common-layout sub-benchmark is the production candidate.
func BenchmarkBrainBriefNestedTestPostprocess(b *testing.B) {
	repoDir := b.TempDir()
	testPath := filepath.Join(repoDir, "src", "auth", "__tests__", "token.test.ts")
	if err := os.MkdirAll(filepath.Dir(testPath), 0o700); err != nil {
		b.Fatalf("mkdir nested test: %v", err)
	}
	if err := os.WriteFile(testPath, []byte("test('token', () => {})\n"), 0o600); err != nil {
		b.Fatalf("write nested test: %v", err)
	}
	editFiles := []string{"src/auth/token.ts"}

	b.Run("direct_sibling_only_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddDirectSiblingTestsForBenchmark(repoDir, editFiles)
		}
	})
	b.Run("common_layout_candidate", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFiles(repoDir, editFiles, nil)
		}
	})
}

func brainBriefAddDirectSiblingTestsForBenchmark(repoRoot string, editFiles []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(editFiles))
	for _, file := range editFiles {
		for _, candidate := range brainBriefSiblingTestCandidates(file) {
			if _, ok := seen[candidate]; ok || !brainBriefRepoFileExists(repoRoot, candidate) {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
			if len(out) >= 6 {
				return out
			}
		}
	}
	return out
}
