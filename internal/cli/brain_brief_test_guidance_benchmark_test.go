package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// BenchmarkBrainBriefTestGuidanceScale covers both the one-target path and a
// saturated mixed-language edit packet. The latter reaches the six-test cap
// after six Java/Python/TypeScript targets while retaining another six edits,
// matching the bounded packet shape used by brain brief.
func BenchmarkBrainBriefTestGuidanceScale(b *testing.B) {
	repoDir := b.TempDir()
	oneJava := []string{"modules/java/src/main/java/com/acme/auth/TokenService.java"}
	writeBrainBriefGuidanceBenchmarkFile(
		b,
		repoDir,
		"modules/java/src/test/java/com/acme/auth/TokenServiceTest.java",
	)

	mixed := make([]string, 0, 12)
	for i := range 4 {
		javaName := fmt.Sprintf("Service%d", i)
		mixed = append(mixed, fmt.Sprintf("modules/java/src/main/java/com/acme/service%d/%s.java", i, javaName))
		writeBrainBriefGuidanceBenchmarkFile(
			b,
			repoDir,
			fmt.Sprintf("modules/java/src/test/java/com/acme/service%d/%sTest.java", i, javaName),
		)

		pythonName := fmt.Sprintf("service%d", i)
		mixed = append(mixed, fmt.Sprintf("src/python%d/%s.py", i, pythonName))
		writeBrainBriefGuidanceBenchmarkFile(
			b,
			repoDir,
			fmt.Sprintf("tests/python%d/test_%s.py", i, pythonName),
		)

		typeScriptName := fmt.Sprintf("service%d", i)
		mixed = append(mixed, fmt.Sprintf("packages/web%d/src/%s.ts", i, typeScriptName))
		writeBrainBriefGuidanceBenchmarkFile(
			b,
			repoDir,
			fmt.Sprintf("packages/web%d/src/__tests__/%s.test.ts", i, typeScriptName),
		)
	}

	for _, tc := range []struct {
		name      string
		editFiles []string
	}{
		{name: "one_java", editFiles: oneJava},
		{name: "twelve_mixed_saturated", editFiles: mixed},
	} {
		legacy := brainBriefAddSiblingTestFilesLegacyBenchmark(repoDir, tc.editFiles)
		candidate := brainBriefAddSiblingTestFiles(repoDir, tc.editFiles, nil)
		if !slices.Equal(candidate, legacy) {
			b.Fatalf("%s output changed: candidate %+v legacy %+v", tc.name, candidate, legacy)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.Run("legacy", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFilesLegacyBenchmark(repoDir, tc.editFiles)
				}
			})
			b.Run("candidate", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFiles(repoDir, tc.editFiles, nil)
				}
			})
		})
	}
}

func brainBriefAddSiblingTestFilesLegacyBenchmark(repoRoot string, editFiles []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(editFiles))
	for _, file := range editFiles {
		directFound := false
		for _, candidate := range brainBriefSiblingTestCandidates(file) {
			if _, ok := seen[candidate]; ok {
				directFound = true
				continue
			}
			if !brainBriefRepoFileExists(repoRoot, candidate) {
				continue
			}
			directFound = true
			seen[candidate] = struct{}{}
			out = append(out, candidate)
			if len(out) >= 6 {
				return out
			}
		}
		if directFound {
			continue
		}
		for _, candidate := range brainBriefNestedTestCandidates(file) {
			if _, ok := seen[candidate]; ok {
				break
			}
			if !brainBriefRepoFileExists(repoRoot, candidate) {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
			if len(out) >= 6 {
				return out
			}
			break
		}
	}
	return out
}

func writeBrainBriefGuidanceBenchmarkFile(t testing.TB, repoDir, path string) {
	t.Helper()
	absolute := filepath.Join(repoDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(absolute, []byte("test\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
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
