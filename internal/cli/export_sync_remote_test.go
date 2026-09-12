package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Checkpoints do not necessarily sync to a remote named "origin". The CLI
// elects ONE remote to carry them — strategy_options.checkpoint_push_remote,
// then a remote a past push captured, then "origin", then the sole/first remote
// in .git/config order — and its read paths consult that elected remote first
// with "origin" behind it as a legacy tier
// (cmd/entire/cli/strategy/checkpoint_read_remotes.go). The export reader looked
// only at the local branch and origin's tracking ref, so a clone whose elected
// remote is named anything else had checkpoints the brain never saw.

const (
	remoteFork     = "fork"
	forkTrackingV1 = "refs/remotes/fork/entire/checkpoints/v1"
)

// syncRemoteRunner scripts a repository with the given remotes in .git/config
// order and one checkpoint tree per named tracking ref.
func syncRemoteRunner(remotes []string, trees map[string]string) *fakeCommandRunner {
	var config string
	for _, name := range remotes {
		config += "remote." + name + ".url https://example.test/" + name + ".git\n"
	}
	responses := map[string]fakeCommandResponse{
		fakeCommandKey("git", "config", "--local", "--get-regexp", `^remote\..*\.url$`): {stdout: config},
		fakeCommandKey("git", "rev-parse", "--git-common-dir"):                          {stdout: ".git\n"},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                {err: errors.New("no local checkpoint branch")},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef):              {err: errors.New("no origin tracking ref")},
	}
	for ref, tree := range trees {
		responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", ref)] = fakeCommandResponse{stdout: tree}
	}
	return &fakeCommandRunner{responses: responses}
}

func sourceIDs(sources map[string]checkpointSnapshotSource) []string {
	ids := make([]string, 0, len(sources))
	for id := range sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func writeRepoSettings(t *testing.T, repoDir, name, body string) {
	t.Helper()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLoadAggregateCheckpointSources_ReadsConfiguredPushRemote pins the
// checkpoint_push_remote tier: the named remote's tracking ref is read.
func TestLoadAggregateCheckpointSources_ReadsConfiguredPushRemote(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	writeRepoSettings(t, repoDir, "settings.local.json", `{"strategy_options":{"checkpoint_push_remote":"fork"}}`)
	runner := syncRemoteRunner([]string{"origin", remoteFork}, map[string]string{
		forkTrackingV1: "aa/aaaaaaaaaa/metadata.json\naa/aaaaaaaaaa/0/metadata.json\naa/aaaaaaaaaa/0/full.jsonl\n",
	})

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, repoDir, true, nil)

	if got := sourceIDs(sources); len(got) != 1 || got[0] != "aaaaaaaaaaaa" {
		t.Fatalf("discovered %v, want the checkpoint on the elected remote %q", got, remoteFork)
	}
	if sources["aaaaaaaaaaaa"].Ref != forkTrackingV1 {
		t.Errorf("checkpoint resolved to %s, want %s", sources["aaaaaaaaaaaa"].Ref, forkTrackingV1)
	}
}

// TestLoadAggregateCheckpointSources_ReadsSoleRemote pins the default tiers:
// with no origin and no setting, the one configured remote is elected.
func TestLoadAggregateCheckpointSources_ReadsSoleRemote(t *testing.T) {
	t.Parallel()

	runner := syncRemoteRunner([]string{"upstream"}, map[string]string{
		"refs/remotes/upstream/entire/checkpoints/v1": "bb/bbbbbbbbbb/metadata.json\nbb/bbbbbbbbbb/0/metadata.json\nbb/bbbbbbbbbb/0/full.jsonl\n",
	})

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, t.TempDir(), true, nil)

	if got := sourceIDs(sources); len(got) != 1 || got[0] != "bbbbbbbbbbbb" {
		t.Fatalf("discovered %v, want the checkpoint on the sole remote", got)
	}
}

// TestLoadAggregateCheckpointSources_KeepsOriginAsLegacyTier pins that origin
// stays in the read set behind a non-origin elected remote, which is where
// pre-election checkpoints live.
func TestLoadAggregateCheckpointSources_KeepsOriginAsLegacyTier(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	writeRepoSettings(t, repoDir, "settings.local.json", `{"strategy_options":{"checkpoint_push_remote":"fork"}}`)
	runner := syncRemoteRunner([]string{"origin", remoteFork}, map[string]string{
		forkTrackingV1: "aa/aaaaaaaaaa/metadata.json\naa/aaaaaaaaaa/0/metadata.json\naa/aaaaaaaaaa/0/full.jsonl\n",
		v1OriginRef:    "bb/bbbbbbbbbb/metadata.json\nbb/bbbbbbbbbb/0/metadata.json\nbb/bbbbbbbbbb/0/full.jsonl\n",
	})

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, repoDir, true, nil)

	got := sourceIDs(sources)
	if len(got) != 2 || got[0] != "aaaaaaaaaaaa" || got[1] != "bbbbbbbbbbbb" {
		t.Fatalf("discovered %v, want both the elected remote's and origin's checkpoints", got)
	}
	if sources["aaaaaaaaaaaa"].Ref != forkTrackingV1 {
		t.Errorf("elected-remote checkpoint resolved to %s, want %s", sources["aaaaaaaaaaaa"].Ref, forkTrackingV1)
	}
	if sources["bbbbbbbbbbbb"].Ref != v1OriginRef {
		t.Errorf("legacy checkpoint resolved to %s, want %s", sources["bbbbbbbbbbbb"].Ref, v1OriginRef)
	}
}

// TestLoadAggregateCheckpointSources_UnreadableSettingsStillReadsOrigin pins the
// read-side fail-open: a broken election must not hide origin's checkpoints,
// which is the CLI's own rule for reads (writes fail closed, reads do not).
func TestLoadAggregateCheckpointSources_UnreadableSettingsStillReadsOrigin(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	writeRepoSettings(t, repoDir, "settings.local.json", `{ this is not json`)
	runner := syncRemoteRunner([]string{"origin"}, map[string]string{
		v1OriginRef: "bb/bbbbbbbbbb/metadata.json\nbb/bbbbbbbbbb/0/metadata.json\nbb/bbbbbbbbbb/0/full.jsonl\n",
	})

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, repoDir, true, nil)

	if got := sourceIDs(sources); len(got) != 1 || got[0] != "bbbbbbbbbbbb" {
		t.Fatalf("discovered %v, want origin's checkpoint despite the unreadable settings", got)
	}
}

// TestLoadAggregateCheckpointSources_ReadsCapturedRemote pins the captured
// tier: a past push recorded the elected remote in the git common dir, and the
// export follows it without any setting.
func TestLoadAggregateCheckpointSources_ReadsCapturedRemote(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	gitDir := filepath.Join(repoDir, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "entire-checkpoint-sync-remotes.json"), []byte(`{"remotes":["fork"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// origin exists and would otherwise win the default tier.
	runner := syncRemoteRunner([]string{"origin", remoteFork}, map[string]string{
		forkTrackingV1: "aa/aaaaaaaaaa/metadata.json\naa/aaaaaaaaaa/0/metadata.json\naa/aaaaaaaaaa/0/full.jsonl\n",
	})

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, repoDir, true, nil)

	if got := sourceIDs(sources); len(got) != 1 || got[0] != "aaaaaaaaaaaa" {
		t.Fatalf("discovered %v, want the checkpoint on the captured remote %q", got, remoteFork)
	}
	if sources["aaaaaaaaaaaa"].Ref != forkTrackingV1 {
		t.Errorf("checkpoint resolved to %s, want %s", sources["aaaaaaaaaaaa"].Ref, forkTrackingV1)
	}
}

// TestLoadAggregateCheckpointSources_RejectsUnconfiguredPushRemote pins the
// fail-closed half of the election: checkpoint_push_remote naming a remote that
// does not exist elects nothing, and the read falls open to origin rather than
// fabricating a tracking ref.
func TestLoadAggregateCheckpointSources_RejectsUnconfiguredPushRemote(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	writeRepoSettings(t, repoDir, "settings.local.json", `{"strategy_options":{"checkpoint_push_remote":"ghost"}}`)
	runner := syncRemoteRunner([]string{"origin"}, map[string]string{
		v1OriginRef: "bb/bbbbbbbbbb/metadata.json\nbb/bbbbbbbbbb/0/metadata.json\nbb/bbbbbbbbbb/0/full.jsonl\n",
	})

	sources, _ := loadAggregateCheckpointSources(context.Background(), runner, repoDir, true, nil)

	if got := sourceIDs(sources); len(got) != 1 || got[0] != "bbbbbbbbbbbb" {
		t.Fatalf("discovered %v, want origin's checkpoint", got)
	}
	for _, call := range runner.calls {
		for _, arg := range call.args {
			if arg == "refs/remotes/ghost/entire/checkpoints/v1" {
				t.Fatalf("read a tracking ref for a remote that is not configured: %+v", call)
			}
		}
	}
}
