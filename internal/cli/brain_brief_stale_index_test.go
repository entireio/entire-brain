package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestBrainBriefPostIndexPromotionUsesCommittedTaskFiles(t *testing.T) {
	repoDir := newBrainBriefPostIndexRepo(t)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"internal/cli/semantic.go":      "package cli\nfunc semanticBundleChecksum() {}\n",
		"internal/cli/semantic_test.go": "package cli\nfunc TestSemanticBundleChecksum() {}\n",
	})
	indexed := commitBrainBriefPostIndexRepo(t, repoDir, "indexed semantic tree")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"internal/cli/brain_brief_compact.go":                 "package cli\n",
		"internal/cli/brain_brief_compact_test.go":            "package cli\n",
		"internal/cli/brain_brief_compact_v2.go":              "package cli\n",
		"internal/cli/brain_brief_compact_v2_test.go":         "package cli\n",
		"internal/cli/brain_brief_compact_v3.go":              "package cli\nfunc compactV3Checksum() {}\n",
		"internal/cli/brain_brief_compact_v3_bench_test.go":   "package cli\n",
		"internal/cli/brain_brief_compact_v3_test.go":         "package cli\nfunc TestCompactV3Checksum() {}\n",
		"internal/cli/testdata/brain_brief_compact_v3.golden": "not an eligible source extension\n",
	})
	current := commitBrainBriefPostIndexRepo(t, repoDir, "add compact v3")
	status := brainBriefPostIndexStatus(repoDir, indexed, current)

	for _, task := range []string{
		"fix compact-v3 checksum validation",
		"repair compact-v3 integrity and canonical checksum rejection",
	} {
		t.Run(task, func(t *testing.T) {
			report := brainBriefReport{
				Task:   task,
				Status: brainBriefOutputStatus(status),
				Semantic: brainBriefSemantic{
					Context: semanticContextResult{Symbols: []semanticRecord{{FilePath: "internal/cli/semantic.go"}}},
					Tests:   semanticTestsResult{Roots: []semanticRecord{{FilePath: "internal/cli/semantic_test.go"}}},
				},
			}
			report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles = brainBriefLikelyFileGroups(repoDir, report, task)
			baseline := report
			brainBriefApplyLayoutGuidance(repoDir, task, &baseline)
			if slices.Contains(baseline.LikelyEditFiles, "internal/cli/brain_brief_compact_v3.go") ||
				slices.Contains(baseline.LikelyTestFiles, "internal/cli/brain_brief_compact_v3_test.go") {
				t.Fatalf("stale indexed baseline unexpectedly found post-index files: edits=%v tests=%v", baseline.LikelyEditFiles, baseline.LikelyTestFiles)
			}

			candidate := report
			brainBriefPromotePostIndexFiles(context.Background(), ExecRunner{}, status, task, &candidate)
			brainBriefApplyLayoutGuidance(repoDir, task, &candidate)
			if len(candidate.LikelyEditFiles) == 0 || candidate.LikelyEditFiles[0] != "internal/cli/brain_brief_compact_v3.go" {
				t.Fatalf("post-index source rank = %v, want compact_v3.go first", candidate.LikelyEditFiles)
			}
			if len(candidate.LikelyTestFiles) == 0 || candidate.LikelyTestFiles[0] != "internal/cli/brain_brief_compact_v3_test.go" {
				t.Fatalf("post-index test rank = %v, want compact_v3_test.go first", candidate.LikelyTestFiles)
			}
			if slices.Contains(candidate.LikelyTestFiles[:1], "internal/cli/brain_brief_compact_v3_bench_test.go") {
				t.Fatalf("non-performance task promoted benchmark over canonical test: %v", candidate.LikelyTestFiles)
			}
			if len(status.Live.ChangedFiles) != 0 || len(candidate.Status.Live.ChangedFiles) != 0 {
				t.Fatalf("post-index bridge polluted live deletion boundary: status=%v candidate=%v", status.Live.ChangedFiles, candidate.Status.Live.ChangedFiles)
			}

			baselineCompact := renderBrainBriefCompactV3ForTest(t, baseline)
			candidateCompact := renderBrainBriefCompactV3ForTest(t, candidate)
			baselineJSON := renderBrainBriefJSONForPostIndexTest(t, baseline)
			candidateJSON := renderBrainBriefJSONForPostIndexTest(t, candidate)
			compactDelta := len(candidateCompact) - len(baselineCompact)
			jsonDelta := len(candidateJSON) - len(baselineJSON)
			if compactDelta != 95 {
				t.Fatalf("compact_v3 packet delta = %d, want 95 bytes", compactDelta)
			}
			if jsonDelta <= 0 || jsonDelta > 256 {
				t.Fatalf("default JSON packet delta = %d, want bounded positive cost", jsonDelta)
			}
			t.Logf("post-index recall source/test 0/2 -> 2/2; edit/test ranks absent -> 1/1; compact_v3 %+d B; default JSON %+d B", compactDelta, jsonDelta)
		})
	}
}

func TestBrainBriefPostIndexPromotionUsesNestedModuleAndLongTaskTail(t *testing.T) {
	repoDir := newBrainBriefPostIndexRepo(t)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"src/base.go": "package src\n"})
	indexed := commitBrainBriefPostIndexRepo(t, repoDir, "base")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"src/auth/token.go":      "package auth\n",
		"src/auth/token_test.go": "package auth\n",
	})
	current := commitBrainBriefPostIndexRepo(t, repoDir, "auth token expiry")
	status := brainBriefPostIndexStatus(repoDir, indexed, current)

	task := "alpha beta gamma delta epsilon zeta repair auth token expiry"
	terms := brainBriefPostIndexLocatorTerms(task)
	if !slices.Contains(terms, "auth") || !slices.Contains(terms, "token") {
		t.Fatalf("long-task tail locators were truncated: %v", terms)
	}
	edits, tests := brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, task)
	if !slices.Equal(edits, []string{"src/auth/token.go"}) || !slices.Equal(tests, []string{"src/auth/token_test.go"}) {
		t.Fatalf("nested module locator pair missed: edits=%v tests=%v terms=%v", edits, tests, terms)
	}
}

func TestBrainBriefPostIndexCommitWidthsAndInvalidRanges(t *testing.T) {
	for _, width := range []int{40, 64} {
		indexed := strings.Repeat("a", width)
		current := strings.Repeat("b", width)
		status := brainBriefPostIndexStatus(t.TempDir(), indexed, current)
		_, gotIndexed, gotCurrent, ok := brainBriefPostIndexRange(status)
		if !ok || gotIndexed != indexed || gotCurrent != current {
			t.Fatalf("%d-character full commit rejected: indexed=%q current=%q ok=%t", width, gotIndexed, gotCurrent, ok)
		}
	}

	validIndexed := strings.Repeat("a", 40)
	validCurrent := strings.Repeat("b", 40)
	tests := []struct {
		name   string
		mutate func(*brainStatusReport)
	}{
		{name: "short indexed", mutate: func(status *brainStatusReport) {
			status.Semantic.Freshness.Axes["head"] = staleAxis{State: "stale", Indexed: "abc", Current: validCurrent}
			status.Manifest.Sources.Semantic.Commit = "abc"
		}},
		{name: "long current", mutate: func(status *brainStatusReport) {
			status.Semantic.Freshness.Axes["head"] = staleAxis{State: "stale", Indexed: validIndexed, Current: strings.Repeat("b", 65)}
			status.Live.Head = strings.Repeat("b", 65)
		}},
		{name: "mixed object formats", mutate: func(status *brainStatusReport) {
			status.Semantic.Freshness.Axes["head"] = staleAxis{State: "stale", Indexed: validIndexed, Current: strings.Repeat("b", 64)}
			status.Live.Head = strings.Repeat("b", 64)
		}},
		{name: "head not stale", mutate: func(status *brainStatusReport) {
			status.Semantic.Freshness.Axes["head"] = staleAxis{State: "ok", Indexed: validIndexed, Current: validCurrent}
		}},
		{name: "live head mismatch", mutate: func(status *brainStatusReport) {
			status.Live.Head = strings.Repeat("c", 40)
		}},
		{name: "manifest mismatch", mutate: func(status *brainStatusReport) {
			status.Manifest.Sources.Semantic.Commit = strings.Repeat("c", 40)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := brainBriefPostIndexStatus(t.TempDir(), validIndexed, validCurrent)
			tt.mutate(&status)
			if _, _, _, ok := brainBriefPostIndexRange(status); ok {
				t.Fatal("invalid stale-index range was accepted")
			}
		})
	}
}

func TestBrainBriefPostIndexRejectsNonAncestor(t *testing.T) {
	repoDir := newBrainBriefPostIndexRepo(t)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"internal/cli/base.go": "package cli\n"})
	base := commitBrainBriefPostIndexRepo(t, repoDir, "base")
	runBrainBriefPostIndexGit(t, repoDir, "checkout", "-q", "-b", "indexed-side", base)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"internal/cli/compact_v3_indexed.go": "package cli\n"})
	indexed := commitBrainBriefPostIndexRepo(t, repoDir, "indexed side")
	runBrainBriefPostIndexGit(t, repoDir, "checkout", "-q", "-b", "live-side", base)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"internal/cli/compact_v3_live.go": "package cli\n"})
	current := commitBrainBriefPostIndexRepo(t, repoDir, "live side")
	status := brainBriefPostIndexStatus(repoDir, indexed, current)
	edits, tests := brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, "fix compact-v3 checksum")
	if len(edits) != 0 || len(tests) != 0 {
		t.Fatalf("non-ancestor semantic commit surfaced files: edits=%v tests=%v", edits, tests)
	}
}

func TestBrainBriefPostIndexPathspecIsLiteralAndBounded(t *testing.T) {
	terms := brainBriefPostIndexLocatorTerms("alpha beta gamma delta epsilon zeta compact-v3 :(exclude)* [wild] checksum")
	if len(terms) != brainBriefPostIndexTermLimit || terms[0] != "v3" ||
		!slices.Contains(terms, "compact") || !slices.Contains(terms, "checksum") {
		t.Fatalf("bounded locator terms = %v, want v3 first and long-task tail locators retained", terms)
	}
	for _, term := range terms {
		if !brainBriefLiteralLocatorTerm(term) {
			t.Fatalf("unsafe locator term escaped extraction: %q", term)
		}
	}

	runner := &brainBriefPostIndexMemoryRunner{output: []byte("internal/cli/compact_v3.go\x00")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, ok := brainBriefPostIndexDiffPaths(ctx, cancel, runner, t.TempDir(), strings.Repeat("a", 40), strings.Repeat("b", 40), terms)
	if !ok || !slices.Equal(paths, []string{"internal/cli/compact_v3.go"}) {
		t.Fatalf("bounded pathspec result = %v, ok=%t", paths, ok)
	}
	dash := slices.Index(runner.args, "--")
	if dash < 0 || len(runner.args[dash+1:]) != len(terms)*2 {
		t.Fatalf("git pathspec args = %v", runner.args)
	}
	for _, arg := range runner.args[dash+1:] {
		if !strings.HasPrefix(arg, ":(top,glob,icase)") || strings.Contains(arg, "exclude") || strings.ContainsAny(arg, "[]!") {
			t.Fatalf("task syntax reached git pathspec magic: %q", arg)
		}
	}

	unsafeRunner := &brainBriefPostIndexMemoryRunner{output: []byte("x\x00")}
	unsafeCtx, unsafeCancel := context.WithCancel(context.Background())
	defer unsafeCancel()
	if _, ok := brainBriefPostIndexDiffPaths(unsafeCtx, unsafeCancel, unsafeRunner, t.TempDir(), strings.Repeat("a", 40), strings.Repeat("b", 40), []string{"compact", ":(exclude)"}); ok || unsafeRunner.streamCalls != 0 {
		t.Fatal("unsafe direct locator reached the git runner")
	}
}

func TestBrainBriefPostIndexBenchmarkPenaltyAffectsMaxGroup(t *testing.T) {
	repoDir := newBrainBriefPostIndexRepo(t)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"internal/cli/base.go": "package cli\n"})
	indexed := commitBrainBriefPostIndexRepo(t, repoDir, "base")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"internal/cli/compact_v3.go":                     "package cli\n",
		"internal/cli/compact_v3_checksum_bench_test.go": "package cli\n",
		"internal/cli/compact_v3_test.go":                "package cli\n",
	})
	current := commitBrainBriefPostIndexRepo(t, repoDir, "candidate group")
	status := brainBriefPostIndexStatus(repoDir, indexed, current)

	edits, tests := brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, "fix compact-v3 checksum validation")
	if !slices.Equal(edits, []string{"internal/cli/compact_v3.go"}) || !slices.Equal(tests, []string{"internal/cli/compact_v3_test.go"}) {
		t.Fatalf("benchmark raw-hit advantage excluded production group: edits=%v tests=%v", edits, tests)
	}
	performanceEdits, performanceTests := brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, "benchmark compact-v3 checksum allocations")
	if !slices.Equal(performanceEdits, []string{"internal/cli/compact_v3.go"}) ||
		!slices.Equal(performanceTests, []string{"internal/cli/compact_v3_checksum_bench_test.go"}) {
		t.Fatalf("performance benchmark erased source or lost test priority: edits=%v tests=%v", performanceEdits, performanceTests)
	}
}

func TestBrainBriefPostIndexPathCapIgnoresOneTermDecoys(t *testing.T) {
	var out strings.Builder
	for i := 0; i <= brainBriefPostIndexPathLimit; i++ {
		fmt.Fprintf(&out, "decoys/compact_only_%03d.go%c", i, byte(0))
	}
	out.WriteString("internal/cli/compact_v3.go\x00internal/cli/compact_v3_test.go\x00")
	runner := &brainBriefPostIndexMemoryRunner{output: []byte(out.String())}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, ok := brainBriefPostIndexDiffPaths(
		ctx,
		cancel,
		runner,
		t.TempDir(),
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		[]string{"compact", "v3", "checksum"},
	)
	want := []string{"internal/cli/compact_v3.go", "internal/cli/compact_v3_test.go"}
	if !ok || !slices.Equal(paths, want) {
		t.Fatalf("one-term decoys consumed qualified path cap: paths=%v ok=%t", paths, ok)
	}
}

func TestBrainBriefPostIndexFailsClosedOnBoundaries(t *testing.T) {
	terms := []string{"compact", "v3", "checksum"}
	indexed := strings.Repeat("a", 40)
	current := strings.Repeat("b", 40)

	t.Run("path count", func(t *testing.T) {
		var out strings.Builder
		for i := 0; i <= brainBriefPostIndexPathLimit; i++ {
			fmt.Fprintf(&out, "internal/cli/compact_v3_%03d.go%c", i, byte(0))
		}
		runner := &brainBriefPostIndexMemoryRunner{output: []byte(out.String())}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if paths, ok := brainBriefPostIndexDiffPaths(ctx, cancel, runner, t.TempDir(), indexed, current, terms); ok || len(paths) != 0 {
			t.Fatalf("oversized path set accepted: paths=%d ok=%t", len(paths), ok)
		}
	})

	t.Run("byte count", func(t *testing.T) {
		runner := &brainBriefPostIndexMemoryRunner{output: append(bytes.Repeat([]byte{'a'}, brainBriefPostIndexOutputLimit+1), 0)}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if paths, ok := brainBriefPostIndexDiffPaths(ctx, cancel, runner, t.TempDir(), indexed, current, terms); ok || len(paths) != 0 {
			t.Fatalf("oversized byte stream accepted: paths=%d ok=%t", len(paths), ok)
		}
	})

	t.Run("truncated NUL stream", func(t *testing.T) {
		runner := &brainBriefPostIndexMemoryRunner{output: []byte("internal/cli/compact_v3.go")}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if paths, ok := brainBriefPostIndexDiffPaths(ctx, cancel, runner, t.TempDir(), indexed, current, terms); ok || len(paths) != 0 {
			t.Fatalf("truncated stream accepted: paths=%d ok=%t", len(paths), ok)
		}
	})

	t.Run("non streaming runner", func(t *testing.T) {
		status := brainBriefPostIndexStatus(t.TempDir(), indexed, current)
		runner := brainBriefPostIndexRunOnlyRunner{}
		edits, tests := brainBriefPostIndexFiles(context.Background(), runner, status, "fix compact-v3 checksum")
		if len(edits) != 0 || len(tests) != 0 {
			t.Fatalf("unbounded runner fallback surfaced files: edits=%v tests=%v", edits, tests)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		status := brainBriefPostIndexStatus(t.TempDir(), indexed, current)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		started := time.Now()
		edits, tests := brainBriefPostIndexFiles(ctx, brainBriefPostIndexBlockingRunner{}, status, "fix compact-v3 checksum")
		if len(edits) != 0 || len(tests) != 0 || time.Since(started) > 250*time.Millisecond {
			t.Fatalf("timed-out discovery did not fail closed promptly: edits=%v tests=%v elapsed=%s", edits, tests, time.Since(started))
		}
	})
}

func TestBrainBriefPostIndexRejectsSymlinkAndNoMatch(t *testing.T) {
	repoDir := newBrainBriefPostIndexRepo(t)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"internal/cli/base.go": "package cli\n"})
	indexed := commitBrainBriefPostIndexRepo(t, repoDir, "base")

	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(repoDir, "internal", "cli", "compact_v3_symlink.go")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"internal/cli/unrelated.go": "package cli\n",
	})
	current := commitBrainBriefPostIndexRepo(t, repoDir, "unsafe candidates")
	status := brainBriefPostIndexStatus(repoDir, indexed, current)

	edits, tests := brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, "fix compact-v3 checksum")
	if len(edits) != 0 || len(tests) != 0 {
		t.Fatalf("symlink post-index candidate escaped regular-file boundary: edits=%v tests=%v", edits, tests)
	}
	edits, tests = brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, "fix quantum-v9 checksum")
	if len(edits) != 0 || len(tests) != 0 {
		t.Fatalf("no-match task surfaced unrelated post-index files: edits=%v tests=%v", edits, tests)
	}
}

func BenchmarkBrainBriefPostIndexFiles(b *testing.B) {
	repoDir := newBrainBriefPostIndexRepo(b)
	writeBrainBriefPostIndexFiles(b, repoDir, map[string]string{"internal/cli/base.go": "package cli\n"})
	indexed := commitBrainBriefPostIndexRepo(b, repoDir, "base")
	writeBrainBriefPostIndexFiles(b, repoDir, map[string]string{
		"internal/cli/brain_brief_compact_v3.go":      "package cli\n",
		"internal/cli/brain_brief_compact_v3_test.go": "package cli\n",
	})
	current := commitBrainBriefPostIndexRepo(b, repoDir, "compact v3")
	status := brainBriefPostIndexStatus(repoDir, indexed, current)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		edits, tests := brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, "fix compact-v3 checksum validation")
		if len(edits) != 1 || len(tests) != 1 {
			b.Fatalf("unexpected candidates: edits=%v tests=%v", edits, tests)
		}
	}
}

type brainBriefPostIndexMemoryRunner struct {
	output      []byte
	waitErr     error
	args        []string
	streamCalls int
}

func (runner *brainBriefPostIndexMemoryRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, nil
}

func (runner *brainBriefPostIndexMemoryRunner) Stream(_ context.Context, _ string, _ string, args ...string) (CommandStream, error) {
	runner.streamCalls++
	runner.args = append([]string(nil), args...)
	return &brainBriefPostIndexMemoryStream{reader: bytes.NewReader(runner.output), waitErr: runner.waitErr}, nil
}

type brainBriefPostIndexMemoryStream struct {
	reader  io.Reader
	waitErr error
}

func (stream *brainBriefPostIndexMemoryStream) Stdout() io.Reader     { return stream.reader }
func (stream *brainBriefPostIndexMemoryStream) Wait() ([]byte, error) { return nil, stream.waitErr }
func (stream *brainBriefPostIndexMemoryStream) Close() error          { return nil }

type brainBriefPostIndexRunOnlyRunner struct{}

func (brainBriefPostIndexRunOnlyRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, nil
}

type brainBriefPostIndexBlockingRunner struct{}

func (brainBriefPostIndexBlockingRunner) Run(ctx context.Context, _ string, _ string, _ ...string) ([]byte, []byte, error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func (brainBriefPostIndexBlockingRunner) Stream(context.Context, string, string, ...string) (CommandStream, error) {
	return nil, errors.New("unexpected stream after timed-out ancestor check")
}

func newBrainBriefPostIndexRepo(t testing.TB) string {
	t.Helper()
	repoDir := t.TempDir()
	runBrainBriefPostIndexGit(t, repoDir, "init", "-q")
	runBrainBriefPostIndexGit(t, repoDir, "config", "user.name", "Entire Brain Test")
	runBrainBriefPostIndexGit(t, repoDir, "config", "user.email", "brain-test@example.invalid")
	runBrainBriefPostIndexGit(t, repoDir, "config", "commit.gpgsign", "false")
	runBrainBriefPostIndexGit(t, repoDir, "config", "core.hooksPath", filepath.Join(repoDir, ".no-hooks"))
	return repoDir
}

func writeBrainBriefPostIndexFiles(t testing.TB, repoDir string, files map[string]string) {
	t.Helper()
	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		path := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(files[rel]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func commitBrainBriefPostIndexRepo(t testing.TB, repoDir, message string) string {
	t.Helper()
	runBrainBriefPostIndexGit(t, repoDir, "add", "-A")
	runBrainBriefPostIndexGit(t, repoDir, "commit", "-q", "-m", message)
	return strings.TrimSpace(runBrainBriefPostIndexGit(t, repoDir, "rev-parse", "HEAD"))
}

func runBrainBriefPostIndexGit(t testing.TB, repoDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func brainBriefPostIndexStatus(repoDir, indexed, current string) brainStatusReport {
	return brainStatusReport{
		Repo: brainStatusRepo{Root: repoDir},
		Live: brainLiveState{Head: current},
		Semantic: &brainStatusSemantic{Freshness: &staleReport{
			Severity: "unsafe",
			Axes: map[string]staleAxis{
				"head": {State: "stale", Indexed: indexed, Current: current},
			},
		}},
		Manifest: &exportManifest{Sources: &brainSources{Semantic: &semanticSourceManifest{Commit: indexed}}},
	}
}

func renderBrainBriefJSONForPostIndexTest(t *testing.T, report brainBriefReport) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := emitBrainBriefReport(cmd, report, true); err != nil {
		t.Fatalf("render default JSON brief: %v", err)
	}
	return out.String()
}
