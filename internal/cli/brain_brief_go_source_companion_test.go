package cli

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestBrainBriefLikelyFilesAddGoTestSourceCompanionsFromEvidence(t *testing.T) {
	tests := []struct {
		name       string
		task       string
		testFile   string
		sourceFile string
		symbol     string
	}{
		{
			name:       "MCP malformed frame",
			task:       "keep the MCP server synchronized after malformed JSON and oversized Content-Length frames",
			testFile:   "internal/cli/mcp_test.go",
			sourceFile: "internal/cli/mcp.go",
			symbol:     "TestMCPRecoversFromMalformedJSONLineFrame",
		},
		{
			name:       "workspace compose graph",
			task:       "connect Docker Compose depends_on services across repositories",
			testFile:   "internal/cli/workspace_test.go",
			sourceFile: "internal/cli/workspace.go",
			symbol:     "TestWorkspaceGraphMatchesComposeServiceResourceCandidates",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			writeBrainBriefGoLayoutFile(t, repoDir, tt.sourceFile, "package cli\n")
			writeBrainBriefGoLayoutFile(t, repoDir, tt.testFile, "package cli\n")
			report := brainBriefReport{
				Task: tt.task,
				Semantic: brainBriefSemantic{Context: semanticContextResult{Symbols: []semanticRecord{{
					Name: tt.symbol, FilePath: tt.testFile,
				}}}},
			}
			if len(report.ActionChecklist) != 0 {
				t.Fatalf("flagship evidence fixture unexpectedly has actions: %+v", report.ActionChecklist)
			}
			report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles = brainBriefLikelyFileGroups(repoDir, report, tt.task)
			if len(report.LikelyEditFiles) != 0 || !slices.Equal(report.LikelyTestFiles, []string{tt.testFile}) {
				t.Fatalf("test-only evidence baseline changed: edits=%+v tests=%+v", report.LikelyEditFiles, report.LikelyTestFiles)
			}
			baseline := report

			brainBriefApplyLayoutGuidance(repoDir, tt.task, &report)
			if !slices.Equal(report.LikelyEditFiles, []string{tt.sourceFile}) {
				t.Fatalf("source companion did not rank first: %+v", report.LikelyEditFiles)
			}
			if !slices.Equal(report.LikelyTestFiles, []string{tt.testFile}) {
				t.Fatalf("test ranking changed: %+v", report.LikelyTestFiles)
			}
			if !slices.Equal(report.LikelyFiles, []string{tt.sourceFile, tt.testFile}) {
				t.Fatalf("combined ordering changed: %+v", report.LikelyFiles)
			}

			baselinePacket := renderBrainBriefCompactV3ForTest(t, baseline)
			candidatePacket := renderBrainBriefCompactV3ForTest(t, report)
			t.Logf(
				"%s: source recall@8 0->1, rank absent->1; compact_v3 bytes %d->%d (%+d), ceil(bytes/4) proxy %d->%d (%+d)",
				tt.name,
				len(baselinePacket), len(candidatePacket), len(candidatePacket)-len(baselinePacket),
				compactPacketByteProxy(baselinePacket), compactPacketByteProxy(candidatePacket),
				compactPacketByteProxy(candidatePacket)-compactPacketByteProxy(baselinePacket),
			)
		})
	}
}

func TestBrainBriefGoTestSourceCompanionsPreserveCapsAndOrdering(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/mcp.go", "package cli\n")
	testFiles := []string{
		"internal/cli/mcp_test.go", "test/two_test.go", "test/three_test.go",
		"test/four_test.go", "test/five_test.go", "test/six_test.go",
	}

	t.Run("saturated edit cap is unchanged", func(t *testing.T) {
		editFiles := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go", "h.go"}
		got := brainBriefAddGoTestSourceCompanions(repoDir, editFiles, testFiles)
		if !slices.Equal(got, editFiles) {
			t.Fatalf("saturated edit ordering changed: got %+v want %+v", got, editFiles)
		}
	})

	t.Run("one open edit slot appends and combined cap holds", func(t *testing.T) {
		editFiles := []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go"}
		got := brainBriefAddGoTestSourceCompanions(repoDir, editFiles, testFiles)
		wantEdits := append(append([]string(nil), editFiles...), "internal/cli/mcp.go")
		if !slices.Equal(got, wantEdits) {
			t.Fatalf("edit order changed: got %+v want %+v", got, wantEdits)
		}
		combined := brainBriefMergeLikelyFiles(got, testFiles)
		if len(combined) != 12 || !slices.Equal(combined[:8], wantEdits) || !slices.Equal(combined[8:], testFiles[:4]) {
			t.Fatalf("combined cap/order changed: %+v", combined)
		}
	})

	t.Run("only the first six ranked tests are probed", func(t *testing.T) {
		outsideBound := []string{
			"README.md", "docs/one.md", "docs/two.md", "docs/three.md", "docs/four.md", "docs/five.md",
			"internal/cli/mcp_test.go",
		}
		if got := brainBriefAddGoTestSourceCompanions(repoDir, nil, outsideBound); len(got) != 0 {
			t.Fatalf("seventh test escaped global probe bound: %+v", got)
		}
	})
}

func TestBrainBriefGoTestSourceCandidateRequiresExactName(t *testing.T) {
	positives := map[string]string{
		"internal/cli/mcp_test.go":        "internal/cli/mcp.go",
		"internal/cli/my_feature_test.go": "internal/cli/my_feature.go",
	}
	for testFile, want := range positives {
		got, ok := brainBriefGoTestSourceCandidate(testFile)
		if !ok || got != want {
			t.Errorf("candidate(%q) = %q, %v; want %q, true", testFile, got, ok, want)
		}
	}
	for _, testFile := range []string{
		"internal/cli/mcp_tests.go",
		"internal/cli/mcp_test.ts",
		"internal/cli/mcp_test.go.bak",
		"internal/cli/_test.go",
		"internal/cli/_mcp_test.go",
		"internal/cli/mcp.go_test.go",
		"internal/cli/mcp_test_test.go",
		"../../internal/cli/mcp_test.go",
		".git/internal/cli/mcp_test.go",
		"build/internal/cli/mcp_test.go",
	} {
		if got, ok := brainBriefGoTestSourceCandidate(testFile); ok {
			t.Errorf("intended negative %q produced %q", testFile, got)
		}
	}
}

func TestBrainBriefGoTestSourceCompanionsRequireSafeRegularSource(t *testing.T) {
	t.Run("source file symlink", func(t *testing.T) {
		repoDir := t.TempDir()
		writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/mcp_test.go", "package cli\n")
		outside := filepath.Join(t.TempDir(), "mcp.go")
		if err := os.WriteFile(outside, []byte("package cli\n"), 0o600); err != nil {
			t.Fatalf("write outside source: %v", err)
		}
		link := filepath.Join(repoDir, "internal", "cli", "mcp.go")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if got := brainBriefAddGoTestSourceCompanions(repoDir, nil, []string{"internal/cli/mcp_test.go"}); len(got) != 0 {
			t.Fatalf("outside source symlink surfaced: %+v", got)
		}
	})

	t.Run("source directory symlink", func(t *testing.T) {
		repoDir := t.TempDir()
		outside := t.TempDir()
		writeBrainBriefGoLayoutFile(t, outside, "mcp.go", "package cli\n")
		link := filepath.Join(repoDir, "internal", "cli")
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatalf("mkdir link parent: %v", err)
		}
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if got := brainBriefAddGoTestSourceCompanions(repoDir, nil, []string{"internal/cli/mcp_test.go"}); len(got) != 0 {
			t.Fatalf("source through directory symlink surfaced: %+v", got)
		}
	})

	t.Run("source socket", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix-domain socket fixture is not portable to Windows")
		}
		repoDir, err := os.MkdirTemp("/tmp", "brain-go-source-")
		if err != nil {
			t.Fatalf("create short socket root: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(repoDir) })
		writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/mcp_test.go", "package cli\n")
		socketPath := filepath.Join(repoDir, "internal", "cli", "mcp.go")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Skipf("unix sockets unsupported: %v", err)
		}
		defer listener.Close()
		if got := brainBriefAddGoTestSourceCompanions(repoDir, nil, []string{"internal/cli/mcp_test.go"}); len(got) != 0 {
			t.Fatalf("source socket surfaced: %+v", got)
		}
	})
}

func TestBrainBriefGoTestSourceCompanionsNoMatchIsByteIdentical(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefGoLayoutFile(t, repoDir, "internal/cli/mcp_test.go", "package cli\n")
	report := brainBriefReport{
		Task:            "repair malformed MCP frames",
		LikelyTestFiles: []string{"internal/cli/mcp_test.go"},
		LikelyFiles:     []string{"internal/cli/mcp_test.go"},
	}
	baseline := renderBrainBriefCompactV3ForTest(t, report)
	brainBriefApplyLayoutGuidance(repoDir, report.Task, &report)
	got := renderBrainBriefCompactV3ForTest(t, report)
	if got != baseline {
		t.Fatalf("missing source changed packet: before=%q after=%q", baseline, got)
	}
}

func BenchmarkBrainBriefGoTestSourceCompanions(b *testing.B) {
	repoDir := b.TempDir()
	writeBrainBriefGoLayoutFile(b, repoDir, "internal/cli/mcp.go", "package cli\n")

	b.Run("no_candidate", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddGoTestSourceCompanions(repoDir, nil, []string{"README.md"})
		}
	})
	b.Run("missing_source", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddGoTestSourceCompanions(repoDir, nil, []string{"internal/cli/missing_test.go"})
		}
	})
	b.Run("exact_match", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchmarkBrainBriefTestGuidanceFiles = brainBriefAddGoTestSourceCompanions(repoDir, nil, []string{"internal/cli/mcp_test.go"})
		}
	})
}
