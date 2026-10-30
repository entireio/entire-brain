package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	"github.com/spf13/cobra"
)

// The provider contract is also tested against real Graph in its repository;
// this fixture isolates Brain's lifetime and read-only guarantees.
func newGraphStoreFixture(t *testing.T) (Options, string, string, graphStoreDescriptor) {
	t.Helper()
	repo := t.TempDir()
	env := semanticTestEnv(t, repo)
	runner := semanticFixtureRunner(repo, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repo)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	db, err := sql.Open(sqlitedriver.Name, path)
	if err != nil {
		t.Fatal(err)
	}
	if err = initializeSemanticSQLite(db); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []semanticRecord{{ID: "render", Kind: "function", Name: "Render", QualifiedName: "demo.Render", FilePath: "main.go", StartLine: 2, EndLine: 3, Language: "Go"}, {ID: "normalize", Kind: "function", Name: "Normalize", QualifiedName: "demo.Normalize", FilePath: "main.go", StartLine: 4, EndLine: 5, Language: "Go"}} {
		if err = insertSemanticSymbol(tx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err = insertSemanticRelation(tx, semanticRecord{FromID: "render", ToID: "normalize", Type: "CALLS", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"symbols", "relations", "files", "reverse_relations", "symbol_fts"} {
		if _, err = db.Exec("CREATE VIEW brain_" + name + "_v1 AS SELECT * FROM " + name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO meta(key,value) VALUES('entry_digest','"digest"')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	d := graphStoreDescriptor{ContractVersion: 1, Path: path, EntryDigest: "digest", Commit: "aaa111", Tree: "tree111", Files: 1, Symbols: 2, Relations: 1, Header: semanticHeader{Provider: "entire-graph", ProviderVersion: "test", SchemaVersion: "1.0", Profile: "full"}}
	data, _ := json.Marshal(d)
	runner.responses[fakeCommandKey("entire", "graph", "store", "--repo", repo, "--profile", "full")] = fakeCommandResponse{stdout: string(data)}
	runner.responses[fakeCommandKey("entire", "graph", "index", "--repo", repo, "--head", "--profile", "full", "--format", "json")] = fakeCommandResponse{stdout: `{}`}
	return opts, repo, storage.BrainDir, d
}

func TestGraphOwnedIndexHasNoBrainGraphArtifacts(t *testing.T) {
	opts, repo, brain, d := newGraphStoreFixture(t)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runSemanticIndex(context.Background(), cmd, opts, semanticIndexOptions{}, repo); err != nil {
		t.Fatal(err)
	}
	m, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	s := m.Sources.Semantic
	if s.StoreOwner != "graph" || s.StorePath != "" || s.SnapshotPath != "" || s.GenerationPath != "" {
		t.Fatalf("persisted a duplicate or local handle: %+v", s)
	}
	for _, rel := range []string{"semantic/generations", "semantic/snapshots"} {
		if _, err := os.Stat(filepath.Join(brain, rel)); !os.IsNotExist(err) {
			t.Fatalf("%s exists: %v", rel, err)
		}
	}
	before, _ := os.ReadFile(d.Path)
	for _, run := range []func() error{
		func() error {
			return runSemanticQuery(context.Background(), cmd, opts, semanticQueryOptions{limit: 10, json: true}, "Render")
		},
		func() error {
			return runSemanticContext(context.Background(), cmd, opts, semanticContextOptions{limit: 10, json: true}, "Render")
		},
		func() error {
			return runSemanticImpact(context.Background(), cmd, opts, semanticImpactOptions{limit: 10, depth: 2, json: true}, "Render")
		},
	} {
		out.Reset()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Render") {
			t.Fatalf("missing symbol: %s", out.String())
		}
	}
	syms, err := semstore.LoadSymbols(brain, semstore.GraphStoreSource(d, brain), 10)
	if err != nil || len(syms) != 2 {
		t.Fatalf("dashboard Graph symbols: %v, %v", syms, err)
	}
	after, _ := os.ReadFile(d.Path)
	if !bytes.Equal(before, after) {
		t.Fatal("Brain changed Graph's store")
	}
	db, err := semstore.OpenReadDB(semstore.GraphStoreSource(d, brain).StorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("DELETE FROM main.symbols"); err == nil {
		t.Fatal("Graph store was writable")
	}
}

func TestGraphStoreDescriptorRaceRefusesReplacement(t *testing.T) {
	_, _, brain, d := newGraphStoreFixture(t)
	d.EntryDigest = "stale"
	if db, err := semstore.OpenReadDB(semstore.GraphStoreSource(d, brain).StorePath); err == nil {
		db.Close()
		t.Fatal("served replacement store")
	}
}

func TestGraphAbsentBriefStillWorksWithLegacyStore(t *testing.T) {
	opts, repo, brain, _ := newGraphStoreFixture(t)
	// Even a corrupt obsolete pointer is ignored by brief; facts/history remain usable.
	if err := writeBrainSemanticSource(brain, "gh/example/repo", &semanticSourceManifest{StorePath: "missing", SnapshotPath: "missing"}); err != nil {
		t.Fatal(err)
	}
	runner := opts.Runner.(*fakeCommandRunner)
	delete(runner.responses, fakeCommandKey("entire", "graph", "store", "--repo", repo, "--profile", "full"))
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runBrainBrief(context.Background(), cmd, opts, brainBriefOptions{json: true, limit: 5}, "modify Render"); err != nil {
		t.Fatal(err)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Semantic.Context.Symbols) != 0 {
		t.Fatal("used obsolete Brain graph")
	}
}

func TestGraphTraceBundleRoundTripContainsNoCodeStore(t *testing.T) {
	opts, repo, brain, _ := newGraphStoreFixture(t)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runSemanticIndex(context.Background(), cmd, opts, semanticIndexOptions{}, repo); err != nil {
		t.Fatal(err)
	}
	observations := []semstore.TraceObservation{{ImportedAt: "2026-10-22T00:00:00Z", SourcePath: semstore.TraceFile, FromID: "render", ToID: "normalize", ObservedType: "CALLS", Matched: 1}}
	if err := writeGraphTraces(brain, observations); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(context.Background(), cmd, opts, archive, false); err != nil {
		t.Fatal(err)
	}
	entries := readTestBundleEntries(t, archive)
	found := false
	for _, name := range entries {
		if name == semstore.TraceFile {
			found = true
		}
		if strings.HasSuffix(name, ".sqlite") || strings.Contains(name, "semantic/snapshots") {
			t.Fatalf("copied Graph into bundle: %s", name)
		}
	}
	if !found {
		t.Fatalf("lost observations: %v", entries)
	}
	// Import into a fresh Brain under the same repository identity.
	opts.Env.PluginDataDir = t.TempDir()
	if err := runSemanticBundleImport(context.Background(), cmd, opts, archive, bundleSHA256(t, archive), false); err != nil {
		t.Fatal(err)
	}
	dest, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, repo)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := semstore.ReadTraces(dest.BrainDir)
	if err != nil || len(rows) != 1 || rows[0] != observations[0] {
		t.Fatalf("observations changed: %+v (%v)", rows, err)
	}
}

func TestGraphMissingStoreDoesNotCreateAFile(t *testing.T) {
	_, _, brain, d := newGraphStoreFixture(t)
	if err := os.Remove(d.Path); err != nil {
		t.Fatal(err)
	}
	if db, err := semstore.OpenReadDB(semstore.GraphStoreSource(d, brain).StorePath); err == nil {
		db.Close()
		t.Fatal("opened absent provider database")
	}
	if _, err := os.Stat(d.Path); !os.IsNotExist(err) {
		t.Fatalf("read created database: %v", err)
	}
}

func overrideGraphStoreResponse(runner *fakeCommandRunner, repo string, d graphStoreDescriptor) {
	data, _ := json.Marshal(d)
	runner.responses[fakeCommandKey("entire", "graph", "store", "--repo", repo, "--profile", "full")] = fakeCommandResponse{stdout: string(data)}
}

// TestGraphStoreNewerContractNamesBrainUpgrade pins defect (a): a Graph build
// newer than this binary understands must say which binary to upgrade.
// Collapsing both directions into one message left an operator on a newer
// Graph trying to "upgrade Graph" when entire-brain was the stale one.
func TestGraphStoreNewerContractNamesBrainUpgrade(t *testing.T) {
	opts, repo, _, d := newGraphStoreFixture(t)
	d.ContractVersion = 2
	overrideGraphStoreResponse(opts.Runner.(*fakeCommandRunner), repo, d)
	_, err := fetchGraphStore(context.Background(), opts, repo, "", "full")
	if err == nil {
		t.Fatal("expected a contract-newer error")
	}
	if !strings.Contains(err.Error(), "upgrade entire-brain") || !strings.Contains(err.Error(), "contract") {
		t.Fatalf("error does not name entire-brain and the store contract: %v", err)
	}
}

// TestGraphStoreOlderContractNamesGraphUpgrade covers the other direction of
// defect (a): an installed Graph that cannot speak the store contract at all
// (a probe failure) or that reports an older one must still say "upgrade
// Graph", and now also names the contract this build requires.
func TestGraphStoreOlderContractNamesGraphUpgrade(t *testing.T) {
	t.Run("probe_failure", func(t *testing.T) {
		opts, repo, _, _ := newGraphStoreFixture(t)
		runner := opts.Runner.(*fakeCommandRunner)
		runner.responses[fakeCommandKey("entire", "graph", "store", "--repo", repo, "--profile", "full")] = fakeCommandResponse{stderr: `unknown command "graph" for "entire"`, err: errors.New("exit status 1")}
		_, err := fetchGraphStore(context.Background(), opts, repo, "", "full")
		if err == nil {
			t.Fatal("expected an unsupported-store error")
		}
		if !strings.Contains(err.Error(), "upgrade Graph") || !strings.Contains(err.Error(), "contract") {
			t.Fatalf("error does not name Graph and the required store contract: %v", err)
		}
	})
	t.Run("older_contract", func(t *testing.T) {
		opts, repo, _, d := newGraphStoreFixture(t)
		d.ContractVersion = 0
		overrideGraphStoreResponse(opts.Runner.(*fakeCommandRunner), repo, d)
		_, err := fetchGraphStore(context.Background(), opts, repo, "", "full")
		if err == nil {
			t.Fatal("expected an unsupported-store error")
		}
		if !strings.Contains(err.Error(), "upgrade Graph") || !strings.Contains(err.Error(), "contract") {
			t.Fatalf("error does not name Graph and the required store contract: %v", err)
		}
	})
}

// TestGraphIndexFailsFastOnOlderContract pins runGraphOwnedIndex's fail-fast
// probe: an installed-but-older-contract Graph must be refused with the clear
// "upgrade Graph (contract N)" message from fetchGraphStore's probe BEFORE the
// real `graph index` command ever runs, not after it runs against an
// incompatible Graph. The probe only checked errGraphStoreUnsupported after
// the contract-newer/older sentinels were split; this pins that it also
// checks errGraphStoreContractOlder. Asserting on the runner's call log
// (rather than just the error text) matters here: the fixture's `graph index`
// response always reports success regardless of contract version, so a test
// that only inspected the returned error would pass even if the probe never
// fired and a later fetchGraphStore call happened to produce the same text.
func TestGraphIndexFailsFastOnOlderContract(t *testing.T) {
	opts, repo, _, d := newGraphStoreFixture(t)
	d.ContractVersion = 0
	runner := opts.Runner.(*fakeCommandRunner)
	overrideGraphStoreResponse(runner, repo, d)
	err := runSemanticIndex(context.Background(), &cobra.Command{}, opts, semanticIndexOptions{}, repo)
	if err == nil {
		t.Fatal("expected an unsupported-store error")
	}
	if !strings.Contains(err.Error(), "upgrade Graph") || !strings.Contains(err.Error(), "contract") {
		t.Fatalf("error does not name Graph and the required store contract: %v", err)
	}
	if fakeRunnerCalled(runner, "entire", "graph", "index", "--repo", repo, "--head", "--profile", "full", "--format", "json") {
		t.Fatal("index ran the real graph index command instead of failing fast on the probe")
	}
}

// TestGraphStoreSchemaMajorRefused pins defect (b)'s hard-error half: a store
// whose semantic schema major is newer than ADR 0001's supported range must be
// refused with semstore.ValidateSchema's own wording, not silently copied in.
func TestGraphStoreSchemaMajorRefused(t *testing.T) {
	opts, repo, _, d := newGraphStoreFixture(t)
	d.Header.SchemaVersion = "2.0"
	overrideGraphStoreResponse(opts.Runner.(*fakeCommandRunner), repo, d)
	_, err := fetchGraphStore(context.Background(), opts, repo, "", "full")
	if err == nil {
		t.Fatal("expected a schema-major error")
	}
	if !strings.Contains(err.Error(), semstore.SupportedSchemaRange) {
		t.Fatalf("error does not name the supported schema range: %v", err)
	}
	if !strings.Contains(err.Error(), "Graph") || !strings.Contains(err.Error(), "entire-brain") {
		t.Fatalf("error does not name both binaries: %v", err)
	}
}

// TestGraphStoreSchemaMinorSkewWarns pins defect (b)'s warn half: a header one
// supported-major minor ahead of this build must reach BOTH live consumers of
// the descriptor that previously never called semstore.SchemaMinorSkewWarning
// at all -- `refresh`'s identity warning and the stale-report ("status")
// freshness axis -- instead of being silently dropped.
func TestGraphStoreSchemaMinorSkewWarns(t *testing.T) {
	opts, repo, _, d := newGraphStoreFixture(t)
	newerMinor := semanticSchemaNewerMinor(t)
	d.Header.SchemaVersion = newerMinor
	overrideGraphStoreResponse(opts.Runner.(*fakeCommandRunner), repo, d)

	report, err := semanticFreshnessReport(context.Background(), opts, repo, true)
	if err != nil {
		t.Fatalf("stale report: %v", err)
	}
	if report.Axes["graph"].State != "degraded" {
		t.Fatalf("stale-report did not degrade the graph axis for a newer minor: %+v", report.Axes)
	}
	if len(report.Warnings) != 1 || report.Warnings[0].Code != "provider_schema_newer_minor" {
		t.Fatalf("stale-report did not carry the skew warning: %+v", report.Warnings)
	}

	_, identityWarning, err := semanticRefreshNeeded(context.Background(), opts, "", repo, nil, false, "")
	if err != nil {
		t.Fatalf("refresh needed: %v", err)
	}
	if !strings.Contains(identityWarning, newerMinor) || !strings.Contains(identityWarning, semstore.SchemaVersion) {
		t.Fatalf("refresh did not surface the skew warning: %q", identityWarning)
	}
}

// TestGraphStoreViewMismatchNamesBothBinaries pins defect (c): if Graph
// renames or drops one of the brain_*_v1 views the CREATE TEMP VIEW aliasing
// relies on, the raw sqlite "no such table" error does not say which binary
// is wrong -- either direction can cause it -- so the wrapped error must name
// both entire-brain and Graph instead.
func TestGraphStoreViewMismatchNamesBothBinaries(t *testing.T) {
	_, _, brain, d := newGraphStoreFixture(t)
	db, err := sql.Open(sqlitedriver.Name, d.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP VIEW brain_symbols_v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = semstore.OpenReadDB(semstore.GraphStoreSource(d, brain).StorePath)
	if err == nil {
		t.Fatal("expected a view-mismatch error")
	}
	if !strings.Contains(err.Error(), "entire-brain") || !strings.Contains(err.Error(), "Graph") || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("error does not name both binaries and the schema disagreement: %v", err)
	}
}
