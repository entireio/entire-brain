package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRuntimeTracePublicationFailuresPreserveGenerationAndRetry(t *testing.T) {
	for _, failure := range []string{"artifact-parent", "trace-insert", "generation-meta"} {
		t.Run(failure, func(t *testing.T) {
			brainDir, _, opts := indexFixtureBrain(t, semanticBoundaryFixtureSnapshot())
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			source := manifest.Sources.Semantic
			storePath := filepath.Join(brainDir, filepath.FromSlash(source.StorePath))
			artifactParent := filepath.Join(brainDir, semanticDirName, "traces")
			execSQL := func(statement string) {
				t.Helper()
				db, err := sql.Open(sqliteDriverName, storePath)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			switch failure {
			case "artifact-parent":
				if err := os.WriteFile(artifactParent, []byte("prior artifact obstruction"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "trace-insert":
				db, err := sql.Open(sqliteDriverName, storePath)
				if err != nil {
					t.Fatal(err)
				}
				if err := ensureSemanticRuntimeTraceTable(db); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				execSQL(`CREATE TRIGGER fail_trace_insert BEFORE INSERT ON runtime_traces BEGIN SELECT RAISE(ABORT, 'trace insert blocked'); END`)
			case "generation-meta":
				execSQL(`CREATE TRIGGER fail_generation_meta BEFORE INSERT ON meta WHEN NEW.key='generation_id' BEGIN SELECT RAISE(ABORT, 'generation metadata blocked'); END`)
			}
			manifestPath := filepath.Join(brainDir, exportManifestFileName)
			beforeManifest := fileDigest(t, manifestPath)
			generationDir := filepath.Join(brainDir, filepath.FromSlash(source.GenerationPath))
			beforeGeneration := privacyTreeDigest(t, generationDir)
			generationsRoot := filepath.Dir(generationDir)
			names := func() []string {
				t.Helper()
				entries, err := os.ReadDir(generationsRoot)
				if err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, e := range entries {
					out = append(out, e.Name())
				}
				return out
			}
			beforeNames := names()
			tracePath := filepath.Join(t.TempDir(), "input.ndjson")
			if err := os.WriteFile(tracePath, []byte("{\"from\":\"ValidateToken\",\"to\":\"GET /tokens/{id}\",\"type\":\"HANDLES_ROUTE\"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			err = runSemanticIngestTraces(cmd, opts, semanticTraceIngestOptions{json: true}, tracePath)
			if err == nil || out.Len() != 0 {
				t.Fatalf("failed publication reported success: %v %s", err, out.String())
			}
			if failure == "trace-insert" && !strings.Contains(err.Error(), "trace insert blocked") {
				t.Fatalf("wrong failure: %v", err)
			}
			if failure == "generation-meta" && !strings.Contains(err.Error(), "generation metadata blocked") {
				t.Fatalf("wrong failure: %v", err)
			}
			if fileDigest(t, manifestPath) != beforeManifest || privacyTreeDigest(t, generationDir) != beforeGeneration || !reflect.DeepEqual(names(), beforeNames) {
				t.Fatal("failed publication changed prior generation, manifest, or leaked staging")
			}
			switch failure {
			case "artifact-parent":
				if err := os.Remove(artifactParent); err != nil {
					t.Fatal(err)
				}
			case "trace-insert":
				execSQL(`DROP TRIGGER fail_trace_insert`)
			case "generation-meta":
				execSQL(`DROP TRIGGER fail_generation_meta`)
			}
			if err := runSemanticIngestTraces(cmd, opts, semanticTraceIngestOptions{json: true}, tracePath); err != nil {
				t.Fatalf("retry: %v", err)
			}
			var report semanticTraceIngestReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.Total != 1 || report.Matched != 1 || report.Unmatched != 0 || report.Path == "" {
				t.Fatalf("retry report=%+v", report)
			}
			current, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			if current.Sources.Semantic.GenerationPath == source.GenerationPath {
				t.Fatal("successful retry did not publish a new generation")
			}
			traces, err := semanticRuntimeTraceFacts(brainDir, current.Sources.Semantic, "ValidateToken", 10)
			if err != nil || len(traces) != 1 || traces[0].Confidence != 1 {
				t.Fatalf("retry facts=%+v err=%v", traces, err)
			}
		})
	}
}
