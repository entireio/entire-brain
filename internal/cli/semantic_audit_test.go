package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	generationRel := filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, "audit"))
	storeRel := filepath.ToSlash(filepath.Join(generationRel, semanticSQLiteName))
	storePath := filepath.Join(storage.BrainDir, filepath.FromSlash(storeRel))
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeSemanticSQLite(db); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO files(path, blob, content_hash, language) VALUES ('a.go', 'blob-a', 'hash-a', 'go')`,
		`INSERT INTO files(path, blob, content_hash, language) VALUES ('component.unknown', 'blob-c', 'hash-c', 'typescript')`,
		`INSERT INTO files(path, blob, content_hash, language) VALUES ('README.md', 'blob-readme', 'hash-readme', 'markdown')`,
		`INSERT INTO symbols(id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version) VALUES ('s1', 'function', 'A', 'A', 'a.go', 1, 2, '', 'go', 'v1')`,
		`INSERT INTO symbols(id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version) VALUES ('s2', 'struct', 'B', 'B', 'a.go', 3, 4, '', 'go', 'v1')`,
		`INSERT INTO symbols(id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version) VALUES ('s3', 'function', 'C', 'C', 'component.unknown', 1, 2, '', 'typescript', 'v1')`,
		`INSERT INTO relations(from_id, to_id, type, confidence, reason, warning_codes) VALUES ('s1', 's2', 'calls', 1, '', '[]')`,
		`INSERT INTO relations(from_id, to_id, type, confidence, reason, warning_codes) VALUES ('s3', 's1', 'imports', 1, '', '[]')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		Sources: &brainSources{Semantic: &semanticSourceManifest{
			Provider:         "entire-sem",
			ProviderVersion:  "0.1.0",
			SchemaVersion:    "1.0",
			Commit:           "headsha",
			Branch:           "main",
			GenerationPath:   generationRel,
			StorePath:        storeRel,
			Files:            3,
			Symbols:          3,
			Relations:        2,
			Capabilities:     []string{"relations", "symbols"},
			NoEgressVerified: true,
			Warnings: []semanticWarning{{
				Code: "W_PARTIAL_LANGUAGE", Severity: "warning", Path: "src/a.go", Detail: "language coverage is partial",
			}},
			PartialFailures: []semanticWarning{{
				Code: "E_PARSE_ERROR", Severity: "error", Path: "src/broken.ts", Effect: "symbols omitted", Detail: "tree-sitter syntax error nodes present",
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
	if report.Symbols != 3 || report.Relations != 2 || report.Files != 3 {
		t.Fatalf("counts missing from audit report: %+v", report)
	}
	if report.Freshness.Severity != "unsafe" || report.Freshness.Axes["snapshot"].State != "unsafe" {
		t.Fatalf("freshness should report unsafe missing snapshot, got %+v", report.Freshness)
	}
	if len(report.BlindSpots) != 1 || report.BlindSpots[0].Path != "src/broken.ts" {
		t.Fatalf("blind spots missing: %+v", report.BlindSpots)
	}
	if report.Warnings != 1 || report.Failures != 1 || len(report.WarningDetails) != 1 || len(report.PartialFailureDetails) != 1 {
		t.Fatalf("warning details missing: %+v", report)
	}
	if got := semanticAuditCountSummary(report.FileLanguages); got != "go=1, markdown=1, typescript=1" {
		t.Fatalf("file-language coverage = %q", got)
	}
	if got := semanticAuditCountSummary(report.Languages); got != "go=2, typescript=1" {
		t.Fatalf("symbol-language coverage = %q", got)
	}
	if got := semanticAuditCountSummary(report.SymbolKinds); got != "function=2, struct=1" {
		t.Fatalf("symbol-kind coverage = %q", got)
	}
	if got := semanticAuditCountSummary(report.RelationTypes); got != "calls=1, imports=1" {
		t.Fatalf("relation-type coverage = %q", got)
	}
}

func TestValidateSemanticSQLiteStoreChecksFileCount(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), semanticSQLiteName)
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeSemanticSQLite(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO files(path, blob, content_hash) VALUES ('a.go', 'blob', 'hash')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	err = validateSemanticSQLiteStore(storePath, 2, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "file count 1 does not match manifest 2") {
		t.Fatalf("expected file count mismatch, got %v", err)
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

func TestSemanticAuditFailOnUnsafeEmitsJSONBeforeError(t *testing.T) {
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
	out, err := execute(t, cmd, "semantic-audit", "--json", "--fail-on", "unsafe")
	if err == nil || !errors.Is(err, errSemanticAuditGate) {
		t.Fatalf("expected semantic audit gate error, got %v\n%s", err, out)
	}
	var report semanticAuditReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode audit JSON after gate failure: %v\n%s", err, out)
	}
	if report.Freshness.Severity != "unsafe" || report.Provider != "entire-sem" {
		t.Fatalf("unexpected audit report before gate failure: %+v", report)
	}
}

func TestSemanticAuditFailOnBlindSpotsEmitsJSONBeforeError(t *testing.T) {
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
			PartialFailures: []semanticWarning{{
				Code:     "E_PARSE_ERROR",
				Severity: "error",
				Path:     "src/broken.ts",
				Effect:   "symbols omitted",
				Detail:   "tree-sitter syntax error nodes present",
			}},
		}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "semantic-audit", "--json", "--fail-on", "blind-spots")
	if err == nil || !errors.Is(err, errSemanticAuditGate) {
		t.Fatalf("expected semantic audit blind-spot gate error, got %v\n%s", err, out)
	}
	var report semanticAuditReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode audit JSON after blind-spot gate failure: %v\n%s", err, out)
	}
	if len(report.BlindSpots) != 1 || report.BlindSpots[0].Path != "src/broken.ts" || report.BlindSpots[0].Code != "E_PARSE_ERROR" {
		t.Fatalf("unexpected blind-spot report before gate failure: %+v", report.BlindSpots)
	}
}

func TestSemanticAuditFailurePolicies(t *testing.T) {
	unsafeReport := semanticAuditReport{Freshness: staleReport{Severity: "unsafe"}}
	degradedReport := semanticAuditReport{Freshness: staleReport{Severity: "degraded"}}
	okReport := semanticAuditReport{Freshness: staleReport{Severity: "ok"}}
	blindSpotReport := semanticAuditReport{
		Freshness:  staleReport{Severity: "ok"},
		BlindSpots: []brainBlindSpot{{Path: "src/broken.ts"}},
	}
	tests := []struct {
		name    string
		failOn  string
		report  semanticAuditReport
		wantErr bool
	}{
		{name: "none ignores unsafe and blind spots", failOn: semanticAuditFailOnNone, report: semanticAuditReport{Freshness: staleReport{Severity: "unsafe"}, BlindSpots: []brainBlindSpot{{Path: "src/broken.ts"}}}},
		{name: "unsafe fails unsafe", failOn: semanticAuditFailOnUnsafe, report: unsafeReport, wantErr: true},
		{name: "unsafe ignores degraded", failOn: semanticAuditFailOnUnsafe, report: degradedReport},
		{name: "degraded fails degraded", failOn: semanticAuditFailOnDegraded, report: degradedReport, wantErr: true},
		{name: "degraded fails unsafe", failOn: semanticAuditFailOnDegraded, report: unsafeReport, wantErr: true},
		{name: "blind spots ignores clean report", failOn: semanticAuditFailOnBlindSpots, report: okReport},
		{name: "blind spots fails spots", failOn: semanticAuditFailOnBlindSpots, report: blindSpotReport, wantErr: true},
		{name: "release accepts clean report", failOn: semanticAuditFailOnRelease, report: okReport},
		{name: "release fails unsafe freshness", failOn: semanticAuditFailOnRelease, report: unsafeReport, wantErr: true},
		{name: "release fails degraded freshness", failOn: semanticAuditFailOnRelease, report: degradedReport, wantErr: true},
		{name: "release fails blind spots", failOn: semanticAuditFailOnRelease, report: blindSpotReport, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := semanticAuditFailureForReport(tc.report, tc.failOn)
			if tc.wantErr {
				if err == nil || !errors.Is(err, errSemanticAuditGate) {
					t.Fatalf("expected gate error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected gate error: %v", err)
			}
		})
	}
}

func TestSemanticAuditSkipsUnsafeStoreCoverage(t *testing.T) {
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
			GenerationPath:   filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, "safe")),
			StorePath:        filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, "other", semanticSQLiteName)),
			Files:            2,
			Symbols:          3,
			Relations:        4,
			NoEgressVerified: true,
		}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}

	report, err := buildSemanticAuditReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("semantic audit should report unsafe freshness instead of failing path validation: %v", err)
	}
	if axis := report.Freshness.Axes["store"]; axis.State != "unsafe" {
		t.Fatalf("store axis should be unsafe for outside-generation path: %+v", axis)
	}
	if len(report.FileLanguages) != 0 || len(report.Languages) != 0 || len(report.RelationTypes) != 0 {
		t.Fatalf("unsafe store should not be opened for coverage: %+v", report)
	}
}
