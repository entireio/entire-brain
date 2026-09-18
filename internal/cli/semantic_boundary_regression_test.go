package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func boundaryRegressionFixture(t *testing.T, snapshot string) Options {
	t.Helper()
	repoDir := t.TempDir()
	opts := Options{
		Env: semanticTestEnv(t, repoDir), Runner: semanticFixtureRunner(repoDir, snapshot),
		Now: func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) },
	}
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestSemanticBoundaryRootTextJSONAndLimits(t *testing.T) {
	snapshot := strings.Replace(semanticBoundaryFixtureSnapshot(), `{"record_type":"summary"}`, `{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"external:route:POST /tokens","type":"HANDLES_ROUTE","confidence":1}
{"record_type":"summary"}`, 1)
	opts := boundaryRegressionFixture(t, snapshot)
	for _, tc := range []struct{ kind, boundary, relation string }{
		{"route", "GET /tokens/{id}", "HANDLES_ROUTE"},
		{"tool", "brain refresh", "HANDLES_TOOL"},
		{"workflow", "token validation", "HANDLES_WORKFLOW"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(opts), "inspect", "boundaries", "--kind", tc.kind)
			if err != nil || !strings.Contains(out, tc.boundary) || !strings.Contains(out, tc.relation) || !strings.Contains(out, "handler auth.ValidateToken internal/auth/token.go:10-20") {
				t.Fatalf("boundary text: err=%v output=%q", err, out)
			}
			if tc.kind == "route" && !strings.Contains(out, "POST /tokens") {
				t.Fatalf("external boundary omitted: %q", out)
			}
			out, err = execute(t, NewRootCommand(opts), "inspect", "boundaries", "--kind", tc.kind, "--limit", "1", "--json")
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Boundary semanticBoundaryResult `json:"boundary"`
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("JSON output: %v\n%s", err, out)
			}
			if len(result.Boundary.Boundaries) != 1 || len(result.Boundary.Handlers) != 1 || len(result.Boundary.Relations) != 1 {
				t.Fatalf("limit must keep one boundary with only its handler/relation: %+v", result.Boundary)
			}
			if result.Boundary.Relations[0].Type != tc.relation || result.Boundary.Handlers[0].Name != "ValidateToken" {
				t.Fatalf("boundary provenance mismatch: %+v", result.Boundary)
			}
		})
	}
}

func TestSemanticBoundaryRootEmptyAndInvalidState(t *testing.T) {
	opts := boundaryRegressionFixture(t, semanticFixtureSnapshot("1.0"))
	out, err := execute(t, NewRootCommand(opts), "inspect", "boundaries", "--kind", "route")
	if err != nil || !strings.Contains(out, "no routes found in the semantic index") {
		t.Fatalf("empty boundary text: err=%v output=%q", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "inspect", "boundaries", "--kind", "route", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Boundary semanticBoundaryResult `json:"boundary"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Boundary.Boundaries == nil || result.Boundary.Handlers == nil || result.Boundary.Relations == nil || len(result.Boundary.Boundaries)+len(result.Boundary.Handlers)+len(result.Boundary.Relations) != 0 {
		t.Fatalf("empty JSON must have empty arrays: %+v err=%v", result, err)
	}
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "-1"}, {"--kind", "invalid"}} {
		out, err := execute(t, NewRootCommand(opts), append([]string{"inspect", "boundaries"}, args...)...)
		if err == nil {
			t.Fatalf("invalid args %v succeeded: %q", args, out)
		}
	}
	brainDir := filepath.Join(opts.Env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := execute(t, NewRootCommand(opts), "inspect", "boundaries", "--json"); err == nil || strings.Contains(out, `"boundary"`) {
		t.Fatalf("corrupt manifest should fail without results: err=%v output=%q", err, out)
	}
}

func TestSemanticBoundaryRootMissingStaleAndCorruptIndex(t *testing.T) {
	t.Run("missing semantic source", func(t *testing.T) {
		opts := boundaryRegressionFixture(t, semanticBoundaryFixtureSnapshot())
		brainDir := filepath.Join(opts.Env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		manifest.Sources.Semantic = nil
		if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(opts), "inspect", "boundaries")
		if err == nil || !strings.Contains(err.Error(), "semantic index missing") || strings.Contains(out, "handler ") {
			t.Fatalf("missing source should fail without boundaries: err=%v output=%q", err, out)
		}
	})
	t.Run("stale index remains explicitly labelled", func(t *testing.T) {
		opts := boundaryRegressionFixture(t, semanticBoundaryFixtureSnapshot())
		runner := opts.Runner.(*fakeCommandRunner)
		runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "bbb222\n"}
		out, err := execute(t, NewRootCommand(opts), "inspect", "boundaries", "--kind", "route")
		if err != nil || !strings.Contains(out, "semantic freshness: unsafe") || !strings.Contains(out, "GET /tokens/{id}") {
			t.Fatalf("stale index should label retained results: err=%v output=%q", err, out)
		}
	})
	t.Run("corrupt store cannot report an empty successful result", func(t *testing.T) {
		opts := boundaryRegressionFixture(t, semanticBoundaryFixtureSnapshot())
		brainDir := filepath.Join(opts.Env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath)), []byte("not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(opts), "inspect", "boundaries", "--json")
		if err == nil || strings.Contains(out, `"boundary"`) {
			t.Fatalf("corrupt store should fail without results: err=%v output=%q", err, out)
		}
	})
}
