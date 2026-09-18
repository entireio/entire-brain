package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func refreshFailureFixture(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	opts := Options{
		Version: "test", Runner: runner,
		Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()},
		Now: func() time.Time { return time.Date(2026, 9, 18, 17, 0, 0, 0, time.UTC) },
	}
	if out, err := execute(t, NewRootCommand(opts), "refresh", "--entire-binary", "entire-test", "--semantic=false", "--agent", "none"); err != nil {
		t.Fatalf("baseline refresh: %v\n%s", err, out)
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

func replaceRefreshComponentDirWithFile(t *testing.T, brainDir, rel, marker string) {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshBestEffortReportsHistoryAndDocsFailuresAndPreservesInputs(t *testing.T) {
	opts, brainDir := refreshFailureFixture(t)
	historyMarker := "committed-history-directory-marker"
	docsMarker := "committed-doc-directory-marker"
	replaceRefreshComponentDirWithFile(t, brainDir, historyDirName, historyMarker)
	replaceRefreshComponentDirWithFile(t, brainDir, docDirName, docsMarker)

	outcomes := map[string]error{}
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := runRefresh(context.Background(), cmd, opts, refreshCommandOptions{
		outputDir: defaultExportDir, checkpointLimit: defaultCheckpointLimit,
		entireBinary: "entire-test", graphBinary: "entire", scope: exportScopeAll,
		skipSessions: true, historyIndex: true,
		seed:      seedCommandOptions{agent: "none"},
		component: func(name string, err error) { outcomes[name] = err },
	})
	if err != nil {
		t.Fatalf("best effort returned aggregate error instead of component outcomes: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	for _, name := range []string{brainComponentSeed, brainComponentHistory, brainComponentDocs} {
		if _, ok := outcomes[name]; !ok {
			t.Fatalf("component %q was not reported: %+v", name, outcomes)
		}
	}
	if outcomes[brainComponentSeed] != nil || outcomes[brainComponentHistory] == nil || outcomes[brainComponentDocs] == nil {
		t.Fatalf("inaccurate component outcomes: %+v", outcomes)
	}
	if !strings.Contains(stderr.String(), "history index") || !strings.Contains(stderr.String(), "doc index") || !strings.Contains(stdout.String(), "refreshed brain:") {
		t.Fatalf("best-effort reporting incomplete\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	for rel, want := range map[string]string{historyDirName: historyMarker, docDirName: docsMarker} {
		got, err := os.ReadFile(filepath.Join(brainDir, rel))
		if err != nil || string(got) != want {
			t.Fatalf("failed %s input was replaced: got=%q err=%v", rel, got, err)
		}
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Seed == nil {
		t.Fatalf("successful seed stage was not committed: manifest=%+v err=%v", manifest, err)
	}
}

func TestRefreshStrictHistoryFailureStopsBeforeSuccess(t *testing.T) {
	opts, brainDir := refreshFailureFixture(t)
	marker := "strict-history-marker"
	replaceRefreshComponentDirWithFile(t, brainDir, historyDirName, marker)
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := runRefresh(context.Background(), cmd, opts, refreshCommandOptions{
		outputDir: defaultExportDir, checkpointLimit: defaultCheckpointLimit,
		entireBinary: "entire-test", graphBinary: "entire", scope: exportScopeAll,
		skipSessions: true, historyIndex: true,
		seed: seedCommandOptions{agent: "none"},
	})
	if err == nil || !strings.Contains(err.Error(), historyDirName) {
		t.Fatalf("strict history error=%v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "refreshed brain:") || strings.Contains(stderr.String(), "doc index") {
		t.Fatalf("strict refresh reported later success after failure\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	got, readErr := os.ReadFile(filepath.Join(brainDir, historyDirName))
	if readErr != nil || string(got) != marker {
		t.Fatalf("strict failure replaced committed marker: got=%q err=%v", got, readErr)
	}
}

func TestRefreshBestEffortReportsFactsFailure(t *testing.T) {
	opts, brainDir := refreshFailureFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Facts = &factSourceManifest{GeneratedAt: opts.Now(), TaxonomyPath: factsTaxonomyPath, Branches: []string{"main"}, Facts: 1}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	factPath := filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("main")))
	if err := os.MkdirAll(filepath.Dir(factPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(factPath, []byte("{broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reported := map[string]error{}
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err = runRefresh(context.Background(), cmd, opts, refreshCommandOptions{
		outputDir: defaultExportDir, checkpointLimit: defaultCheckpointLimit,
		entireBinary: "entire-test", graphBinary: "entire", scope: exportScopeAll,
		historyIndex: false, seed: seedCommandOptions{agent: "none"},
		component: func(name string, err error) { reported[name] = err },
	})
	if err != nil {
		t.Fatalf("best effort facts/patterns: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if reported[brainComponentFacts] == nil {
		t.Fatalf("missing component failures: %#v\n%s", reported, stderr.String())
	}
	if !strings.Contains(stdout.String(), "refreshed brain:") || !strings.Contains(stderr.String(), "reclassify facts") {
		t.Fatalf("incomplete best-effort report\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if got, err := os.ReadFile(factPath); err != nil || string(got) != "{broken\n" {
		t.Fatalf("facts failure replaced corrupt input: %q err=%v", got, err)
	}
}

func TestRefreshRejectsInvalidOptionsBeforeMutation(t *testing.T) {
	opts, _ := refreshFailureFixture(t)
	before := privacyTreeDigest(t, opts.Env.RepoRoot)
	cmd := &cobra.Command{}
	if err := runRefresh(context.Background(), cmd, opts, refreshCommandOptions{allBranches: true}); err == nil || !strings.Contains(err.Error(), "--all-branches requires --semantic") {
		t.Fatalf("all-branches validation=%v", err)
	}
	cmd.Flags().String("output", "", "")
	if err := cmd.Flags().Set("output", ""); err != nil {
		t.Fatal(err)
	}
	if err := runRefresh(context.Background(), cmd, opts, refreshCommandOptions{}); err == nil || !strings.Contains(err.Error(), "--output must not be empty") {
		t.Fatalf("empty output validation=%v", err)
	}
	if privacyTreeDigest(t, opts.Env.RepoRoot) != before {
		t.Fatal("invalid refresh options mutated repository")
	}
	for name, output := range map[string]struct {
		path  string
		force bool
	}{
		"existing output":  {path: t.TempDir()},
		"forced repo root": {path: opts.Env.RepoRoot, force: true},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("output", "", "")
			if err := cmd.Flags().Set("output", output.path); err != nil {
				t.Fatal(err)
			}
			err := runRefresh(context.Background(), cmd, opts, refreshCommandOptions{outputDir: output.path, force: output.force})
			if err == nil {
				t.Fatalf("unsafe output %q unexpectedly accepted", output.path)
			}
		})
	}
}

func TestRefreshBestEffortReportsMissingSemanticProvider(t *testing.T) {
	opts, _ := refreshFailureFixture(t)
	reported := map[string]error{}
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := runRefresh(context.Background(), cmd, opts, refreshCommandOptions{
		outputDir: defaultExportDir, checkpointLimit: defaultCheckpointLimit,
		entireBinary: "entire-test", graphBinary: "definitely-missing-entire-graph", scope: exportScopeAll,
		skipSessions: true, semantic: true, seed: seedCommandOptions{agent: "none"},
		component: func(name string, err error) { reported[name] = err },
	})
	if err != nil {
		t.Fatalf("missing semantic provider should be component-scoped: %v\n%s", err, stderr.String())
	}
	if reported[brainComponentSemantic] == nil || !strings.Contains(stderr.String(), "semantic") || !strings.Contains(stdout.String(), "refreshed brain:") {
		t.Fatalf("semantic outcome/report missing: %#v\nstdout=%s\nstderr=%s", reported, stdout.String(), stderr.String())
	}
}
