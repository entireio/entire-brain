package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBrainBriefActionTargetsAddMirroredPythonTests(t *testing.T) {
	repoDir := t.TempDir()
	for path, body := range map[string]string{
		"src/auth/token.py":        "def validate_token():\n    pass\n",
		"tests/auth/test_token.py": "def test_token():\n    pass\n",
	} {
		writeBrainBriefPythonLayoutFile(t, repoDir, path, body)
	}
	for _, candidate := range brainBriefSiblingTestCandidates("src/auth/token.py") {
		if brainBriefRepoFileExists(repoDir, candidate) {
			t.Fatalf("fixture unexpectedly has a direct sibling test: %s", candidate)
		}
	}

	report := brainBriefReport{
		Task:            "fix token validation",
		ActionChecklist: []brainBriefAction{{File: "src/auth/token.py", Symbol: "validate_token", Action: "preserve expiry validation"}},
		LikelyEditFiles: []string{"src/auth/token.py"},
		LikelyTestFiles: []string{
			"tests/test_distractor_1.py", "tests/test_distractor_2.py", "tests/test_distractor_3.py",
			"tests/test_distractor_4.py", "tests/test_distractor_5.py", "tests/test_distractor_6.py",
		},
	}
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	baseline := report

	brainBriefPrioritizeActionTargets(repoDir, &report)
	wantTests := []string{
		"tests/auth/test_token.py",
		"tests/test_distractor_1.py", "tests/test_distractor_2.py", "tests/test_distractor_3.py",
		"tests/test_distractor_4.py", "tests/test_distractor_5.py",
	}
	if !slices.Equal(report.LikelyTestFiles, wantTests) {
		t.Fatalf("mirrored action test should rank first under the six-file cap: got %+v want %+v", report.LikelyTestFiles, wantTests)
	}
	wantLikely := append([]string{"src/auth/token.py"}, wantTests...)
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
		"mirrored-python-test scenario: recall@6 0->1, reciprocal-rank 0->1; compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(baselinePacket), len(candidatePacket), byteDelta,
		compactPacketByteProxy(baselinePacket), compactPacketByteProxy(candidatePacket), proxyDelta,
	)

	emptyBaseline := baseline
	emptyBaseline.LikelyTestFiles = nil
	emptyBaseline.LikelyFiles = brainBriefMergeLikelyFiles(emptyBaseline.LikelyEditFiles, nil)
	emptyCandidate := emptyBaseline
	brainBriefPrioritizeActionTargets(repoDir, &emptyCandidate)
	if !slices.Equal(emptyCandidate.LikelyTestFiles, []string{"tests/auth/test_token.py"}) {
		t.Fatalf("empty test section did not receive the mirrored test: %+v", emptyCandidate.LikelyTestFiles)
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

func TestBrainBriefMirroredPythonTestsAreFallbackToDirectSiblings(t *testing.T) {
	repoDir := t.TempDir()
	for _, path := range []string{
		"src/auth/token_test.py",
		"tests/auth/test_token.py",
	} {
		writeBrainBriefPythonLayoutFile(t, repoDir, path, "def test_token():\n    pass\n")
	}

	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"src/auth/token.py"}, nil)
	want := []string{"src/auth/token_test.py"}
	if !slices.Equal(tests, want) {
		t.Fatalf("direct sibling should suppress the mirrored fallback: got %+v want %+v", tests, want)
	}
}

func TestBrainBriefMirroredPythonTestsStayBoundedToSrcLayout(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefPythonLayoutFile(t, repoDir, "tests/auth/test_token.py", "def test_token():\n    pass\n")

	if tests := brainBriefAddSiblingTestFiles(repoDir, []string{"lib/auth/token.py"}, nil); len(tests) != 0 {
		t.Fatalf("non-src Python path should not guess a mirrored test: %+v", tests)
	}
	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"src/auth/new_token.py"}, nil)
	if len(tests) != 0 {
		t.Fatalf("nonexistent mirrored candidate should not be surfaced: %+v", tests)
	}
}

func TestBrainBriefMirroredPythonTestsRequireSafeExistingFiles(t *testing.T) {
	repoDir := t.TempDir()
	outsideDir := t.TempDir()
	writeBrainBriefPythonLayoutFile(t, outsideDir, "test_token.py", "def test_token():\n    pass\n")
	linkDir := filepath.Join(repoDir, "tests", "auth")
	if err := os.MkdirAll(filepath.Dir(linkDir), 0o700); err != nil {
		t.Fatalf("mkdir link parent: %v", err)
	}
	if err := os.Symlink(outsideDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	tests := brainBriefAddSiblingTestFiles(repoDir, []string{"src/auth/token.py"}, nil)
	if len(tests) != 0 {
		t.Fatalf("test under outside symlinked directory should not be surfaced: %+v", tests)
	}
}

func TestBrainBriefMirroredPythonTestSupportsIntendedCreateSource(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefPythonLayoutFile(t, repoDir, "tests/auth/test_new_token.py", "def test_new_token():\n    pass\n")
	report := brainBriefReport{
		ActionChecklist: []brainBriefAction{{File: "src/auth/new_token.py", Action: "create the token helper"}},
		LikelyEditFiles: []string{"src/auth/new_token.py"},
	}

	brainBriefPrioritizeActionTargets(repoDir, &report)
	if !slices.Equal(report.LikelyEditFiles, []string{"src/auth/new_token.py"}) {
		t.Fatalf("intended-create edit path changed: %+v", report.LikelyEditFiles)
	}
	if !slices.Equal(report.LikelyTestFiles, []string{"tests/auth/test_new_token.py"}) {
		t.Fatalf("existing validation for intended-create source was not surfaced: %+v", report.LikelyTestFiles)
	}
}

func writeBrainBriefPythonLayoutFile(t testing.TB, repoDir, path, body string) {
	t.Helper()
	absolute := filepath.Join(repoDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
