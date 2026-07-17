package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBrainBriefActionTargetsAddRelevantGoPrefixTest(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/history.go", "package cli\n")
	for _, path := range []string{
		"internal/cli/history_cache_test.go",
		"internal/cli/history_cache_v6_test.go",
		"internal/cli/history_fts_test.go",
		"internal/cli/history_scan_test.go",
		"internal/cli/history_single_read_test.go",
		"internal/cli/history_vec_test.go",
	} {
		writeBrainBriefGoLayoutFile(t, repoDir, path, "package cli\n")
	}
	for i := 1; i <= 6; i++ {
		writeBrainBriefGoLayoutFile(t, repoDir, fmt.Sprintf("internal/cli/distractor_%d_test.go", i), "package cli\n")
	}

	report := brainBriefReport{
		Task: "repair history cache reuse and invalidation",
		ActionChecklist: []brainBriefAction{{
			File:   "internal/cli/history.go",
			Symbol: "loadHistoryScanCache",
			Action: "preserve the scan cache reuse contract",
		}},
		LikelyEditFiles: []string{"internal/cli/history.go"},
		LikelyTestFiles: []string{
			"internal/cli/distractor_1_test.go", "internal/cli/distractor_2_test.go",
			"internal/cli/distractor_3_test.go", "internal/cli/distractor_4_test.go",
			"internal/cli/distractor_5_test.go", "internal/cli/distractor_6_test.go",
		},
	}
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	baseline := report
	if slices.Contains(baseline.LikelyTestFiles, "internal/cli/history_cache_test.go") {
		t.Fatal("baseline unexpectedly contains the relevant prefixed Go test")
	}

	brainBriefPrioritizeActionTargets(repoDir, &report)
	wantTests := []string{
		"internal/cli/history_cache_test.go",
		"internal/cli/distractor_1_test.go", "internal/cli/distractor_2_test.go",
		"internal/cli/distractor_3_test.go", "internal/cli/distractor_4_test.go",
		"internal/cli/distractor_5_test.go",
	}
	if !slices.Equal(report.LikelyTestFiles, wantTests) {
		t.Fatalf("relevant prefixed action test should rank first under the six-file cap: got %+v want %+v", report.LikelyTestFiles, wantTests)
	}
	if len(report.LikelyFiles) != 7 || report.LikelyFiles[1] != "internal/cli/history_cache_test.go" {
		t.Fatalf("combined packet lost the prefixed action-test priority: %+v", report.LikelyFiles)
	}

	baselinePacket := renderBrainBriefCompactV3ForTest(t, baseline)
	candidatePacket := renderBrainBriefCompactV3ForTest(t, report)
	byteDelta := len(candidatePacket) - len(baselinePacket)
	proxyDelta := compactPacketByteProxy(candidatePacket) - compactPacketByteProxy(baselinePacket)
	if byteDelta > 64 || proxyDelta > 16 {
		t.Fatalf("one recalled Go test grew the saturated packet too much: bytes %+d proxy %+d", byteDelta, proxyDelta)
	}
	t.Logf(
		"prefixed-Go saturated scenario: recall@6 0->1, reciprocal-rank 0->1; compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(baselinePacket), len(candidatePacket), byteDelta,
		compactPacketByteProxy(baselinePacket), compactPacketByteProxy(candidatePacket), proxyDelta,
	)

	emptyBaseline := baseline
	emptyBaseline.LikelyTestFiles = nil
	emptyBaseline.LikelyFiles = brainBriefMergeLikelyFiles(emptyBaseline.LikelyEditFiles, nil)
	emptyCandidate := emptyBaseline
	brainBriefPrioritizeActionTargets(repoDir, &emptyCandidate)
	if !slices.Equal(emptyCandidate.LikelyTestFiles, []string{"internal/cli/history_cache_test.go"}) {
		t.Fatalf("empty test section did not receive the relevant Go test: %+v", emptyCandidate.LikelyTestFiles)
	}
	emptyBaselinePacket := renderBrainBriefCompactV3ForTest(t, emptyBaseline)
	emptyCandidatePacket := renderBrainBriefCompactV3ForTest(t, emptyCandidate)
	emptyByteDelta := len(emptyCandidatePacket) - len(emptyBaselinePacket)
	emptyProxyDelta := compactPacketByteProxy(emptyCandidatePacket) - compactPacketByteProxy(emptyBaselinePacket)
	if emptyByteDelta > 64 || emptyProxyDelta > 16 {
		t.Fatalf("one recalled Go test grew the empty packet too much: bytes %+d proxy %+d", emptyByteDelta, emptyProxyDelta)
	}
	t.Logf(
		"prefixed-Go empty scenario: compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
		len(emptyBaselinePacket), len(emptyCandidatePacket), emptyByteDelta,
		compactPacketByteProxy(emptyBaselinePacket), compactPacketByteProxy(emptyCandidatePacket), emptyProxyDelta,
	)
}

func TestBrainBriefGoPrefixTestRankingUsesTaskActionAndSymbolTerms(t *testing.T) {
	repoDir := t.TempDir()
	for _, path := range []string{
		"internal/cli/history_cache_test.go",
		"internal/cli/history_cache_v6_test.go",
		"internal/cli/history_scan_test.go",
		"internal/cli/history_single_read_test.go",
	} {
		writeBrainBriefGoLayoutFile(t, repoDir, path, "package cli\n")
	}

	tests := []struct {
		name   string
		task   string
		symbol string
		action string
		want   string
	}{
		{
			name:   "task term outranks incidental symbol term",
			task:   "repair history cache reuse",
			symbol: "scanHistoryFile",
			action: "keep the cached records stable",
			want:   "internal/cli/history_cache_test.go",
		},
		{
			name:   "specific multi-term variant wins",
			task:   "preserve history single read rebuild",
			symbol: "buildHistoryIndex",
			action: "avoid reading a transcript twice",
			want:   "internal/cli/history_single_read_test.go",
		},
		{
			name:   "camel case action symbol contributes terms",
			task:   "repair transcript behavior",
			symbol: "loadHistoryCache",
			action: "keep invalidation stable",
			want:   "internal/cli/history_cache_test.go",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := brainBriefReport{
				Task: tt.task,
				ActionChecklist: []brainBriefAction{{
					File: "internal/cli/history.go", Symbol: tt.symbol, Action: tt.action,
				}},
				LikelyEditFiles: []string{"internal/cli/history.go"},
			}
			brainBriefPrioritizeActionTargets(repoDir, &report)
			if !slices.Equal(report.LikelyTestFiles, []string{tt.want}) {
				t.Fatalf("ranked Go fallback mismatch: got %+v want %s", report.LikelyTestFiles, tt.want)
			}
		})
	}
}

func TestBrainBriefGoPrefixTestFallbackStaysBoundedAndPreservesDirectSibling(t *testing.T) {
	t.Run("direct sibling wins", func(t *testing.T) {
		repoDir := t.TempDir()
		for _, path := range []string{"internal/cli/history_test.go", "internal/cli/history_cache_test.go"} {
			writeBrainBriefGoLayoutFile(t, repoDir, path, "package cli\n")
		}
		report := brainBriefReport{
			Task:            "repair history cache",
			ActionChecklist: []brainBriefAction{{File: "internal/cli/history.go", Action: "repair cache reuse"}},
			LikelyEditFiles: []string{"internal/cli/history.go"},
		}
		brainBriefPrioritizeActionTargets(repoDir, &report)
		if !slices.Equal(report.LikelyTestFiles, []string{"internal/cli/history_test.go"}) {
			t.Fatalf("direct Go sibling lost precedence: %+v", report.LikelyTestFiles)
		}
	})

	t.Run("distractor heavy directory emits one ranked test", func(t *testing.T) {
		repoDir := t.TempDir()
		writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/history_cache_test.go", "package cli\n")
		for i := 0; i < 180; i++ {
			writeBrainBriefGoLayoutFile(t, repoDir, fmt.Sprintf("internal/cli/history_noise_%03d_test.go", i), "package cli\n")
		}
		report := brainBriefReport{
			Task:            "repair history cache reuse",
			ActionChecklist: []brainBriefAction{{File: "internal/cli/history.go", Action: "repair cache reuse"}},
			LikelyEditFiles: []string{"internal/cli/history.go"},
		}
		brainBriefPrioritizeActionTargets(repoDir, &report)
		if !slices.Equal(report.LikelyTestFiles, []string{"internal/cli/history_cache_test.go"}) {
			t.Fatalf("distractor-heavy directory leaked prefix tests: %+v", report.LikelyTestFiles)
		}
	})

	t.Run("candidate probes are hard capped", func(t *testing.T) {
		suffixes := brainBriefGoActionTestSuffixes(
			"alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango",
			[]brainBriefAction{{File: "internal/cli/history.go", Action: "uniform victor whiskey xray yankee zulu"}},
		)["internal/cli/history.go"]
		if len(suffixes) != brainBriefGoTestCandidateLimit {
			t.Fatalf("candidate cap changed: got %d want %d (%+v)", len(suffixes), brainBriefGoTestCandidateLimit, suffixes)
		}
		seen := make(map[string]bool, len(suffixes))
		for _, suffix := range suffixes {
			if seen[suffix] {
				t.Fatalf("candidate derivation emitted a duplicate: %q in %+v", suffix, suffixes)
			}
			if strings.ContainsAny(suffix, `/\.`) || strings.Contains(suffix, "..") {
				t.Fatalf("candidate suffix contains a path separator or traversal: %q", suffix)
			}
			seen[suffix] = true
		}
		unsafeInput := brainBriefGoActionTestSuffixes(
			`../../cache ..\single/read`,
			[]brainBriefAction{{File: "internal/cli/history.go", Action: `preserve ../../cache`}},
		)["internal/cli/history.go"]
		for _, suffix := range unsafeInput {
			if strings.ContainsAny(suffix, `/\.`) || strings.Contains(suffix, "..") {
				t.Fatalf("untrusted context escaped the filename tokenizer: %q in %+v", suffix, unsafeInput)
			}
		}
		tooLong := brainBriefGoActionTestSuffixes(
			strings.Repeat("x", 81),
			[]brainBriefAction{{File: "internal/cli/history.go"}},
		)["internal/cli/history.go"]
		if len(tooLong) != 0 {
			t.Fatalf("oversized filename suffix escaped the bound: %+v", tooLong)
		}
	})
}

func TestBrainBriefGoPrefixTestFallbackRequiresSafeRegularRepoFile(t *testing.T) {
	t.Run("symlink file", func(t *testing.T) {
		repoDir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "history_cache_test.go")
		if err := os.WriteFile(outside, []byte("package cli\n"), 0o600); err != nil {
			t.Fatalf("write outside test: %v", err)
		}
		link := filepath.Join(repoDir, "internal", "cli", "history_cache_test.go")
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatalf("mkdir link parent: %v", err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		assertNoBrainBriefGoPrefixTest(t, repoDir, "internal/cli/history.go")
	})

	t.Run("symlink package directory", func(t *testing.T) {
		repoDir := t.TempDir()
		outside := t.TempDir()
		writeBrainBriefGoLayoutFile(t, outside, "history_cache_test.go", "package cli\n")
		link := filepath.Join(repoDir, "internal", "cli")
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatalf("mkdir link parent: %v", err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		assertNoBrainBriefGoPrefixTest(t, repoDir, "internal/cli/history.go")
	})

	t.Run("socket file", func(t *testing.T) {
		repoDir, err := os.MkdirTemp("/tmp", "brain-go-test-")
		if err != nil {
			t.Fatalf("create short socket root: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
		socketPath := filepath.Join(repoDir, "internal", "cli", "history_cache_test.go")
		if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
			t.Fatalf("mkdir socket parent: %v", err)
		}
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Skipf("unix sockets unsupported: %v", err)
		}
		defer listener.Close()
		assertNoBrainBriefGoPrefixTest(t, repoDir, "internal/cli/history.go")
	})

	t.Run("unsafe source path", func(t *testing.T) {
		repoDir := t.TempDir()
		writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/history_cache_test.go", "package cli\n")
		assertNoBrainBriefGoPrefixTest(t, repoDir, "../../internal/cli/history.go")
	})
}

func BenchmarkBrainBriefGoPrefixTestPostprocess(b *testing.B) {
	repoDir := b.TempDir()
	for _, path := range []string{
		"internal/cli/history_cache_test.go",
		"internal/cli/history_cache_v6_test.go",
		"internal/cli/history_fts_test.go",
		"internal/cli/history_scan_test.go",
		"internal/cli/history_single_read_test.go",
		"internal/cli/history_vec_test.go",
	} {
		writeBrainBriefGoLayoutFile(b, repoDir, path, "package cli\n")
	}
	report := brainBriefReport{
		Task: "repair history cache reuse and invalidation",
		ActionChecklist: []brainBriefAction{{
			File: "internal/cli/history.go", Symbol: "loadHistoryScanCache", Action: "preserve scan cache reuse",
		}},
		LikelyEditFiles: []string{"internal/cli/history.go"},
	}
	actionFiles := brainBriefActionFiles(report.ActionChecklist, report.LikelyEditFiles)
	b.Run("direct_only_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFiles(repoDir, actionFiles, nil)
		}
	})
	b.Run("history_cache_scenario", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFilesWithGoSuffixes(
				repoDir,
				actionFiles,
				nil,
				brainBriefGoActionTestSuffixes(report.Task, report.ActionChecklist),
			)
		}
	})
	firstMatch := brainBriefReport{
		Task:            "cache",
		ActionChecklist: []brainBriefAction{{File: "internal/cli/history.go"}},
		LikelyEditFiles: []string{"internal/cli/history.go"},
	}
	b.Run("history_cache_first_match", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFilesWithGoSuffixes(
				repoDir,
				actionFiles,
				nil,
				brainBriefGoActionTestSuffixes(firstMatch.Task, firstMatch.ActionChecklist),
			)
		}
	})
	worstCase := brainBriefReport{
		Task: "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango",
		ActionChecklist: []brainBriefAction{{
			File: "internal/cli/history.go", Action: "uniform victor whiskey xray yankee zulu",
		}},
	}
	if got := len(brainBriefGoActionTestSuffixes(worstCase.Task, worstCase.ActionChecklist)["internal/cli/history.go"]); got != brainBriefGoTestCandidateLimit {
		b.Fatalf("worst-case fixture produced %d candidates, want %d", got, brainBriefGoTestCandidateLimit)
	}
	b.Run("bounded_24_miss_worst_case", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFilesWithGoSuffixes(
				repoDir,
				actionFiles,
				nil,
				brainBriefGoActionTestSuffixes(worstCase.Task, worstCase.ActionChecklist),
			)
		}
	})
}

func assertNoBrainBriefGoPrefixTest(t *testing.T, repoDir, source string) {
	t.Helper()
	report := brainBriefReport{
		Task:            "repair history cache",
		ActionChecklist: []brainBriefAction{{File: source, Action: "repair cache reuse"}},
		LikelyEditFiles: []string{source},
	}
	brainBriefPrioritizeActionTargets(repoDir, &report)
	if len(report.LikelyTestFiles) != 0 {
		t.Fatalf("unsafe Go fallback surfaced a test: %+v", report.LikelyTestFiles)
	}
}

func writeBrainBriefGoLayoutFile(t testing.TB, repoDir, path, body string) {
	t.Helper()
	absolute := filepath.Join(repoDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(absolute, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
