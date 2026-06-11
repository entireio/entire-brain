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

// statusSemanticDetail builds the full status report the way the status
// command does: base report plus the expensive semantic detail pass (coverage
// and blind spots, the former semantic-audit content).
func statusSemanticDetail(t *testing.T, opts Options, repoDir string) brainStatusReport {
	t.Helper()
	report, err := buildBrainStatusReport(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatalf("status report: %v", err)
	}
	populateBrainStatusSemanticDetail(context.Background(), opts, &report)
	return report
}

func TestStatusReportsSemanticCountsFreshnessAndBlindSpots(t *testing.T) {
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
	db, err := sql.Open(sqliteDriverName, storePath)
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

	report := statusSemanticDetail(t, opts, repoDir)
	sem := report.Semantic
	if sem == nil || sem.Coverage == nil {
		t.Fatalf("semantic section missing from status report: %+v", report)
	}
	if sem.Coverage.Symbols != 3 || sem.Coverage.Relations != 2 || sem.Coverage.Files != 3 {
		t.Fatalf("counts missing from semantic coverage: %+v", sem.Coverage)
	}
	if sem.Freshness == nil || sem.Freshness.Severity != "unsafe" || sem.Freshness.Axes["snapshot"].State != "unsafe" {
		t.Fatalf("freshness should report unsafe missing snapshot, got %+v", sem.Freshness)
	}
	if len(sem.BlindSpots) != 1 || sem.BlindSpots[0].Path != "src/broken.ts" {
		t.Fatalf("blind spots missing: %+v", sem.BlindSpots)
	}
	if sem.Coverage.Warnings != 1 || sem.Coverage.PartialFailures != 1 || len(sem.Coverage.WarningDetails) != 1 || len(sem.Coverage.PartialFailureDetails) != 1 {
		t.Fatalf("warning details missing: %+v", sem.Coverage)
	}
	if got := semanticAuditCountSummary(sem.Coverage.FileLanguages); got != "go=1, markdown=1, typescript=1" {
		t.Fatalf("file-language coverage = %q", got)
	}
	if got := semanticAuditCountSummary(sem.Coverage.Languages); got != "go=2, typescript=1" {
		t.Fatalf("symbol-language coverage = %q", got)
	}
	if got := semanticAuditCountSummary(sem.Coverage.SymbolKinds); got != "function=2, struct=1" {
		t.Fatalf("symbol-kind coverage = %q", got)
	}
	if got := semanticAuditCountSummary(sem.Coverage.RelationTypes); got != "calls=1, imports=1" {
		t.Fatalf("relation-type coverage = %q", got)
	}
}

func TestValidateSemanticSQLiteStoreChecksFileCount(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), semanticSQLiteName)
	db, err := sql.Open(sqliteDriverName, storePath)
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

func statusGateFixtureOptions(t *testing.T, repoDir string, partialFailures []semanticWarning) Options {
	t.Helper()
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
			PartialFailures:  partialFailures,
		}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return opts
}

func TestStatusJSONIncludesSemanticSection(t *testing.T) {
	opts := statusGateFixtureOptions(t, t.TempDir(), nil)
	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "status", "--json")
	if err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	var report brainStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, out)
	}
	if report.Semantic == nil || report.Semantic.Provider == nil || report.Semantic.Provider.Name != "entire-sem" {
		t.Fatalf("semantic provider missing: %+v", report.Semantic)
	}
	if report.Semantic.Coverage == nil || report.Semantic.Coverage.Symbols != 3 || report.Semantic.Coverage.Relations != 4 {
		t.Fatalf("semantic coverage missing: %+v", report.Semantic.Coverage)
	}
	// The raw manifest is internal-only: it duplicates the structured sections
	// and its session list scales with brain size.
	if strings.Contains(out, `"manifest"`) {
		t.Fatalf("status JSON must not embed the raw manifest:\n%s", out)
	}
}

func TestStatusFailOnUnsafeAndReleaseEmitJSONBeforeError(t *testing.T) {
	opts := statusGateFixtureOptions(t, t.TempDir(), nil)
	for _, failOn := range []string{"unsafe", "release"} {
		t.Run(failOn, func(t *testing.T) {
			cmd := NewRootCommand(opts)
			out, err := execute(t, cmd, "status", "--json", "--fail-on", failOn)
			if err == nil || !errors.Is(err, errSemanticAuditGate) {
				t.Fatalf("expected semantic audit gate error, got %v\n%s", err, out)
			}
			var report brainStatusReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("decode status JSON after gate failure: %v\n%s", err, out)
			}
			if got := brainStatusFreshnessSeverity(report); got != "unsafe" {
				t.Fatalf("unexpected freshness before gate failure: %q\n%s", got, out)
			}
		})
	}
}

func TestStatusFailOnBlindSpotsEmitsJSONBeforeError(t *testing.T) {
	opts := statusGateFixtureOptions(t, t.TempDir(), []semanticWarning{{
		Code:     "E_PARSE_ERROR",
		Severity: "error",
		Path:     "src/broken.ts",
		Effect:   "symbols omitted",
		Detail:   "tree-sitter syntax error nodes present",
	}})
	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "status", "--json", "--fail-on", "blind-spots")
	if err == nil || !errors.Is(err, errSemanticAuditGate) {
		t.Fatalf("expected blind-spot gate error, got %v\n%s", err, out)
	}
	var report brainStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode status JSON after blind-spot gate failure: %v\n%s", err, out)
	}
	spots := brainStatusBlindSpots(report)
	if len(spots) != 1 || spots[0].Path != "src/broken.ts" || spots[0].Code != "E_PARSE_ERROR" {
		t.Fatalf("unexpected blind-spot report before gate failure: %+v", spots)
	}
}

func TestSemanticAuditFailurePolicies(t *testing.T) {
	tests := []struct {
		name       string
		failOn     string
		severity   string
		blindSpots int
		wantErr    bool
	}{
		{name: "none ignores unsafe and blind spots", failOn: semanticAuditFailOnNone, severity: "unsafe", blindSpots: 1},
		{name: "unsafe fails unsafe", failOn: semanticAuditFailOnUnsafe, severity: "unsafe", wantErr: true},
		{name: "unsafe ignores degraded", failOn: semanticAuditFailOnUnsafe, severity: "degraded"},
		{name: "degraded fails degraded", failOn: semanticAuditFailOnDegraded, severity: "degraded", wantErr: true},
		{name: "degraded fails unsafe", failOn: semanticAuditFailOnDegraded, severity: "unsafe", wantErr: true},
		{name: "blind spots ignores clean report", failOn: semanticAuditFailOnBlindSpots, severity: "ok"},
		{name: "blind spots fails spots", failOn: semanticAuditFailOnBlindSpots, severity: "ok", blindSpots: 1, wantErr: true},
		{name: "release accepts clean report", failOn: semanticAuditFailOnRelease, severity: "ok"},
		{name: "release fails unsafe freshness", failOn: semanticAuditFailOnRelease, severity: "unsafe", wantErr: true},
		{name: "release fails degraded freshness", failOn: semanticAuditFailOnRelease, severity: "degraded", wantErr: true},
		{name: "release fails blind spots", failOn: semanticAuditFailOnRelease, severity: "ok", blindSpots: 1, wantErr: true},
		// A brain with no semantic source has no severity at all; a release
		// health check must not treat that as healthy.
		{name: "release fails missing semantic source", failOn: semanticAuditFailOnRelease, severity: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := semanticAuditFailureForReport(tc.severity, tc.blindSpots, tc.failOn)
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

func TestStatusSkipsUnsafeStoreCoverage(t *testing.T) {
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

	report := statusSemanticDetail(t, opts, repoDir)
	sem := report.Semantic
	if sem == nil || sem.Freshness == nil {
		t.Fatalf("semantic section missing: %+v", report)
	}
	if axis := sem.Freshness.Axes["store"]; axis.State != "unsafe" {
		t.Fatalf("store axis should be unsafe for outside-generation path: %+v", axis)
	}
	if c := sem.Coverage; c == nil || len(c.FileLanguages) != 0 || len(c.Languages) != 0 || len(c.RelationTypes) != 0 {
		t.Fatalf("unsafe store should not be opened for coverage: %+v", c)
	}
}
