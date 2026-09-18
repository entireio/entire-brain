package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSemanticAuditStoreAndSnapshotCoverageAgree(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshotFixture := strings.Replace(semanticFixtureSnapshot("1.0"), "\n{\"record_type\":\"symbol\"", "\n{\"record_type\":\"file\",\"path\":\"internal/auth/token.go\",\"language\":\"Go\"}\n{\"record_type\":\"symbol\"", 1)
	runner := semanticFixtureRunner(repoDir, snapshotFixture)
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatal(err)
	}
	brainDir, err := brainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	source := *mustSemanticSource(t, env)
	freshness := staleReport{Axes: map[string]staleAxis{"store": {State: "ok"}, "snapshot": {State: "ok"}}}
	store, err := semanticAuditStoreCoverage(brainDir, &source, freshness)
	if err != nil {
		t.Fatal(err)
	}
	source.StorePath, source.GenerationPath = "", ""
	snapshot, err := semanticAuditStoreCoverage(brainDir, &source, freshness)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store, snapshot) || !reflect.DeepEqual(store.Languages, []semanticAuditCount{{Name: "Go", Count: 1}}) {
		t.Fatalf("store/snapshot coverage differs: store=%+v snapshot=%+v", store, snapshot)
	}
}

func TestSemanticAuditSnapshotCoverageCountsDedupesAndFailsClosed(t *testing.T) {
	brainDir := t.TempDir()
	rel := filepath.ToSlash(filepath.Join(semanticDirName, semanticSnapshotsDir, "audit", semanticSnapshotName))
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	valid := `{"schema_version":"1.0"}
{"record_type":"file","path":"src/a.go","language":"Go"}
{"record_type":"file","path":"src/a.go","language":""}
{"record_type":"symbol","id":"one","kind":"function","language":"Go","file_path":"src/a.go"}
{"record_type":"symbol","id":"two","kind":"","language":"","file_path":"docs/readme.md"}
{"record_type":"relation","type":"CALLS"}
{"record_type":"relation","type":""}
`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &semanticSourceManifest{SnapshotPath: rel}
	freshness := staleReport{Axes: map[string]staleAxis{"snapshot": {State: "ok"}}}
	coverage, err := semanticAuditSnapshotCoverage(brainDir, source, freshness)
	if err != nil {
		t.Fatal(err)
	}
	want := semanticAuditCoverage{
		FileLanguages: []semanticAuditCount{{Name: "Go", Count: 1}, {Name: "unknown", Count: 1}},
		Languages:     []semanticAuditCount{{Name: "Go", Count: 1}, {Name: "unknown", Count: 1}},
		SymbolKinds:   []semanticAuditCount{{Name: "function", Count: 1}, {Name: "unknown", Count: 1}},
		RelationTypes: []semanticAuditCount{{Name: "CALLS", Count: 1}, {Name: "unknown", Count: 1}},
	}
	if !reflect.DeepEqual(coverage, want) {
		t.Fatalf("coverage=%+v want=%+v", coverage, want)
	}
	for name, data := range map[string]string{
		"malformed": `{"schema_version":"1.0"}` + "\n" + `{not json}` + "\n",
		"truncated": `{"schema_version":"1.0"}` + "\n" + `{"record_type":"symbol"`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := semanticAuditSnapshotCoverage(brainDir, source, freshness); err == nil {
				t.Fatal("invalid snapshot produced coverage")
			}
		})
	}
	unsafe := *source
	unsafe.SnapshotPath = "../outside/snapshot.ndjson"
	if _, err := semanticAuditSnapshotCoverage(brainDir, &unsafe, freshness); err == nil {
		t.Fatal("unsafe snapshot path produced coverage")
	}
}
