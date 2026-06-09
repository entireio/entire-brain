package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSemanticAuditReportsCountsFreshnessAndBlindSpots(t *testing.T) {
	repoDir := t.TempDir()
	dataDir := t.TempDir()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                   {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):                    {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                              {stdout: "headsha\n"},
		fakeCommandKey("git", "branch", "--show-current"):                       {stdout: "main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                          {},
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {},
	}}
	opts := Options{
		Env:    EntireEnv{RepoRoot: repoDir, PluginDataDir: dataDir},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		Sources: &brainSources{Semantic: &semanticSourceManifest{
			Provider:         "entire-sem",
			ProviderVersion:  "0.1.0",
			SchemaVersion:    "1.0",
			Commit:           "headsha",
			Branch:           "main",
			Files:            2,
			Symbols:          3,
			Relations:        4,
			NoEgressVerified: true,
			PartialFailures: []semanticWarning{{
				Code: "E_PARSE_ERROR", Path: "src/broken.ts", Detail: "tree-sitter syntax error nodes present",
			}},
		}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}

	report, err := buildSemanticAuditReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("semantic audit: %v", err)
	}
	if report.Symbols != 3 || report.Relations != 4 || report.Files != 2 {
		t.Fatalf("counts missing from audit report: %+v", report)
	}
	if report.Freshness.Severity != "unsafe" || report.Freshness.Axes["snapshot"].State != "unsafe" {
		t.Fatalf("freshness should report unsafe missing snapshot, got %+v", report.Freshness)
	}
	if len(report.BlindSpots) != 1 || report.BlindSpots[0].Path != "src/broken.ts" {
		t.Fatalf("blind spots missing: %+v", report.BlindSpots)
	}
}

func TestSemanticAuditCommandJSON(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                   {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):                    {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                              {stdout: "headsha\n"},
		fakeCommandKey("git", "branch", "--show-current"):                       {stdout: "main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                          {},
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {},
	}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		Sources: &brainSources{Semantic: &semanticSourceManifest{
			Provider:         "entire-sem",
			ProviderVersion:  "0.1.0",
			SchemaVersion:    "1.0",
			Commit:           "headsha",
			Branch:           "main",
			Files:            2,
			Symbols:          3,
			Relations:        4,
			NoEgressVerified: true,
		}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "semantic-audit", "--json")
	if err != nil {
		t.Fatalf("semantic-audit --json: %v\n%s", err, out)
	}
	var report semanticAuditReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode audit JSON: %v\n%s", err, out)
	}
	if report.Provider != "entire-sem" || report.Symbols != 3 || report.Relations != 4 {
		t.Fatalf("unexpected audit report: %+v", report)
	}
}
