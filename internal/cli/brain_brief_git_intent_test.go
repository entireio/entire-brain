package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBrainBriefGitIntentLiveShapedOperationalRecall(t *testing.T) {
	repoDir := newBrainBriefPostIndexRepo(t)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"internal/cli/export.go":   "package cli\n",
		"internal/cli/hook_cmd.go": "package cli\n",
	})
	commitBrainBriefPostIndexRepo(t, repoDir, "base source tree")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"scripts/install.sh": "#!/bin/sh\nset -eu\n",
	})
	commitBrainBriefPostIndexRepo(t, repoDir, "Add single end-to-end installer (CLI + MCP + hooks)")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		".github/workflows/test.yml": "name: test\n",
	})
	commitBrainBriefPostIndexRepo(t, repoDir, "ci: give go test a 20m timeout for the slow Windows/-race lane")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"mise.toml": "[env]\nCGO_CFLAGS = '{{env.CGO_CFLAGS}} -I/sqlite-vec'\n",
	})
	commitBrainBriefPostIndexRepo(t, repoDir, "Append to inherited CGO_CFLAGS instead of clobbering it")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"mise.toml": "[env]\nCGO_CFLAGS = '{{env.CGO_CFLAGS}} -I/sqlite-vec -Wno-deprecated-declarations'\n",
	})
	commitBrainBriefPostIndexRepo(t, repoDir, "Silence sqlite-vec auto-extension deprecation warnings on macOS")
	// A generic `test` grep used to fill the 16-commit bound before the older
	// Windows workflow fix. Domain-only candidate selection must see through
	// this ordinary recent history without widening the bound.
	for i := 0; i < brainBriefGitIntentCommitLimit+1; i++ {
		runBrainBriefPostIndexGit(t, repoDir, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("test unrelated maintenance %02d", i))
	}

	tests := []struct {
		name, task, want string
	}{
		{
			name: "bash installer",
			task: "the one command installer must configure the CLI MCP server and hooks idempotently",
			want: "scripts/install.sh",
		},
		{
			name: "yaml workflow",
			task: "the Windows race lane times out before the full Go test suite completes",
			want: ".github/workflows/test.yml",
		},
		{
			name: "toml inherited flags",
			task: "preserve inherited CGO compiler flags while adding sqlite vector headers",
			want: "mise.toml",
		},
		{
			name: "toml macOS warning",
			task: "silence sqlite vector automatic extension deprecation warnings on macOS builds",
			want: "mise.toml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := brainBriefGitIntentBaseline(tt.task)
			candidate := baseline
			started := time.Now()
			if !brainBriefPromoteGitIntentFile(context.Background(), ExecRunner{}, repoDir, tt.task, &candidate) {
				t.Fatalf("git-intent overlay did not activate: terms=%v edits=%v", brainBriefGitIntentTerms(tt.task), candidate.LikelyEditFiles)
			}
			elapsed := time.Since(started)
			if len(candidate.LikelyEditFiles) == 0 || candidate.LikelyEditFiles[0] != tt.want {
				t.Fatalf("edit rank = %v, want %q first", candidate.LikelyEditFiles, tt.want)
			}
			if slices.Contains(baseline.LikelyEditFiles, tt.want) {
				t.Fatalf("baseline already contained expected operational file: %v", baseline.LikelyEditFiles)
			}
			if !slices.Equal(candidate.LikelyTestFiles, baseline.LikelyTestFiles) {
				t.Fatalf("operational promotion changed tests: got=%v want=%v", candidate.LikelyTestFiles, baseline.LikelyTestFiles)
			}
			jsonDelta := len(renderBrainBriefJSONForPostIndexTest(t, candidate)) - len(renderBrainBriefJSONForPostIndexTest(t, baseline))
			compactDelta := len(renderBrainBriefCompactV3ForTest(t, candidate)) - len(renderBrainBriefCompactV3ForTest(t, baseline))
			if jsonDelta <= 0 || jsonDelta > 128 || compactDelta <= 0 || compactDelta > 128 {
				t.Fatalf("packet cost outside one-path bound: JSON=%+d compact_v3=%+d", jsonDelta, compactDelta)
			}
			t.Logf("%s: absent -> edit rank 1; JSON %+d B; compact_v3 %+d B; helper %s", tt.want, jsonDelta, compactDelta, elapsed)
		})
	}
}

func TestBrainBriefGitIntentActivationAndNoopPacketIdentity(t *testing.T) {
	base := brainBriefGitIntentBaseline("install hooks and configure the MCP server")
	validLog := brainBriefGitIntentLogRecord(strings.Repeat("a", 40), "install hooks and configure the MCP server")

	tests := []struct {
		name   string
		task   string
		mutate func(*brainBriefReport)
	}{
		{
			name: "existing action checklist",
			task: base.Task,
			mutate: func(report *brainBriefReport) {
				report.ActionChecklist = []brainBriefAction{{File: "internal/cli/export.go", Action: "inspect"}}
			},
		},
		{
			name: "strong operational path",
			task: "repair Windows race workflow lane",
			mutate: func(report *brainBriefReport) {
				report.LikelyEditFiles = []string{".github/workflows/windows_race.yml", "internal/cli/export.go"}
				report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
			},
		},
		{
			name: "strong source task path",
			task: "install hooks configuration",
			mutate: func(report *brainBriefReport) {
				report.LikelyEditFiles = []string{"internal/cli/install_hooks.go", "internal/cli/export.go"}
				report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
			},
		},
		{
			name:   "non operational task",
			task:   "repair semantic symbol ranking",
			mutate: func(*brainBriefReport) {},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := brainBriefGitIntentBaseline(tt.task)
			tt.mutate(&report)
			before := report
			runner := &brainBriefGitIntentScriptedRunner{outputs: [][]byte{validLog}}
			if brainBriefPromoteGitIntentFile(context.Background(), runner, t.TempDir(), tt.task, &report) {
				t.Fatal("guarded no-op reported a promotion")
			}
			if runner.streamCalls != 0 {
				t.Fatalf("guarded no-op invoked git %d times", runner.streamCalls)
			}
			assertBrainBriefGitIntentPacketIdentity(t, before, report)
		})
	}
}

func TestBrainBriefGitIntentArgumentsKeepTaskOutOfRevisionRegexAndPathspec(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"scripts/install.sh": "#!/bin/sh\n"})
	hash := strings.Repeat("a", 40)
	runner := &brainBriefGitIntentScriptedRunner{outputs: [][]byte{
		brainBriefGitIntentLogRecord(hash, "install hooks safely"),
		[]byte("scripts/install.sh\x00"),
	}}
	task := "install hooks HEAD --all :(exclude)* [wild] ^regex$ -- path"
	report := brainBriefGitIntentBaseline(task)
	if !brainBriefPromoteGitIntentFile(context.Background(), runner, repoDir, task, &report) {
		t.Fatalf("safe literal task failed to promote: edits=%v", report.LikelyEditFiles)
	}
	if runner.streamCalls != 2 || len(runner.calls) != 2 {
		t.Fatalf("git calls = %d, want log + diff-tree", runner.streamCalls)
	}
	logArgs := runner.calls[0]
	if len(logArgs) < 2 || logArgs[len(logArgs)-2] != "HEAD" || logArgs[len(logArgs)-1] != "--" {
		t.Fatalf("log revision/path boundary missing: %v", logArgs)
	}
	fixed := slices.Index(logArgs, "--fixed-strings")
	if fixed < 0 {
		t.Fatalf("fixed-string Git matching missing: %v", logArgs)
	}
	for i, arg := range logArgs {
		if !strings.HasPrefix(arg, "--grep=") {
			continue
		}
		if i < fixed || !brainBriefLiteralLocatorTerm(strings.TrimPrefix(arg, "--grep=")) {
			t.Fatalf("unsafe task-derived Git argument: %q in %v", arg, logArgs)
		}
	}
	joined := strings.Join(logArgs, " ")
	for _, unsafe := range []string{"--all", ":(exclude)", "[wild]", "^regex$"} {
		if strings.Contains(joined, unsafe) {
			t.Fatalf("task syntax reached Git arguments: %q in %v", unsafe, logArgs)
		}
	}
	pathArgs := runner.calls[1]
	if pathArgs[len(pathArgs)-1] != "--" || !slices.Contains(pathArgs, hash) {
		t.Fatalf("diff-tree is not bound to selected full commit and --: %v", pathArgs)
	}
}

func TestBrainBriefGitIntentUnrelatedOperationalRankOneDoesNotSuppressPromotion(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"scripts/install.sh": "#!/bin/sh\n",
		"scripts/release.sh": "#!/bin/sh\n",
	})
	hash := strings.Repeat("a", 40)
	runner := &brainBriefGitIntentScriptedRunner{outputs: [][]byte{
		brainBriefGitIntentLogRecord(hash, "Add single end-to-end installer with hooks"),
		[]byte("scripts/install.sh\x00"),
	}}
	task := "the one command installer must configure the CLI MCP server and hooks idempotently"
	report := brainBriefGitIntentBaseline(task)
	report.LikelyEditFiles = append([]string{"scripts/release.sh"}, report.LikelyEditFiles...)
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	if !brainBriefPromoteGitIntentFile(context.Background(), runner, repoDir, task, &report) {
		t.Fatal("unrelated rank-1 operational path suppressed Git-intent evidence")
	}
	if !slices.Equal(report.LikelyEditFiles[:2], []string{"scripts/install.sh", "scripts/release.sh"}) {
		t.Fatalf("promoted/noisy operational ordering = %v", report.LikelyEditFiles)
	}
}

func TestBrainBriefGitIntentCommitFailuresAreByteIdentical(t *testing.T) {
	hashA := strings.Repeat("a", 40)
	hashB := strings.Repeat("b", 40)
	validSubject := "install hooks configuration"
	var tooMany bytes.Buffer
	for i := 0; i <= brainBriefGitIntentCommitLimit; i++ {
		tooMany.WriteString(fmt.Sprintf("%040x", i+1))
		tooMany.WriteByte(0)
		tooMany.WriteString(validSubject)
		tooMany.WriteByte(0)
	}
	tests := []struct {
		name    string
		outputs [][]byte
		errs    []error
	}{
		{name: "truncated log", outputs: [][]byte{[]byte(hashA + "\x00" + validSubject)}},
		{name: "invalid hash", outputs: [][]byte{brainBriefGitIntentLogRecord("HEAD", validSubject)}},
		{name: "commit cap", outputs: [][]byte{tooMany.Bytes()}},
		{name: "output cap", outputs: [][]byte{bytes.Repeat([]byte{'x'}, brainBriefGitIntentOutputLimit+1)}},
		{name: "ambiguous best commits", outputs: [][]byte{append(brainBriefGitIntentLogRecord(hashA, validSubject), brainBriefGitIntentLogRecord(hashB, validSubject)...)}},
		{name: "nonzero git", outputs: [][]byte{nil}, errs: []error{errors.New("git failed")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := brainBriefGitIntentBaseline("install hooks configuration")
			after := before
			runner := &brainBriefGitIntentScriptedRunner{outputs: tt.outputs, waitErrs: tt.errs}
			if brainBriefPromoteGitIntentFile(context.Background(), runner, t.TempDir(), after.Task, &after) {
				t.Fatal("malformed/ambiguous commit evidence promoted a file")
			}
			assertBrainBriefGitIntentPacketIdentity(t, before, after)
		})
	}

	t.Run("non-streaming runner", func(t *testing.T) {
		before := brainBriefGitIntentBaseline("install hooks configuration")
		after := before
		if brainBriefPromoteGitIntentFile(context.Background(), brainBriefPostIndexRunOnlyRunner{}, t.TempDir(), after.Task, &after) {
			t.Fatal("non-streaming runner promoted a file")
		}
		assertBrainBriefGitIntentPacketIdentity(t, before, after)
	})
}

func TestBrainBriefGitIntentPathFailuresAreByteIdentical(t *testing.T) {
	hash := strings.Repeat("a", 40)
	validLog := brainBriefGitIntentLogRecord(hash, "install hooks configuration")
	var tooMany bytes.Buffer
	for i := 0; i <= brainBriefGitIntentPathLimit; i++ {
		fmt.Fprintf(&tooMany, "scripts/install_%03d.sh%c", i, byte(0))
	}
	tests := []struct {
		name       string
		pathOutput []byte
		files      map[string]string
		waitErrs   []error
	}{
		{name: "truncated paths", pathOutput: []byte("scripts/install.sh"), files: map[string]string{"scripts/install.sh": "x"}},
		{name: "unsafe parent", pathOutput: []byte("../scripts/install.sh\x00")},
		{name: "leading whitespace path", pathOutput: []byte(" scripts/install.sh\x00"), files: map[string]string{"scripts/install.sh": "x"}},
		{name: "path cap", pathOutput: tooMany.Bytes()},
		{name: "path output cap", pathOutput: bytes.Repeat([]byte{'x'}, brainBriefGitIntentOutputLimit+1)},
		{name: "ambiguous eligible paths", pathOutput: []byte("scripts/alpha.sh\x00scripts/bravo.sh\x00"), files: map[string]string{"scripts/alpha.sh": "x", "scripts/bravo.sh": "x"}},
		{name: "generated excluded", pathOutput: []byte("scripts/generated/install.sh\x00"), files: map[string]string{"scripts/generated/install.sh": "x"}},
		{name: "departed file", pathOutput: []byte("scripts/install.sh\x00")},
		{name: "unsupported source", pathOutput: []byte("internal/cli/install.go\x00"), files: map[string]string{"internal/cli/install.go": "package cli\n"}},
		{name: "nonzero diff tree", pathOutput: []byte("scripts/install.sh\x00"), files: map[string]string{"scripts/install.sh": "x"}, waitErrs: []error{nil, errors.New("diff failed")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir := t.TempDir()
			if tt.files != nil {
				writeBrainBriefPostIndexFiles(t, repoDir, tt.files)
			}
			before := brainBriefGitIntentBaseline("install hooks configuration")
			after := before
			runner := &brainBriefGitIntentScriptedRunner{outputs: [][]byte{validLog, tt.pathOutput}, waitErrs: tt.waitErrs}
			if brainBriefPromoteGitIntentFile(context.Background(), runner, repoDir, after.Task, &after) {
				t.Fatal("invalid path evidence promoted a file")
			}
			assertBrainBriefGitIntentPacketIdentity(t, before, after)
		})
	}
}

func TestBrainBriefGitIntentRejectsSymlinkAndTimeout(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		repoDir := t.TempDir()
		outside := filepath.Join(t.TempDir(), "install.sh")
		if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repoDir, "scripts"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(repoDir, "scripts", "install.sh")); err != nil {
			t.Fatal(err)
		}
		before := brainBriefGitIntentBaseline("install hooks configuration")
		after := before
		runner := &brainBriefGitIntentScriptedRunner{outputs: [][]byte{
			brainBriefGitIntentLogRecord(strings.Repeat("a", 40), "install hooks configuration"),
			[]byte("scripts/install.sh\x00"),
		}}
		if brainBriefPromoteGitIntentFile(context.Background(), runner, repoDir, after.Task, &after) {
			t.Fatal("symlinked file was promoted")
		}
		assertBrainBriefGitIntentPacketIdentity(t, before, after)
	})

	t.Run("hard timeout", func(t *testing.T) {
		before := brainBriefGitIntentBaseline("install hooks configuration")
		after := before
		started := time.Now()
		if brainBriefPromoteGitIntentFile(context.Background(), brainBriefGitIntentBlockingRunner{}, t.TempDir(), after.Task, &after) {
			t.Fatal("timed-out Git call promoted a file")
		}
		timeout := brainBriefGitIntentTimeoutForPlatform()
		if elapsed := time.Since(started); elapsed > timeout+500*time.Millisecond {
			t.Fatalf("%s hard timeout returned after %s", timeout, elapsed)
		}
		assertBrainBriefGitIntentPacketIdentity(t, before, after)
	})
}

func TestBrainBriefGitIntentDownranksTestsUnlessRequested(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"scripts/release.sh":        "#!/bin/sh\n",
		"tests/release_test.sh":     "#!/bin/sh\n",
		"scripts/vendor/release.sh": "#!/bin/sh\n",
	})
	paths := []string{"tests/release_test.sh", "scripts/release.sh", "scripts/vendor/release.sh"}
	terms := brainBriefGitIntentTerms("repair release packaging hook")
	got, ok := brainBriefGitIntentBestPath(repoDir, "repair release packaging hook", terms, paths)
	if !ok || got != "scripts/release.sh" {
		t.Fatalf("non-test intent path = %q ok=%t, want production script", got, ok)
	}
	testTerms := brainBriefGitIntentTerms("test release packaging hook")
	got, ok = brainBriefGitIntentBestPath(repoDir, "test release packaging hook", testTerms, paths)
	if !ok || got != "tests/release_test.sh" {
		t.Fatalf("test intent path = %q ok=%t, want focused test script", got, ok)
	}
}

func BenchmarkBrainBriefGitIntentPromotion(b *testing.B) {
	repoDir := newBrainBriefPostIndexRepo(b)
	writeBrainBriefPostIndexFiles(b, repoDir, map[string]string{"internal/cli/base.go": "package cli\n"})
	commitBrainBriefPostIndexRepo(b, repoDir, "base")
	writeBrainBriefPostIndexFiles(b, repoDir, map[string]string{"scripts/install.sh": "#!/bin/sh\n"})
	commitBrainBriefPostIndexRepo(b, repoDir, "Add single end-to-end installer (CLI + MCP + hooks)")
	task := "the one command installer must configure the CLI MCP server and hooks idempotently"
	baseline := brainBriefGitIntentBaseline(task)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		report := baseline
		if !brainBriefPromoteGitIntentFile(context.Background(), ExecRunner{}, repoDir, task, &report) ||
			len(report.LikelyEditFiles) == 0 || report.LikelyEditFiles[0] != "scripts/install.sh" {
			b.Fatalf("unexpected promotion: %v", report.LikelyEditFiles)
		}
	}
}

func brainBriefGitIntentBaseline(task string) brainBriefReport {
	report := brainBriefReport{
		Task: task,
		LikelyEditFiles: []string{
			"internal/cli/export.go",
			"internal/cli/hook_cmd.go",
			"internal/cli/embed_ollama.go",
		},
		LikelyTestFiles: []string{
			"internal/cli/export_test.go",
			"internal/cli/hook_cmd_test.go",
		},
	}
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	return report
}

func brainBriefGitIntentLogRecord(hash, subject string) []byte {
	return []byte(hash + "\x00" + subject + "\x00")
}

func assertBrainBriefGitIntentPacketIdentity(t *testing.T, before, after brainBriefReport) {
	t.Helper()
	beforeJSON := renderBrainBriefJSONForPostIndexTest(t, before)
	afterJSON := renderBrainBriefJSONForPostIndexTest(t, after)
	if beforeJSON != afterJSON {
		t.Fatalf("default JSON packet changed on fail-closed no-op\nbefore:\n%s\nafter:\n%s", beforeJSON, afterJSON)
	}
	beforeCompact := renderBrainBriefCompactV3ForTest(t, before)
	afterCompact := renderBrainBriefCompactV3ForTest(t, after)
	if beforeCompact != afterCompact {
		t.Fatalf("compact_v3 packet changed on fail-closed no-op\nbefore:\n%s\nafter:\n%s", beforeCompact, afterCompact)
	}
}

type brainBriefGitIntentScriptedRunner struct {
	outputs     [][]byte
	waitErrs    []error
	streamCalls int
	calls       [][]string
}

func (*brainBriefGitIntentScriptedRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New("buffered runner must not be used")
}

func (runner *brainBriefGitIntentScriptedRunner) Stream(_ context.Context, _ string, _ string, args ...string) (CommandStream, error) {
	index := runner.streamCalls
	runner.streamCalls++
	runner.calls = append(runner.calls, append([]string(nil), args...))
	if index >= len(runner.outputs) {
		return nil, errors.New("unexpected extra stream call")
	}
	var waitErr error
	if index < len(runner.waitErrs) {
		waitErr = runner.waitErrs[index]
	}
	return &brainBriefGitIntentMemoryStream{reader: bytes.NewReader(runner.outputs[index]), waitErr: waitErr}, nil
}

type brainBriefGitIntentMemoryStream struct {
	reader  io.Reader
	waitErr error
}

func (stream *brainBriefGitIntentMemoryStream) Stdout() io.Reader     { return stream.reader }
func (stream *brainBriefGitIntentMemoryStream) Wait() ([]byte, error) { return nil, stream.waitErr }
func (stream *brainBriefGitIntentMemoryStream) Close() error          { return nil }

type brainBriefGitIntentBlockingRunner struct{}

func (brainBriefGitIntentBlockingRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New("buffered runner must not be used")
}

func (brainBriefGitIntentBlockingRunner) Stream(ctx context.Context, _ string, _ string, _ ...string) (CommandStream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
