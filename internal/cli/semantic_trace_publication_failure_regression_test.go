package cli

import (
	"bytes"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func TestRuntimeTracePublicationFailurePreservesStoreAndRetry(t *testing.T) {
	brain, _, opts := indexFixtureBrain(t, semanticBoundaryFixtureSnapshot())
	d := opts.Runner.(*fakeCommandRunner).graphStoreFixtures[opts.Env.RepoRoot+"\x00full"]
	original := fileDigest(t, d.Path)
	manifest := fileDigest(t, filepath.Join(brain, bmanifest.FileName))
	parent := filepath.Join(brain, "semantic", "traces")
	if err := os.MkdirAll(filepath.Dir(parent), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	input := writeTraceFile(t, `[{"from":"ValidateToken","to":"GET /tokens/{id}","type":"HANDLES_ROUTE"}]`)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runSemanticIngestTraces(cmd, opts, semanticTraceIngestOptions{json: true}, input); err == nil || out.Len() != 0 {
		t.Fatalf("failed publication returned success: %v %s", err, out.String())
	}
	if original != fileDigest(t, d.Path) || manifest != fileDigest(t, filepath.Join(brain, bmanifest.FileName)) {
		t.Fatal("failed trace publication changed Graph or manifest")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := runSemanticIngestTraces(cmd, opts, semanticTraceIngestOptions{json: true}, input); err != nil {
		t.Fatal(err)
	}
	rows, err := semstore.ReadTraces(brain)
	if err != nil || len(rows) != 1 || rows[0].Matched != 1 {
		t.Fatalf("retry lost observation: %+v %v", rows, err)
	}
	if original != fileDigest(t, d.Path) {
		t.Fatal("successful trace publication changed Graph")
	}
}
