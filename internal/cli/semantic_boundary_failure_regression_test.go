package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSemanticIngestRejectsTraceInputBeforePublishing(t *testing.T) {
	brainDir, _, opts := indexFixtureBrain(t, semanticFixtureSnapshot("1.0"))
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Semantic == nil {
		t.Fatalf("semantic manifest: %+v err=%v", manifest.Sources, err)
	}
	priorGeneration := manifest.Sources.Semantic.GenerationPath

	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "missing", want: "no such file"},
		{name: "malformed", data: []byte(`{"from":`), want: "unexpected end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "trace.ndjson")
			if tc.data != nil {
				if err := os.WriteFile(path, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := &cobra.Command{Use: "ingest-traces"}
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			err := runSemanticIngestTraces(cmd, opts, semanticTraceIngestOptions{json: true}, path)
			missing := tc.name == "missing" && errors.Is(err, os.ErrNotExist)
			malformed := tc.name == "malformed" && err != nil && strings.Contains(strings.ToLower(err.Error()), tc.want)
			if err == nil || (!missing && !malformed) {
				t.Fatalf("ingest error=%v, want substring %q", err, tc.want)
			}
			after, readErr := os.ReadFile(manifestPath)
			if readErr != nil || !bytes.Equal(after, before) {
				t.Fatalf("failed ingest changed manifest: read=%v bytes_equal=%v", readErr, bytes.Equal(after, before))
			}
			current, loadErr := loadBrainManifest(brainDir)
			if loadErr != nil || current.Sources == nil || current.Sources.Semantic == nil || current.Sources.Semantic.GenerationPath != priorGeneration {
				t.Fatalf("failed ingest changed generation: manifest=%+v err=%v", current, loadErr)
			}
		})
	}
}

func TestSemanticRuntimeTraceFactsRejectsInvalidDeclaredStore(t *testing.T) {
	brainDir := t.TempDir()
	for _, source := range []*semanticSourceManifest{
		{StorePath: "../outside/semantic.sqlite"},
		{StorePath: filepath.ToSlash(filepath.Join(semanticDirName, "missing", semanticSQLiteName))},
	} {
		records, err := semanticRuntimeTraceFacts(brainDir, source, "", 10)
		if err == nil || records != nil {
			t.Fatalf("invalid store source returned records=%v err=%v", records, err)
		}
	}
	if records, err := semanticRuntimeTraceFacts(brainDir, &semanticSourceManifest{}, "", 10); err != nil || len(records) != 0 {
		t.Fatalf("empty store source = records=%v err=%v, want empty", records, err)
	}
	if records, err := semanticRuntimeTraceFacts(brainDir, &semanticSourceManifest{StorePath: "ignored"}, "", 0); err != nil || len(records) != 0 {
		t.Fatalf("zero limit = records=%v err=%v, want empty", records, err)
	}
}

func TestSemanticBoundaryPropagatesStorageDirectoryResolutionFailure(t *testing.T) {
	opts := boundaryRegressionFixture(t, semanticBoundaryFixtureSnapshot())
	opts.Env.PluginDataDir = "relative-data-dir"
	cmd := &cobra.Command{Use: "boundaries"}
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var output bytes.Buffer
	cmd.SetOut(&output)
	err := runSemanticBoundary(cmd.Context(), cmd, opts, semanticBoundaryOptions{limit: 10, json: true}, semanticBoundarySpec{
		Name: "routes", SymbolKinds: []string{"route"}, RelationTypes: []string{"HANDLES_ROUTE"},
	})
	if err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("storage directory failure error=%v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("storage directory failure wrote output: %q", output.String())
	}
}

func TestSemanticBoundaryPropagatesGetwdFailureInUnixChild(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("this platform retains a deleted current directory for os.Getwd; Linux Unix CI exercises the ENOENT branch")
	}
	if os.Getenv("ENTIRE_TEST_SEMANTIC_BOUNDARY_GETWD_HELPER") == "1" {
		dir, err := os.MkdirTemp("", "semantic-boundary-getwd-")
		if err != nil {
			os.Exit(2)
		}
		if err := os.Chdir(dir); err != nil {
			os.Exit(3)
		}
		if err := os.RemoveAll(dir); err != nil {
			os.Exit(4)
		}
		_ = os.Unsetenv("PWD")
		cmd := &cobra.Command{Use: "boundaries"}
		var output bytes.Buffer
		cmd.SetOut(&output)
		err = runSemanticBoundary(cmd.Context(), cmd, Options{}, semanticBoundaryOptions{limit: 1, json: true}, semanticBoundarySpec{Name: "routes"})
		if !errors.Is(err, os.ErrNotExist) || output.Len() != 0 {
			fmt.Fprintf(os.Stderr, "err=%v notexist=%v output=%q\n", err, errors.Is(err, os.ErrNotExist), output.String())
			os.Exit(5)
		}
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestSemanticBoundaryPropagatesGetwdFailureInUnixChild$", "-test.v=false")
	child.Env = append(os.Environ(), "ENTIRE_TEST_SEMANTIC_BOUNDARY_GETWD_HELPER=1")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("isolated Getwd helper failed: %v\n%s", err, output)
	}
}

func TestSemanticBoundaryReportsMissingLocalRepoDuringFreshnessCheck(t *testing.T) {
	opts := boundaryRegressionFixture(t, semanticBoundaryFixtureSnapshot())
	brainDir := filepath.Join(opts.Env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(opts.Env.RepoRoot); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "boundaries"}
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var output bytes.Buffer
	cmd.SetOut(&output)
	err = runSemanticBoundary(cmd.Context(), cmd, opts, semanticBoundaryOptions{limit: 10, json: true}, semanticBoundarySpec{Name: "routes"})
	if err == nil || !strings.Contains(err.Error(), "stale requires a local repository path") {
		t.Fatalf("missing local repo error=%v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("missing local repo wrote output: %q", output.String())
	}
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || !bytes.Equal(after, before) {
		t.Fatalf("missing local repo changed manifest: read=%v equal=%v", readErr, bytes.Equal(after, before))
	}
}
