package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBrainBriefActionTargetsAddNativeGoTests(t *testing.T) {
	repoDir := t.TempDir()
	for path, body := range map[string]string{
		"src/auth/token.go":      "package auth\nfunc validateToken() {}\n",
		"src/auth/token_test.go": "package auth\nimport \"testing\"\nfunc TestToken(t *testing.T) {}\n",
	} {
		writeBrainBriefGoLayoutFile(t, repoDir, path, body)
	}

	report := brainBriefReport{
		Task:            "fix token validation",
		ActionChecklist: []brainBriefAction{{File: "src/auth/token.go", Symbol: "validateToken", Action: "preserve expiry validation"}},
		LikelyEditFiles: []string{"src/auth/token.go"},
		LikelyTestFiles: []string{
			"tests/distractor_1_test.go", "tests/distractor_2_test.go", "tests/distractor_3_test.go",
			"tests/distractor_4_test.go", "tests/distractor_5_test.go", "tests/distractor_6_test.go",
		},
	}
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	baseline := report

	brainBriefPrioritizeActionTargets(repoDir, &report)
	wantTests := []string{
		"src/auth/token_test.go",
		"tests/distractor_1_test.go", "tests/distractor_2_test.go", "tests/distractor_3_test.go",
		"tests/distractor_4_test.go", "tests/distractor_5_test.go",
	}
	if !slices.Equal(report.LikelyTestFiles, wantTests) {
		t.Fatalf("sibling action test should rank first under the six-file cap: got %+v want %+v", report.LikelyTestFiles, wantTests)
	}
	wantLikely := append([]string{"src/auth/token.go"}, wantTests...)
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
		"sibling-go-test scenario: recall@6 0->1, reciprocal-rank 0->1; compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(baselinePacket), len(candidatePacket), byteDelta,
		compactPacketByteProxy(baselinePacket), compactPacketByteProxy(candidatePacket), proxyDelta,
	)

	emptyBaseline := baseline
	emptyBaseline.LikelyTestFiles = nil
	emptyBaseline.LikelyFiles = brainBriefMergeLikelyFiles(emptyBaseline.LikelyEditFiles, nil)
	emptyCandidate := emptyBaseline
	brainBriefPrioritizeActionTargets(repoDir, &emptyCandidate)
	if !slices.Equal(emptyCandidate.LikelyTestFiles, []string{"src/auth/token_test.go"}) {
		t.Fatalf("empty test section did not receive the sibling test: %+v", emptyCandidate.LikelyTestFiles)
	}
	emptyBaselinePacket := renderBrainBriefCompactV3ForTest(t, emptyBaseline)
	emptyCandidatePacket := renderBrainBriefCompactV3ForTest(t, emptyCandidate)
	emptyByteDelta := len(emptyCandidatePacket) - len(emptyBaselinePacket)
	emptyProxyDelta := compactPacketByteProxy(emptyCandidatePacket) - compactPacketByteProxy(emptyBaselinePacket)
	if emptyByteDelta > 64 || emptyProxyDelta > 16 {
		t.Fatalf("one recalled test grew an empty test section too much: bytes %+d proxy %+d", emptyByteDelta, emptyProxyDelta)
	}
	t.Logf(
		"empty-test-section packet cost: compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(emptyBaselinePacket), len(emptyCandidatePacket), emptyByteDelta,
		compactPacketByteProxy(emptyBaselinePacket), compactPacketByteProxy(emptyCandidatePacket), emptyProxyDelta,
	)
}

func TestBrainBriefNativeGoTestsPreferSamePackage(t *testing.T) {
	repoDir := t.TempDir()
	for _, path := range []string{
		"src/auth/token_test.go",
		"tests/auth/token_test.go",
	} {
		writeBrainBriefGoLayoutFile(t, repoDir, path, "package auth\nimport \"testing\"\nfunc TestToken(t *testing.T) {}\n")
	}

	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"src/auth/token.go"}, nil)
	want := []string{"src/auth/token_test.go"}
	if !slices.Equal(tests, want) {
		t.Fatalf("direct sibling should exclude unrelated test directories: got %+v want %+v", tests, want)
	}
}

func TestBrainBriefNativeGoTestsDoNotGuessMissingSiblings(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefGoLayoutFile(t, repoDir, "src/auth/token_test.go", "package auth\nimport \"testing\"\nfunc TestToken(t *testing.T) {}\n")

	if tests := brainBriefAddSiblingTestFiles(repoDir, []string{"lib/auth/token.go"}, nil); len(tests) != 0 {
		t.Fatalf("Go path without a sibling should not guess a test: %+v", tests)
	}
	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"src/auth/new_token.go"}, nil)
	if len(tests) != 0 {
		t.Fatalf("nonexistent sibling candidate should not be surfaced: %+v", tests)
	}
}

func TestBrainBriefNativeGoTestsRequireSafeExistingFiles(t *testing.T) {
	repoDir := t.TempDir()
	outsideDir := t.TempDir()
	writeBrainBriefGoLayoutFile(t, outsideDir, "token_test.go", "package auth\nimport \"testing\"\nfunc TestToken(t *testing.T) {}\n")
	linkDir := filepath.Join(repoDir, "src", "auth")
	if err := os.MkdirAll(filepath.Dir(linkDir), 0o700); err != nil {
		t.Fatalf("mkdir link parent: %v", err)
	}
	if err := os.Symlink(outsideDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"src/auth/token.go"}, nil)
	if len(tests) != 0 {
		t.Fatalf("test under outside symlinked directory should not be surfaced: %+v", tests)
	}
}

func TestBrainBriefNativeGoTestSupportsIntendedCreateSource(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefGoLayoutFile(t, repoDir, "src/auth/new_token_test.go", "package auth\nimport \"testing\"\nfunc TestNewToken(t *testing.T) {}\n")
	report := brainBriefReport{
		ActionChecklist: []brainBriefAction{{File: "src/auth/new_token.go", Action: "create the token helper"}},
		LikelyEditFiles: []string{"src/auth/new_token.go"},
	}

	brainBriefPrioritizeActionTargets(repoDir, &report)
	if !slices.Equal(report.LikelyEditFiles, []string{"src/auth/new_token.go"}) {
		t.Fatalf("intended-create edit path changed: %+v", report.LikelyEditFiles)
	}
	if !slices.Equal(report.LikelyTestFiles, []string{"src/auth/new_token_test.go"}) {
		t.Fatalf("existing validation for intended-create source was not surfaced: %+v", report.LikelyTestFiles)
	}
}
