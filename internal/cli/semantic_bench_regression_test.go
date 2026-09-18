package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func semanticBenchFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	// Benchmark temp stores must be isolated from other tests and user runs.
	temporaryRoot := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, temporaryRoot)
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.1"), `"capabilities"`, `"profile":"full","capabilities"`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	runner.semanticSnapshotAny = &fakeCommandResponse{stdout: snapshot}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time {
		return time.Date(2026, 9, 18, 13, 0, 0, 0, time.UTC)
	}}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(storage.BrainDir, "preexisting-marker")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, marker
}

func TestSemanticBenchRootRoutesJSONTextProgressAndCleanup(t *testing.T) {
	opts, repoDir, marker := semanticBenchFixture(t)
	var out string
	var err error
	progress := captureStderr(t, func() {
		out, err = execute(t, NewRootCommand(opts), "bench", "semantic", repoDir, "--json", "--profile", "full", "--progress")
	})
	if err != nil {
		t.Fatalf("json bench: %v\n%s", err, out)
	}
	for _, phase := range []string{"preparing isolated store", "verifying provider", "parsing sources", "building store"} {
		if !strings.Contains(progress, "semantic bench: "+phase) {
			t.Fatalf("progress omitted %q:\n%s", phase, progress)
		}
	}
	var report semanticBenchReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode bench report: %v\n%s", err, out)
	}
	if report.GeneratedAt != opts.Now().UTC() || report.RepoRoot != repoDir || report.Profile != "full" || report.Provider != "entire-graph" || report.SchemaVersion != "1.1" {
		t.Fatalf("unexpected fixed report fields: %+v", report)
	}
	if report.Files != 1 || report.Symbols != 1 || report.Relations != 1 || report.Kept || report.TempRoot == "" {
		t.Fatalf("unexpected fixture counts or lifecycle: %+v", report)
	}
	if _, err := os.Stat(report.TempRoot); !os.IsNotExist(err) {
		t.Fatalf("default isolated store was retained: %s err=%v", report.TempRoot, err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged" {
		t.Fatalf("benchmark touched original brain marker: %q err=%v", got, err)
	}

	out, err = execute(t, NewRootCommand(opts), "bench", "semantic", repoDir, "--profile", "full")
	if err != nil || !strings.Contains(out, "semantic bench for "+repoDir) || !strings.Contains(out, "profile: full") || strings.Contains(out, "brain_dir:") {
		t.Fatalf("text bench output: err=%v\n%s", err, out)
	}
}

func TestSemanticBenchKeepAndFailureCleanupContracts(t *testing.T) {
	opts, repoDir, _ := semanticBenchFixture(t)
	out, err := execute(t, NewRootCommand(opts), "bench", "semantic", repoDir, "--json", "--keep")
	if err != nil {
		t.Fatalf("kept bench: %v\n%s", err, out)
	}
	var kept semanticBenchReport
	if err := json.Unmarshal([]byte(out), &kept); err != nil || !kept.Kept {
		t.Fatalf("kept report: %+v err=%v", kept, err)
	}
	manifest, err := loadBrainManifest(kept.BrainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Semantic == nil || manifest.Sources.Semantic.Symbols != 1 {
		t.Fatalf("kept brain lost indexed evidence: %+v %v", manifest, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(kept.TempRoot) })

	bad := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.1"))
	bad.semanticSnapshotAny = &fakeCommandResponse{err: errors.New("fixture provider failed")}
	for key := range bad.responses {
		if strings.Contains(key, "graph\x00snapshot") {
			bad.responses[key] = fakeCommandResponse{err: errors.New("fixture provider failed")}
		}
	}
	opts.Runner = bad
	before, globErr := filepath.Glob(filepath.Join(os.TempDir(), "entire-brain-graph-bench-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	if err := runSemanticBench(context.Background(), cmd, opts, semanticBenchOptions{graphBinary: "entire"}, repoDir); err == nil || !strings.Contains(err.Error(), "fixture provider failed") {
		t.Fatalf("default failure error = %v", err)
	}
	after, globErr := filepath.Glob(filepath.Join(os.TempDir(), "entire-brain-graph-bench-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if !slices.Equal(after, before) {
		t.Fatalf("default error retained isolated store: before=%v after=%v", before, after)
	}
}

type semanticBenchFailWriter struct{}

func (semanticBenchFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("report writer failed")
}

func TestSemanticBenchJSONWriterFailureKeepsRequestedStore(t *testing.T) {
	opts, repoDir, _ := semanticBenchFixture(t)
	cmd := &cobra.Command{}
	cmd.SetOut(semanticBenchFailWriter{})
	err := runSemanticBench(context.Background(), cmd, opts, semanticBenchOptions{json: true, keep: true, graphBinary: "entire", profile: "syntax-only"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "report writer failed") {
		t.Fatalf("json writer error = %v", err)
	}
	// The only retained store is discoverable and readable, so callers can inspect
	// the completed index after a reporting failure.
	roots, globErr := filepath.Glob(filepath.Join(os.TempDir(), "entire-brain-graph-bench-*"))
	if globErr != nil || len(roots) != 1 {
		t.Fatalf("kept writer-failure store unavailable: roots=%v err=%v", roots, globErr)
	}
	root := roots[len(roots)-1]
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("kept writer-failure store unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
}
