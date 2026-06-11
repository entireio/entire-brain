package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCommandRunner struct {
	responses map[string]fakeCommandResponse
	sequences map[string][]fakeCommandResponse
	calls     []fakeCommandCall
}

type fakeCommandResponse struct {
	stdout string
	stderr string
	err    error
}

type fakeCommandCall struct {
	dir         string
	name        string
	args        []string
	hasDeadline bool
}

func (r *fakeCommandRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	key := fakeCommandKey(name, args...)
	var hasDeadline bool
	if ctx != nil {
		_, hasDeadline = ctx.Deadline()
	}
	r.calls = append(r.calls, fakeCommandCall{
		dir:         dir,
		name:        name,
		args:        append([]string(nil), args...),
		hasDeadline: hasDeadline,
	})

	if sequence, ok := r.sequences[key]; ok && len(sequence) > 0 {
		response := sequence[0]
		r.sequences[key] = sequence[1:]
		return []byte(response.stdout), []byte(response.stderr), response.err
	}

	response, ok := r.responses[key]
	if !ok {
		if key == fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD") {
			return []byte("origin/main\n"), nil, nil
		}
		return nil, nil, errors.New("unexpected command: " + key)
	}
	return []byte(response.stdout), []byte(response.stderr), response.err
}

func fakeCommandKey(name string, args ...string) string {
	return name + "\x00" + strings.Join(args, "\x00")
}

func TestDiscoverCheckpointsFallsBackToCheckpointRemote(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "strategy_options": {
    "checkpoints_version": 2,
    "checkpoint_remote": {
      "provider": "github",
      "repo": "entireio/cli-checkpoints"
    }
  }
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	remoteURL := "https://github.com/entireio/cli-checkpoints.git"
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "init", "-q"): {},
			fakeCommandKey("git", "fetch", "--no-tags", "--depth=1", "--filter=blob:none", remoteURL, "+"+v2MainRef+":"+v2MainRef):     {},
			fakeCommandKey("git", "fetch", "--no-tags", "--depth=1", "--filter=blob:none", remoteURL, "+"+v1RemoteRef+":"+v1RemoteRef): {},
		},
		sequences: map[string][]fakeCommandResponse{
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef): {
				{err: errors.New("local v2 missing")},
				{stdout: "aa/a111aaa111/metadata.json\n"},
			},
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
				{err: errors.New("local v1 missing")},
				{stdout: "bb/b222bbb222/metadata.json\n"},
			},
		},
	}

	checkpoints, warnings, err := discoverCheckpoints(context.Background(), runner, repoDir, "entire-test", exportScopeAll, 10)
	if err != nil {
		t.Fatalf("discover checkpoints: %v", err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoint count = %d, want 2: %+v", len(checkpoints), checkpoints)
	}
	if checkpoints[0].CheckpointID != "aaa111aaa111" || checkpoints[1].CheckpointID != "bbb222bbb222" {
		t.Fatalf("unexpected checkpoints: %+v", checkpoints)
	}

	joinedWarnings := strings.Join(warnings, "\n")
	if !strings.Contains(joinedWarnings, "discovered checkpoint refs from configured checkpoint remote") {
		t.Fatalf("missing remote discovery warning: %v", warnings)
	}

	var sawRemoteFetch bool
	for _, call := range runner.calls {
		if call.name == "git" && len(call.args) >= 2 && call.args[0] == "fetch" && call.args[len(call.args)-2] == remoteURL {
			sawRemoteFetch = true
		}
	}
	if !sawRemoteFetch {
		t.Fatalf("expected checkpoint remote fetch, calls: %+v", runner.calls)
	}
}

func TestListAllCheckpointRefsNoEgressSkipsCheckpointRemote(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "strategy_options": {
    "checkpoints_version": 2,
    "checkpoint_remote": {
      "provider": "github",
      "repo": "entireio/cli-checkpoints"
    }
  }
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}, sequences: map[string][]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef): {{err: errors.New("local v2 missing")}},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {{err: errors.New("local v1 missing")}},
	}}

	checkpoints, warnings, err := listAllCheckpointRefs(context.Background(), runner, repoDir, 10)
	if err != nil {
		t.Fatalf("listAllCheckpointRefs: %v", err)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("expected no checkpoints from remote in no-egress mode, got %+v", checkpoints)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "no_egress") {
		t.Fatalf("expected no-egress warning, got %v", warnings)
	}
	for _, call := range runner.calls {
		if call.name == "git" && len(call.args) > 0 && call.args[0] == "fetch" {
			t.Fatalf("no-egress checkpoint discovery must not fetch: %+v", call)
		}
	}
}

func TestDiscoverCheckpointsNoEgressDoesNotRunEntireFallback(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}, sequences: map[string][]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):   {{err: errors.New("local v1 missing")}},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {{err: errors.New("local origin missing")}},
	}}

	checkpoints, warnings, err := discoverCheckpoints(context.Background(), runner, repoDir, "entire-test", exportScopeAll, 10)
	if err != nil {
		t.Fatalf("discoverCheckpoints: %v", err)
	}
	if len(checkpoints) != 0 {
		t.Fatalf("expected no checkpoint fallback in no-egress mode, got %+v", checkpoints)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "branch checkpoint fallback skipped") {
		t.Fatalf("expected no-egress fallback warning, got %v", warnings)
	}
	for _, call := range runner.calls {
		if call.name == "entire-test" {
			t.Fatalf("no-egress checkpoint discovery must not run entire fallback: %+v", runner.calls)
		}
	}
}

func TestValidateExportDirAvailableRejectsSymlinkOutput(t *testing.T) {
	target := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "brain-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := validateExportDirAvailable(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink output rejection, got %v", err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("symlink target should remain untouched, entries=%d err=%v", len(entries), err)
	}
}

func TestRemoveForcedOutputDirRejectsArbitraryJSONManifest(t *testing.T) {
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outputDir, exportManifestFileName), []byte(`{"name":"web-app"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	oldPath := filepath.Join(outputDir, "old.txt")
	if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
		t.Fatalf("write old file: %v", err)
	}

	err := removeForcedOutputDir(outputDir, "")
	if err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("expected arbitrary manifest rejection, got %v", err)
	}
	if _, statErr := os.Stat(oldPath); statErr != nil {
		t.Fatalf("arbitrary manifest output was removed: %v", statErr)
	}
}

func TestRemoveForcedOutputDirChecksResolvedSymlinkParent(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := writeBrainManifestAndReadme(repoDir, exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: time.Now().UTC()}},
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	linkRoot := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(root, linkRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linkedRepo := filepath.Join(linkRoot, "repo")
	err := removeForcedOutputDir(linkedRepo, repoDir)
	if err == nil || !strings.Contains(err.Error(), "containing the repository") {
		t.Fatalf("expected resolved repo deletion rejection, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repoDir, exportManifestFileName)); statErr != nil {
		t.Fatalf("repo manifest was removed through symlink parent: %v", statErr)
	}
}

func TestLoadConfiguredCheckpointSnapshotNoEgressSkipsRemoteFetch(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "strategy_options": {
    "checkpoints_version": 2,
    "checkpoint_remote": {
      "provider": "github",
      "repo": "entireio/cli-checkpoints"
    }
  }
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef): {err: errors.New("local v2 missing")},
	}}

	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 10, checkpointBranchDestinations{}, nil, nil)
	if snapshot != nil {
		t.Fatalf("expected no remote snapshot, got %+v", snapshot)
	}
	if err == nil || !errors.Is(err, errCheckpointSnapshotUnavailable) {
		t.Fatalf("expected checkpoint snapshot unavailable, got %v", err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "no_egress") {
		t.Fatalf("expected no-egress warning, got %v", warnings)
	}
	for _, call := range runner.calls {
		if call.name == "git" && len(call.args) > 0 && call.args[0] == "fetch" {
			t.Fatalf("no-egress checkpoint snapshot must not fetch: %+v", call)
		}
	}
}

func TestDiscoverCheckpointsMirrorsV1Settings(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "strategy_options": {
    "checkpoint_remote": {
      "provider": "github",
      "repo": "entireio/cli-checkpoints"
    }
  }
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	remoteURL := "https://github.com/entireio/cli-checkpoints.git"
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "init", "-q"): {},
			fakeCommandKey("git", "fetch", "--no-tags", "--depth=1", "--filter=blob:none", remoteURL, "+"+v1RemoteRef+":"+v1RemoteRef): {},
		},
		sequences: map[string][]fakeCommandResponse{
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
				{err: errors.New("local v1 missing")},
				{stdout: "bb/b222bbb222/metadata.json\n"},
			},
		},
	}

	checkpoints, _, err := discoverCheckpoints(context.Background(), runner, repoDir, "entire-test", exportScopeAll, 10)
	if err != nil {
		t.Fatalf("discover checkpoints: %v", err)
	}
	if len(checkpoints) != 1 || checkpoints[0].CheckpointID != "bbb222bbb222" {
		t.Fatalf("unexpected checkpoints: %+v", checkpoints)
	}

	for _, call := range runner.calls {
		for _, arg := range call.args {
			if arg == v2MainRef || strings.Contains(arg, v2MainRef) {
				t.Fatalf("v2 ref should not be used when repo settings default to v1, calls: %+v", runner.calls)
			}
		}
	}
}

func TestExportUsesConfiguredV1CheckpointRemoteDirectly(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "strategy_options": {
    "checkpoint_remote": {
      "provider": "github",
      "repo": "entireio/cli-checkpoints"
    }
  }
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	outputDir := filepath.Join(t.TempDir(), "export")
	remoteURL := "https://github.com/entireio/cli-checkpoints.git"
	checkpointID := "aaa111aaa111"
	checkpointDir := "aa/a111aaa111"
	sessionMetadataPath := checkpointDir + "/0/metadata.json"
	transcriptPath := checkpointDir + "/0/full.jsonl"
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "init", "-q"): {},
			fakeCommandKey("git", "fetch", "--no-tags", "--depth=1", "--filter="+checkpointRemoteBlobFilter, remoteURL, "+"+v1RemoteRef+":"+v1RemoteRef): {},
			fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+checkpointDir+"/metadata.json"): {
				stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "branch": "main",
  "sessions": [
    {
      "metadata": "/aa/a111aaa111/0/metadata.json",
      "transcript": "/aa/a111aaa111/0/full.jsonl"
    }
  ]
}`,
			},
			fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+sessionMetadataPath): {
				stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "session_id": "session-one",
  "branch": "main",
  "agent": "Codex",
  "model": "gpt-5",
  "created_at": "2026-02-03T04:05:06Z",
  "checkpoints_count": 7
}`,
			},
			fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+transcriptPath): {
				stdout: "{\"type\":\"message\",\"text\":\"from v1 full log\"}\n",
			},
		},
		sequences: map[string][]fakeCommandResponse{
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1RemoteRef): {
				{err: errors.New("local v1 missing")},
				{stdout: checkpointDir + "/metadata.json\n" + sessionMetadataPath + "\n" + transcriptPath + "\n"},
			},
		},
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			CLIVersion: "cli-test",
			RepoRoot:   repoDir,
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "export", "--output", outputDir, "--checkpoint-limit", "10", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	if !strings.Contains(out, "exported 1 sessions from 1 checkpoints") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	for _, want := range []string{
		"warnings: 2",
		"warning: Could not determine which checkpoints are reachable from the default branch; branch folders may be less precise: could not resolve main",
		"warning: Could not build the checkpoint author index; exported sessions may omit author metadata:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("export output missing %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{
		"local v1 missing",
		"exporting raw transcripts directly",
		"compact transcript unavailable for v1 checkpoints",
	} {
		if strings.Contains(out, hidden) {
			t.Fatalf("export output should hide debug warning %q:\n%s", hidden, out)
		}
	}

	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(outputDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.TranscriptMode != "raw" {
		t.Fatalf("transcript mode = %q, want raw", manifest.TranscriptMode)
	}
	if len(manifest.Sessions) != 1 || manifest.Sessions[0].LatestCheckpoint != checkpointID {
		t.Fatalf("unexpected sessions: %+v", manifest.Sessions)
	}
	if manifest.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want main", manifest.DefaultBranch)
	}
	if manifest.Sessions[0].Branch != "main" {
		t.Fatalf("session branch = %q, want main", manifest.Sessions[0].Branch)
	}
	if !strings.HasPrefix(manifest.Sessions[0].TranscriptPath, "sessions/main/") {
		t.Fatalf("transcript path = %q, want sessions/main", manifest.Sessions[0].TranscriptPath)
	}
	transcript, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(manifest.Sessions[0].TranscriptPath)))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(transcript), "from v1 full log") {
		t.Fatalf("unexpected transcript:\n%s", transcript)
	}

	for _, call := range runner.calls {
		if call.name == "entire-test" {
			t.Fatalf("direct remote export should not shell out to Entire checkpoint explain, calls: %+v", runner.calls)
		}
		for _, arg := range call.args {
			if arg == v2MainRef || strings.Contains(arg, v2MainRef) {
				t.Fatalf("v2 ref should not be used for default v1 settings, calls: %+v", runner.calls)
			}
		}
	}
}

func TestExportDebugShowsCheckpointFallbackDiagnostics(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "strategy_options": {
    "checkpoint_remote": {
      "provider": "github",
      "repo": "entireio/cli-checkpoints"
    }
  }
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	outputDir := filepath.Join(t.TempDir(), "export")
	remoteURL := "https://github.com/entireio/cli-checkpoints.git"
	checkpointDir := "aa/a111aaa111"
	sessionMetadataPath := checkpointDir + "/0/metadata.json"
	transcriptPath := checkpointDir + "/0/full.jsonl"
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "init", "-q"): {},
			fakeCommandKey("git", "fetch", "--no-tags", "--depth=1", "--filter="+checkpointRemoteBlobFilter, remoteURL, "+"+v1RemoteRef+":"+v1RemoteRef): {},
			fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+checkpointDir+"/metadata.json"): {
				stdout: `{"sessions":[{"metadata":"/aa/a111aaa111/0/metadata.json","transcript":"/aa/a111aaa111/0/full.jsonl"}]}`,
			},
			fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+sessionMetadataPath): {
				stdout: `{"checkpoint_id":"aaa111aaa111","session_id":"session-one","branch":"main","agent":"Codex","created_at":"2026-02-03T04:05:06Z"}`,
			},
			fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+transcriptPath): {
				stdout: "{\"type\":\"message\",\"text\":\"from v1 full log\"}\n",
			},
		},
		sequences: map[string][]fakeCommandResponse{
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1RemoteRef): {
				{err: errors.New("local v1 missing")},
				{stdout: checkpointDir + "/metadata.json\n" + sessionMetadataPath + "\n" + transcriptPath + "\n"},
			},
		},
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			CLIVersion: "cli-test",
			RepoRoot:   repoDir,
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "export", "--debug", "--output", outputDir, "--checkpoint-limit", "10", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	for _, want := range []string{
		"warning: exporting raw transcripts directly from configured checkpoint remote ref refs/heads/entire/checkpoints/v1",
		"warning: This repository uses v1 checkpoints, which only store raw full.jsonl transcripts; exported transcripts are raw.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug export output missing %q:\n%s", want, out)
		}
	}
}

func TestExportUsesLocalV1WithoutSettings(t *testing.T) {
	repoDir := t.TempDir()
	outputDir := filepath.Join(t.TempDir(), "export")
	checkpointID := "ccc333ccc333"
	checkpointDir := "cc/c333ccc333"
	sessionMetadataPath := checkpointDir + "/0/metadata.json"
	transcriptPath := checkpointDir + "/0/full.jsonl"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1RemoteRef): {
			stdout: checkpointDir + "/metadata.json\n" + sessionMetadataPath + "\n" + transcriptPath + "\n",
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+checkpointDir+"/metadata.json"): {
			stdout: `{"sessions":[{"metadata":"/cc/c333ccc333/0/metadata.json","transcript":"/cc/c333ccc333/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+sessionMetadataPath): {
			stdout: `{
  "checkpoint_id": "ccc333ccc333",
  "session_id": "local-session",
  "agent": "Codex",
  "created_at": "2026-02-05T00:00:00Z"
}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":"+transcriptPath): {
			stdout: "{\"type\":\"message\",\"text\":\"local v1\"}\n",
		},
	}}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot: repoDir,
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 2, 5, 1, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "export", "--output", outputDir, "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}

	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(outputDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(manifest.Sessions) != 1 || manifest.Sessions[0].LatestCheckpoint != checkpointID {
		t.Fatalf("unexpected sessions: %+v", manifest.Sessions)
	}
	if manifest.TranscriptMode != "raw" {
		t.Fatalf("transcript mode = %q, want raw", manifest.TranscriptMode)
	}
	for _, call := range runner.calls {
		if call.name == "entire-test" {
			t.Fatalf("local v1 direct export should not shell out to Entire, calls: %+v", runner.calls)
		}
		for _, arg := range call.args {
			if arg == v2MainRef || strings.Contains(arg, v2MainRef) {
				t.Fatalf("v2 ref should not be used when settings are absent and local v1 exists, calls: %+v", runner.calls)
			}
		}
	}
}

func TestExportUsesOriginTrackingV1RefWithoutSettings(t *testing.T) {
	repoDir := t.TempDir()
	outputDir := filepath.Join(t.TempDir(), "export")
	checkpointID := "ddd444ddd444"
	checkpointDir := "dd/d444ddd444"
	sessionMetadataPath := checkpointDir + "/0/metadata.json"
	transcriptPath := checkpointDir + "/0/full.jsonl"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
			err: errors.New("local v1 branch missing"),
		},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {
			stdout: checkpointDir + "/metadata.json\n" + sessionMetadataPath + "\n" + transcriptPath + "\n",
		},
		fakeCommandKey("git", "cat-file", "-p", v1OriginRef+":"+checkpointDir+"/metadata.json"): {
			stdout: `{"sessions":[{"metadata":"/dd/d444ddd444/0/metadata.json","transcript":"/dd/d444ddd444/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1OriginRef+":"+sessionMetadataPath): {
			stdout: `{
  "checkpoint_id": "ddd444ddd444",
  "session_id": "origin-session",
  "agent": "Codex",
  "created_at": "2026-02-06T00:00:00Z"
}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1OriginRef+":"+transcriptPath): {
			stdout: "{\"type\":\"message\",\"text\":\"origin v1\"}\n",
		},
	}}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot: repoDir,
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 2, 6, 1, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "export", "--output", outputDir, "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}

	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(outputDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(manifest.Sessions) != 1 || manifest.Sessions[0].LatestCheckpoint != checkpointID {
		t.Fatalf("unexpected sessions: %+v", manifest.Sessions)
	}
	if strings.Contains(strings.Join(manifest.Warnings, "\n"), "checkpoint ref ") {
		t.Fatalf("manifest leaked debug checkpoint ref warning: %+v", manifest.Warnings)
	}
	if strings.Contains(out, "local v1 branch missing") {
		t.Fatalf("export output leaked debug checkpoint ref warning:\n%s", out)
	}
	for _, call := range runner.calls {
		if call.name == "entire-test" {
			t.Fatalf("origin-tracking v1 export should not shell out to Entire, calls: %+v", runner.calls)
		}
	}
}

func TestSnapshotMetadataSkipsNewerSessionWithoutTranscript(t *testing.T) {
	treePaths := map[string]struct{}{
		"aa/a111aaa111/metadata.json":   {},
		"aa/a111aaa111/0/metadata.json": {},
		"aa/a111aaa111/0/full.jsonl":    {},
		"bb/b222bbb222/metadata.json":   {},
		"bb/b222bbb222/0/metadata.json": {},
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":aa/a111aaa111/metadata.json"): {
			stdout: `{"sessions":[{"metadata":"/aa/a111aaa111/0/metadata.json","transcript":"/aa/a111aaa111/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":bb/b222bbb222/metadata.json"): {
			stdout: `{"sessions":[{"metadata":"/bb/b222bbb222/0/metadata.json","transcript":"/bb/b222bbb222/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":aa/a111aaa111/0/metadata.json"): {
			stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "session_id": "session-one",
  "created_at": "2026-01-01T00:00:00Z"
}`,
		},
	}}

	reader := &checkpointBlobReader{runner: runner, gitDir: "/repo/root", ref: v1RemoteRef}
	selected, _, warnings, err := readCheckpointSnapshotMetadata(context.Background(), reader, v1TranscriptFileName, treePaths, 10, checkpointBranchDestinations{}, nil)
	if err != nil {
		t.Fatalf("read snapshot metadata: %v", err)
	}
	session, ok := selected[selectedSessionKey("", "session-one")]
	if !ok {
		t.Fatalf("session-one was not selected: %+v", selected)
	}
	if session.CheckpointID != "aaa111aaa111" {
		t.Fatalf("selected checkpoint = %q, want older checkpoint with transcript", session.CheckpointID)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "skipped 1 session metadata entries with no transcript bytes") {
		t.Fatalf("missing skipped transcript warning: %v", warnings)
	}
	for _, call := range runner.calls {
		for _, arg := range call.args {
			if strings.Contains(arg, "bb/b222bbb222/0/metadata.json") {
				t.Fatalf("newer metadata without transcript should not be read, calls: %+v", runner.calls)
			}
		}
	}
}

func TestSnapshotMetadataSelectsLatestSessionPerBranch(t *testing.T) {
	treePaths := map[string]struct{}{
		"aa/a111aaa111/metadata.json":   {},
		"aa/a111aaa111/0/metadata.json": {},
		"aa/a111aaa111/0/full.jsonl":    {},
		"bb/b222bbb222/metadata.json":   {},
		"bb/b222bbb222/0/metadata.json": {},
		"bb/b222bbb222/0/full.jsonl":    {},
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":aa/a111aaa111/metadata.json"): {
			stdout: `{"branch":"main","sessions":[{"metadata":"/aa/a111aaa111/0/metadata.json","transcript":"/aa/a111aaa111/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":bb/b222bbb222/metadata.json"): {
			stdout: `{"branch":"feature/branch","sessions":[{"metadata":"/bb/b222bbb222/0/metadata.json","transcript":"/bb/b222bbb222/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":aa/a111aaa111/0/metadata.json"): {
			stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "session_id": "session-one",
  "branch": "main",
  "created_at": "2026-01-01T00:00:00Z"
}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1RemoteRef+":bb/b222bbb222/0/metadata.json"): {
			stdout: `{
  "checkpoint_id": "bbb222bbb222",
  "session_id": "session-one",
  "branch": "feature/branch",
  "created_at": "2026-01-02T00:00:00Z"
}`,
		},
	}}

	reader := &checkpointBlobReader{runner: runner, gitDir: "/repo/root", ref: v1RemoteRef}
	selected, _, _, err := readCheckpointSnapshotMetadata(context.Background(), reader, v1TranscriptFileName, treePaths, 10, checkpointBranchDestinations{}, nil)
	if err != nil {
		t.Fatalf("read snapshot metadata: %v", err)
	}
	if len(selected) != 2 {
		t.Fatalf("selected sessions = %d, want 2: %+v", len(selected), selected)
	}
	byBranch := map[string]selectedSession{}
	for _, session := range selected {
		byBranch[session.Branch] = session
	}
	if byBranch["main"].CheckpointID != "aaa111aaa111" {
		t.Fatalf("main selected checkpoint = %q, want aaa111aaa111", byBranch["main"].CheckpointID)
	}
	if byBranch["feature/branch"].CheckpointID != "bbb222bbb222" {
		t.Fatalf("feature selected checkpoint = %q, want bbb222bbb222", byBranch["feature/branch"].CheckpointID)
	}
}

func TestSelectedSessionOnDefaultBranchOverridesSourceBranch(t *testing.T) {
	selected := map[string]selectedSession{}
	destinations := checkpointBranchDestinations{
		DefaultBranch: "main",
		DefaultCheckpointIDs: map[string]struct{}{
			"aaa111aaa111": {},
		},
	}

	addSelectedSession(selected, selectedSession{
		CheckpointID: "aaa111aaa111",
		SessionID:    "session-one",
		Branch:       "feature/already-merged",
		CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, destinations)

	session, ok := selected[selectedSessionKey("main", "session-one")]
	if !ok {
		t.Fatalf("session was not selected for main: %+v", selected)
	}
	if session.Branch != "main" {
		t.Fatalf("session branch = %q, want main", session.Branch)
	}
	if session.SourceBranch != "feature/already-merged" {
		t.Fatalf("source branch = %q, want original feature branch", session.SourceBranch)
	}
}

func TestCheckpointAuthorsFromGitLog(t *testing.T) {
	data := []byte(
		"commit-one\x00Alice\x00alice@example.com\x002026-01-01T00:00:00Z\x00Add thing\n\nEntire-Checkpoint: aaa111aaa111\n" + gitLogRecordSeparator +
			"commit-two\x00Bob\x00bob@example.com\x002026-01-02T00:00:00Z\x00Review thing\n\nEntire-Checkpoint: aaa111aaa111\nEntire-Checkpoint: bbb222bbb222\n" + gitLogRecordSeparator +
			"commit-three\x00Alice\x00alice@example.com\x002026-01-03T00:00:00Z\x00Follow-up\n\nEntire-Checkpoint: aaa111aaa111\nEntire-Checkpoint: aaa111aaa111\n" + gitLogRecordSeparator,
	)

	index := checkpointAuthorsFromGitLog(data)
	authors := index.AuthorsFor("aaa111aaa111")
	if len(authors) != 2 {
		t.Fatalf("authors len = %d, want 2: %+v", len(authors), authors)
	}
	if authors[0].Name != "Alice" || authors[0].Email != "alice@example.com" || authors[0].Commits != 2 {
		t.Fatalf("first author = %+v, want Alice with two commits", authors[0])
	}
	if authors[0].LastCommitAt == nil || !authors[0].LastCommitAt.Equal(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("first author last commit = %+v, want 2026-01-03", authors[0].LastCommitAt)
	}
	if authors[1].Name != "Bob" || authors[1].Commits != 1 {
		t.Fatalf("second author = %+v, want Bob with one commit", authors[1])
	}

	authors = index.AuthorsFor("bbb222bbb222")
	if len(authors) != 1 || authors[0].Name != "Bob" {
		t.Fatalf("bbb authors = %+v, want Bob", authors)
	}
}

func TestExportSelectsLatestCheckpointPerSession(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"enabled":true,"strategy_options":{"checkpoints_version":2}}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	outputDir := filepath.Join(t.TempDir(), "export")
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef): {
			stdout: "aa/a111aaa111/metadata.json\nbb/b222bbb222/metadata.json\nbb/b222bbb222/0/metadata.json\n",
		},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
			stdout: "aa/a111aaa111/metadata.json\n",
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
			stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "checkpoints_count": 1,
  "session_count": 1,
  "sessions": [
    {
      "index": 0,
      "session_id": "session-one",
      "agent": "Codex",
      "model": "gpt-old",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}`,
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "bbb222bbb222"): {
			stdout: `{
  "checkpoint_id": "bbb222bbb222",
  "checkpoints_count": 2,
  "session_count": 2,
  "sessions": [
    {
      "index": 0,
      "session_id": "session-one",
      "agent": "Codex",
      "model": "gpt-new",
      "created_at": "2026-01-03T00:00:00Z",
      "summary": {
        "intent": "newer work",
        "outcome": "done"
      }
    },
    {
      "index": 1,
      "session_id": "session-two",
      "agent": "Claude Code",
      "model": "sonnet",
      "created_at": "2026-01-02T12:00:00Z"
    }
  ]
}`,
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "0", "bbb222bbb222"): {
			stdout: "{\"type\":\"message\",\"text\":\"latest session one\"}\n",
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "1", "bbb222bbb222"): {
			stdout: "{\"type\":\"message\",\"text\":\"session two\"}\n",
		},
	}}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			CLIVersion: "cli-test",
			RepoRoot:   repoDir,
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "export", "--output", outputDir, "--checkpoint-limit", "2", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	if !strings.Contains(out, "exported 2 sessions from 2 checkpoints") {
		t.Fatalf("unexpected output:\n%s", out)
	}

	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(outputDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	if len(manifest.Sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2", len(manifest.Sessions))
	}
	if manifest.Sessions[0].SessionID != "session-two" {
		t.Fatalf("first chronological session = %q, want session-two", manifest.Sessions[0].SessionID)
	}
	sessionOne := manifest.Sessions[1]
	if sessionOne.SessionID != "session-one" {
		t.Fatalf("second chronological session = %q, want session-one", sessionOne.SessionID)
	}
	if sessionOne.LatestCheckpoint != "bbb222bbb222" {
		t.Fatalf("session one checkpoint = %q, want latest checkpoint", sessionOne.LatestCheckpoint)
	}
	if sessionOne.Model != "gpt-new" {
		t.Fatalf("session one model = %q, want gpt-new", sessionOne.Model)
	}
	if sessionOne.Summary == nil || sessionOne.Summary.Intent != "newer work" {
		t.Fatalf("session one summary missing latest metadata: %+v", sessionOne.Summary)
	}

	transcript, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(sessionOne.TranscriptPath)))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(transcript), "latest session one") {
		t.Fatalf("transcript missing latest content:\n%s", transcript)
	}

	for _, call := range runner.calls {
		if call.dir != repoDir {
			t.Fatalf("command dir = %q, want %s", call.dir, repoDir)
		}
	}
}

func TestExportRejectsNonEmptyOutputDirectory(t *testing.T) {
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outputDir, "existing.txt"), []byte("old"), 0o600); err != nil {
		t.Fatalf("seed output dir: %v", err)
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Runner:  &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})

	_, err := execute(t, cmd, "export", "--output", outputDir)
	if err == nil {
		t.Fatal("export returned nil error for non-empty output directory")
	}
	if !strings.Contains(err.Error(), "output directory is not empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildBranchDirectoriesStableAcrossSlugCollisions(t *testing.T) {
	base := buildBranchDirectories([]exportSession{{Branch: "a/b"}}, "main")
	withCollision := buildBranchDirectories([]exportSession{{Branch: "a/b"}, {Branch: "a-b"}}, "main")

	if base["main"] != filepath.Join(exportSessionsDirectory, "main") {
		t.Fatalf("default branch directory changed: %q", base["main"])
	}
	if base["a/b"] != withCollision["a/b"] {
		t.Fatalf("branch directory moved when a colliding branch appeared: base=%q collision=%q", base["a/b"], withCollision["a/b"])
	}
	if withCollision["a/b"] == withCollision["a-b"] {
		t.Fatalf("colliding branches share a transcript directory: %+v", withCollision)
	}
	if !strings.HasPrefix(withCollision["a/b"], filepath.Join(exportSessionsDirectory, exportBranchesDirectory, "a-b-")) {
		t.Fatalf("unexpected stable branch directory: %q", withCollision["a/b"])
	}
}

func TestReadBrainRelativeFileRejectsSymlinkComponents(t *testing.T) {
	brainDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.jsonl"), []byte("outside brain\n"), 0o600); err != nil {
		t.Fatalf("write outside transcript: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(brainDir, "sessions")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	content, err := readBrainRelativeFile(brainDir, "sessions/secret.jsonl")
	if err == nil {
		t.Fatalf("expected symlink rejection, read %q", content)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestWriteTranscriptFileRejectsSymlinkDirectory(t *testing.T) {
	outputDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(outputDir, exportSessionsDirectory)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := writeTranscriptFile(outputDir, filepath.Join(exportSessionsDirectory, "main", "session.jsonl"), []byte("safe\n"))
	if err == nil {
		t.Fatal("expected writeTranscriptFile to reject symlink directory")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
}

func TestReuseTranscriptFromCursorRejectsSymlinkedTranscript(t *testing.T) {
	outputDir := t.TempDir()
	relPath := filepath.Join(exportSessionsDirectory, "main", "session.jsonl")
	if err := os.MkdirAll(filepath.Join(outputDir, exportSessionsDirectory, "main"), 0o700); err != nil {
		t.Fatalf("create transcript dir: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outsidePath, []byte("outside\n"), 0o600); err != nil {
		t.Fatalf("write outside transcript: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(outputDir, relPath)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	session := exportSession{Branch: "main", SessionID: "sess1", LatestCheckpoint: "cp1"}
	cursor := &exportCursor{Sessions: map[string]exportCursorSession{
		selectedSessionKey(session.Branch, session.SessionID): {
			LatestCheckpoint: session.LatestCheckpoint,
			TranscriptMode:   "raw",
			TranscriptPath:   filepath.ToSlash(relPath),
		},
	}}

	if reuseTranscriptFromCursor(outputDir, cursor, session, relPath, "raw") {
		t.Fatal("expected cursor reuse to reject symlinked transcript")
	}
}

func TestExportDefaultUsesBrainAndCursor(t *testing.T) {
	repoDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")

	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {
			stdout: "https://github.com/entireio/cli.git\n",
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--limit", "10000"): {
			stdout: `[{"checkpoint_id":"aaa111aaa111","date":"2026-01-01T00:00:00Z","is_logs_only":true}]`,
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
			stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "checkpoints_count": 1,
  "session_count": 1,
  "sessions": [
    {
      "index": 0,
      "session_id": "session-one",
      "agent": "Codex",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}`,
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "0", "aaa111aaa111"): {
			stdout: "{\"type\":\"message\",\"text\":\"session one\"}\n",
		},
	}}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:       repoDir,
			PluginDataDir:  dataDir,
			PluginStateDir: stateDir,
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "export", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}

	brain, err := brainDirForKey(EntireEnv{PluginDataDir: dataDir}, "gh/entireio/cli")
	if err != nil {
		t.Fatalf("brain dir: %v", err)
	}
	cursor, err := headPathForKey(EntireEnv{PluginStateDir: stateDir}, "gh/entireio/cli")
	if err != nil {
		t.Fatalf("head path: %v", err)
	}
	if !strings.Contains(out, "output: "+brain) {
		t.Fatalf("export did not use brain dir:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(brain, exportManifestFileName)); err != nil {
		t.Fatalf("manifest not written to brain: %v", err)
	}
	if _, err := os.Stat(cursor); err != nil {
		t.Fatalf("head not written to state dir: %v", err)
	}
	for _, stale := range []string{
		filepath.Join(brain, exportSessionsDirectory, "main", "stale.json"),
		filepath.Join(brain, exportSessionsDirectory, "main", "stale.jsonl"),
	} {
		if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
			t.Fatalf("create stale dir: %v", err)
		}
		if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
			t.Fatalf("write stale transcript: %v", err)
		}
	}

	secondRunner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {
			stdout: "https://github.com/entireio/cli.git\n",
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--limit", "10000"): {
			stdout: `[{"checkpoint_id":"aaa111aaa111","date":"2026-01-01T00:00:00Z","is_logs_only":true}]`,
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
			stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "checkpoints_count": 1,
  "session_count": 1,
  "sessions": [
    {
      "index": 0,
      "session_id": "session-one",
      "agent": "Codex",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}`,
		},
	}}
	cmd = NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:       repoDir,
			PluginDataDir:  dataDir,
			PluginStateDir: stateDir,
		},
		Runner: secondRunner,
		Now: func() time.Time {
			return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
		},
	})
	if _, err := execute(t, cmd, "export", "--entire-binary", "entire-test"); err != nil {
		t.Fatalf("second export should reuse cursor transcript: %v", err)
	}
	for _, call := range secondRunner.calls {
		if len(call.args) > 2 && call.args[0] == "checkpoint" && call.args[1] == "explain" && call.args[2] == "--transcript" {
			t.Fatalf("second export pulled unchanged transcript despite cursor: %+v", secondRunner.calls)
		}
	}
	for _, stale := range []string{
		filepath.Join(brain, exportSessionsDirectory, "main", "stale.json"),
		filepath.Join(brain, exportSessionsDirectory, "main", "stale.jsonl"),
	} {
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Fatalf("stale transcript still exists after persistent export: %s", stale)
		}
	}
}
