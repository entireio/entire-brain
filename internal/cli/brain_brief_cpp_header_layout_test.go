package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBrainBriefCxxHeaderLayoutGuidanceIsReachableFromRankedEvidence(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefCxxLayoutFile(t, repoDir, "src/auth/token.cpp", "bool validate_token();\n")
	writeBrainBriefCxxLayoutFile(t, repoDir, "include/auth/token.h", "bool validate_token();\n")

	task := "change the public API signature for token validation"
	report := brainBriefReport{Task: task}
	report.Semantic.Context.Symbols = append(report.Semantic.Context.Symbols, semanticRecord{
		ID: "target", Kind: "function", Name: "validate_token", FilePath: "src/auth/token.cpp",
	})
	for i := 1; i <= 7; i++ {
		path := fmt.Sprintf("src/noise/noise_%d.cpp", i)
		writeBrainBriefCxxLayoutFile(t, repoDir, path, "bool noise();\n")
		report.Semantic.Context.Symbols = append(report.Semantic.Context.Symbols, semanticRecord{
			ID: fmt.Sprintf("noise-%d", i), Kind: "function", Name: "noise", FilePath: path,
		})
	}

	report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles = brainBriefLikelyFileGroups(repoDir, report, task)
	report.LikelyTestFiles = brainBriefAddSiblingTestFiles(repoDir, report.LikelyEditFiles, report.LikelyTestFiles)
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	baseline := report
	if len(baseline.LikelyEditFiles) != 8 || baseline.LikelyEditFiles[0] != "src/auth/token.cpp" ||
		slices.Contains(baseline.LikelyEditFiles, "include/auth/token.h") {
		t.Fatalf("ranked evidence did not reproduce the live miss: %+v", baseline.LikelyEditFiles)
	}

	brainBriefAddCxxHeaderLayoutGuidance(repoDir, task, &report)
	want := []string{
		"src/auth/token.cpp", "include/auth/token.h",
		"src/noise/noise_1.cpp", "src/noise/noise_2.cpp", "src/noise/noise_3.cpp",
		"src/noise/noise_4.cpp", "src/noise/noise_5.cpp", "src/noise/noise_6.cpp",
	}
	if !slices.Equal(report.LikelyEditFiles, want) {
		t.Fatalf("header should follow its top-ranked source under the cap: got %+v want %+v", report.LikelyEditFiles, want)
	}
	if len(report.ActionChecklist) != 0 {
		t.Fatalf("live layout guidance unexpectedly depended on actions: %+v", report.ActionChecklist)
	}

	baselinePacket := renderBrainBriefCompactV3ForTest(t, baseline)
	candidatePacket := renderBrainBriefCompactV3ForTest(t, report)
	byteDelta := len(candidatePacket) - len(baselinePacket)
	proxyDelta := compactPacketByteProxy(candidatePacket) - compactPacketByteProxy(baselinePacket)
	if byteDelta > 64 || proxyDelta > 16 {
		t.Fatalf("one recalled header grew the saturated packet too much: bytes %+d proxy %+d", byteDelta, proxyDelta)
	}
	t.Logf(
		"live C++ public-header scenario: recall@8 0->1, reciprocal-rank 0->0.5; compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(baselinePacket), len(candidatePacket), byteDelta,
		compactPacketByteProxy(baselinePacket), compactPacketByteProxy(candidatePacket), proxyDelta,
	)

	emptyBaseline := baseline
	emptyBaseline.LikelyEditFiles = []string{"src/auth/token.cpp"}
	emptyBaseline.LikelyFiles = brainBriefMergeLikelyFiles(emptyBaseline.LikelyEditFiles, nil)
	emptyCandidate := emptyBaseline
	brainBriefAddCxxHeaderLayoutGuidance(repoDir, task, &emptyCandidate)
	wantEmpty := []string{"src/auth/token.cpp", "include/auth/token.h"}
	if !slices.Equal(emptyCandidate.LikelyEditFiles, wantEmpty) {
		t.Fatalf("one-source edit list did not receive the public header: got %+v want %+v", emptyCandidate.LikelyEditFiles, wantEmpty)
	}
	emptyBaselinePacket := renderBrainBriefCompactV3ForTest(t, emptyBaseline)
	emptyCandidatePacket := renderBrainBriefCompactV3ForTest(t, emptyCandidate)
	emptyByteDelta := len(emptyCandidatePacket) - len(emptyBaselinePacket)
	emptyProxyDelta := compactPacketByteProxy(emptyCandidatePacket) - compactPacketByteProxy(emptyBaselinePacket)
	if emptyByteDelta > 64 || emptyProxyDelta > 16 {
		t.Fatalf("one recalled header grew the one-source packet too much: bytes %+d proxy %+d", emptyByteDelta, emptyProxyDelta)
	}
	t.Logf(
		"C++ public-header one-source scenario: compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(emptyBaselinePacket), len(emptyCandidatePacket), emptyByteDelta,
		compactPacketByteProxy(emptyBaselinePacket), compactPacketByteProxy(emptyCandidatePacket), emptyProxyDelta,
	)
}

func TestBrainBriefCxxHeaderLayoutGuidanceKeepsNegativeAndAmbiguousTasksByteIdentical(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefCxxLayoutFile(t, repoDir, "src/auth/token.cpp", "bool validate_token();\n")
	writeBrainBriefCxxLayoutFile(t, repoDir, "include/auth/token.h", "bool validate_token();\n")
	for _, tc := range []struct{ name, task string }{
		{"public_api_unchanged", "optimize the implementation without changing its public API"},
		{"api_request_handler", "update public API request handling"},
		{"header_untouched", "fix the implementation without touching the header"},
		{"signature_unchanged", "update token validation while keeping the signature unchanged"},
		{"do_not_change_signature", "update token validation but do not change the signature"},
		{"explicit_do_not_change", "do not change the public API signature while fixing token validation"},
		{"bare_header", "update header parsing in the implementation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := brainBriefReport{
				Task:            tc.task,
				LikelyEditFiles: []string{"src/auth/token.cpp", "src/auth/cache.cpp"},
				LikelyFiles:     []string{"src/auth/token.cpp", "src/auth/cache.cpp"},
			}
			before, err := json.Marshal(report)
			if err != nil {
				t.Fatalf("marshal baseline: %v", err)
			}
			brainBriefAddCxxHeaderLayoutGuidance(repoDir, tc.task, &report)
			after, err := json.Marshal(report)
			if err != nil {
				t.Fatalf("marshal candidate: %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("negative or ambiguous task gained header noise\nbefore: %s\nafter:  %s", before, after)
			}
			if len(report.ActionChecklist) != 0 {
				t.Fatalf("negative scenario unexpectedly gained actions: %+v", report.ActionChecklist)
			}
		})
	}
}

func TestBrainBriefCxxHeaderLayoutGuidancePreservesOrderCapAndModuleBoundary(t *testing.T) {
	repoDir := t.TempDir()
	for path, body := range map[string]string{
		"packages/auth/src/token.cpp":          "bool token();\n",
		"packages/auth/include/token.hpp":      "bool token();\n",
		"packages/billing/src/invoice.cpp":     "bool invoice();\n",
		"packages/billing/include/invoice.hpp": "bool invoice();\n",
		"packages/noise/include/token.hpp":     "bool noise();\n",
	} {
		writeBrainBriefCxxLayoutFile(t, repoDir, path, body)
	}
	report := brainBriefReport{
		LikelyEditFiles: []string{
			"src/stronger.go",
			"packages/auth/src/token.cpp",
			"packages/billing/src/invoice.cpp",
			"src/noise/one.cpp", "src/noise/two.cpp", "src/noise/three.cpp", "src/noise/four.cpp", "src/noise/five.cpp",
		},
	}
	brainBriefAddCxxHeaderLayoutGuidance(repoDir, "update the token declaration", &report)
	want := []string{
		"src/stronger.go",
		"packages/auth/src/token.cpp",
		"packages/auth/include/token.hpp",
		"packages/billing/src/invoice.cpp",
		"src/noise/one.cpp", "src/noise/two.cpp", "src/noise/three.cpp", "src/noise/four.cpp",
	}
	if !slices.Equal(report.LikelyEditFiles, want) {
		t.Fatalf("rank order, cap, or module-local companion mismatch: got %+v want %+v", report.LikelyEditFiles, want)
	}
	if slices.Contains(report.LikelyEditFiles, "packages/billing/include/invoice.hpp") ||
		slices.Contains(report.LikelyEditFiles, "packages/noise/include/token.hpp") {
		t.Fatalf("more than one companion or cross-module distractor was surfaced: %+v", report.LikelyEditFiles)
	}
}

func TestBrainBriefCxxHeaderLayoutGuidanceDoesNotGuessAmbiguousExtension(t *testing.T) {
	repoDir := t.TempDir()
	for _, path := range []string{"src/auth/token.cpp", "include/auth/token.h", "include/auth/token.hpp"} {
		writeBrainBriefCxxLayoutFile(t, repoDir, path, "bool token();\n")
	}
	report := brainBriefReport{
		LikelyEditFiles: []string{"src/auth/token.cpp", "src/auth/cache.cpp"},
		LikelyFiles:     []string{"src/auth/token.cpp", "src/auth/cache.cpp"},
	}
	before, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	brainBriefAddCxxHeaderLayoutGuidance(repoDir, "change the token signature", &report)
	after, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("ambiguous .h/.hpp layout guessed a companion\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestBrainBriefCxxHeaderLayoutGuidanceRejectsUnsafeAndNonregularCandidates(t *testing.T) {
	positiveTask := "change the token signature"
	assertUnchanged := func(t *testing.T, repoDir, source string) {
		t.Helper()
		report := brainBriefReport{LikelyEditFiles: []string{source}, LikelyFiles: []string{source}}
		brainBriefAddCxxHeaderLayoutGuidance(repoDir, positiveTask, &report)
		if !slices.Equal(report.LikelyEditFiles, []string{source}) {
			t.Fatalf("unsafe or nonregular candidate was surfaced: %+v", report.LikelyEditFiles)
		}
	}

	t.Run("noncanonical_source", func(t *testing.T) {
		repoDir := t.TempDir()
		writeBrainBriefCxxLayoutFile(t, repoDir, "lib/auth/token.cpp", "bool token();\n")
		writeBrainBriefCxxLayoutFile(t, repoDir, "include/auth/token.h", "bool token();\n")
		assertUnchanged(t, repoDir, "lib/auth/token.cpp")
	})
	t.Run("symlink_file", func(t *testing.T) {
		repoDir := t.TempDir()
		writeBrainBriefCxxLayoutFile(t, repoDir, "src/auth/token.cpp", "bool token();\n")
		outside := filepath.Join(t.TempDir(), "token.h")
		if err := os.WriteFile(outside, []byte("bool outside();\n"), 0o600); err != nil {
			t.Fatalf("write outside header: %v", err)
		}
		link := filepath.Join(repoDir, "include", "auth", "token.h")
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatalf("mkdir header parent: %v", err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		assertUnchanged(t, repoDir, "src/auth/token.cpp")
	})
	t.Run("symlink_directory", func(t *testing.T) {
		repoDir := t.TempDir()
		writeBrainBriefCxxLayoutFile(t, repoDir, "src/auth/token.cpp", "bool token();\n")
		outside := t.TempDir()
		writeBrainBriefCxxLayoutFile(t, outside, "token.h", "bool outside();\n")
		if err := os.MkdirAll(filepath.Join(repoDir, "include"), 0o700); err != nil {
			t.Fatalf("mkdir include: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(repoDir, "include", "auth")); err != nil {
			t.Skipf("directory symlink unsupported: %v", err)
		}
		assertUnchanged(t, repoDir, "src/auth/token.cpp")
	})
	t.Run("directory_instead_of_header", func(t *testing.T) {
		repoDir := t.TempDir()
		writeBrainBriefCxxLayoutFile(t, repoDir, "src/auth/token.cpp", "bool token();\n")
		if err := os.MkdirAll(filepath.Join(repoDir, "include", "auth", "token.h"), 0o700); err != nil {
			t.Fatalf("mkdir header-shaped directory: %v", err)
		}
		assertUnchanged(t, repoDir, "src/auth/token.cpp")
	})
}

func TestBrainBriefCxxHeaderLayoutGuidanceComposesWithSiblingTests(t *testing.T) {
	repoDir := t.TempDir()
	for path, body := range map[string]string{
		"src/auth/token.cpp":               "bool token();\n",
		"include/auth/token.h":             "bool token();\n",
		"src/web/client.ts":                "export function client() {}\n",
		"src/web/__tests__/client.test.ts": "test('client', () => {})\n",
	} {
		writeBrainBriefCxxLayoutFile(t, repoDir, path, body)
	}
	report := brainBriefReport{LikelyEditFiles: []string{"src/auth/token.cpp", "src/web/client.ts"}}
	report.LikelyTestFiles = brainBriefAddSiblingTestFiles(repoDir, report.LikelyEditFiles, nil)
	brainBriefAddCxxHeaderLayoutGuidance(repoDir, "change the token declaration", &report)
	wantEdits := []string{"src/auth/token.cpp", "include/auth/token.h", "src/web/client.ts"}
	wantTests := []string{"src/web/__tests__/client.test.ts"}
	wantAll := append(append([]string{}, wantEdits...), wantTests...)
	if !slices.Equal(report.LikelyEditFiles, wantEdits) || !slices.Equal(report.LikelyTestFiles, wantTests) ||
		!slices.Equal(report.LikelyFiles, wantAll) {
		t.Fatalf("combined layout guidance mismatch: edits=%+v tests=%+v all=%+v", report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles)
	}
	if len(report.ActionChecklist) != 0 {
		t.Fatalf("combined layout guidance unexpectedly needed actions: %+v", report.ActionChecklist)
	}
}

func TestBrainBriefCxxHeaderCandidatesStayCanonical(t *testing.T) {
	for _, source := range []string{"src/auth/token.c", "src/auth/token.cc", "src/auth/token.cpp"} {
		candidates := brainBriefCxxHeaderCandidates(source)
		if !slices.Equal(candidates, []string{"include/auth/token.h", "include/auth/token.hpp"}) {
			t.Fatalf("canonical candidates for %s = %+v", source, candidates)
		}
	}
	for _, source := range []string{"lib/auth/token.cpp", "foosrc/auth/token.cpp", "src/auth/token.cxx", "src/auth/token.rs"} {
		if candidates := brainBriefCxxHeaderCandidates(source); len(candidates) != 0 {
			t.Fatalf("noncanonical source %s guessed headers: %+v", source, candidates)
		}
	}
}

var benchmarkBrainBriefCxxHeaderReport brainBriefReport

func BenchmarkBrainBriefCxxHeaderLayoutGuidance(b *testing.B) {
	repoDir := b.TempDir()
	writeBrainBriefCxxLayoutFile(b, repoDir, "src/auth/token.cpp", "bool token();\n")
	writeBrainBriefCxxLayoutFile(b, repoDir, "include/auth/token.h", "bool token();\n")
	base := brainBriefReport{
		LikelyEditFiles: []string{"src/auth/token.cpp", "src/auth/cache.cpp"},
		LikelyFiles:     []string{"src/auth/token.cpp", "src/auth/cache.cpp"},
	}
	b.Run("implementation_only_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			report := base
			brainBriefAddCxxHeaderLayoutGuidance(repoDir, "optimize token validation", &report)
			benchmarkBrainBriefCxxHeaderReport = report
		}
	})
	b.Run("public_header_candidate", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			report := base
			brainBriefAddCxxHeaderLayoutGuidance(repoDir, "change the token signature", &report)
			benchmarkBrainBriefCxxHeaderReport = report
		}
	})
}

func writeBrainBriefCxxLayoutFile(t testing.TB, repoDir, path, body string) {
	t.Helper()
	absolute := filepath.Join(repoDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
