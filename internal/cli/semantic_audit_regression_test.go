package cli

import (
	"context"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSemanticAuditReadsGraphCoverage(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshotFixture := strings.Replace(semanticFixtureSnapshot("1.0"), "\n{\"record_type\":\"symbol\"", "\n{\"record_type\":\"file\",\"path\":\"internal/auth/token.go\",\"language\":\"Go\"}\n{\"record_type\":\"symbol\"", 1)
	runner := semanticFixtureRunner(repoDir, snapshotFixture)
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatal(err)
	}
	brainDir, err := base.BrainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	source := *semstore.GraphStoreSource(runner.graphStoreFixtures[repoDir+"\x00full"], brainDir)
	freshness := staleReport{Axes: map[string]staleAxis{"store": {State: "ok"}, "snapshot": {State: "ok"}}}
	store, err := semanticAuditStoreCoverage(brainDir, &source, freshness)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.Languages, []semanticAuditCount{{Name: "Go", Count: 1}}) {
		t.Fatalf("Graph coverage missing: %+v", store)
	}
}
