package cli

import (
	"bytes"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCorruptSnapshotIsNotCurrent(t *testing.T) {
	for _, snapshotOnly := range []bool{false, true} {
		for _, damage := range []string{"invalid", "truncated", "wrong_commit"} {
			t.Run(fmtSnapshotCase(snapshotOnly, damage), func(t *testing.T) {
				repoDir := t.TempDir()
				env := semanticTestEnv(t, repoDir)
				opts := Options{Env: env, Runner: semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semanticSchemaVersion)), Now: time.Now}
				cmd := &cobra.Command{}
				cmd.SetOut(&bytes.Buffer{})
				if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
					t.Fatal(err)
				}
				root := bundleTestBrainDir(env)
				source := mustSemanticSource(t, env)
				if snapshotOnly {
					source.StorePath = ""
					source.GenerationPath = ""
				}
				current := func() bool {
					_, ok := semanticIndexAlreadyCurrent(root, source, semanticIndexOptions{}, source.Commit, source.Tree, "")
					return ok
				}
				if !current() {
					t.Fatal("valid snapshot must be current")
				}
				path := filepath.Join(root, filepath.FromSlash(source.SnapshotPath))
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				switch damage {
				case "invalid":
					data = []byte("not a semantic snapshot")
				case "truncated":
					data = data[:bytes.IndexByte(data, '\n')+1]
				case "wrong_commit":
					data = bytes.Replace(data, []byte(source.Commit), []byte("othercommit"), 1)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if current() {
					t.Fatal("damaged snapshot accepted as current")
				}
			})
		}
	}
}
func fmtSnapshotCase(only bool, damage string) string {
	if only {
		return "snapshot_only/" + damage
	}
	return "with_store/" + damage
}
