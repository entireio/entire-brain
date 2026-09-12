package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI does not read strategy_options.checkpoints_version at all — the only
// mention of the key in entireio/cli is
// TestNewGitStore_IgnoresCheckpointsVersion, which pins that the store resolves
// refs without consulting it — and nothing in the CLI ever writes
// refs/entire/checkpoints/v2/main. A repository carrying that setting therefore
// still has ordinary v1 checkpoints, and the export must read them.
func TestLoadConfiguredCheckpointSnapshotIgnoresCheckpointsVersionSetting(t *testing.T) {
	t.Parallel()

	for _, raw := range []bool{true, false} {
		name := "compact"
		if raw {
			name = "raw"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			repoDir := t.TempDir()
			settingsDir := filepath.Join(repoDir, ".entire")
			if err := os.MkdirAll(settingsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"strategy_options":{"checkpoints_version":2}}`), 0o600); err != nil {
				t.Fatal(err)
			}

			root := checkpointPath("aaa111aaa111")
			runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
				fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
				fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
					stdout: root + "/metadata.json\n" + root + "/0/metadata.json\n" + root + "/0/full.jsonl\n",
				},
				fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {},
				fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/metadata.json"): {
					stdout: `{"branch":"main","sessions":[{"metadata":"/` + root + `/0/metadata.json","transcript":"/` + root + `/0/full.jsonl"}]}`,
				},
				fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/0/metadata.json"): {
					stdout: `{"checkpoint_id":"aaa111aaa111","session_id":"v1-session","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
				},
			}}

			snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, raw, 10, checkpointBranchDestinations{}, nil, nil)
			if err != nil {
				t.Fatalf("checkpoints_version=2 diverted the export away from the v1 checkpoints that exist: %v (warnings %v)", err, warnings)
			}
			if snapshot == nil || snapshot.CheckpointCount != 1 {
				t.Fatalf("snapshot = %+v, want the one v1 checkpoint", snapshot)
			}
			if snapshot.Version != checkpointStorageV1 {
				t.Fatalf("snapshot.Version = %d, want %d", snapshot.Version, checkpointStorageV1)
			}
			for _, call := range runner.calls {
				for _, arg := range call.args {
					if strings.Contains(arg, "checkpoints/v2") {
						t.Fatalf("export consulted a v2 checkpoint ref no writer produces: %+v", call)
					}
				}
			}
		})
	}
}

// removedV2MainRef is the aggregate ref the removed checkpoints-v2 reader used
// to consult. No released CLI ever writes it (nothing in entireio/cli mentions
// the ref at all), so it is kept here only so the existing guards can keep
// asserting that no export path reaches for it again.
const removedV2MainRef = "refs/entire/checkpoints/v2/main"
