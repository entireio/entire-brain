package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
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

	brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
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
	brainBriefApplyLayoutGuidance(repoDir, emptyCandidate.Task, &emptyCandidate)
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

func TestBrainBriefLayoutGuidanceRunsWithoutActionChecklist(t *testing.T) {
	tests := []struct {
		name   string
		task   string
		source string
		target string
	}{
		{
			name:   "Go focused same-package test",
			task:   "repair history cache reuse and invalidation",
			source: "internal/cli/history.go",
			target: "internal/cli/history_cache_test.go",
		},
		{
			name:   "nested JavaScript test",
			task:   "repair token validation",
			source: "src/auth/token.ts",
			target: "src/auth/__tests__/token.test.ts",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			writeBrainBriefGoLayoutFile(t, repoDir, tt.source, "package fixture\n")
			writeBrainBriefGoLayoutFile(t, repoDir, tt.target, "package fixture\n")

			report := brainBriefReport{Task: tt.task}
			report.Semantic.Context.Symbols = []semanticRecord{{FilePath: tt.source}}
			for i := 1; i <= 6; i++ {
				path := fmt.Sprintf("tests/distractor_%d_test.go", i)
				writeBrainBriefGoLayoutFile(t, repoDir, path, "package tests\n")
				report.Semantic.Tests.Roots = append(report.Semantic.Tests.Roots, semanticRecord{FilePath: path})
			}

			report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles = brainBriefLikelyFileGroups(repoDir, report, tt.task)
			if !slices.Equal(report.LikelyEditFiles, []string{tt.source}) {
				t.Fatalf("evidence did not produce the expected ranked edit: %+v", report.LikelyEditFiles)
			}
			if len(report.LikelyTestFiles) != 6 || slices.Contains(report.LikelyTestFiles, tt.target) {
				t.Fatalf("saturated baseline should miss the exact layout test: %+v", report.LikelyTestFiles)
			}
			if actions := brainBriefActionChecklist(repoDir, report, tt.task); len(actions) != 0 {
				t.Fatalf("fixture unexpectedly depends on an action scanner: %+v", actions)
			}
			if len(report.ActionChecklist) != 0 {
				t.Fatalf("action checklist should still be empty before live layout synthesis: %+v", report.ActionChecklist)
			}

			baseline := report
			brainBriefApplyLayoutGuidance(repoDir, tt.task, &report)
			wantTests := append([]string{tt.target}, baseline.LikelyTestFiles[:5]...)
			if !slices.Equal(report.LikelyTestFiles, wantTests) {
				t.Fatalf("live exact layout test should rank first: got %+v want %+v", report.LikelyTestFiles, wantTests)
			}
			if len(report.ActionChecklist) != 0 {
				t.Fatalf("layout synthesis manufactured an action checklist: %+v", report.ActionChecklist)
			}
			if len(report.LikelyFiles) != 7 || report.LikelyFiles[1] != tt.target {
				t.Fatalf("combined likely files lost live layout priority: %+v", report.LikelyFiles)
			}

			baselinePacket := renderBrainBriefCompactV3ForTest(t, baseline)
			candidatePacket := renderBrainBriefCompactV3ForTest(t, report)
			t.Logf(
				"live %s: recall@6 0->1, reciprocal-rank 0->1; compact_v3 bytes %d->%d (%+d), proxy %d->%d (%+d)",
				tt.name,
				len(baselinePacket), len(candidatePacket), len(candidatePacket)-len(baselinePacket),
				compactPacketByteProxy(baselinePacket), compactPacketByteProxy(candidatePacket),
				compactPacketByteProxy(candidatePacket)-compactPacketByteProxy(baselinePacket),
			)
		})
	}
}

func TestBrainBriefLiveLayoutGuidanceEmitsOneExactTestPerSource(t *testing.T) {
	repoDir := t.TempDir()
	for _, path := range []string{"src/auth/token.test.ts", "src/auth/token.spec.ts"} {
		writeBrainBriefGoLayoutFile(t, repoDir, path, "test('token', () => {})\n")
	}
	report := brainBriefReport{Task: "repair token validation", LikelyEditFiles: []string{"src/auth/token.ts"}}
	brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
	want := []string{"src/auth/token.test.ts"}
	if !slices.Equal(report.LikelyTestFiles, want) {
		t.Fatalf("one source crowded the live packet with naming variants: got %+v want %+v", report.LikelyTestFiles, want)
	}
}

func TestBrainBriefLiveLayoutGuidanceNonmatchIsByteIdentical(t *testing.T) {
	repoDir := t.TempDir()
	report := brainBriefReport{
		Task:            "render dashboard summary",
		LikelyEditFiles: []string{"internal/cli/history.go"},
		LikelyTestFiles: []string{"tests/existing_test.go"},
	}
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	before := renderBrainBriefCompactV3ForTest(t, report)
	brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
	after := renderBrainBriefCompactV3ForTest(t, report)
	if after != before {
		t.Fatalf("nonmatching live layout changed the packet\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestBrainBriefGoPrefixTestRankingUsesTaskTerms(t *testing.T) {
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
		name string
		task string
		want string
	}{
		{
			name: "focused cache term",
			task: "repair history cache reuse",
			want: "internal/cli/history_cache_test.go",
		},
		{
			name: "specific multi-term variant wins",
			task: "preserve history single read rebuild",
			want: "internal/cli/history_single_read_test.go",
		},
		{
			name: "alternate focused term",
			task: "repair history scan behavior",
			want: "internal/cli/history_scan_test.go",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := brainBriefReport{
				Task:            tt.task,
				ActionChecklist: []brainBriefAction{{File: "internal/cli/history.go"}},
				LikelyEditFiles: []string{"internal/cli/history.go"},
			}
			brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
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
		brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
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
		brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
		brainBriefPrioritizeActionTargets(repoDir, &report)
		if !slices.Equal(report.LikelyTestFiles, []string{"internal/cli/history_cache_test.go"}) {
			t.Fatalf("distractor-heavy directory leaked prefix tests: %+v", report.LikelyTestFiles)
		}
	})

	t.Run("candidate probes are hard capped", func(t *testing.T) {
		suffixes := brainBriefGoTaskTestSuffixes(
			"alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango",
			[]string{"internal/cli/history.go"},
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
		unsafeInput := brainBriefGoTaskTestSuffixes(
			`../../cache ..\single/read`,
			[]string{"internal/cli/history.go"},
		)["internal/cli/history.go"]
		for _, suffix := range unsafeInput {
			if strings.ContainsAny(suffix, `/\.`) || strings.Contains(suffix, "..") {
				t.Fatalf("untrusted context escaped the filename tokenizer: %q in %+v", suffix, unsafeInput)
			}
		}
		tooLong := brainBriefGoTaskTestSuffixes(
			strings.Repeat("x", 81),
			[]string{"internal/cli/history.go"},
		)["internal/cli/history.go"]
		if len(tooLong) != 0 {
			t.Fatalf("oversized filename suffix escaped the bound: %+v", tooLong)
		}

		repoDir := t.TempDir()
		writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/other_alpha_bravo_test.go", "package cli\n")
		report := brainBriefReport{
			Task:            "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango",
			LikelyEditFiles: []string{"internal/cli/history.go", "internal/cli/other.go"},
		}
		brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
		if len(report.LikelyTestFiles) != 0 {
			t.Fatalf("lower-ranked source escaped the global %d-probe budget: %+v", brainBriefGoTestCandidateLimit, report.LikelyTestFiles)
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
		if runtime.GOOS == "windows" {
			t.Skip("Unix-domain socket fixture is not portable to Windows")
		}
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
				brainBriefGoTaskTestSuffixes(report.Task, actionFiles),
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
				brainBriefGoTaskTestSuffixes(firstMatch.Task, actionFiles),
			)
		}
	})
	worstCase := brainBriefReport{
		Task: "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango",
		ActionChecklist: []brainBriefAction{{
			File: "internal/cli/history.go", Action: "uniform victor whiskey xray yankee zulu",
		}},
	}
	if got := len(brainBriefGoTaskTestSuffixes(worstCase.Task, actionFiles)["internal/cli/history.go"]); got != brainBriefGoTestCandidateLimit {
		b.Fatalf("worst-case fixture produced %d candidates, want %d", got, brainBriefGoTestCandidateLimit)
	}
	b.Run("bounded_24_miss_worst_case", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddSiblingTestFilesWithGoSuffixes(
				repoDir,
				actionFiles,
				nil,
				brainBriefGoTaskTestSuffixes(worstCase.Task, actionFiles),
			)
		}
	})
}

func BenchmarkBrainBriefLiveLayoutGuidance(b *testing.B) {
	b.Run("nested_js_saturated", func(b *testing.B) {
		repoDir := b.TempDir()
		writeBrainBriefGoLayoutFile(b, repoDir, "src/auth/__tests__/token.test.ts", "test('token', () => {})\n")
		report := brainBriefReport{Task: "repair token validation", LikelyEditFiles: []string{"src/auth/token.ts"}}
		for i := 1; i <= 6; i++ {
			report.LikelyTestFiles = append(report.LikelyTestFiles, fmt.Sprintf("tests/distractor_%d_test.go", i))
		}
		report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)

		b.Run("append_only_baseline", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				candidate := report
				candidate.LikelyTestFiles = brainBriefAddSiblingTestFiles(repoDir, candidate.LikelyEditFiles, candidate.LikelyTestFiles)
				candidate.LikelyFiles = brainBriefMergeLikelyFiles(candidate.LikelyEditFiles, candidate.LikelyTestFiles)
				benchmarkBrainBriefTestGuidanceFiles = candidate.LikelyTestFiles
			}
		})
		b.Run("prioritized_live_candidate", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				candidate := report
				brainBriefApplyLayoutGuidance(repoDir, candidate.Task, &candidate)
				benchmarkBrainBriefTestGuidanceFiles = candidate.LikelyTestFiles
			}
		})
	})

	b.Run("eight_go_edits_no_match", func(b *testing.B) {
		repoDir := b.TempDir()
		report := brainBriefReport{
			Task: "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango",
		}
		for i := 0; i < 8; i++ {
			path := fmt.Sprintf("internal/cli/component_%d.go", i)
			writeBrainBriefGoLayoutFile(b, repoDir, path, "package cli\n")
			report.LikelyEditFiles = append(report.LikelyEditFiles, path)
		}
		for i := 1; i <= 6; i++ {
			report.LikelyTestFiles = append(report.LikelyTestFiles, fmt.Sprintf("tests/distractor_%d_test.go", i))
		}
		report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)

		b.Run("append_only_baseline", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				candidate := report
				candidate.LikelyTestFiles = brainBriefAddSiblingTestFiles(repoDir, candidate.LikelyEditFiles, candidate.LikelyTestFiles)
				candidate.LikelyFiles = brainBriefMergeLikelyFiles(candidate.LikelyEditFiles, candidate.LikelyTestFiles)
				benchmarkBrainBriefTestGuidanceFiles = candidate.LikelyTestFiles
			}
		})
		b.Run("prioritized_live_candidate", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				candidate := report
				brainBriefApplyLayoutGuidance(repoDir, candidate.Task, &candidate)
				benchmarkBrainBriefTestGuidanceFiles = candidate.LikelyTestFiles
			}
		})
	})
}

func assertNoBrainBriefGoPrefixTest(t *testing.T, repoDir, source string) {
	t.Helper()
	report := brainBriefReport{
		Task:            "repair history cache",
		LikelyEditFiles: []string{source},
	}
	brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
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
