package cli

import (
	"context"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/command/dash"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestFilelessReadsSurviveExportingWorktreeRemoval(t *testing.T) {
	opts, _, repo := filelessCheckpointFixture(t)
	worktree := filepath.Join(t.TempDir(), "linked")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("worktree", "add", "--detach", worktree)
	opts.Env.RepoRoot = worktree
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, worktree)
	if err != nil {
		t.Fatal(err)
	}
	runFilelessExport(t, opts, "", false)
	manifest, err := bmanifest.Load(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	expectedGitDir, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.CheckpointGitDir != expectedGitDir {
		t.Fatalf("shared directory = %q", manifest.CheckpointGitDir)
	}
	rel := manifest.Sessions[0].TranscriptPath
	warm := newTranscriptStore(opts.Runner, storage.BrainDir)
	if _, err := warm.Read(context.Background(), rel); err != nil {
		t.Fatal(err)
	}
	git("worktree", "remove", "--force", worktree)
	for _, store := range []*transcriptStore{warm, newTranscriptStore(opts.Runner, storage.BrainDir)} {
		data, err := store.Read(context.Background(), rel)
		if err != nil || string(data) != conversationClaudeFixture {
			t.Fatalf("read after worktree removal: %v %q", err, data)
		}
	}
}

// Only routed discovery is scripted; Git runs against real local refs under
// the production no-egress environment policy.
type routedLocalTranscriptRunner struct {
	ExecRunner
	session string
	index   int
	calls   int
}

func (r *routedLocalTranscriptRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if name != "entire-test" {
		return r.ExecRunner.Run(ctx, dir, name, args...)
	}
	r.calls++
	joined := strings.Join(args, " ")
	if strings.HasPrefix(joined, "checkpoint explain --json") {
		if args[len(args)-1] == "aaa111aaa111" {
			return []byte(fmt.Sprintf(`{"checkpoint_id":"aaa111aaa111","branch":"main","sessions":[{"index":%d,"session_id":%q,"branch":"main","created_at":"2026-01-01T00:00:00Z"}]}`, r.index, r.session)), nil, nil
		}
		return []byte(`[{"checkpoint_id":"aaa111aaa111","is_logs_only":true}]`), nil, nil
	}
	return nil, nil, fmt.Errorf("unexpected routed command %v", args)
}

func TestFilelessRoutedCatalogUsesLocalIdentities(t *testing.T) {
	for _, scope := range []string{exportScopeAll, exportScopeBranch} {
		t.Run(scope, func(t *testing.T) {
			opts, brain, repo := filelessCheckpointFixture(t)
			if err := os.WriteFile(filepath.Join(repo, ".entire", "settings.json"), []byte(`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"example/checkpoints"}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			runner := &routedLocalTranscriptRunner{session: "session-one"}
			opts.Runner = runner
			run := func() error {
				cmd := &cobra.Command{}
				cmd.SetOut(io.Discard)
				return runExport(context.Background(), cmd, opts, exportCommandOptions{scope: scope, entireBinary: "entire-test", rawTranscript: true})
			}
			if err := run(); err != nil {
				t.Fatal(err)
			}
			if runner.calls == 0 {
				t.Fatal("routed discovery was bypassed")
			}
			manifest, err := bmanifest.Load(brain)
			if err != nil {
				t.Fatal(err)
			}
			data, err := newTranscriptStore(opts.Runner, brain).Read(context.Background(), manifest.Sessions[0].TranscriptPath)
			if err != nil || string(data) != conversationClaudeFixture {
				t.Fatalf("read: %v %q", err, data)
			}
			before, err := os.ReadFile(filepath.Join(brain, bmanifest.FileName))
			if err != nil {
				t.Fatal(err)
			}
			for _, mismatch := range []struct {
				session string
				index   int
			}{{"remote-only-session", 0}, {"session-one", 1}} {
				runner.session, runner.index = mismatch.session, mismatch.index
				if err := run(); err == nil || !strings.Contains(err.Error(), "requires local checkpoint refs") {
					t.Fatalf("missing local identity accepted: %v", err)
				}
				after, err := os.ReadFile(filepath.Join(brain, bmanifest.FileName))
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Fatal("refused refresh changed manifest")
				}
			}
		})
	}
}

func TestFilelessSessionsConflictWithBundleImport(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", false)
	bundle := filepath.Join(t.TempDir(), "bundle")
	runFilelessExport(t, opts, bundle, false)
	conflicts, err := brainBundleLayerConflicts(brain, bundle)
	if err != nil || !reflect.DeepEqual(conflicts, []string{bundleLayerSessions}) {
		t.Fatalf("conflicts = %v, %v", conflicts, err)
	}
	empty := t.TempDir()
	conflicts, err = brainBundleLayerConflicts(empty, bundle)
	if err != nil || len(conflicts) != 0 {
		t.Fatalf("empty brain conflicts = %v, %v", conflicts, err)
	}
}

func TestFilelessDashboardSourcesReadOnDemand(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", false)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	session := manifest.Sessions[0]
	if err := os.RemoveAll(filepath.Join(brain, "sessions")); err != nil {
		t.Fatal(err)
	}
	reader := brainSourceReader(context.Background(), opts.Runner, brain)
	targets := []string{
		dash.SessionViews(manifest.Sessions, brain, 0, sessionReadGuard{})[0].Source,
		dash.HistoryViews([]historyRecord{{Path: session.TranscriptPath}}, brain, 0)[0].Source,
		dash.FactViews([]factRecord{{Provenance: []factAnchor{{Transcript: session.TranscriptPath}}}}, brain, 0)[0].Source,
		dash.SearchResultFromUnified(unifiedResult{Source: "history", Path: session.TranscriptPath}, brain).OpenPath,
	}
	for _, target := range targets {
		data, handled, err := reader(target)
		if err != nil || !handled || data != conversationClaudeFixture {
			t.Fatalf("open %s: handled=%v %v %q", target, handled, err, data)
		}
	}
	if _, handled, err := reader(filepath.Join(brain, "docs", "source.md")); err != nil || handled {
		t.Fatalf("ordinary file intercepted: %v %v", handled, err)
	}
	stones := brainstore.EmptySessionTombstones()
	stones.Excluded[session.SessionID] = sessionTombstone{At: opts.Now(), Reason: "purged"}
	if err := brainstore.SaveSessionTombstones(brain, stones); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		data, handled, err := reader(target)
		if err == nil || !handled || data != "" {
			t.Fatalf("purged source opened: %v %v %q", handled, err, data)
		}
	}
	if _, err := os.Stat(filepath.Join(brain, "sessions")); !os.IsNotExist(err) {
		t.Fatalf("viewer materialized transcripts: %v", err)
	}
}

// The tree and metadata remain readable after removing only a transcript blob.
// Refresh must reject this before changing the existing catalog.
func TestFilelessRefreshRejectsMissingTranscriptObjects(t *testing.T) {
	for _, scope := range []string{"direct", "routed", "branch"} {
		for _, variant := range []string{"raw", "compact", "chunk"} {
			t.Run(scope+"/"+variant, func(t *testing.T) {
				opts, brain, repo := filelessCheckpointFixture(t)
				git := func(args ...string) string {
					t.Helper()
					cmd := exec.Command("git", args...)
					cmd.Dir = repo
					cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("git %v: %v %s", args, err, out)
					}
					return strings.TrimSpace(string(out))
				}
				root := checkpointPath("aaa111aaa111")
				missingPath := root + "/0/full.jsonl"
				if variant != "raw" {
					if variant == "compact" {
						missingPath = root + "/0/compact.jsonl"
						metadata := fmt.Sprintf(`{"branch":"main","sessions":[{"metadata":"/%s/0/metadata.json","transcript":"/%s/0/full.jsonl","compact_transcript":"/%s/0/compact.jsonl"}]}`, root, root, root)
						if err := os.WriteFile(filepath.Join(repo, root, "metadata.json"), []byte(metadata), 0600); err != nil {
							t.Fatal(err)
						}
					} else {
						missingPath += ".001"
					}
					if err := os.WriteFile(filepath.Join(repo, missingPath), []byte("unique transcript part\n"), 0600); err != nil {
						t.Fatal(err)
					}
					git("add", ".")
					git("-c", "commit.gpgsign=false", "commit", "-qm", "add transcript variant")
					git("update-ref", v1MainRef, "HEAD")
				}
				exportScope := exportScopeAll
				if scope != "direct" {
					opts.Runner = &routedLocalTranscriptRunner{session: "session-one"}
					if scope == "branch" {
						exportScope = exportScopeBranch
					} else {
						if err := os.WriteFile(filepath.Join(repo, ".entire", "settings.json"), []byte(`{"enabled":true,"strategy_options":{"checkpoint_remote":{"provider":"github","repo":"example/checkpoints"}}}`), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				run := func() error {
					cmd := &cobra.Command{}
					cmd.SetOut(io.Discard)
					return runExport(context.Background(), cmd, opts, exportCommandOptions{scope: exportScope, entireBinary: "entire-test", rawTranscript: variant != "compact"})
				}
				if err := run(); err != nil {
					t.Fatalf("initial refresh: %v", err)
				}
				before, err := os.ReadFile(filepath.Join(brain, bmanifest.FileName))
				if err != nil {
					t.Fatal(err)
				}
				oid := git("rev-parse", v1MainRef+":"+missingPath)
				objectPath := filepath.Join(repo, ".git", "objects", oid[:2], oid[2:])
				blob, err := os.ReadFile(objectPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(objectPath); err != nil {
					t.Fatal(err)
				}
				if err := run(); err == nil || !strings.Contains(err.Error(), "unreadable local transcript") || !strings.Contains(err.Error(), missingPath) {
					t.Fatalf("missing blob was not rejected: %v", err)
				}
				after, err := os.ReadFile(filepath.Join(brain, bmanifest.FileName))
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(before) {
					t.Fatal("failed validation replaced the session catalog")
				}
				if err := os.WriteFile(objectPath, blob, 0444); err != nil {
					t.Fatal(err)
				}
				if err := run(); err != nil {
					t.Fatalf("restored blob did not heal refresh: %v", err)
				}
			})
		}
	}
}

func TestLocalTranscriptValidationCannotFetchOrReadPayloads(t *testing.T) {
	opts, brain, repo := filelessCheckpointFixture(t)
	runFilelessExport(t, opts, "", false)
	manifest, err := bmanifest.Load(brain)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := loadLocalCheckpointSnapshotLocalOnly(context.Background(), opts.Runner, repo, true, checkpointBranchDestinations{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Cleanup()
	source := snapshot.Sources[snapshot.SessionCheckpoints[0].SourceKey]
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-t", source.Ref+":"+checkpointPath("aaa111aaa111")+"/0/full.jsonl"): {stdout: "blob\n"},
	}}
	if err := validateLocalTranscriptSessions(context.Background(), runner, snapshot, manifest.Sessions, "raw"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %+v", runner.calls)
	}
	call := runner.calls[0]
	if call.env[gitNoLazyFetchEnv] != "1" || strings.Join(call.args[:2], " ") != "cat-file -t" {
		t.Fatalf("validation read bytes or allowed fetching: %+v", call)
	}
}
