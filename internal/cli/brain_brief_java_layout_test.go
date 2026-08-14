package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBrainBriefActionTargetsAddMirroredJavaTests(t *testing.T) {
	repoDir := t.TempDir()
	for path, body := range map[string]string{
		"src/main/java/com/acme/auth/TokenService.java":     "package com.acme.auth;\nclass TokenService {}\n",
		"src/test/java/com/acme/auth/TokenServiceTest.java": "package com.acme.auth;\nclass TokenServiceTest {}\n",
	} {
		writeBrainBriefJavaLayoutFile(t, repoDir, path, body)
	}
	for _, candidate := range brainBriefSiblingTestCandidates("src/main/java/com/acme/auth/TokenService.java") {
		if brainBriefRepoFileExists(repoDir, candidate) {
			t.Fatalf("fixture unexpectedly has a direct sibling test: %s", candidate)
		}
	}

	report := brainBriefReport{
		Task: "fix token validation",
		ActionChecklist: []brainBriefAction{{
			File:   "src/main/java/com/acme/auth/TokenService.java",
			Symbol: "TokenService",
			Action: "preserve expiry validation",
		}},
		LikelyEditFiles: []string{"src/main/java/com/acme/auth/TokenService.java"},
		LikelyTestFiles: []string{
			"src/test/java/com/acme/noise/OneTest.java",
			"src/test/java/com/acme/noise/TwoTest.java",
			"src/test/java/com/acme/noise/ThreeTest.java",
			"src/test/java/com/acme/noise/FourTest.java",
			"src/test/java/com/acme/noise/FiveTest.java",
			"src/test/java/com/acme/noise/SixTest.java",
		},
	}
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	baseline := report

	brainBriefPrioritizeActionTargets(repoDir, &report)
	wantTests := []string{
		"src/test/java/com/acme/auth/TokenServiceTest.java",
		"src/test/java/com/acme/noise/OneTest.java",
		"src/test/java/com/acme/noise/TwoTest.java",
		"src/test/java/com/acme/noise/ThreeTest.java",
		"src/test/java/com/acme/noise/FourTest.java",
		"src/test/java/com/acme/noise/FiveTest.java",
	}
	if !slices.Equal(report.LikelyTestFiles, wantTests) {
		t.Fatalf("mirrored action test should rank first under the six-file cap: got %+v want %+v", report.LikelyTestFiles, wantTests)
	}
	wantLikely := append([]string{"src/main/java/com/acme/auth/TokenService.java"}, wantTests...)
	if !slices.Equal(report.LikelyFiles, wantLikely) {
		t.Fatalf("combined packet lost the exact delivered top-six test order: got %+v want %+v", report.LikelyFiles, wantLikely)
	}

	baselinePacket := renderBrainBriefCompactV3ForTest(t, baseline)
	candidatePacket := renderBrainBriefCompactV3ForTest(t, report)
	byteDelta := len(candidatePacket) - len(baselinePacket)
	proxyDelta := compactPacketByteProxy(candidatePacket) - compactPacketByteProxy(baselinePacket)
	if byteDelta > 64 || proxyDelta > 16 {
		t.Fatalf("one recalled test grew the bounded packet too much: bytes %+d proxy %+d", byteDelta, proxyDelta)
	}
	t.Logf(
		"mirrored-java-test saturated scenario: recall@6 0->1, reciprocal-rank 0->1; compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(baselinePacket), len(candidatePacket), byteDelta,
		compactPacketByteProxy(baselinePacket), compactPacketByteProxy(candidatePacket), proxyDelta,
	)

	emptyBaseline := baseline
	emptyBaseline.LikelyTestFiles = nil
	emptyBaseline.LikelyFiles = brainBriefMergeLikelyFiles(emptyBaseline.LikelyEditFiles, nil)
	emptyCandidate := emptyBaseline
	brainBriefPrioritizeActionTargets(repoDir, &emptyCandidate)
	if !slices.Equal(emptyCandidate.LikelyTestFiles, []string{"src/test/java/com/acme/auth/TokenServiceTest.java"}) {
		t.Fatalf("empty test section did not receive the mirrored test: %+v", emptyCandidate.LikelyTestFiles)
	}
	emptyBaselinePacket := renderBrainBriefCompactV3ForTest(t, emptyBaseline)
	emptyCandidatePacket := renderBrainBriefCompactV3ForTest(t, emptyCandidate)
	emptyByteDelta := len(emptyCandidatePacket) - len(emptyBaselinePacket)
	emptyProxyDelta := compactPacketByteProxy(emptyCandidatePacket) - compactPacketByteProxy(emptyBaselinePacket)
	if emptyByteDelta > 80 || emptyProxyDelta > 20 {
		t.Fatalf("one recalled test grew an empty test section too much: bytes %+d proxy %+d", emptyByteDelta, emptyProxyDelta)
	}
	t.Logf(
		"mirrored-java-test empty scenario: compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(emptyBaselinePacket), len(emptyCandidatePacket), emptyByteDelta,
		compactPacketByteProxy(emptyBaselinePacket), compactPacketByteProxy(emptyCandidatePacket), emptyProxyDelta,
	)
}

func TestBrainBriefMirroredJavaTestsPreserveModulePrefix(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefJavaLayoutFile(
		t,
		repoDir,
		"services/auth/src/test/java/com/acme/auth/TokenServiceTest.java",
		"package com.acme.auth;\nclass TokenServiceTest {}\n",
	)

	tests := brainBriefAddSiblingTestFiles(
		repoDir,
		[]string{"services/auth/src/main/java/com/acme/auth/TokenService.java"},
		nil,
	)
	want := []string{"services/auth/src/test/java/com/acme/auth/TokenServiceTest.java"}
	if !slices.Equal(tests, want) {
		t.Fatalf("module-local mirrored test mismatch: got %+v want %+v", tests, want)
	}
}

func TestBrainBriefMirroredJavaTestsStayBoundedToCanonicalLayout(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefJavaLayoutFile(
		t,
		repoDir,
		"src/test/java/com/acme/auth/TokenServiceTest.java",
		"package com.acme.auth;\nclass TokenServiceTest {}\n",
	)

	for _, source := range []string{
		"src/java/com/acme/auth/TokenService.java",
		"src/main/kotlin/com/acme/auth/TokenService.kt",
		"foosrc/main/java/com/acme/auth/TokenService.java",
	} {
		if tests := brainBriefAddSiblingTestFiles(repoDir, []string{source}, nil); len(tests) != 0 {
			t.Fatalf("non-canonical Java source %q guessed a mirrored test: %+v", source, tests)
		}
	}
	if tests := brainBriefAddSiblingTestFiles(
		repoDir,
		[]string{"src/main/java/com/acme/auth/MissingService.java"},
		nil,
	); len(tests) != 0 {
		t.Fatalf("nonexistent mirrored candidate should not be surfaced: %+v", tests)
	}
}

func TestBrainBriefMirroredJavaTestsRequireSafeExistingFiles(t *testing.T) {
	repoDir := t.TempDir()
	outsideDir := t.TempDir()
	writeBrainBriefJavaLayoutFile(
		t,
		outsideDir,
		"TokenServiceTest.java",
		"class TokenServiceTest {}\n",
	)
	linkDir := filepath.Join(repoDir, "src", "test", "java", "com", "acme", "auth")
	if err := os.MkdirAll(filepath.Dir(linkDir), 0o700); err != nil {
		t.Fatalf("mkdir link parent: %v", err)
	}
	if err := os.Symlink(outsideDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tests := brainBriefAddSiblingTestFiles(
		repoDir,
		[]string{"src/main/java/com/acme/auth/TokenService.java"},
		nil,
	)
	if len(tests) != 0 {
		t.Fatalf("test under outside symlinked directory should not be surfaced: %+v", tests)
	}
}

func TestBrainBriefMirroredJavaTestSupportsIntendedCreateSource(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefJavaLayoutFile(
		t,
		repoDir,
		"src/test/java/com/acme/auth/NewTokenServiceTest.java",
		"package com.acme.auth;\nclass NewTokenServiceTest {}\n",
	)
	report := brainBriefReport{
		ActionChecklist: []brainBriefAction{{
			File:   "src/main/java/com/acme/auth/NewTokenService.java",
			Action: "create the token service",
		}},
		LikelyEditFiles: []string{"src/main/java/com/acme/auth/NewTokenService.java"},
	}

	brainBriefPrioritizeActionTargets(repoDir, &report)
	if !slices.Equal(report.LikelyEditFiles, []string{"src/main/java/com/acme/auth/NewTokenService.java"}) {
		t.Fatalf("intended-create edit path changed: %+v", report.LikelyEditFiles)
	}
	if !slices.Equal(report.LikelyTestFiles, []string{"src/test/java/com/acme/auth/NewTokenServiceTest.java"}) {
		t.Fatalf("existing validation for intended-create source was not surfaced: %+v", report.LikelyTestFiles)
	}
}

func BenchmarkBrainBriefMirroredJavaTestPostprocess(b *testing.B) {
	repoDir := b.TempDir()
	writeBrainBriefJavaLayoutFile(
		b,
		repoDir,
		"src/test/java/com/acme/auth/TokenServiceTest.java",
		"package com.acme.auth;\nclass TokenServiceTest {}\n",
	)
	editFiles := []string{"src/main/java/com/acme/auth/TokenService.java"}

	b.Run("direct_sibling_only_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddDirectSiblingTestsForBenchmark(repoDir, editFiles)
		}
	})
	b.Run("mirrored_layout_candidate", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFiles(repoDir, editFiles, nil)
		}
	})
}

func writeBrainBriefJavaLayoutFile(t testing.TB, repoDir, path, body string) {
	t.Helper()
	absolute := filepath.Join(repoDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
