package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCommandRunner struct {
	responses           map[string]fakeCommandResponse
	sequences           map[string][]fakeCommandResponse
	semanticSnapshotAny *fakeCommandResponse
	// fallback answers commands no explicit response covers, so a fixture can
	// script a WALKER (any revision range at any --max-count) instead of
	// pre-baking the one range a caller happened to ask for last. Explicit
	// responses still win, so a test can force a specific failure.
	fallback func(name string, args []string) (fakeCommandResponse, bool)
	calls    []fakeCommandCall
}

type fakeCommandResponse struct {
	stdout string
	stderr string
	err    error
}

type runOnlyCommandRunner struct {
	inner *fakeCommandRunner
}

func (r runOnlyCommandRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return r.inner.Run(ctx, dir, name, args...)
}

type fakeCommandCall struct {
	dir         string
	name        string
	args        []string
	env         map[string]string
	hasDeadline bool
}

func (r *fakeCommandRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return r.run(ctx, dir, nil, name, args...)
}

func (r *fakeCommandRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	return r.run(ctx, dir, env, name, args...)
}

func (r *fakeCommandRunner) run(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	key := fakeCommandKey(name, args...)
	var hasDeadline bool
	if ctx != nil {
		_, hasDeadline = ctx.Deadline()
	}
	r.calls = append(r.calls, fakeCommandCall{
		dir:         dir,
		name:        name,
		args:        append([]string(nil), args...),
		env:         mapsClone(env),
		hasDeadline: hasDeadline,
	})

	if sequence, ok := r.sequences[key]; ok && len(sequence) > 0 {
		response := sequence[0]
		r.sequences[key] = sequence[1:]
		return []byte(response.stdout), []byte(response.stderr), response.err
	}

	response, ok := r.responses[key]
	if !ok {
		if r.semanticSnapshotAny != nil && fakeCommandIsSemanticSnapshotAnyRepo(name, args) {
			response := *r.semanticSnapshotAny
			return []byte(response.stdout), []byte(response.stderr), response.err
		}
		if key == fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD") {
			return []byte("origin/main\n"), nil, nil
		}
		if r.fallback != nil {
			if response, handled := r.fallback(name, args); handled {
				return []byte(response.stdout), []byte(response.stderr), response.err
			}
		}
		return nil, nil, errors.New("unexpected command: " + key)
	}
	return []byte(response.stdout), []byte(response.stderr), response.err
}

func mapsClone(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

// Stream mirrors Run for tests, exposing the configured stdout as a streaming
// reader so the streaming semantic indexer exercises the same fixtures. The
// deadline is recorded identically to Run so timeout assertions still hold.
func (r *fakeCommandRunner) Stream(ctx context.Context, dir, name string, args ...string) (CommandStream, error) {
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

	response := r.lookupResponse(key, name, args)
	return &fakeCommandStream{
		stdout: strings.NewReader(response.stdout),
		stderr: []byte(response.stderr),
		err:    response.err,
	}, nil
}

func (r *fakeCommandRunner) lookupResponse(key, name string, args []string) fakeCommandResponse {
	if sequence, ok := r.sequences[key]; ok && len(sequence) > 0 {
		response := sequence[0]
		r.sequences[key] = sequence[1:]
		return response
	}
	if response, ok := r.responses[key]; ok {
		return response
	}
	if r.semanticSnapshotAny != nil && fakeCommandIsSemanticSnapshotAnyRepo(name, args) {
		return *r.semanticSnapshotAny
	}
	return fakeCommandResponse{err: errors.New("unexpected command: " + key)}
}

type fakeCommandStream struct {
	stdout *strings.Reader
	stderr []byte
	err    error
}

func (s *fakeCommandStream) Stdout() io.Reader     { return s.stdout }
func (s *fakeCommandStream) Wait() ([]byte, error) { return s.stderr, s.err }
func (s *fakeCommandStream) Close() error          { return nil }

func fakeCommandKey(name string, args ...string) string {
	return name + "\x00" + strings.Join(args, "\x00")
}

func fakeCommandIsSemanticSnapshotAnyRepo(name string, args []string) bool {
	if name != "entire" || len(args) < 6 {
		return false
	}
	if args[0] != "graph" || args[1] != "snapshot" || args[2] != "--repo" {
		return false
	}
	for _, arg := range args {
		if arg == "--ignore-file" {
			return false
		}
	}
	return true
}

func TestDiscoverCheckpointsUsesCompleteRoutedRemoteUnion(t *testing.T) {
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
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef):                  {err: errors.New("local v2 missing")},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                  {err: errors.New("local v1 missing")},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef):                {err: errors.New("local origin missing")},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all", "--limit", "10"): {
			stdout: `[
  {"checkpoint_id":"bbb222bbb222","is_logs_only":true},
  {"checkpoint_id":"aaa111aaa111","is_logs_only":true}
]`,
		},
	}}

	checkpoints, warnings, err := discoverCheckpoints(context.Background(), runner, repoDir, "entire-test", exportScopeAll, 10)
	if err != nil {
		t.Fatalf("discover checkpoints: %v", err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoint count = %d, want 2: %+v", len(checkpoints), checkpoints)
	}
	if checkpoints[0].CheckpointID != "bbb222bbb222" || checkpoints[1].CheckpointID != "aaa111aaa111" {
		t.Fatalf("unexpected checkpoints: %+v", checkpoints)
	}

	for _, call := range runner.calls {
		if call.name == "git" && len(call.args) > 0 && call.args[0] == "fetch" {
			t.Fatalf("complete routed discovery must not perform aggregate-only fetches: %+v", call)
		}
	}
	if len(warnings) == 0 {
		t.Fatalf("expected local source diagnostics, got none")
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

func TestListAllCheckpointRefsUnionsPerCheckpointAndLegacyRefs(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "checkpoints": {"primary": {"type": "git-refs"}}
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	const ulid = "01KVBJCWYA4YW6J5M9GP655HZN"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {
			stdout: checkpointRefPrefix + "ZN/" + ulid + "\n" +
				checkpointRefPrefix + "v2/main\n" +
				checkpointRefPrefix + "XX/01KVBJCWYA4YW6J5M9GP655HZN\n",
		},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
			stdout: "aa/a111aaa111/metadata.json\n",
		},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {
			err: errors.New("origin ref missing"),
		},
	}}

	checkpoints, _, err := listAllCheckpointRefs(context.Background(), runner, repoDir, 10)
	if err != nil {
		t.Fatalf("listAllCheckpointRefs: %v", err)
	}
	if len(checkpoints) != 2 {
		t.Fatalf("checkpoint count = %d, want 2: %+v", len(checkpoints), checkpoints)
	}
	want := map[string]bool{"aaa111aaa111": true, ulid: true}
	for _, checkpoint := range checkpoints {
		if !want[checkpoint.CheckpointID] {
			t.Fatalf("unexpected checkpoint ID %q in %+v", checkpoint.CheckpointID, checkpoints)
		}
		delete(want, checkpoint.CheckpointID)
	}
	if len(want) != 0 {
		t.Fatalf("missing checkpoint IDs: %v", want)
	}
}

func TestCheckpointIDFromRefNameValidatesLayoutAndShard(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "ulid", ref: checkpointRefPrefix + "ZN/01KVBJCWYA4YW6J5M9GP655HZN", want: "01KVBJCWYA4YW6J5M9GP655HZN"},
		{name: "migrated hex", ref: checkpointRefPrefix + "f6/a1b2c3d4e5f6", want: "a1b2c3d4e5f6"},
		{name: "wrong shard", ref: checkpointRefPrefix + "AA/01KVBJCWYA4YW6J5M9GP655HZN"},
		{name: "aggregate v2 ref", ref: v2MainRef},
		{name: "extra component", ref: checkpointRefPrefix + "ZN/extra/01KVBJCWYA4YW6J5M9GP655HZN"},
		{name: "lowercase ulid", ref: checkpointRefPrefix + "zn/01kvbjcwya4yw6j5m9gp655hzn"},
		{name: "overflow ulid", ref: checkpointRefPrefix + "ZN/81KVBJCWYA4YW6J5M9GP655HZN"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := checkpointIDFromRefName(test.ref)
			if ok != (test.want != "") || got != test.want {
				t.Fatalf("checkpointIDFromRefName(%q) = (%q, %t), want (%q, %t)", test.ref, got, ok, test.want, test.want != "")
			}
		})
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

func TestDiscoverCheckpointsGitRefsCombinesLegacyAndRoutedResults(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "checkpoints": {"primary": {"type": "git-refs"}}
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	const ulid = "01KVBJCWYA4YW6J5M9GP655HZN"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
			stdout: "aa/a111aaa111/metadata.json\n",
		},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {
			err: errors.New("origin legacy ref missing"),
		},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all", "--limit", "10"): {
			stdout: `[
  {"checkpoint_id":"01KVBJCWYA4YW6J5M9GP655HZN","is_logs_only":true},
  {"checkpoint_id":"aaa111aaa111","is_logs_only":true}
]`,
		},
	}}

	checkpoints, warnings, err := discoverCheckpoints(context.Background(), runner, repoDir, "entire-test", exportScopeAll, 10)
	if err != nil {
		t.Fatalf("discover checkpoints: %v", err)
	}
	if len(checkpoints) != 2 || checkpoints[0].CheckpointID != ulid || checkpoints[1].CheckpointID != "aaa111aaa111" {
		t.Fatalf("combined checkpoints = %+v", checkpoints)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "combined locally enumerated") {
		t.Fatalf("missing combined-discovery warning: %v", warnings)
	}
}

func TestDiscoverCheckpointsUnknownBackendCombinesLocalAndRoutedResults(t *testing.T) {
	repoDir := t.TempDir()
	const ulid = "01KVBJCWYA4YW6J5M9GP655HZN"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
			stdout: "aa/a111aaa111/metadata.json\n",
		},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef): {
			err: errors.New("origin legacy ref missing"),
		},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all", "--limit", "10"): {
			stdout: `[{"checkpoint_id":"01KVBJCWYA4YW6J5M9GP655HZN","is_logs_only":true}]`,
		},
	}}

	checkpoints, warnings, err := discoverCheckpoints(context.Background(), runner, repoDir, "entire-test", exportScopeAll, 10)
	if err != nil {
		t.Fatalf("discover checkpoints: %v", err)
	}
	if len(checkpoints) != 2 || checkpoints[0].CheckpointID != ulid || checkpoints[1].CheckpointID != "aaa111aaa111" {
		t.Fatalf("combined checkpoints = %+v", checkpoints)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "combined locally enumerated") {
		t.Fatalf("missing combined-discovery warning: %v", warnings)
	}
}

func TestListAllRoutedCheckpointsOmitsLimitWhenUnbounded(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all"): {
			stdout: `[{"checkpoint_id":"01KVBJCWYA4YW6J5M9GP655HZN","is_logs_only":true}]`,
		},
	}}
	checkpoints, warnings, err := listAllRoutedCheckpoints(context.Background(), runner, "/repo", "entire-test", 0)
	if err != nil || len(warnings) != 0 || len(checkpoints) != 1 {
		t.Fatalf("unbounded routed list = checkpoints:%+v warnings:%v err:%v", checkpoints, warnings, err)
	}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].args, " ") != "checkpoint explain --json --search-all" {
		t.Fatalf("unexpected routed argv: %+v", runner.calls)
	}
}

func TestListAllRoutedCheckpointsClassifiesStructuredIncompleteScope(t *testing.T) {
	stderr := entireCheckpointScopePrefix + `{"schema_version":1,"code":"checkpoint_scope_incomplete","complete":false,"issues":[{"code":"checkpoint_remote_enumeration_failed","count":1}]}` + "\n"
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all"): {
			stdout: `[{"checkpoint_id":"aaa111aaa111","is_logs_only":true}]`, stderr: stderr,
		},
	}}
	checkpoints, warnings, err := listAllRoutedCheckpoints(context.Background(), runner, "/repo", "entire-test", 0)
	if err != nil || len(checkpoints) != 1 {
		t.Fatalf("structured incomplete routed list = checkpoints:%+v warnings:%v err:%v", checkpoints, warnings, err)
	}
	if !hasCheckpointScopeIncompleteWarning(warnings) || !strings.Contains(strings.Join(warnings, "\n"), "checkpoint_remote_enumeration_failed=1") {
		t.Fatalf("structured scope status was not classified: %v", warnings)
	}
	if strings.Contains(strings.Join(warnings, "\n"), entireCheckpointScopePrefix) {
		t.Fatalf("raw machine record leaked into product warnings: %v", warnings)
	}
}

func TestDiscoverCheckpointsRejectsIncompleteZeroResult(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all"): {
			stdout: `[]`,
			stderr: entireCheckpointScopePrefix + `{"schema_version":1,"code":"checkpoint_scope_incomplete","complete":false,"issues":[{"code":"local_checkpoint_unreadable","count":1}]}` + "\n",
		},
	}}
	checkpoints, warnings, err := discoverCheckpoints(context.Background(), runner, "/repo", "entire-test", exportScopeAll, 0)
	if err == nil || !strings.Contains(err.Error(), checkpointScopeIncompleteCode) || len(checkpoints) != 0 {
		t.Fatalf("incomplete zero result = checkpoints:%+v warnings:%v err:%v", checkpoints, warnings, err)
	}
	if strings.Contains(strings.Join(warnings, "\n"), "complete routed checkpoint list") {
		t.Fatalf("incomplete scope was described as complete: %v", warnings)
	}
}

func TestSelectRoutedCheckpointSessionsSkipsUnreadableDetailSibling(t *testing.T) {
	checkpoints := []checkpointListEntry{
		{CheckpointID: "aaa111aaa111", IsLogsOnly: true},
		{CheckpointID: "bbb222bbb222", IsLogsOnly: true, Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
			err: errors.New("backend diagnostic containing private transcript text"),
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "bbb222bbb222"): {
			stdout: `{"checkpoint_id":"bbb222bbb222","branch":"main","sessions":[{"index":0,"session_id":"readable-session","branch":"main"}]}`,
		},
	}}
	selected, warnings, err := selectRoutedCheckpointSessions(context.Background(), runner, "/repo", "entire-test", checkpoints, checkpointBranchDestinations{}, nil)
	if err != nil || len(selected) != 1 {
		t.Fatalf("partial routed detail selection = selected:%+v warnings:%v err:%v", selected, warnings, err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), checkpointScopeIncompleteCode+": skipped checkpoint aaa111aaa111") {
		t.Fatalf("missing typed unreadable-detail warning: %v", warnings)
	}
	if strings.Contains(strings.Join(warnings, "\n"), "private transcript text") {
		t.Fatalf("backend content leaked through warning: %v", warnings)
	}

	selected, warnings, err = selectRoutedCheckpointSessions(context.Background(), runner, "/repo", "entire-test", checkpoints[:1], checkpointBranchDestinations{}, nil)
	if err == nil || len(selected) != 0 || !strings.Contains(err.Error(), checkpointScopeIncompleteCode) {
		t.Fatalf("all-unreadable routed details = selected:%+v warnings:%v err:%v", selected, warnings, err)
	}
	if strings.Contains(err.Error(), "private transcript text") {
		t.Fatalf("backend content leaked through terminal error: %v", err)
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
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef):                  {err: errors.New("local v2 missing")},
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

func TestLoadConfiguredCheckpointSnapshotGitRefsPrimaryRequiresReadableLocalUnion(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{
  "enabled": true,
  "checkpoints": {"primary": {"type": "git-refs"}}
}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}

	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 10, checkpointBranchDestinations{}, nil, nil)
	if snapshot != nil {
		t.Fatalf("expected routed fallback, got snapshot %+v", snapshot)
	}
	if !errors.Is(err, errCheckpointSnapshotUnavailable) {
		t.Fatalf("expected checkpoint snapshot unavailable, got %v", err)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected source diagnostics, got %v", warnings)
	}
	if len(runner.calls) == 0 {
		t.Fatalf("git-refs primary must inspect both local checkpoint stores")
	}
}

func TestLoadConfiguredCheckpointSnapshotUsesLocalPrimaryWithoutBaseSettings(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.local.json"), []byte(`{
  "checkpoints": {"primary": {"type": "git-refs"}}
}`), 0o600); err != nil {
		t.Fatalf("write local settings: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}

	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 10, checkpointBranchDestinations{}, nil, nil)
	if snapshot != nil {
		t.Fatalf("expected routed fallback, got snapshot %+v", snapshot)
	}
	if !errors.Is(err, errCheckpointSnapshotUnavailable) {
		t.Fatalf("expected checkpoint snapshot unavailable, got %v", err)
	}
	if len(warnings) == 0 {
		t.Fatalf("expected source diagnostics, got %v", warnings)
	}
	if len(runner.calls) == 0 {
		t.Fatalf("local git-refs primary must inspect the local union even without base settings")
	}
}

func TestConfiguredCheckpointPrimaryPrecedence(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatalf("create settings dir: %v", err)
	}
	base := entireSettingsFile{Checkpoints: &entireCheckpointsSettings{
		Primary: entireCheckpointBackendSettings{Type: checkpointBackendGitBranch},
	}}

	if err := os.WriteFile(filepath.Join(settingsDir, "settings.local.json"), []byte(`{
  "checkpoints": {"primary": {"type": "git-refs"}}
}`), 0o600); err != nil {
		t.Fatalf("write local settings: %v", err)
	}
	primary, configured, err := configuredCheckpointPrimary(repoDir, base)
	if err != nil || !configured || primary != checkpointBackendGitRefs {
		t.Fatalf("local selection = (%q, %t, %v), want git-refs", primary, configured, err)
	}

	t.Setenv(checkpointPrimaryEnv, checkpointBackendGitBranch)
	primary, configured, err = configuredCheckpointPrimary(repoDir, base)
	if err != nil || !configured || primary != checkpointBackendGitBranch {
		t.Fatalf("environment selection = (%q, %t, %v), want git-branch", primary, configured, err)
	}
}

func TestConfiguredCheckpointPrimaryMatchesEntireParserFidelity(t *testing.T) {
	t.Run("environment values are trimmed but not lowercased", func(t *testing.T) {
		repoDir := t.TempDir()
		t.Setenv(checkpointPrimaryEnv, "  git-refs  ")
		primary, configured, err := configuredCheckpointPrimary(repoDir, entireSettingsFile{})
		if err != nil || !configured || primary != checkpointBackendGitRefs {
			t.Fatalf("trimmed environment selection = (%q, %t, %v)", primary, configured, err)
		}
		t.Setenv(checkpointPrimaryEnv, "Git-Refs")
		if _, _, err := configuredCheckpointPrimary(repoDir, entireSettingsFile{}); err == nil {
			t.Fatal("mixed-case environment backend must remain invalid")
		}
	})

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "file whitespace is not normalized", body: `{"checkpoints":{"primary":{"type":" git-refs "}}}`},
		{name: "file case is not normalized", body: `{"checkpoints":{"primary":{"type":"Git-Refs"}}}`},
		{name: "unknown checkpoint field fails closed", body: `{"checkpoints":{"primary":{"type":"git-refs"},"primry":{"type":"git-branch"}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			repoDir := t.TempDir()
			settingsDir := filepath.Join(repoDir, ".entire")
			if err := os.MkdirAll(settingsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, configured, err := configuredCheckpointPrimary(repoDir, entireSettingsFile{}); err == nil || !configured {
				t.Fatalf("selection = configured=%t err=%v, want strict error", configured, err)
			}
		})
	}
}

func TestLoadConfiguredCheckpointSnapshotUnionsAggregateAndGitRefsNewestFirst(t *testing.T) {
	const (
		legacyID = "aaa111aaa111"
		ulidID   = "01KVBJCWYA4YW6J5M9GP655HZN"
	)
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-branch"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyRoot := checkpointPath(legacyID)
	ulidRef := checkpointRefPrefix + ulidID[len(ulidID)-2:] + "/" + ulidID
	newRunner := func() *fakeCommandRunner {
		return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
				stdout: legacyRoot + "/metadata.json\n" + legacyRoot + "/0/metadata.json\n" + legacyRoot + "/0/full.jsonl\n",
			},
			fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {stdout: ulidRef + "\n"},
			fakeCommandKey("git", "ls-tree", "-r", "--name-only", ulidRef): {
				stdout: "metadata.json\n0/metadata.json\n0/full.jsonl\n",
			},
			fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+legacyRoot+"/metadata.json"): {
				stdout: `{"branch":"main","sessions":[{"metadata":"/` + legacyRoot + `/0/metadata.json","transcript":"/` + legacyRoot + `/0/full.jsonl"}]}`,
			},
			fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+legacyRoot+"/0/metadata.json"): {
				stdout: `{"checkpoint_id":"` + legacyID + `","session_id":"legacy-session","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
			},
			fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+legacyRoot+"/0/full.jsonl"): {stdout: "legacy transcript\n"},
			fakeCommandKey("git", "cat-file", "-p", ulidRef+":metadata.json"): {
				stdout: `{"branch":"feature","sessions":[{"metadata":"/0/metadata.json","transcript":"/0/full.jsonl"}]}`,
			},
			fakeCommandKey("git", "cat-file", "-p", ulidRef+":0/metadata.json"): {
				stdout: `{"checkpoint_id":"` + ulidID + `","session_id":"ulid-session","branch":"feature","created_at":"2026-02-01T00:00:00Z"}`,
			},
			fakeCommandKey("git", "cat-file", "-p", ulidRef+":0/full.jsonl"): {stdout: "ulid transcript\n"},
		}}
	}

	limited, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), newRunner(), repoDir, true, 1, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		t.Fatalf("limited union: %v (%v)", err, warnings)
	}
	if limited.CheckpointCount != 1 || len(limited.Selected) != 1 {
		t.Fatalf("limited snapshot = checkpoints:%d selected:%+v", limited.CheckpointCount, limited.Selected)
	}
	for _, session := range limited.Selected {
		if session.CheckpointID != ulidID {
			t.Fatalf("limit selected %q, want newest %q", session.CheckpointID, ulidID)
		}
	}

	unboundedRunner := newRunner()
	unbounded, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), unboundedRunner, repoDir, true, 0, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		t.Fatalf("unbounded union: %v (%v)", err, warnings)
	}
	if unbounded.CheckpointCount != 2 || len(unbounded.Selected) != 2 || len(unbounded.Sources) != 2 {
		t.Fatalf("unbounded snapshot = checkpoints:%d sources:%d selected:%+v", unbounded.CheckpointCount, len(unbounded.Sources), unbounded.Selected)
	}
	if got := usefulExportWarnings(warnings, false); len(got) != 0 {
		t.Fatalf("healthy mixed-backend routing emitted user warnings: %v", got)
	}
	for _, session := range unbounded.Selected {
		data, err := readSnapshotTranscriptFromSource(context.Background(), unboundedRunner, unbounded, session.SourceKey, session.SourceTranscriptPath)
		if err != nil {
			t.Fatalf("read %s transcript: %v", session.CheckpointID, err)
		}
		if session.CheckpointID == legacyID && string(data) != "legacy transcript\n" {
			t.Fatalf("legacy transcript = %q", data)
		}
		if session.CheckpointID == ulidID && string(data) != "ulid transcript\n" {
			t.Fatalf("ULID transcript = %q", data)
		}
	}
}

func TestLoadConfiguredCheckpointSnapshotSkipsUnreadableSibling(t *testing.T) {
	const (
		readableID = "aaa111aaa111"
		corruptID  = "01KVBJCWYA4YW6J5M9GP655HZN"
	)
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-branch"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := checkpointPath(readableID)
	corruptRef := checkpointRefPrefix + corruptID[len(corruptID)-2:] + "/" + corruptID
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {
			stdout: root + "/metadata.json\n" + root + "/0/metadata.json\n" + root + "/0/full.jsonl\n",
		},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {stdout: corruptRef + "\n"},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", corruptRef):                 {err: errors.New("checkpoint object is missing")},
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/metadata.json"): {
			stdout: `{"branch":"main","sessions":[{"metadata":"/` + root + `/0/metadata.json","transcript":"/` + root + `/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+root+"/0/metadata.json"): {
			stdout: `{"checkpoint_id":"` + readableID + `","session_id":"readable-session","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
		},
	}}

	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, true, 0, checkpointBranchDestinations{}, nil, nil)
	if err != nil {
		t.Fatalf("partial local catalog should retain readable sibling: %v (%v)", err, warnings)
	}
	if snapshot.CheckpointCount != 1 || !checkpointExistsInSnapshot(snapshot, readableID) {
		t.Fatalf("readable checkpoint missing from snapshot: %+v", snapshot)
	}
	if !checkpointUnreadableInSnapshot(snapshot, corruptID) {
		t.Fatalf("corrupt checkpoint was not retained as unreadable: %+v", snapshot.UnreadableCheckpoints)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), checkpointScopeIncompleteCode+": skipped unreadable local checkpoint "+corruptID) {
		t.Fatalf("missing typed incomplete-scope warning: %v", warnings)
	}
}

func TestCheckpointRootSummaryRejectsDuplicatePathOwnership(t *testing.T) {
	const (
		badID  = "aaa111aaa111"
		goodID = "bbb222bbb222"
	)
	badRoot := checkpointPath(badID)
	goodRoot := checkpointPath(goodID)
	tests := []struct {
		name       string
		badSummary string
	}{
		{
			name: "metadata pointer",
			badSummary: `{"sessions":[` +
				`{"metadata":"/` + badRoot + `/0/metadata.json","transcript":"/` + badRoot + `/0/full.jsonl"},` +
				`{"metadata":"/` + badRoot + `/0/metadata.json","transcript":"/` + badRoot + `/1/full.jsonl"}]}`,
		},
		{
			name: "transcript pointer",
			badSummary: `{"sessions":[` +
				`{"metadata":"/` + badRoot + `/0/metadata.json","transcript":"/` + badRoot + `/0/full.jsonl"},` +
				`{"metadata":"/` + badRoot + `/1/metadata.json","transcript":"/` + badRoot + `/0/full.jsonl"}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repoDir := t.TempDir()
			settingsDir := filepath.Join(repoDir, ".entire")
			if err := os.MkdirAll(settingsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-branch"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			tree := strings.Join([]string{
				badRoot + "/metadata.json", badRoot + "/0/metadata.json", badRoot + "/0/full.jsonl", badRoot + "/1/metadata.json", badRoot + "/1/full.jsonl",
				goodRoot + "/metadata.json", goodRoot + "/0/metadata.json", goodRoot + "/0/full.jsonl",
			}, "\n") + "\n"
			runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
				fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                  {stdout: tree},
				fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
				fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+badRoot+"/metadata.json"):   {stdout: test.badSummary},
				fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+goodRoot+"/metadata.json"): {
					stdout: `{"sessions":[{"metadata":"/` + goodRoot + `/0/metadata.json","transcript":"/` + goodRoot + `/0/full.jsonl"}]}`,
				},
				fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+goodRoot+"/0/metadata.json"): {
					stdout: `{"checkpoint_id":"` + goodID + `","session_id":"good-session","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
				},
				fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+goodRoot+"/0/full.jsonl"): {stdout: "good transcript\n"},
			}}

			snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, true, 0, checkpointBranchDestinations{}, nil, nil)
			if err != nil || snapshot.CheckpointCount != 1 || !checkpointUnreadableInSnapshot(snapshot, badID) {
				t.Fatalf("duplicate ownership snapshot = snapshot:%+v warnings:%v err:%v", snapshot, warnings, err)
			}
			if !strings.Contains(strings.Join(warnings, "\n"), "root summary assigns a retained path to multiple sessions") {
				t.Fatalf("missing ownership warning: %v", warnings)
			}

			sessions := flattenSessions(snapshot.Selected)
			branchDirs := buildBranchDirectories(sessions, "main")
			outputDir := t.TempDir()
			if err := ensureExportDirectories(outputDir, branchDirs); err != nil {
				t.Fatal(err)
			}
			written, _, _, err := writeSnapshotSessionTranscripts(context.Background(), runner, snapshot, outputDir, sessions, branchDirs, nil)
			if err != nil || len(written) != 1 || written[0].LatestCheckpoint != goodID {
				t.Fatalf("written sessions = %+v err=%v", written, err)
			}
			for _, call := range runner.calls {
				joined := strings.Join(call.args, " ")
				if call.name == "git" && call.args[0] == "cat-file" && strings.Contains(joined, badRoot+"/") && strings.Contains(joined, "full.jsonl") {
					t.Fatalf("duplicate-owned transcript was read: %+v", call)
				}
			}
		})
	}
}

func TestWriteSnapshotTranscriptsSkipsUnreadableSibling(t *testing.T) {
	const (
		badID  = "aaa111aaa111"
		goodID = "bbb222bbb222"
	)
	badPath := snapshotTranscriptPath(badID, 0, v1TranscriptFileName)
	goodPath := snapshotTranscriptPath(goodID, 0, v1TranscriptFileName)
	snapshot := &checkpointSnapshot{
		TranscriptMode:     "raw",
		TranscriptFileName: v1TranscriptFileName,
		Sources: map[string]checkpointSnapshotSource{
			"bad": {
				GitDir: "/repo", Ref: v1MainRef, VirtualRoot: checkpointPath(badID), ActualRoot: checkpointPath(badID),
				TreePaths: map[string]struct{}{badPath: {}},
			},
			"good": {
				GitDir: "/repo", Ref: v1OriginRef, VirtualRoot: checkpointPath(goodID), ActualRoot: checkpointPath(goodID),
				TreePaths: map[string]struct{}{goodPath: {}},
			},
		},
	}
	sessions := []exportSession{
		{SessionID: "bad-session", Branch: "main", LatestCheckpoint: badID, SourceKey: "bad", SourceTranscriptPath: badPath, CreatedAt: time.Now().UTC()},
		{SessionID: "good-session", Branch: "main", LatestCheckpoint: goodID, SourceKey: "good", SourceTranscriptPath: goodPath, CreatedAt: time.Now().UTC()},
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+badPath): {
			err: errors.New("missing object with private transcript diagnostic"),
		},
		fakeCommandKey("git", "cat-file", "-p", v1OriginRef+":"+goodPath): {stdout: "good transcript\n"},
	}}
	branchDirs := buildBranchDirectories(sessions, "main")
	outputDir := t.TempDir()
	if err := ensureExportDirectories(outputDir, branchDirs); err != nil {
		t.Fatal(err)
	}
	written, warnings, _, err := writeSnapshotSessionTranscripts(context.Background(), runner, snapshot, outputDir, sessions, branchDirs, nil)
	if err != nil || len(written) != 1 || written[0].LatestCheckpoint != goodID {
		t.Fatalf("partial transcript write = written:%+v warnings:%v err:%v", written, warnings, err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, checkpointScopeIncompleteCode+": skipped unreadable local checkpoint "+badID) {
		t.Fatalf("missing typed transcript warning: %v", warnings)
	}
	if strings.Contains(joined, "private transcript diagnostic") {
		t.Fatalf("source error content leaked through warning: %v", warnings)
	}
}

func TestLoadConfiguredCheckpointSnapshotRoutesWhenRemoteIsLocalOnly(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-branch"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.local.json"), []byte(`{"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"example/checkpoints"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef): {stdout: "aa/a111aaa111/metadata.json\n"},
	}}
	snapshot, _, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, true, 10, checkpointBranchDestinations{}, nil, nil)
	if snapshot != nil || !errors.Is(err, errCheckpointSnapshotUnavailable) || !strings.Contains(err.Error(), "complete routed discovery") {
		t.Fatalf("local-only remote selection = snapshot:%+v err:%v", snapshot, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("direct local catalog must be skipped before it can hide remote-only refs: %+v", runner.calls)
	}
}

func TestLoadConfiguredCheckpointSnapshotDoesNotMaskGitRefsWithV2FastPath(t *testing.T) {
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"strategy_options":{"checkpoints_version":2},"checkpoints":{"primary":{"type":"git-branch"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef): {stdout: "aa/a111aaa111/metadata.json\n"},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {
			stdout: checkpointRefPrefix + "ZN/01KVBJCWYA4YW6J5M9GP655HZN\n",
		},
	}}
	snapshot, _, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 10, checkpointBranchDestinations{}, nil, nil)
	if snapshot != nil || !errors.Is(err, errCheckpointSnapshotUnavailable) || !strings.Contains(err.Error(), "mixed v2 and git-refs") {
		t.Fatalf("v2 mixed topology = snapshot:%+v err:%v", snapshot, err)
	}
	if len(runner.calls) != 1 || runner.calls[0].args[0] != "for-each-ref" {
		t.Fatalf("v2 topology probe should inspect names only: %+v", runner.calls)
	}
}

func TestLoadConfiguredCheckpointSnapshotKeepsCompleteV2OnlyNoEgress(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"strategy_options":{"checkpoints_version":2},"checkpoints":{"primary":{"type":"git-branch"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := checkpointPath("aaa111aaa111")
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v2MainRef): {
			stdout: root + "/metadata.json\n" + root + "/0/metadata.json\n" + root + "/0/transcript.jsonl\n",
		},
		fakeCommandKey("git", "cat-file", "-p", v2MainRef+":"+root+"/metadata.json"): {
			stdout: `{"branch":"main","sessions":[{"metadata":"/` + root + `/0/metadata.json","transcript":"/` + root + `/0/transcript.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", v2MainRef+":"+root+"/0/metadata.json"): {
			stdout: `{"checkpoint_id":"aaa111aaa111","session_id":"v2-session","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
		},
	}}
	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runner, repoDir, false, 10, checkpointBranchDestinations{}, nil, nil)
	if err != nil || snapshot == nil || snapshot.TranscriptMode != "compact" || snapshot.CheckpointCount != 1 {
		t.Fatalf("v2-only no-egress snapshot = %+v warnings:%v err:%v", snapshot, warnings, err)
	}
	for _, call := range runner.calls {
		if call.env[gitNoLazyFetchEnv] != "1" {
			t.Fatalf("v2-only no-egress read lacks lazy-fetch guard: %+v", call)
		}
	}
}

func TestExportNoEgressUsesOnlyLocalGitRefsObjects(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	const checkpointID = "01KVBJCWYA4YW6J5M9GP655HZN"
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-refs"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := checkpointRefPrefix + checkpointID[len(checkpointID)-2:] + "/" + checkpointID
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                  {err: errors.New("legacy branch absent")},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef):                {err: errors.New("legacy origin absent")},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {stdout: ref + "\n"},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", ref): {
			stdout: "metadata.json\n0/metadata.json\n0/full.jsonl\n",
		},
		fakeCommandKey("git", "cat-file", "-p", ref+":metadata.json"): {
			stdout: `{"branch":"main","sessions":[{"metadata":"/0/metadata.json","transcript":"/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", ref+":0/metadata.json"): {
			stdout: `{"checkpoint_id":"` + checkpointID + `","session_id":"local-session","branch":"main","created_at":"2026-02-01T00:00:00Z"}`,
		},
		fakeCommandKey("git", "cat-file", "-p", ref+":0/full.jsonl"): {stdout: "local only\n"},
	}}
	outputDir := filepath.Join(t.TempDir(), "brain")
	cmd := NewRootCommand(Options{
		Version: "test",
		Env:     EntireEnv{RepoRoot: repoDir},
		Runner:  runner,
		Now:     func() time.Time { return time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC) },
	})
	if out, err := execute(t, cmd, "refresh", "sessions", "--raw", "--output", outputDir, "--entire-binary", "entire-must-not-run"); err != nil {
		t.Fatalf("no-egress export: %v\n%s", err, out)
	}

	for _, call := range runner.calls {
		if call.name == "entire-must-not-run" {
			t.Fatalf("no-egress export invoked Entire: %+v", call)
		}
		if call.name != "git" || len(call.args) == 0 {
			continue
		}
		switch call.args[0] {
		case "fetch", "pull", "push", "ls-remote":
			t.Fatalf("no-egress export invoked network-capable Git: %+v", call)
		case "ls-tree", "for-each-ref", "cat-file":
			if call.env[gitNoLazyFetchEnv] != "1" {
				t.Fatalf("checkpoint object read lacks GIT_NO_LAZY_FETCH=1: %+v", call)
			}
		}
	}
}

func TestExportNoEgressMissingPromisorTranscriptPublishesNothing(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	const checkpointID = "01KVBJCWYA4YW6J5M9GP655HZN"
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-refs"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := checkpointRefPrefix + checkpointID[len(checkpointID)-2:] + "/" + checkpointID
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                  {err: os.ErrNotExist},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef):                {err: os.ErrNotExist},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {stdout: ref + "\n"},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", ref): {
			stdout: "metadata.json\n0/metadata.json\n0/full.jsonl\n",
		},
		fakeCommandKey("git", "cat-file", "-p", ref+":metadata.json"): {
			stdout: `{"sessions":[{"metadata":"/0/metadata.json","transcript":"/0/full.jsonl"}]}`,
		},
		fakeCommandKey("git", "cat-file", "-p", ref+":0/metadata.json"): {
			stdout: `{"checkpoint_id":"` + checkpointID + `","session_id":"missing-blob","branch":"main","created_at":"2026-02-01T00:00:00Z"}`,
		},
		fakeCommandKey("git", "cat-file", "-p", ref+":0/full.jsonl"): {err: errors.New("missing promisor object while lazy fetch is disabled")},
	}}
	outputDir := filepath.Join(t.TempDir(), "unpublished")
	cmd := NewRootCommand(Options{Env: EntireEnv{RepoRoot: repoDir}, Runner: runner, Now: time.Now})
	out, err := execute(t, cmd, "refresh", "sessions", "--raw", "--output", outputDir, "--entire-binary", "entire-must-not-run")
	if err == nil || !strings.Contains(err.Error(), "missing promisor object") {
		t.Fatalf("missing-promisor export = err:%v\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, exportManifestFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed export published a manifest: %v", statErr)
	}
	var files []string
	_ = filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && entry != nil && !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if len(files) != 0 {
		t.Fatalf("failed export published partial files: %v", files)
	}
	for _, call := range runner.calls {
		if call.name == "entire-must-not-run" || (call.name == "git" && len(call.args) > 0 && (call.args[0] == "fetch" || call.args[0] == "ls-remote")) {
			t.Fatalf("missing local object crossed the egress boundary: %+v", call)
		}
	}
}

func TestNoEgressFailsClosedWithoutEnvironmentAwareRunner(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	repoDir := t.TempDir()
	settingsDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), []byte(`{"checkpoints":{"primary":{"type":"git-branch"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	snapshot, warnings, err := loadConfiguredCheckpointSnapshot(context.Background(), runOnlyCommandRunner{inner: inner}, repoDir, true, 0, checkpointBranchDestinations{}, nil, nil)
	if snapshot != nil || !errors.Is(err, errCheckpointSnapshotUnavailable) || !strings.Contains(strings.Join(warnings, "\n"), "cannot enforce GIT_NO_LAZY_FETCH=1") {
		t.Fatalf("non-environment runner result = snapshot:%+v warnings:%v err:%v", snapshot, warnings, err)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("fail-closed boundary executed an unguarded command: %+v", inner.calls)
	}
}

func TestExportNoEgressBranchScopeFailsInsteadOfPublishingFalseEmpty(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	repoDir := t.TempDir()
	outputDir := filepath.Join(t.TempDir(), "branch-brain")
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Env: EntireEnv{RepoRoot: repoDir}, Runner: runner, Now: time.Now})
	out, err := execute(t, cmd, "refresh", "sessions", "--scope", "branch", "--output", outputDir, "--entire-binary", "entire-must-not-run")
	if err == nil || !strings.Contains(err.Error(), "false-empty export") {
		t.Fatalf("branch no-egress result = err:%v\n%s", err, out)
	}
	if _, statErr := os.Stat(outputDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed branch export created output state: %v", statErr)
	}
	for _, call := range runner.calls {
		if call.name == "entire-must-not-run" {
			t.Fatalf("branch no-egress invoked Entire: %+v", call)
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

	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef):                  {err: errors.New("local v1 missing")},
		fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef):                {err: errors.New("local origin missing")},
		fakeCommandKey("git", "for-each-ref", "--format=%(refname)", checkpointRefPrefix): {},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all", "--limit", "10"): {
			stdout: `[{"checkpoint_id":"bbb222bbb222","is_logs_only":true}]`,
		},
	}}

	checkpoints, _, err := discoverCheckpoints(context.Background(), runner, repoDir, "entire-test", exportScopeAll, 10)
	if err != nil {
		t.Fatalf("discover checkpoints: %v", err)
	}
	if len(checkpoints) != 1 || checkpoints[0].CheckpointID != "bbb222bbb222" {
		t.Fatalf("unexpected checkpoints: %+v", checkpoints)
	}

	for _, call := range runner.calls {
		if call.name == "git" && len(call.args) > 0 && call.args[0] == "fetch" {
			t.Fatalf("configured remotes must use complete routed discovery, got fetch: %+v", call)
		}
		for _, arg := range call.args {
			if arg == v2MainRef || strings.Contains(arg, v2MainRef) {
				t.Fatalf("v2 ref should not be used when repo settings default to v1, calls: %+v", runner.calls)
			}
		}
	}
}

func TestExportUsesCompleteRoutedDiscoveryForConfiguredCheckpointRemote(t *testing.T) {
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
			fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all", "--limit", "10"): {
				stdout: `[{"checkpoint_id":"aaa111aaa111","date":"2026-02-03T04:05:06Z","is_logs_only":true}]`,
			},
			fakeCommandKey("entire-test", "checkpoint", "explain", "--json", checkpointID): {
				stdout: `{"checkpoint_id":"aaa111aaa111","branch":"main","sessions":[{"index":0,"checkpoint_id":"aaa111aaa111","session_id":"session-one","branch":"main","agent":"Codex","model":"gpt-5","created_at":"2026-02-03T04:05:06Z","checkpoints_count":7}]}`,
			},
			fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "0", checkpointID): {
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

	out, err := execute(t, cmd, "refresh", "sessions", "--output", outputDir, "--checkpoint-limit", "10", "--entire-binary", "entire-test")
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
	if manifest.TranscriptMode != "compact" {
		t.Fatalf("transcript mode = %q, want compact routed export", manifest.TranscriptMode)
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

	var sawSearchAll bool
	for _, call := range runner.calls {
		if call.name == "entire-test" && strings.Join(call.args, " ") == "checkpoint explain --json --search-all --limit 10" {
			sawSearchAll = true
		}
		if call.name == "git" && len(call.args) > 0 && call.args[0] == "fetch" {
			t.Fatalf("scope-all remote export must not perform aggregate-only fetches: %+v", call)
		}
		for _, arg := range call.args {
			if arg == v2MainRef || strings.Contains(arg, v2MainRef) {
				t.Fatalf("v2 ref should not be used for default v1 settings, calls: %+v", runner.calls)
			}
		}
	}
	if !sawSearchAll {
		t.Fatalf("expected complete routed --search-all discovery, calls: %+v", runner.calls)
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
			fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all", "--limit", "10"): {
				stdout: `[{"checkpoint_id":"aaa111aaa111","date":"2026-02-03T04:05:06Z","is_logs_only":true}]`,
			},
			fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
				stdout: `{"checkpoint_id":"aaa111aaa111","branch":"main","sessions":[{"index":0,"session_id":"session-one","branch":"main","agent":"Codex","created_at":"2026-02-03T04:05:06Z"}]}`,
			},
			fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "0", "aaa111aaa111"): {
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

	out, err := execute(t, cmd, "refresh", "sessions", "--debug", "--output", outputDir, "--checkpoint-limit", "10", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	for _, want := range []string{
		"warning: checkpoint ref refs/heads/entire/checkpoints/v1 unavailable: local v1 missing",
		"warning: used Entire's complete routed checkpoint list because no local metadata refs were enumerable",
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

	out, err := execute(t, cmd, "refresh", "sessions", "--output", outputDir, "--entire-binary", "entire-test")
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

	out, err := execute(t, cmd, "refresh", "sessions", "--output", outputDir, "--entire-binary", "entire-test")
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
	selected, _, _, warnings, err := readCheckpointSnapshotMetadata(context.Background(), reader, v1TranscriptFileName, treePaths, 10, checkpointBranchDestinations{}, nil)
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

func TestSnapshotMetadataRejectsCrossCheckpointSummaryPointers(t *testing.T) {
	const (
		checkpointA = "aaa111aaa111"
		checkpointB = "bbb222bbb222"
	)
	rootA := checkpointPath(checkpointA)
	rootB := checkpointPath(checkpointB)
	refA := checkpointRefPrefix + checkpointA[len(checkpointA)-2:] + "/" + checkpointA
	treePaths := map[string]struct{}{
		rootA + "/metadata.json": {},
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", refA+":metadata.json"): {
			stdout: `{"sessions":[{"metadata":"/` + rootB + `/0/metadata.json","transcript":"/` + rootB + `/0/full.jsonl"}]}`,
		},
	}}
	reader := &checkpointBlobReader{runner: runner, gitDir: "/repo", ref: refA, virtualRoot: rootA}
	_, _, _, warnings, err := readCheckpointSnapshotMetadata(context.Background(), reader, v1TranscriptFileName, treePaths, 0, checkpointBranchDestinations{}, nil)
	if !errors.Is(err, errCheckpointSnapshotUnavailable) || !strings.Contains(strings.Join(warnings, "\n"), "unsafe checkpoint "+checkpointA) {
		t.Fatalf("cross-checkpoint pointer result = warnings:%v err:%v", warnings, err)
	}
	for _, call := range runner.calls {
		for _, arg := range call.args {
			if strings.Contains(arg, rootB) {
				t.Fatalf("checkpoint A caused a read through checkpoint B: %+v", call)
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
	selected, _, _, _, err := readCheckpointSnapshotMetadata(context.Background(), reader, v1TranscriptFileName, treePaths, 10, checkpointBranchDestinations{}, nil)
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

func TestCheckpointIDsFromCommitMessageText(t *testing.T) {
	const ulid = "01KVBJCWYA4YW6J5M9GP655HZN"
	tests := []struct {
		name    string
		message string
		want    []string
	}{
		{
			name: "legacy and canonical ULID trailers",
			message: "Ship feature\n\n" +
				"Entire-Checkpoint: aaa111aaa111\n" +
				"Entire-Checkpoint: " + ulid + "\n" +
				"Entire-Checkpoint: aaa111aaa111\n",
			want: []string{"aaa111aaa111", ulid},
		},
		{
			name: "rejects noncanonical lookalikes",
			message: "Invalid checkpoints\n\n" +
				"Entire-Checkpoint: 01kvbjcwya4yw6j5m9gp655hzn\n" +
				"Entire-Checkpoint: 81KVBJCWYA4YW6J5M9GP655HZN\n" +
				"Entire-Checkpoint: 01KVBJCWYA4YW6J5M9GP655HZI\n" +
				"Entire-Checkpoint: aaa111aaa111f\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := checkpointIDsFromCommitMessageText(test.message)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("checkpoint IDs = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildCheckpointBranchDestinationsRoutesCanonicalULIDToDefaultBranch(t *testing.T) {
	const (
		ulid       = "01KVBJCWYA4YW6J5M9GP655HZN"
		legacyID   = "aaa111aaa111"
		defaultRef = "refs/heads/main"
	)
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "show-ref", "--verify", "--quiet", defaultRef): {},
		fakeCommandKey("git", "log", "--format=%B", "--grep", checkpointTrailerKey+":", defaultRef): {
			stdout: "Merge checkpoints\n\nEntire-Checkpoint: " + ulid + "\nEntire-Checkpoint: " + legacyID + "\n",
		},
	}}

	destinations, warnings := buildCheckpointBranchDestinations(context.Background(), runner, repoDir, "main")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if destinations.DefaultBranch != "main" {
		t.Fatalf("default branch = %q, want main", destinations.DefaultBranch)
	}
	for _, checkpointID := range []string{ulid, legacyID} {
		if _, ok := destinations.DefaultCheckpointIDs[checkpointID]; !ok {
			t.Fatalf("default checkpoint IDs = %v, missing %q", destinations.DefaultCheckpointIDs, checkpointID)
		}
		branches := destinations.BranchesFor(checkpointID, "feature/already-merged")
		if len(branches) != 1 || branches[0] != "main" {
			t.Fatalf("branches for %q = %v, want [main]", checkpointID, branches)
		}
	}
}

func TestCheckpointAuthorsFromGitLog(t *testing.T) {
	const ulid = "01KVBJCWYA4YW6J5M9GP655HZN"
	data := []byte(
		"commit-one\x00Alice\x00alice@example.com\x002026-01-01T00:00:00Z\x00Add thing\n\nEntire-Checkpoint: aaa111aaa111\n" + gitLogRecordSeparator +
			"commit-two\x00Bob\x00bob@example.com\x002026-01-02T00:00:00Z\x00Review thing\n\nEntire-Checkpoint: aaa111aaa111\nEntire-Checkpoint: bbb222bbb222\n" + gitLogRecordSeparator +
			"commit-three\x00Alice\x00alice@example.com\x002026-01-03T00:00:00Z\x00Follow-up\n\nEntire-Checkpoint: aaa111aaa111\nEntire-Checkpoint: aaa111aaa111\n" + gitLogRecordSeparator +
			"commit-four\x00Carol\x00carol@example.com\x002026-01-04T00:00:00Z\x00ULID checkpoint\n\nEntire-Checkpoint: " + ulid + "\n" + gitLogRecordSeparator,
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

	authors = index.AuthorsFor(ulid)
	if len(authors) != 1 || authors[0].Name != "Carol" || authors[0].Email != "carol@example.com" || authors[0].Commits != 1 {
		t.Fatalf("ULID authors = %+v, want Carol with one commit", authors)
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

	out, err := execute(t, cmd, "refresh", "sessions", "--output", outputDir, "--checkpoint-limit", "2", "--entire-binary", "entire-test")
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

	_, err := execute(t, cmd, "refresh", "sessions", "--output", outputDir)
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

	content, err := readBrainRelativeStateFile(brainDir, "sessions/secret.jsonl")
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
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all"): {
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

	out, err := execute(t, cmd, "refresh", "sessions", "--entire-binary", "entire-test")
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
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all"): {
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
	if _, err := execute(t, cmd, "refresh", "sessions", "--entire-binary", "entire-test"); err != nil {
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
