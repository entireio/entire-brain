package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	"github.com/entireio/entire-brain/internal/providerdetail"
	"github.com/spf13/cobra"
)

func TestSemanticIndexCommandParsesGraphProviderFlags(t *testing.T) {
	cmd := newSemanticIndexCommand(Options{})
	if err := cmd.ParseFlags([]string{
		"--skip-graph",
		"--graph-timeout", "17m",
		"--graph-inactivity-timeout", "42s",
	}); err != nil {
		t.Fatalf("parse graph provider flags: %v", err)
	}

	skipGraph, err := cmd.Flags().GetBool("skip-graph")
	if err != nil || !skipGraph {
		t.Fatalf("skip-graph = %v, err = %v", skipGraph, err)
	}
	timeout, err := cmd.Flags().GetDuration("graph-timeout")
	if err != nil || timeout != 17*time.Minute {
		t.Fatalf("graph-timeout = %s, err = %v", timeout, err)
	}
	inactivityTimeout, err := cmd.Flags().GetDuration("graph-inactivity-timeout")
	if err != nil || inactivityTimeout != 42*time.Second {
		t.Fatalf("graph-inactivity-timeout = %s, err = %v", inactivityTimeout, err)
	}
}

func TestSemanticIndexAcceptsEntireProxyOrigin(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: "entire://cluster.example/gh/example/repo\n"}
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     func() time.Time { return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC) },
	}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	manifestPath := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", bmanifest.FileName)
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest under canonical proxy path: %v", err)
	}
}

func TestValidateSemanticRecordPathRejectsEscapingEvidence(t *testing.T) {
	record := semanticRecord{
		RecordType: "relation",
		Evidence:   []semanticEvidence{{Kind: "call", FilePath: "../outside.go"}},
	}
	if err := validateSemanticRecordPath(&record); err == nil || !strings.Contains(err.Error(), "semantic evidence path") {
		t.Fatalf("escaping evidence path was accepted: %v", err)
	}

	record.Evidence[0].FilePath = "internal/safe.go"
	if err := validateSemanticRecordPath(&record); err != nil {
		t.Fatalf("clean repository-relative evidence path: %v", err)
	}
	if got := record.Evidence[0].FilePath; got != "internal/safe.go" {
		t.Fatalf("evidence path was not normalized: %q", got)
	}
}

// TestSemanticProviderRepoKeyAcceptsCaseOnlyDifference: entire-graph preserves
// the case of a github.com remote path while the brain folds its storage key to
// lower case, so gh/Owner/Repo and gh/owner/repo name the same repository.
//
// This assertion used to live on validateLiveSemanticHeader, where it could not
// fail: scanSemanticStream normalizes header.RepoKey to the storage key before
// that function ever sees it. It belongs here, on the one comparison that reads
// the provider's own untrusted spelling.
func TestSemanticProviderRepoKeyAcceptsCaseOnlyDifference(t *testing.T) {
	err := validateSemanticProviderRepoKey(
		context.Background(), nil, t.TempDir(),
		"gh/redacted-contributor/ultron", // the brain's storage key, folded
		"gh/redacted-contributor/Ultron", // what the provider stamped
		"", "entire",
	)
	if err != nil {
		t.Fatalf("case-only repo key difference should be accepted: %v", err)
	}
}

func TestSemanticIndexRunsProviderCommandsWithTimeouts(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     time.Now,
	}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	for _, args := range [][]string{
		{"graph", "doctor", "--json"},
		{"graph", "index", "--repo", repoDir, "--head", "--profile", "full", "--format", "json"},
	} {
		var found bool
		for _, call := range runner.calls {
			if call.name == "entire" && semanticArgsEqual(call.args, args) {
				found = true
				if !call.hasDeadline {
					t.Fatalf("provider call %q did not use a timeout context", strings.Join(args, " "))
				}
			}
		}
		if !found {
			t.Fatalf("provider call %q was not recorded: %+v", strings.Join(args, " "), runner.calls)
		}
	}
}

func TestSemanticIndexPassesBrainignoreToProvider(t *testing.T) {
	repoDir := t.TempDir()
	brainignorePath := filepath.Join(repoDir, ".brainignore")
	if err := os.WriteFile(brainignorePath, []byte("generated/\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     time.Now,
	}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	if !fakeRunnerCalled(runner, "entire", "graph", "index", "--repo", repoDir, "--head", "--profile", "full", "--format", "json", "--ignore-file", brainignorePath) {
		t.Fatalf("semantic provider was not called with .brainignore: %+v", runner.calls)
	}
}

func TestSemanticRefreshAllBranchesWritesBoundedReport(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	runner.responses[fakeCommandKey("git", "rev-parse", "refs/heads/main")] = fakeCommandResponse{stdout: "base111\n"}
	runner.responses[fakeCommandKey("git", "for-each-ref", "--format=%(refname:short)%00%(committerdate:unix)%00%(objectname)", "refs/heads")] = fakeCommandResponse{
		stdout: "recent\x001779840000\x00recent111\nold\x001700000000\x00old111\n",
	}
	opts := Options{Env: env, Runner: runner, Now: func() time.Time { return now }}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := runSemanticRefreshAllBranches((&cobra.Command{}).Context(), opts, refreshCommandOptions{semantic: true}, repoDir); err != nil {
		t.Fatalf("refresh all branches: %v", err)
	}
	reportPath := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", semstore.DirName, "overlays", "all-branches.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read branch report: %v", err)
	}
	if !strings.Contains(string(data), `"branch": "recent"`) || !strings.Contains(string(data), `"state": "overlay_written"`) {
		t.Fatalf("recent branch missing from report:\n%s", data)
	}
	if !strings.Contains(string(data), `"overlay_path": "semantic/overlays/base111..recent111.json"`) {
		t.Fatalf("recent branch overlay missing from report:\n%s", data)
	}
	if !strings.Contains(string(data), `"branch": "old"`) || !strings.Contains(string(data), `"reason": "older_than_30d"`) {
		t.Fatalf("old branch skip missing from report:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", semstore.DirName, "overlays", "base111..recent111.json")); err != nil {
		t.Fatalf("branch overlay was not written: %v", err)
	}
	overlayData, err := os.ReadFile(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", semstore.DirName, "overlays", "base111..recent111.json"))
	if err != nil {
		t.Fatalf("read branch overlay: %v", err)
	}
	if strings.Contains(string(overlayData), "snapshot_path") {
		t.Fatalf("non-current branch overlay should not reuse current snapshot:\n%s", overlayData)
	}
	// Branch overlays are Brain metadata, and must survive later Graph indexes.
	for _, force := range []bool{false, true} {
		if err := runSemanticIndex(context.Background(), &cobra.Command{}, opts, semanticIndexOptions{force: force}, repoDir); err != nil {
			t.Fatal(err)
		}
		for path, want := range map[string][]byte{reportPath: data, filepath.Join(filepath.Dir(reportPath), "base111..recent111.json"): overlayData} {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(want) {
				t.Fatalf("index changed branch metadata %s: %v", path, err)
			}
		}
	}
}

func TestSemanticIndexFailsBeforeSnapshotWhenProviderRequiresNetwork(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("entire", "graph", "doctor", "--json")] = fakeCommandResponse{stdout: `{"requires_network":true}`}
	delete(runner.responses, fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "provider_network_required") {
		t.Fatalf("index err = %v", err)
	}
	for _, call := range runner.calls {
		if call.name == "entire" && len(call.args) > 1 && call.args[0] == "graph" && call.args[1] == "snapshot" {
			t.Fatalf("snapshot was called after network-required diagnostics")
		}
	}
}

func TestSemanticIndexFailsClosedWhenProviderNoEgressUnknown(t *testing.T) {
	// Each case fails closed for a different underlying reason (the doctor
	// call itself errored, its output didn't parse, or it simply said
	// nothing about egress), so each gets its own expected substring instead
	// of one blanket "no-egress" check — see
	// TestSemanticIndexDoctorFailureNamesRootCause for the detailed root-cause
	// coverage of these same failure modes.
	for name, tc := range map[string]struct {
		response    fakeCommandResponse
		wantErrText string
	}{
		"doctor-failed":    {response: fakeCommandResponse{err: os.ErrNotExist}, wantErrText: "doctor failed to run graph binary"},
		"doctor-malformed": {response: fakeCommandResponse{stdout: `{not-json`}, wantErrText: "returned output that could not be parsed"},
		"doctor-unknown":   {response: fakeCommandResponse{stdout: `{}`}, wantErrText: "no-egress"},
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			runner.responses[fakeCommandKey("entire", "graph", "doctor", "--json")] = tc.response
			delete(runner.responses, fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrText) {
				t.Fatalf("index err = %v, want it to contain %q", err, tc.wantErrText)
			}
			for _, call := range runner.calls {
				if call.name == "entire" && len(call.args) > 1 && call.args[0] == "graph" && call.args[1] == "snapshot" {
					t.Fatalf("snapshot was called without verified no-egress")
				}
			}
		})
	}
}

// TestSemanticIndexDoctorFailureNamesRootCause locks down that a semantic
// doctor failure surfaces WHY it failed, not just a blanket "no-egress status
// is not verified" claim. Each doctor failure mode has a distinct underlying
// cause (binary missing from PATH, malformed doctor JSON, provider declaring
// it needs the network, doctor silent on egress) and an operator reading the
// error must be able to tell which one happened and against which binary,
// without re-running with extra flags to find out.
func TestSemanticIndexDoctorFailureNamesRootCause(t *testing.T) {
	for name, tc := range map[string]struct {
		response       fakeCommandResponse
		wantSubstrings []string
	}{
		"exec-not-found": {
			response: fakeCommandResponse{err: errors.New(`entire [graph doctor --json]: exec: "entire": executable file not found in $PATH`)},
			wantSubstrings: []string{
				"entire",
				"executable file not found in $PATH",
			},
		},
		"malformed-json": {
			response: fakeCommandResponse{stdout: `{not-json`},
			wantSubstrings: []string{
				"entire",
				"invalid character",
			},
		},
		"network-required": {
			response: fakeCommandResponse{stdout: `{"requires_network":true}`},
			wantSubstrings: []string{
				"entire",
				"provider doctor reports network access is required",
			},
		},
		"egress-status-unreported": {
			response: fakeCommandResponse{stdout: `{}`},
			wantSubstrings: []string{
				"entire",
				"provider doctor did not report no-egress status",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			runner.responses[fakeCommandKey("entire", "graph", "doctor", "--json")] = tc.response
			delete(runner.responses, fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
			if err == nil {
				t.Fatalf("index err = nil, want a descriptive doctor-failure error")
			}
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("index err = %q, want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestSemanticIndexWorktreeCleanRejectsProviderTreeMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"tree":"tree111"`, `"tree":"dirtytree"`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: snapshot}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire", worktree: true}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "tree") {
		t.Fatalf("index err = %v", err)
	}
}

func TestWorktreeFingerprintIncludesUntrackedContent(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "notes.txt")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: "?? notes.txt\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite untracked: %v", err)
	}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first == second {
		t.Fatalf("fingerprint did not change for untracked content: %s", first)
	}
}

func TestWorktreeFingerprintSkipsSymlinksInsideUntrackedDirectory(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "notes"), 0o700); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("one"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(repoDir, "notes", "secret.txt")); err != nil {
		t.Fatalf("symlink secret: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: "?? notes/\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(secret, []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite secret: %v", err)
	}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprint depended on symlink target: %s != %s", first, second)
	}
}

func TestWorktreeFingerprintIgnoresBrainignoredUntrackedContent(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "ignored"), 0o700); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored", "secret.txt"), []byte("one"), 0o600); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "keep.txt"), []byte("one"), 0o600); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: "?? keep.txt\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored", "secret.txt"), []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite ignored: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? ignored/\n?? keep.txt\n"}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprint changed for ignored content: %s != %s", first, second)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "keep.txt"), []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite keep: %v", err)
	}
	third, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("third fingerprint: %v", err)
	}
	if third == second {
		t.Fatalf("fingerprint did not change for unignored content")
	}
}

func TestWorktreeFingerprintReadsQuotedUntrackedPath(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "new file.go")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: "?? \"new file.go\"\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):                       {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"):           {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite untracked: %v", err)
	}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first == second {
		t.Fatalf("fingerprint did not include quoted untracked path")
	}
}

func TestWorktreeFingerprintUsesIgnoredDiffPathspecs(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	ignore := brainIgnore{patterns: []string{"ignored/"}}
	diffArgs := append([]string{"diff", "--binary", "HEAD", "--", "."}, ignore.gitPathspecExclusions()...)
	cachedArgs := append([]string{"diff", "--cached", "--binary", "HEAD", "--", "."}, ignore.gitPathspecExclusions()...)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: ""},
		fakeCommandKey("git", diffArgs...):                            {},
		fakeCommandKey("git", cachedArgs...):                          {},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {stdout: "ignored diff should not be used"},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {stdout: "ignored cached diff should not be used"},
	}}
	if _, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if !fakeRunnerCalled(runner, "git", diffArgs...) || !fakeRunnerCalled(runner, "git", cachedArgs...) {
		t.Fatalf("ignored pathspec diff was not used: %+v", runner.calls)
	}
}

func TestSemanticIndexAndStaleIgnoreBrainignoredDirtyFiles(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "ignored"), 0o700); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? ignored/\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: semanticTestEnv(t, repoDir), Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index ignored dirty worktree: %v", err)
	}
	report, err := semanticStaleReport(cmd.Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Axes["worktree"].State != "" {
		t.Fatalf("worktree axis = %+v", report.Axes["worktree"])
	}
}

func TestWorktreeDirtyKeepsRenameFromIgnoredToPublic(t *testing.T) {
	repoDir := t.TempDir()
	ignore := brainIgnore{patterns: []string{"ignored/"}}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"): {stdout: "R  ignored/file.go -> public/file.go\n"},
	}}
	dirty, err := worktreeDirtyWithIgnore((&cobra.Command{}).Context(), runner, repoDir, ignore)
	if err != nil {
		t.Fatalf("dirty: %v", err)
	}
	if !dirty {
		t.Fatalf("rename into public path was filtered as clean")
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "R  ignored/file.go -> ignored/other.go\n"}
	dirty, err = worktreeDirtyWithIgnore((&cobra.Command{}).Context(), runner, repoDir, ignore)
	if err != nil {
		t.Fatalf("dirty ignored: %v", err)
	}
	if dirty {
		t.Fatalf("rename within ignored paths was not filtered")
	}
}

func TestWorktreeDirtyUsesExpandedUntrackedStatus(t *testing.T) {
	repoDir := t.TempDir()
	ignore := brainIgnore{patterns: []string{"secrets/*.pem"}}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: "?? secrets/token.pem\n"},
		fakeCommandKey("git", "status", "--porcelain"):                          {stdout: "?? secrets/\n"},
	}}
	dirty, err := worktreeDirtyWithIgnore((&cobra.Command{}).Context(), runner, repoDir, ignore)
	if err != nil {
		t.Fatalf("dirty: %v", err)
	}
	if dirty {
		t.Fatalf("expanded ignored untracked file was treated as dirty")
	}
	if !fakeRunnerCalled(runner, "git", "status", "--porcelain", "--untracked-files=all") {
		t.Fatalf("expanded status was not used: %+v", runner.calls)
	}
}

// TestSemanticIndexNoOpsWhenIndexIsAlreadyCurrent replaces
// TestSemanticIndexRequiresForceWhenIndexExists, which pinned the behaviour
// that made the documented refresh sequence fail on every run after the first:
// a second `refresh index` exited 1 purely because an index existed, while its
// parent `refresh` was idempotent and exited 0.
//
// The contract is now the one a scripted loop can use. Already current is a
// no-op that succeeds and says so; --force still rebuilds unconditionally. The
// cases that make a re-run do real work (stale commit, unreadable store) are
// pinned in honest_reporting_fixes_test.go.
func TestSemanticIndexNoOpsWhenIndexIsAlreadyCurrent(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("first index: %v", err)
	}
	second := &cobra.Command{Use: "index"}
	var out bytes.Buffer
	second.SetOut(&out)
	if err := runSemanticIndex(second.Context(), second, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("second index must be an idempotent no-op: %v", err)
	}
	if !strings.Contains(out.String(), "already current") {
		t.Fatalf("second index did not report the no-op: %q", out.String())
	}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire", force: true}, repoDir); err != nil {
		t.Fatalf("force index: %v", err)
	}
}

func TestSemanticResetRequiresForceAndSemanticOnlyPreservesManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := execute(t, cmd, "reset", "--semantic-only"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("reset without force err = %v", err)
	}
	out, err := execute(t, cmd, "reset", "--semantic-only", "--force")
	if err != nil {
		t.Fatalf("reset semantic-only: %v", err)
	}
	if !strings.Contains(out, "reset code index") {
		t.Fatalf("reset output = %q", out)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	if _, err := os.Stat(filepath.Join(brainDir, semstore.DirName)); !os.IsNotExist(err) {
		t.Fatalf("semantic dir still exists: %v", err)
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources != nil && manifest.Sources.Semantic != nil {
		t.Fatalf("semantic source still present: %+v", manifest.Sources.Semantic)
	}
}

func TestSemanticResetForceLeavesEmptyBrainDirectory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	if _, err := execute(t, cmd, "reset", "--force"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	info, err := os.Stat(brainDir)
	if err != nil {
		t.Fatalf("brain dir missing after reset: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("brain path is not a directory after reset")
	}
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		t.Fatalf("read brain dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != brainstore.BrainLockDirName {
		t.Fatalf("brain dir entries after reset = %+v, want only %s", entries, brainstore.BrainLockDirName)
	}
	if _, err := os.Stat(filepath.Join(brainDir, brainstore.BrainLockDirName, brainstore.BrainWriteLockName)); err != nil {
		t.Fatalf("write lock metadata should remain after reset: %v", err)
	}
}

func TestSemanticJSONErrorsUseStructuredEnvelope(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	for name, args := range map[string][]string{
		"runtime":        {"inspect", "code", "ValidateToken", "--json", "--limit", "0"},
		"args":           {"inspect", "code", "--json"},
		"flags":          {"inspect", "code", "--json", "--bogus"},
		"flags-reversed": {"inspect", "code", "--bogus", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
			out, err := execute(t, cmd, args...)
			if err == nil {
				t.Fatalf("search succeeded unexpectedly:\n%s", out)
			}
			var envelope commandJSONError
			if decodeErr := json.Unmarshal([]byte(out), &envelope); decodeErr != nil {
				t.Fatalf("error output was not JSON: %v\n%s", decodeErr, out)
			}
			if envelope.Code != "command_failed" || envelope.Message == "" {
				t.Fatalf("unexpected JSON error envelope: %+v", envelope)
			}
		})
	}
}

func TestSemanticFlagErrorsStayPlainTextWithoutJSON(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	for name, args := range map[string][]string{
		"omitted":    {"inspect", "code", "ValidateToken", "--bogus"},
		"false-flag": {"inspect", "code", "ValidateToken", "--json=false", "--limit", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
			out, err := execute(t, cmd, args...)
			if err == nil {
				t.Fatalf("query succeeded unexpectedly:\n%s", out)
			}
			if json.Valid([]byte(out)) {
				t.Fatalf("non-json error was rendered as JSON:\n%s", out)
			}
		})
	}
}

func TestSanitizeSemanticWarningTextPreservesColonDiagnostics(t *testing.T) {
	for _, text := range []string{
		"parse failed: unexpected token",
		"warning: skipped optional metadata",
		"file.go:12: unexpected token",
		"GET /tokens/{id}",
	} {
		if got := sanitizeSemanticWarningText(text, ""); got != text {
			t.Fatalf("sanitizeSemanticWarningText(%q) = %q, want unchanged", text, got)
		}
	}
	if got := sanitizeSemanticWarningText("failed at D:/work/repo/file.go", ""); got != "<redacted>" {
		t.Fatalf("Windows absolute path was not redacted: %q", got)
	}
	if got := sanitizeSemanticWarningText("failed at /opt/build/repo/secret.go", ""); got != "<redacted>" {
		t.Fatalf("Unix absolute path was not redacted: %q", got)
	}
	if got := sanitizeSemanticWarningText("failed at /builds/acme/repo/private", ""); got != "<redacted>" {
		t.Fatalf("extensionless Unix absolute path was not redacted: %q", got)
	}
}

func TestSemanticIndexReportsPluginDataWriteFailures(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	dataFile := filepath.Join(t.TempDir(), "plugin-data-file")
	if err := os.WriteFile(dataFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write plugin data file: %v", err)
	}
	env.PluginDataDir = dataFile
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err == nil {
		t.Fatalf("index succeeded with file plugin data dir")
	}
}

func TestSemanticIndexReportsReadOnlyBrainDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod read-only directory semantics are not portable on Windows")
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	brainRoot := filepath.Join(env.PluginDataDir, base.RepoStoreDirName)
	if err := os.MkdirAll(brainRoot, 0o500); err != nil {
		t.Fatalf("mkdir brain root: %v", err)
	}
	defer func() { _ = os.Chmod(brainRoot, 0o700) }()
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err == nil {
		t.Fatalf("index succeeded with read-only brain root")
	}
}

func TestSemanticIndexRejectsProviderPathEscape(t *testing.T) {
	absoluteProviderPath := "/tmp/secret.txt"
	if runtime.GOOS == "windows" {
		absoluteProviderPath = "C:/tmp/secret.txt"
	}
	for name, snapshot := range map[string]string{
		"parent": strings.Replace(semanticFixtureSnapshot("1.0"), `"file_path":"internal/auth/token.go"`, `"file_path":"../../secret.txt"`, 1),
		"windows-drive-relative": `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","path":"C:secret.txt"}
`,
		"windows-drive": `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","path":"C:/tmp/secret.txt"}
`,
		"absolute": `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","path":"` + absoluteProviderPath + `"}
`,
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, snapshot)
			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
			if err == nil || !strings.Contains(err.Error(), "semantic provider path") {
				t.Fatalf("index err = %v", err)
			}
		})
	}
}

func TestBundleExportSanitizesLegacyManifestWarnings(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	absPath := filepath.ToSlash(filepath.Join(repoDir, "legacy", "secret.go"))
	manifest.Sources.Semantic.Warnings = []semanticWarning{{
		Code:     "legacy_absolute",
		Severity: "warning",
		Path:     absPath,
		Effect:   "read " + absPath,
		Detail:   "failed at " + absPath,
	}}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	bundleManifest := readTestBundleFile(t, output, bmanifest.FileName)
	if strings.Contains(string(bundleManifest), repoDir) || strings.Contains(string(bundleManifest), absPath) {
		t.Fatalf("bundle manifest leaked legacy absolute warning path:\n%s", bundleManifest)
	}
}

func TestSemanticIndexDoesNotDefaultIgnoreGitHubPaths(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithGitHubWorkflow())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := bmanifest.Load(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Symbols != 1 {
		t.Fatalf(".github symbol was unexpectedly ignored: %+v", manifest.Sources.Semantic)
	}
}

func TestBrainIgnoreDoesNotOvermatchDefaultDirectoryPrefixes(t *testing.T) {
	ignore := brainIgnore{}
	for _, path := range []string{"vendorized/pkg.go", "distill/main.go", "buildkite/pipeline.yml", "targeted/file.rs"} {
		if ignore.Ignored(path) {
			t.Fatalf("path %s was unexpectedly ignored", path)
		}
	}
	for _, path := range []string{"vendor/pkg.go", "web/dist/app.js", "pkg/build/out.go", "target/debug/app"} {
		if !ignore.Ignored(path) {
			t.Fatalf("path %s was not ignored", path)
		}
	}
}

func TestBrainIgnoreMatchesRecursivePatterns(t *testing.T) {
	ignore := brainIgnore{patterns: []string{"secrets/**", "generated/**/private-*.go"}}
	for _, path := range []string{"secrets/token.go", "secrets/nested/key.go", "generated/private-token.go", "generated/deep/private-token.go"} {
		if !ignore.Ignored(path) {
			t.Fatalf("path %s was not ignored", path)
		}
	}
	for _, path := range []string{"secret/token.go", "generated/deep/public.go"} {
		if ignore.Ignored(path) {
			t.Fatalf("path %s was unexpectedly ignored", path)
		}
	}
}

func TestSemanticIndexKeepsRelationsWhenIgnorePatternMatchesRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	// "repo" is a substring of the repo key (gh/example/repo) and of every
	// symbol ID, but it must only ignore files literally named "repo".
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("repo\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithIgnoredRelationID())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := bmanifest.Load(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Relations != 1 {
		t.Fatalf("relation dropped because ignore pattern matched the repo key substring: %+v", manifest.Sources.Semantic)
	}
}

// TestSemanticEndpointPath is superseded by
// TestSemanticEndpointPathRecoversFieldsAroundColons in
// semantic_endpoint_path_test.go, which covers every case below plus the ID
// shapes whose fields contain ":".

func TestSemanticIndexDoesNotTreatSecretDirectoryAsDefaultIgnore(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithIgnoredRelationID())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := bmanifest.Load(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Relations != 1 {
		t.Fatalf("secret/ relation was treated as default ignored: %+v", manifest.Sources.Semantic)
	}
}

func TestSemanticFileContentHashSkipsOversizedFiles(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "large.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create large file: %v", err)
	}
	if err := f.Truncate(semanticParseCacheMaxFile + 1); err != nil {
		_ = f.Close()
		t.Fatalf("truncate large file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close large file: %v", err)
	}
	if got := semanticFileContentHash(repoDir, "large.bin"); got != "" {
		t.Fatalf("oversized content hash = %q", got)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "small.go"), []byte("package small\n"), 0o600); err != nil {
		t.Fatalf("write small file: %v", err)
	}
	if got := semanticFileContentHash(repoDir, "small.go"); !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("small content hash = %q", got)
	}
}

func TestSemanticStaleReportsDirtyUnindexedWorktree(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/cli/semantic.go\n"}
	report, err := semanticStaleReport(cmd.Context(), Options{Env: env, Runner: runner}, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Severity != "degraded" {
		t.Fatalf("severity = %s, want degraded", report.Severity)
	}
	if report.Axes["worktree"].State != "dirty-unindexed" {
		t.Fatalf("worktree axis = %+v", report.Axes["worktree"])
	}
}

func TestSemanticQueryFindsExactSymbol(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	results, err := semstore.FindSymbolsInSQLite(semstore.GraphStoreSource(runner.graphStoreFixtures[repoDir+"\x00full"], "").StorePath, "ValidateToken", 10, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 || results[0].QualifiedName != "auth.ValidateToken" {
		t.Fatalf("results = %+v", results)
	}
}

func TestSemanticIndexNormalizesSymbolPathIntoSQLiteFilePath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"file_path":"internal/auth/token.go"`, `"path":"internal/auth/token.go"`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	results, err := semstore.FindSymbolsInSQLite(semstore.GraphStoreSource(runner.graphStoreFixtures[repoDir+"\x00full"], "").StorePath, "ValidateToken", 10, 0)
	if err != nil {
		t.Fatalf("query sqlite: %v", err)
	}
	if len(results) != 1 || results[0].FilePath != "internal/auth/token.go" {
		t.Fatalf("results = %+v", results)
	}
}

func TestSemanticQueryTreatsSQLiteSearchTextLiterally(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	results, err := semstore.FindSymbolsInSQLite(semstore.GraphStoreSource(runner.graphStoreFixtures[repoDir+"\x00full"], "").StorePath, "%", 10, 0)
	if err != nil {
		t.Fatalf("query sqlite: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("LIKE wildcard matched literal query: %+v", results)
	}
}

func TestSemanticQueryJSONIncludesPagination(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&out)
	if err := runSemanticQuery(queryCmd.Context(), queryCmd, opts, semanticQueryOptions{limit: 1, offset: 0, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out.String(), `"pagination"`) || !strings.Contains(out.String(), `"count": 1`) {
		t.Fatalf("query JSON missing pagination:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"record_type"`) {
		t.Fatalf("default code JSON retained full provider records:\n%s", out.String())
	}
	out.Reset()
	if err := runSemanticQuery(queryCmd.Context(), queryCmd, opts, semanticQueryOptions{limit: 1, json: true, details: true}, "ValidateToken"); err != nil {
		t.Fatalf("detailed query: %v", err)
	}
	if !strings.Contains(out.String(), `"record_type"`) {
		t.Fatalf("detailed code JSON omitted provider fields:\n%s", out.String())
	}
}

func TestSemanticContextJSONIncludesRelations(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	contextCmd := &cobra.Command{Use: "context"}
	contextCmd.SetOut(&out)
	if err := runSemanticContext(contextCmd.Context(), contextCmd, opts, semanticContextOptions{limit: 10, json: true}, "gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken"); err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out.String(), `"relations"`) || !strings.Contains(out.String(), `"CALLS"`) {
		t.Fatalf("context JSON missing relations:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"record_type"`) {
		t.Fatalf("default context JSON retained full provider records:\n%s", out.String())
	}
	out.Reset()
	if err := runSemanticContext(contextCmd.Context(), contextCmd, opts, semanticContextOptions{limit: 10, json: true, details: true}, "gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken"); err != nil {
		t.Fatalf("detailed context: %v", err)
	}
	if !strings.Contains(out.String(), `"record_type"`) {
		t.Fatalf("detailed context JSON omitted provider fields:\n%s", out.String())
	}
}

func TestSemanticContextJSONIncludesRelationNeighbors(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithQualifiedCallerSymbol())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	contextCmd := &cobra.Command{Use: "context"}
	contextCmd.SetOut(&out)
	if err := runSemanticContext(contextCmd.Context(), contextCmd, opts, semanticContextOptions{limit: 10, json: true}, "gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken"); err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out.String(), `"neighbors"`) || !strings.Contains(out.String(), `"CallValidateToken"`) {
		t.Fatalf("context JSON missing relation neighbor records:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"internal/auth/caller.go"`) {
		t.Fatalf("context JSON missing neighbor file path:\n%s", out.String())
	}
}

func TestSemanticImpactTraversesRelations(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	impactCmd := &cobra.Command{Use: "impact"}
	impactCmd.SetOut(&out)
	if err := runSemanticImpact(impactCmd.Context(), impactCmd, opts, semanticImpactOptions{limit: 10, depth: 1, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("impact: %v", err)
	}
	if !strings.Contains(out.String(), `"relations"`) || !strings.Contains(out.String(), `"CALLS"`) {
		t.Fatalf("impact JSON missing relation:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"record_type"`) {
		t.Fatalf("default impact JSON retained full provider records:\n%s", out.String())
	}
	out.Reset()
	if err := runSemanticImpact(impactCmd.Context(), impactCmd, opts, semanticImpactOptions{limit: 10, depth: 1, json: true, details: true}, "ValidateToken"); err != nil {
		t.Fatalf("detailed impact: %v", err)
	}
	if !strings.Contains(out.String(), `"record_type"`) {
		t.Fatalf("detailed impact JSON omitted provider record fields:\n%s", out.String())
	}
}

func TestSemanticGraphCommandsUseSQLiteStore(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "internal/auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 25)
	for i := range lines {
		lines[i] = fmt.Sprintf("// line %02d", i+1)
	}
	lines[9] = "func ValidateToken(token string) error {"
	lines[10] = "    return nil"
	lines[19] = "}"
	if err := os.WriteFile(filepath.Join(repoDir, "internal/auth/token.go"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithQualifiedCallerSymbol())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	for name, run := range map[string]func(*cobra.Command) error{
		"schema": func(c *cobra.Command) error {
			return runSemanticGraphSchema(c, opts, semanticGraphSchemaOptions{json: true})
		},
		"ui": func(c *cobra.Command) error {
			return runSemanticGraphUI(c, opts, semanticGraphUIOptions{limit: 10, json: true}, filepath.Join(repoDir, "graph.html"))
		},
		"query": func(c *cobra.Command) error {
			return runSemanticQueryGraph(c, opts, semanticGraphQueryOptions{limit: 10, json: true}, `MATCH (a)-[r:CALLS]->(b) WHERE a.name = "CallValidateToken" RETURN a,r,b LIMIT 5`)
		},
		"trace": func(c *cobra.Command) error {
			return runSemanticTracePath(c, opts, semanticTracePathOptions{depth: 2, json: true}, "CallValidateToken", "ValidateToken")
		},
		"snippet": func(c *cobra.Command) error {
			return runSemanticSnippet(c, opts, semanticSnippetOptions{contextLines: 0, json: true}, "ValidateToken")
		},
		"dead": func(c *cobra.Command) error {
			return runSemanticDeadCode(c, opts, semanticDeadCodeOptions{limit: 10, json: true})
		},
	} {
		var out bytes.Buffer
		readCmd := &cobra.Command{Use: name}
		readCmd.SetOut(&out)
		if err := run(readCmd); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "schema" && (!strings.Contains(out.String(), `"metrics"`) || !strings.Contains(out.String(), `"hotspots"`)) {
			t.Fatalf("schema output missing graph metrics:\n%s", out.String())
		}
		if name == "ui" {
			htmlPath := filepath.Join(repoDir, "graph.html")
			data, err := os.ReadFile(htmlPath)
			if err != nil {
				t.Fatalf("read graph ui: %v", err)
			}
			if !strings.Contains(string(data), "Semantic Graph") || !strings.Contains(string(data), "CallValidateToken") {
				t.Fatalf("graph ui missing expected embedded graph data:\n%s", data)
			}
		}
		if name == "trace" {
			var result semanticTracePathResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("trace JSON invalid: %v\n%s", err, out.String())
			}
			if !result.Found || len(result.Path) != 2 {
				t.Fatalf("trace path length = %+v", result)
			}
			if result.Path[0].Name != "CallValidateToken" || result.Path[1].Name != "ValidateToken" {
				t.Fatalf("trace path order = %+v", result.Path)
			}
		}
		if !strings.Contains(out.String(), "ValidateToken") && name != "schema" && name != "ui" {
			t.Fatalf("%s output missing fixture symbol:\n%s", name, out.String())
		}
	}

	var countOut bytes.Buffer
	countCmd := &cobra.Command{Use: "query-count"}
	countCmd.SetOut(&countOut)
	if err := runSemanticQueryGraph(countCmd, opts, semanticGraphQueryOptions{limit: 10, json: true}, `MATCH (a)-[r:CALLS]->(b) WHERE a.name = "CallValidateToken" RETURN count(r)`); err != nil {
		t.Fatalf("query count: %v", err)
	}
	if !strings.Contains(countOut.String(), `"count": 1`) {
		t.Fatalf("query count JSON missing count:\n%s", countOut.String())
	}
	countOut.Reset()
	if err := runSemanticQueryGraph(countCmd, opts, semanticGraphQueryOptions{limit: 10, json: true}, `MATCH (a)-[r]->(b) WHERE a.name = "CallValidateToken" AND r.type = "CALLS" RETURN count(r)`); err != nil {
		t.Fatalf("query count with relation predicate: %v", err)
	}
	if !strings.Contains(countOut.String(), `"count": 1`) {
		t.Fatalf("query count with relation predicate JSON missing count:\n%s", countOut.String())
	}
	countOut.Reset()
	if err := runSemanticQueryGraph(countCmd, opts, semanticGraphQueryOptions{limit: 10, json: true}, `MATCH (a)<-[r:CALLS]-(b) WHERE a.name = "ValidateToken" RETURN count(r)`); err != nil {
		t.Fatalf("reverse query count: %v", err)
	}
	if !strings.Contains(countOut.String(), `"count": 1`) {
		t.Fatalf("reverse query count JSON missing count:\n%s", countOut.String())
	}

	tracePath := filepath.Join(repoDir, "trace.ndjson")
	if err := os.WriteFile(tracePath, []byte(`{"from":"CallValidateToken","to":"ValidateToken","type":"CALLS"}`+"\n"+`{"from":"ValidateToken","to":"CallValidateToken","type":"OBSERVED_CALL"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage, err := repoStoragePaths(cmd.Context(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	beforeIngest, err := bmanifest.Load(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	oldStorePath := runner.graphStoreFixtures[repoDir+"\x00full"].Path
	var out bytes.Buffer
	ingestCmd := &cobra.Command{Use: "ingest"}
	ingestCmd.SetOut(&out)
	if err := runSemanticIngestTraces(ingestCmd, opts, semanticTraceIngestOptions{json: true}, tracePath); err != nil {
		t.Fatalf("ingest traces: %v", err)
	}
	if !strings.Contains(out.String(), `"matched_static_edges": 1`) {
		t.Fatalf("trace ingest did not validate static edge:\n%s", out.String())
	}
	afterIngest, err := bmanifest.Load(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	if afterIngest.Sources.Semantic.GenerationPath != beforeIngest.Sources.Semantic.GenerationPath {
		t.Fatalf("trace ingest created a Brain-owned generation: %s", afterIngest.Sources.Semantic.GenerationPath)
	}
	oldDB, err := sql.Open(sqlitedriver.Name, brainstore.SQLiteReadOnlyDSN(oldStorePath))
	if err != nil {
		t.Fatal(err)
	}
	var oldTraceCount int
	if err := oldDB.QueryRow(`SELECT COUNT(*) FROM runtime_traces`).Scan(&oldTraceCount); err != nil {
		_ = oldDB.Close()
		t.Fatal(err)
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}
	if oldTraceCount != 0 {
		t.Fatalf("previous immutable generation contains %d runtime traces, want 0", oldTraceCount)
	}
	out.Reset()
	queryTraceCmd := &cobra.Command{Use: "query-trace"}
	queryTraceCmd.SetOut(&out)
	if err := runSemanticQueryGraph(queryTraceCmd, opts, semanticGraphQueryOptions{limit: 10, json: true}, "type:RUNTIME_TRACE"); err != nil {
		t.Fatalf("query runtime traces: %v", err)
	}
	if !strings.Contains(out.String(), `"record_type": "runtime_trace"`) || !strings.Contains(out.String(), `"RUNTIME_TRACE"`) {
		t.Fatalf("runtime trace facts were not queryable:\n%s", out.String())
	}
	out.Reset()
	traceRuntimeCmd := &cobra.Command{Use: "trace-runtime"}
	traceRuntimeCmd.SetOut(&out)
	if err := runSemanticTracePath(traceRuntimeCmd, opts, semanticTracePathOptions{depth: 2, json: true}, "ValidateToken", "CallValidateToken"); err != nil {
		t.Fatalf("runtime trace path: %v", err)
	}
	var runtimePath semanticTracePathResult
	if err := json.Unmarshal(out.Bytes(), &runtimePath); err != nil {
		t.Fatalf("runtime trace path JSON invalid: %v\n%s", err, out.String())
	}
	if !runtimePath.Found || len(runtimePath.Relations) != 1 || runtimePath.Relations[0].Type != "RUNTIME_TRACE" {
		t.Fatalf("trace-path did not use runtime trace facts: %+v", runtimePath)
	}
	out.Reset()
	schemaTraceCmd := &cobra.Command{Use: "schema-trace"}
	schemaTraceCmd.SetOut(&out)
	if err := runSemanticGraphSchema(schemaTraceCmd, opts, semanticGraphSchemaOptions{json: true}); err != nil {
		t.Fatalf("schema after traces: %v", err)
	}
	if !strings.Contains(out.String(), `"RUNTIME_TRACE"`) {
		t.Fatalf("schema metrics did not include runtime trace facts:\n%s", out.String())
	}
}

func TestSemanticGraphQueriesDataFlowRelations(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticDataFlowFixtureSnapshot())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	var schemaOut bytes.Buffer
	schemaCmd := &cobra.Command{Use: "schema"}
	schemaCmd.SetOut(&schemaOut)
	if err := runSemanticGraphSchema(schemaCmd, opts, semanticGraphSchemaOptions{json: true}); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if !strings.Contains(schemaOut.String(), `"DATA_FLOWS"`) {
		t.Fatalf("schema output missing DATA_FLOWS:\n%s", schemaOut.String())
	}

	var queryOut bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&queryOut)
	if err := runSemanticQueryGraph(queryCmd, opts, semanticGraphQueryOptions{limit: 10, json: true}, `MATCH (a)-[r:DATA_FLOWS]->(b) WHERE a.name = "run" RETURN a,r,b LIMIT 5`); err != nil {
		t.Fatalf("query data flow: %v", err)
	}
	if !strings.Contains(queryOut.String(), `"DATA_FLOWS"`) ||
		!strings.Contains(queryOut.String(), `"run"`) ||
		!strings.Contains(queryOut.String(), `"normalize"`) {
		t.Fatalf("query output missing data-flow edge:\n%s", queryOut.String())
	}
}

func TestSemanticGraphQueriesResolvedFileImports(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticResolvedImportFixtureSnapshot())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	var queryOut bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&queryOut)
	query := `MATCH (a)-[r:IMPORTS]->(b) WHERE a.file_path CONTAINS "apps/web/src/app.ts" AND b.file_path CONTAINS "packages/utils/src/index.ts" RETURN a,r,b LIMIT 5`
	if err := runSemanticQueryGraph(queryCmd, opts, semanticGraphQueryOptions{limit: 10, json: true}, query); err != nil {
		t.Fatalf("query resolved imports: %v", err)
	}
	output := queryOut.String()
	for _, want := range []string{`"IMPORTS"`, `"record_type": "file"`, `"apps/web/src/app.ts"`, `"packages/utils/src/index.ts"`} {
		if !strings.Contains(output, want) {
			t.Fatalf("query output missing %s:\n%s", want, output)
		}
	}

	var countOut bytes.Buffer
	countCmd := &cobra.Command{Use: "query-count"}
	countCmd.SetOut(&countOut)
	if err := runSemanticQueryGraph(countCmd, opts, semanticGraphQueryOptions{limit: 10, json: true}, `MATCH (a)-[r:IMPORTS]->(b) WHERE a.kind = "file" AND b.name = "index.ts" RETURN count(r)`); err != nil {
		t.Fatalf("query resolved import count: %v", err)
	}
	if !strings.Contains(countOut.String(), `"count": 1`) {
		t.Fatalf("query count JSON missing resolved import count:\n%s", countOut.String())
	}
}

func TestSemanticImpactReturnsRelationsWhenRootsFillLimit(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	roots, symbols, relations, err := semstore.ImpactFacts(brainDir, semstore.GraphStoreSource(runner.graphStoreFixtures[repoDir+"\x00full"], ""), "ValidateToken", 1, 1)
	if err != nil {
		t.Fatalf("impact facts: %v", err)
	}
	if len(roots) != 1 || len(symbols) != 1 || len(relations) != 1 {
		t.Fatalf("impact = roots %+v symbols %+v relations %+v", roots, symbols, relations)
	}
}

func TestInspectChangesPersistsLatestReportByDefault(t *testing.T) {
	cmd := newInspectChangesCommand(Options{})
	flag := cmd.Flags().Lookup("write-report")
	if flag == nil || flag.DefValue != "true" {
		t.Fatalf("write-report default = %v, want true", flag)
	}
}

func TestSemanticChangesMapsChangedFilesToSymbols(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(out.String(), `"internal/auth/token.go"`) || !strings.Contains(out.String(), `"ValidateToken"`) {
		t.Fatalf("changes JSON missing symbol:\n%s", out.String())
	}
	reportPath := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", semstore.DirName, "changes", "latest.json")
	if _, err := os.Stat(reportPath); !os.IsNotExist(err) {
		t.Fatalf("default changes unexpectedly wrote report: %v", err)
	}
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true, persist: true}); err != nil {
		t.Fatalf("changes with persisted report: %v", err)
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("persisted changes report missing: %v", err)
	}
}

func TestSemanticChangesMapsDiffHunksInsteadOfFirstFileSymbols(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticChangesRangeFixtureSnapshot())
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}
	runner.responses[fakeCommandKey("git", "diff", "--unified=0", "--no-ext-diff", "--no-color", "--no-prefix", "HEAD")] = fakeCommandResponse{stdout: "diff --git internal/auth/token.go internal/auth/token.go\n--- internal/auth/token.go\n+++ internal/auth/token.go\n@@ -12 +12 @@\n-old\n+new\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	var payload struct {
		Changes semanticChangesReport `json:"changes"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode changes: %v\n%s", err, out.String())
	}
	if len(payload.Changes.Ranges) != 1 || payload.Changes.Ranges[0].StartLine != 12 || payload.Changes.Ranges[0].EndLine != 12 {
		t.Fatalf("ranges = %+v", payload.Changes.Ranges)
	}
	if len(payload.Changes.Symbols) != 1 || payload.Changes.Symbols[0].Name != "ValidateToken" {
		t.Fatalf("hunk symbols = %+v", payload.Changes.Symbols)
	}
}

func TestParseSemanticChangedRangesUsesIndexedCoordinatePlane(t *testing.T) {
	diff := "diff --git internal/auth/token.go internal/auth/token.go\n--- internal/auth/token.go\n+++ internal/auth/token.go\n@@ -10,2 +10,4 @@\n--- header-like deleted content\n+++ header-like added content\n@@ -20 +22 @@\n"
	tracked := parseSemanticChangedRanges(diff, false)
	if len(tracked) != 2 || tracked[0].StartLine != 10 || tracked[0].EndLine != 11 || tracked[1].File != "internal/auth/token.go" || tracked[1].StartLine != 20 {
		t.Fatalf("tracked ranges = %+v", tracked)
	}
	worktree := parseSemanticChangedRanges(diff, true)
	if len(worktree) != 2 || worktree[0].StartLine != 10 || worktree[0].EndLine != 13 || worktree[1].File != "internal/auth/token.go" || worktree[1].StartLine != 22 {
		t.Fatalf("worktree ranges = %+v", worktree)
	}
}

func TestSemanticChangesDefaultDoesNotAcquireIndexLock(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	lockDir := filepath.Join(brainDir, semanticLockDir)
	if err := os.Remove(filepath.Join(lockDir, semanticIndexLockName)); err != nil {
		t.Fatalf("remove index lock file: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(lockDir, semanticIndexLockName)); err != nil {
		t.Fatalf("replace index lock with symlink: %v", err)
	}
	changesCmd := &cobra.Command{Use: "changes"}
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("read-only changes should not inspect or acquire the index lock: %v", err)
	}
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true, persist: true}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("persisted changes should validate the index lock path, got: %v", err)
	}
}

func TestSemanticChangesCleanWorktreeSignalsNoChanges(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: ""}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(out.String(), `"clean": true`) {
		t.Fatalf("clean worktree JSON missing clean=true:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"files": []`) || !strings.Contains(out.String(), `"symbols": []`) {
		t.Fatalf("clean worktree JSON should have empty arrays not null:\n%s", out.String())
	}
	// Text mode should say so explicitly rather than render nothing.
	var textOut bytes.Buffer
	textCmd := &cobra.Command{Use: "changes"}
	textCmd.SetOut(&textOut)
	if err := runSemanticChanges(textCmd.Context(), textCmd, opts, semanticChangesOptions{limit: 10, json: false}); err != nil {
		t.Fatalf("changes text: %v", err)
	}
	if !strings.Contains(textOut.String(), "no changes since the indexed HEAD") {
		t.Fatalf("clean worktree text missing explicit signal:\n%s", textOut.String())
	}
}

func TestSemanticChangesIncludesRenamedOldPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "R100\tinternal/auth/token.go\tinternal/auth/token_new.go\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(out.String(), `"internal/auth/token.go"`) || !strings.Contains(out.String(), `"ValidateToken"`) {
		t.Fatalf("changes JSON missing renamed old-path symbol:\n%s", out.String())
	}
}

func TestSemanticChangesFiltersBrainignoredFiles(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "ignored"), 0o700); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored", "secret.go"), []byte("package ignored\n"), 0o600); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD"): {stdout: "M\tignored/tracked.go\nM\tinternal/auth/token.go\n"},
		fakeCommandKey("git", "status", "--porcelain"):                     {stdout: "?? ignored/\n?? .env\n"},
	}}
	files, err := changedSemanticFiles((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if len(files) != 1 || files[0] != "internal/auth/token.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSemanticChangesDecodesQuotedUntrackedPath(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "new file.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD"): {},
		fakeCommandKey("git", "status", "--porcelain"):                     {stdout: "?? \"new file.go\"\n"},
	}}
	files, err := changedSemanticFiles((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if len(files) != 1 || files[0] != "new file.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSemanticChangesExpandsUntrackedDirectory(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "newpkg"), 0o700); err != nil {
		t.Fatalf("mkdir newpkg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "newpkg", "foo.go"), []byte("package newpkg\n"), 0o600); err != nil {
		t.Fatalf("write new file: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), "internal/auth/token.go", "newpkg/foo.go", -1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? newpkg/\n"}
	files, err := changedSemanticFiles(cmd.Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if len(files) != 1 || files[0] != "newpkg/foo.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSemanticChangesIncludesUntrackedAndFailsOnDiffError(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: ""}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? internal/auth/token.go\n"}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(out.String(), `"ValidateToken"`) {
		t.Fatalf("changes did not include untracked symbol:\n%s", out.String())
	}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{err: errors.New("diff failed")}
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err == nil {
		t.Fatalf("changes succeeded after diff failure")
	}
}

func TestSemanticBoundaryCommandsListRoutesToolsAndWorkflows(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	for _, tc := range []struct {
		kind string
		want string
	}{
		{kind: "route", want: `"GET /tokens/{id}"`},
		{kind: "tool", want: `"brain refresh"`},
		{kind: "workflow", want: `"token validation"`},
	} {
		out, err := execute(t, cmd, "inspect", "boundaries", "--kind", tc.kind, "--json")
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%s output missing %s:\n%s", tc.kind, tc.want, out)
		}
	}
}

func TestSemanticBoundaryLimitFiltersRelationsAndHandlers(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshotWithExtraRoute())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	out, err := execute(t, cmd, "inspect", "boundaries", "--kind", "route", "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	if !strings.Contains(out, `"GET /tokens/{id}"`) {
		t.Fatalf("routes output missing first route:\n%s", out)
	}
	if strings.Contains(out, `"POST /sessions"`) || strings.Contains(out, `"CreateSession"`) {
		t.Fatalf("routes limit leaked omitted boundary relation:\n%s", out)
	}
}

func TestSemanticTestsSuggestsRelevantTests(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	out, err := execute(t, cmd, "inspect", "tests", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("tests: %v", err)
	}
	if !strings.Contains(out, `"TestValidateToken"`) || !strings.Contains(out, `"reason"`) {
		t.Fatalf("tests output missing relevant suggestion:\n%s", out)
	}
	if strings.Contains(out, `"record_type"`) {
		t.Fatalf("default tests JSON retained full provider records:\n%s", out)
	}
	detailed := &cobra.Command{Use: "tests"}
	var detailedOut bytes.Buffer
	detailed.SetOut(&detailedOut)
	if err := runSemanticTests(detailed.Context(), detailed, Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}, semanticTestsOptions{limit: 5, json: true, details: true}, "ValidateToken"); err != nil {
		t.Fatalf("detailed tests: %v", err)
	}
	if !strings.Contains(detailedOut.String(), `"record_type"`) {
		t.Fatalf("detailed tests JSON omitted provider record fields:\n%s", detailedOut.String())
	}
}

func TestSemanticContextSQLiteFiltersRelationsBeforeLimit(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithDelayedRelevantRelation())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	symbols, relations, _, err := semstore.ContextFacts(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo"), semstore.GraphStoreSource(runner.graphStoreFixtures[repoDir+"\x00full"], ""), "ValidateToken", 1, 0)
	if err != nil {
		t.Fatalf("context facts: %v", err)
	}
	if len(symbols) != 1 || len(relations) != 1 || relations[0].FromID != "caller" {
		t.Fatalf("context = symbols %+v relations %+v", symbols, relations)
	}
}

// semanticFixtureSnapshotWithCallerSymbol has the relation endpoint present as
// its own symbol record, so context can materialize it as a neighbor.
func semanticFixtureSnapshotWithCallerSymbol() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"target","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"caller","kind":"function","name":"HandleLogin","qualified_name":"api.HandleLogin","file_path":"internal/api/login.go","start_line":30,"end_line":50,"signature":"func HandleLogin() error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"caller","to_id":"target","type":"CALLS","confidence":1}
{"record_type":"summary"}
`
}

func TestSemanticContextMaterializesNeighbors(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithCallerSymbol())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	source := semstore.GraphStoreSource(runner.graphStoreFixtures[repoDir+"\x00full"], filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo"))
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	symbols, relations, neighbors, err := semstore.ContextFacts(brainDir, source, "ValidateToken", 1, 0)
	if err != nil {
		t.Fatalf("context facts: %v", err)
	}
	if len(symbols) != 1 || symbols[0].ID != "target" {
		t.Fatalf("symbols = %+v", symbols)
	}
	if len(relations) != 1 || relations[0].FromID != "caller" {
		t.Fatalf("relations = %+v", relations)
	}
	if len(neighbors) != 1 {
		t.Fatalf("neighbors = %+v, want the caller materialized", neighbors)
	}
	got := neighbors[0]
	if got.ID != "caller" || got.Name != "HandleLogin" || got.FilePath != "internal/api/login.go" || got.StartLine != 30 {
		t.Fatalf("neighbor = %+v, want the HandleLogin caller record", got)
	}
}

func TestSemanticContextContentSkipsSymlinkedRepoPath(t *testing.T) {
	repoDir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(repoDir, "link.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	content := semanticContextContent(repoDir, []semanticRecord{{FilePath: "link.go", StartLine: 1, EndLine: 1}})
	if len(content) != 0 {
		t.Fatalf("symlinked content was included: %+v", content)
	}
	if hash := semanticFileContentHash(repoDir, "link.go"); hash != "" {
		t.Fatalf("symlinked content was hashed: %s", hash)
	}
}

func TestSemanticContextIncludeContentRequiresFreshness(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "internal", "auth"), 0o700); err != nil {
		t.Fatalf("mkdir repo file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "internal", "auth", "token.go"), []byte("package auth\n"), 0o600); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n"}
	err := runSemanticContext(cmd.Context(), cmd, opts, semanticContextOptions{limit: 10, includeContent: true}, "ValidateToken")
	if err == nil || !strings.Contains(err.Error(), "requires fresh index data") {
		t.Fatalf("context err = %v", err)
	}
}

func TestSemanticContextSnippetReadsRequestedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.go")
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write large file: %v", err)
	}
	text, truncated, err := readSemanticContextSnippet(path, 4999, 5000)
	if err != nil {
		t.Fatalf("snippet: %v", err)
	}
	if truncated {
		t.Fatalf("snippet was truncated")
	}
	if text != "line 4999\nline 5000" {
		t.Fatalf("snippet = %q", text)
	}
}

func TestSemanticContextSnippetTruncatesLargeRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.go")
	var b strings.Builder
	for i := 1; i <= semanticContextMaxLines+20; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write large file: %v", err)
	}
	text, truncated, err := readSemanticContextSnippet(path, 1, semanticContextMaxLines+20)
	if err != nil {
		t.Fatalf("snippet: %v", err)
	}
	if !truncated {
		t.Fatalf("snippet was not truncated")
	}
	if strings.Contains(text, fmt.Sprintf("line %d", semanticContextMaxLines+1)) {
		t.Fatalf("snippet exceeded max lines")
	}
}

func TestSemanticQueryJSONIncludesFreshness(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n"}
	var out bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&out)
	if err := runSemanticQuery(queryCmd.Context(), queryCmd, opts, semanticQueryOptions{limit: 10, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out.String(), `"freshness"`) || !strings.Contains(out.String(), `"severity": "degraded"`) {
		t.Fatalf("query JSON missing dirty worktree freshness:\n%s", out.String())
	}
}

func TestSemanticResponsesIncludeTrust(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	d := runner.graphStoreFixtures[repoDir+"\x00full"]
	d.Header.Stats = json.RawMessage(`{"completeness_level":"degraded"}`)
	d.Header.LanguageTiers = map[string]string{"Go": "semantic"}
	runner.graphStoreFixtures[repoDir+"\x00full"] = d

	assertTrust := func(label, output string) {
		t.Helper()
		if !strings.Contains(output, `"completeness_level": "degraded"`) || !strings.Contains(output, `"trust": "partial"`) {
			t.Fatalf("%s JSON missing trust diagnostics:\n%s", label, output)
		}
		if !strings.Contains(output, `"language_tiers"`) || !strings.Contains(output, `"Go": "semantic"`) {
			t.Fatalf("%s JSON missing language_tiers:\n%s", label, output)
		}
	}

	var queryOut bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&queryOut)
	if err := runSemanticQuery(queryCmd.Context(), queryCmd, opts, semanticQueryOptions{limit: 10, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("query: %v", err)
	}
	assertTrust("query", queryOut.String())

	var contextOut bytes.Buffer
	contextCmd := &cobra.Command{Use: "context"}
	contextCmd.SetOut(&contextOut)
	if err := runSemanticContext(contextCmd.Context(), contextCmd, opts, semanticContextOptions{limit: 10, json: true}, "gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken"); err != nil {
		t.Fatalf("context: %v", err)
	}
	assertTrust("context", contextOut.String())

	var schemaOut bytes.Buffer
	schemaCmd := &cobra.Command{Use: "schema"}
	schemaCmd.SetOut(&schemaOut)
	if err := runSemanticGraphSchema(schemaCmd, opts, semanticGraphSchemaOptions{json: true}); err != nil {
		t.Fatalf("schema: %v", err)
	}
	assertTrust("schema", schemaOut.String())
}

func TestSemanticIndexLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("lock err = %v", err)
	}
}

func TestSemanticIndexIgnoresStaleLockFile(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	lockDir := filepath.Join(brainDir, semanticLockDir)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, semanticIndexLockName), []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index with stale lock file: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedLockDirectory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	external := t.TempDir()
	if err := os.MkdirAll(brainDir, 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(brainDir, semanticLockDir)); err != nil {
		t.Fatalf("symlink lock dir: %v", err)
	}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, semanticIndexLockName)); !os.IsNotExist(err) {
		t.Fatalf("external lock was created: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedBrainRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	if err := os.MkdirAll(filepath.Dir(brainDir), 0o700); err != nil {
		t.Fatalf("mkdir brain parent: %v", err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, brainDir); err != nil {
		t.Fatalf("symlink brain root: %v", err)
	}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, semstore.DirName)); !os.IsNotExist(err) {
		t.Fatalf("semantic directory was created through symlinked brain root: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedBrainParent(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	parent := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("mkdir brain parent: %v", err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(parent, "example")); err != nil {
		t.Fatalf("symlink brain parent: %v", err)
	}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, "repo")); !os.IsNotExist(err) {
		t.Fatalf("repo directory was created through symlinked brain parent: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedSnapshotDestination(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	snapshotsParent := filepath.Join(brainDir, semstore.DirName)
	if err := os.MkdirAll(snapshotsParent, 0o700); err != nil {
		t.Fatalf("mkdir semantic dir: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(snapshotsParent, semanticSnapshotsDir)); err != nil {
		t.Fatalf("symlink snapshots dir: %v", err)
	}

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index symlink err = %v", err)
	}
}

func TestBundleExportLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)

	err := runSemanticBundleExport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle export"}, Options{Env: env, Runner: runner, Now: time.Now}, filepath.Join(t.TempDir(), "brain.tar"), false)
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("export lock err = %v", err)
	}
}

func TestSemanticReadCommandsDoNotRequireExclusiveIndexLock(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)

	for _, tc := range []struct {
		name string
		run  func(*cobra.Command) error
	}{
		{
			name: "query",
			run: func(readCmd *cobra.Command) error {
				return runSemanticQuery(readCmd.Context(), readCmd, opts, semanticQueryOptions{limit: 10, json: true}, "ValidateToken")
			},
		},
		{
			name: "context",
			run: func(readCmd *cobra.Command) error {
				return runSemanticContext(readCmd.Context(), readCmd, opts, semanticContextOptions{limit: 10, json: true}, "ValidateToken")
			},
		},
		{
			name: "impact",
			run: func(readCmd *cobra.Command) error {
				return runSemanticImpact(readCmd.Context(), readCmd, opts, semanticImpactOptions{limit: 10, depth: 1, json: true}, "ValidateToken")
			},
		},
		{
			name: "routes",
			run: func(readCmd *cobra.Command) error {
				return runSemanticBoundary(readCmd.Context(), readCmd, opts, semanticBoundaryOptions{limit: 10, json: true}, semanticBoundarySpec{
					SymbolKinds:   []string{"route", "http_route"},
					RelationTypes: []string{"HANDLES_ROUTE"},
				})
			},
		},
		{
			name: "tests",
			run: func(readCmd *cobra.Command) error {
				return runSemanticTests(readCmd.Context(), readCmd, opts, semanticTestsOptions{limit: 10, json: true}, "ValidateToken")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			readCmd := &cobra.Command{Use: tc.name}
			readCmd.SetOut(&out)
			if err := tc.run(readCmd); err != nil {
				t.Fatalf("%s with index lock: %v", tc.name, err)
			}
		})
	}
}

func TestBundleImportLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)
	archive := filepath.Join(t.TempDir(), "semantic.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"generated_at":"2026-05-31T00:00:00Z","provider":"entire-graph","schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})

	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("import lock err = %v", err)
	}
}

func TestBundleRejectsRemotePaths(t *testing.T) {
	for _, path := range []string{"https://example.com/brain.tar", "s3://bucket/brain.tar", "git+ssh://host/repo"} {
		if err := rejectRemoteBundlePath(path); err == nil {
			t.Fatalf("expected remote path rejection for %s", path)
		}
	}
}

func TestBundleExportCreatesPrivateArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix group/other permission bits for this assertion")
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("bundle mode = %o, want no group/other permissions", info.Mode().Perm())
	}
	entries := readTestBundleEntries(t, output)
	if semanticTestContainsEntrySuffix(entries, semstore.SQLiteName) {
		t.Fatalf("bundle copied Graph sqlite store: %+v", entries)
	}
	if semanticTestContainsEntrySuffix(entries, semanticMetricsName) {
		t.Fatalf("bundle copied retired semantic metrics: %+v", entries)
	}
}

func TestSemanticOnlyBrainReadmeDescribesSemanticIndex(t *testing.T) {
	brainDir := t.TempDir()
	source := &semanticSourceManifest{
		GeneratedAt:   time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		Commit:        "aaa111",
		Provider:      "entire-graph",
		SchemaVersion: "1.0",
		SnapshotPath:  "semantic/snapshots/aaa111/snapshot.ndjson",
		Symbols:       1,
	}
	if err := writeBrainSemanticSource(brainDir, "gh/example/repo", source); err != nil {
		t.Fatalf("write semantic source: %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(brainDir, exportReadmeFileName))
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if !strings.Contains(string(readme), "Code Index (Graph-owned)") || strings.Contains(string(readme), "Session Export") {
		t.Fatalf("semantic README not rendered correctly:\n%s", readme)
	}
	if strings.Contains(string(readme), "- Snapshot:") {
		t.Fatalf("README still advertises a snapshot nothing writes:\n%s", readme)
	}
}

func TestBundleExportTightensExistingArchivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix group/other permission bits for this assertion")
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.WriteFile(output, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old bundle: %v", err)
	}
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("bundle mode = %o, want no group/other permissions", info.Mode().Perm())
	}
}

func TestBundleExportExcludesLocalAuditLog(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	if err := appendSemanticAudit(brainDir, "bundle_export", "/local/path/brain.tar", "abc"); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	for _, entry := range readTestBundleEntries(t, output) {
		if entry == "semantic/audit.jsonl" {
			t.Fatalf("bundle included local audit log")
		}
	}
}

func TestBundleExportRejectsSymlinkedAuditLog(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	auditPath := filepath.Join(brainDir, semstore.DirName, semanticAuditLogName)
	if err := os.Remove(auditPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove audit: %v", err)
	}
	target := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(target, []byte("external\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(auditPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, auditPath); err != nil {
		t.Fatalf("symlink audit: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, filepath.Join(t.TempDir(), "brain.tar"), false)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsOutputInsideBrainDir(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", semstore.DirName, semanticBundleDir, "export.tar")
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false)
	if err == nil || !strings.Contains(err.Error(), "outside the active brain directory") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsSymlinkAncestorIntoBrainDir(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	link := filepath.Join(t.TempDir(), "link-to-brain")
	if err := os.Symlink(brainDir, link); err != nil {
		t.Fatalf("symlink to brain: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, filepath.Join(link, "new", "bundle.tar"), false)
	if err == nil || !strings.Contains(err.Error(), "outside the active brain directory") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsSymlinkOutputIntoBrainDir(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	target := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", semstore.DirName, semanticSnapshotsDir, "self.tar")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	output := filepath.Join(t.TempDir(), "link.tar")
	if err := os.Symlink(target, output); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false)
	if err == nil || !strings.Contains(err.Error(), "outside the active brain directory") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsHardLinkedOutputToBrainFile(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")

	snapshotPath := filepath.Join(brainDir, bmanifest.FileName)
	before, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.Link(snapshotPath, output); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	err = runSemanticBundleExport(cmd.Context(), cmd, opts, output, false)
	if err == nil || !strings.Contains(err.Error(), "hard link") && !strings.Contains(err.Error(), "hardlink") {
		t.Fatalf("export err = %v", err)
	}
	after, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot after export: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("snapshot was modified through hard-linked output")
	}
}

func TestBundleImportRejectsRepoKeyMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/other/repo","sources":{"semantic":{"snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil || !strings.Contains(err.Error(), "does not match current repo") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsLegacySnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "blank-lines.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-graph","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": strings.Replace(semanticFixtureSnapshot("1.0"), "\n{\"record_type\":\"relation\"", "\n\n{\"record_type\":\"relation\"", 1),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if !errors.Is(err, errLegacyBrainLayout) {
		t.Fatalf("expected old-layout error: %v", err)
	}
}

func TestBundleImportRejectsAuditLogEntry(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "audit.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/audit.jsonl":                      `{"path":"/local/path"}` + "\n",
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsSymlinkedAuditLog(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "brain.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	auditPath := filepath.Join(brainDir, semstore.DirName, semanticAuditLogName)
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		t.Fatalf("mkdir audit dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(target, []byte("external\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(auditPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, auditPath); err != nil {
		t.Fatalf("symlink audit: %v", err)
	}
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("import err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, bmanifest.FileName)); !os.IsNotExist(err) {
		t.Fatalf("manifest was written despite audit preflight failure: %v", err)
	}
}

func TestBundleExportUsesSanitizedManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.RepoRoot = "/private/local/repo"
	manifest.Sessions = []exportSession{{SessionID: "session", TranscriptPath: "sessions/main/session.jsonl"}}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("export: %v", err)
	}
	manifestData := readTestBundleFile(t, output, bmanifest.FileName)
	if strings.Contains(string(manifestData), "repo_root") || strings.Contains(string(manifestData), "sessions/main/session.jsonl") {
		t.Fatalf("bundle manifest was not sanitized:\n%s", manifestData)
	}
	if strings.Contains(string(manifestData), `"semantic"`) {
		t.Fatalf("bundle retained retired semantic source:\n%s", manifestData)
	}
}

func TestBundleExportRejectsSymlinkOutput(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.tar")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.Symlink(target, output); err != nil {
		t.Fatalf("symlink output: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("export err = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "keep" {
		t.Fatalf("symlink target was modified: %q", data)
	}
}

func TestBundleImportRejectsMissingRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "missing-repo-key.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil || !strings.Contains(err.Error(), "missing repo_key") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsOperationalSemanticState(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "lock.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/locks/index.lock":                 "locked",
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil || !strings.Contains(err.Error(), "outside allowed paths") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsChecksumMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad-checksum.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, strings.Repeat("0", sha256.Size*2), false)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("import err = %v", err)
	}
}

// TestBundleImportRefusesNewerSchema pins that the bundle's own manifest
// schema_version -- a different field from sources.semantic.schema_version,
// which semstore.ValidateSchema already enforces -- is checked before any
// other validation in validateImportedBundle. The fixture manifest omits
// repo_key, which would otherwise fail first; if the version check ever moved
// behind that check, this test would start seeing "missing repo_key" instead.
func TestBundleImportRefusesNewerSchema(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "newer-schema.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName: `{"schema_version":99}`,
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil {
		t.Fatal("import err = nil, want rejection of a newer bundle schema")
	}
	msg := err.Error()
	for _, want := range []string{"99", strconv.Itoa(bmanifest.SchemaVersion), "upgrade entire-brain"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("import err = %q, want it to contain %q", msg, want)
		}
	}
}

func TestBundleImportAcceptsOlderSchema(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "current-schema.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName: fmt.Sprintf(`{"schema_version":%d,"repo_key":"gh/example/repo","sources":{}}`, bmanifest.SchemaVersion),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false); err != nil {
		t.Fatalf("import: %v", err)
	}
}

// TestBundleImportNoticesDiscardedCodeIndex pins that a bundle carrying a
// Graph-owned source tells the operator its code index was not imported.
// Pre-Graph sources are refused before any layers are restored.
func TestBundleImportNoticesDiscardedCodeIndex(t *testing.T) {
	const notice = "code index was not imported"

	t.Run("with semantic payload", func(t *testing.T) {
		repoDir := t.TempDir()
		env := semanticTestEnv(t, repoDir)
		runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
		archive := filepath.Join(t.TempDir(), "legacy-semantic.tar")
		writeTestBundle(t, archive, map[string]string{
			bmanifest.FileName: `{"schema_version":4,"repo_key":"gh/example/repo","sources":{"semantic":{"store_owner":"graph","provider":"entire-graph"}}}`,
		})
		cmd := &cobra.Command{Use: "bundle import"}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := runSemanticBundleImport(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false); err != nil {
			t.Fatalf("import: %v", err)
		}
		if !strings.Contains(out.String(), notice) {
			t.Fatalf("import output = %q, want it to contain %q", out.String(), notice)
		}
	})

	t.Run("without semantic payload", func(t *testing.T) {
		repoDir := t.TempDir()
		env := semanticTestEnv(t, repoDir)
		runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
		archive := filepath.Join(t.TempDir(), "no-semantic.tar")
		writeTestBundle(t, archive, map[string]string{
			bmanifest.FileName: `{"schema_version":3,"repo_key":"gh/example/repo","sources":{}}`,
		})
		cmd := &cobra.Command{Use: "bundle import"}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := runSemanticBundleImport(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false); err != nil {
			t.Fatalf("import: %v", err)
		}
		if strings.Contains(out.String(), notice) {
			t.Fatalf("import output = %q, want no discarded-code-index notice without a semantic payload", out.String())
		}
	})
}

func TestBundleImportChecksumCoversTrailingBytes(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "trailing.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	originalChecksum := bundleSHA256(t, archive)
	f, err := os.OpenFile(archive, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open bundle append: %v", err)
	}
	if _, err := f.WriteString("trailing garbage"); err != nil {
		_ = f.Close()
		t.Fatalf("append trailing: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close bundle: %v", err)
	}
	err = runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, originalChecksum, false)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsUnreferencedSnapshots(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "extra-snapshot.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                         `{"schema_version":4,"repo_key":"gh/example/repo","sources":{}}`,
		"semantic/snapshots/extra/snapshot.ndjson": `{"schema_version":"1.0","repo_key":"gh/other/repo"}` + "\n",
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if !errors.Is(err, errLegacyBrainLayout) {
		t.Fatalf("expected old-layout error: %v", err)
	}
	extraPath := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo", "semantic", "snapshots", "extra", "snapshot.ndjson")
	if _, err := os.Stat(extraPath); !os.IsNotExist(err) {
		t.Fatalf("unreferenced snapshot was persisted: %v", err)
	}
}

func TestBundleImportRejectsTotalSizeLimit(t *testing.T) {
	previous := semanticBundleMaxTotal
	semanticBundleMaxTotal = 10
	defer func() { semanticBundleMaxTotal = previous }()

	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "large.tar")
	writeTestBundle(t, archive, map[string]string{
		bmanifest.FileName:                          `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive), false)
	if err == nil || !strings.Contains(err.Error(), "total size") {
		t.Fatalf("import err = %v", err)
	}
}

func semanticTestEnv(t *testing.T, repoDir string) EntireEnv {
	t.Helper()
	return EntireEnv{
		RepoRoot:        repoDir,
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
}

func writeSemanticTestLock(t *testing.T, brainDir string) {
	t.Helper()
	unlock, err := acquireSemanticIndexLock(brainDir)
	if err != nil {
		t.Fatalf("acquire semantic test lock: %v", err)
	}
	t.Cleanup(unlock)
	// Every caller either asserts fast lock-contention failure or never
	// contends, so the lowered timeout is always safe and never forgotten.
	shortSemanticLockTimeout(t)
}

// shortSemanticLockTimeout makes lock contention fail in milliseconds instead
// of the production window. The retry loop still runs: the override stays a
// multiple of FileLockRetryInterval. Not safe in parallel tests.
func shortSemanticLockTimeout(t *testing.T) {
	t.Helper()
	prev := semanticIndexLockTimeout
	semanticIndexLockTimeout = 5 * brainstore.FileLockRetryInterval
	t.Cleanup(func() { semanticIndexLockTimeout = prev })
}

func semanticTestSQLCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open(sqlitedriver.Name, path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func mustSemanticSource(t *testing.T, env EntireEnv) *semanticSourceManifest {
	t.Helper()
	manifest, err := bmanifest.Load(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return manifest.Sources.Semantic
}

func semanticTestContainsEntrySuffix(entries []string, suffix string) bool {
	for _, entry := range entries {
		if strings.HasSuffix(entry, suffix) {
			return true
		}
	}
	return false
}

func semanticFixtureRunner(repoDir, snapshot string) *fakeCommandRunner {
	brainignorePath := filepath.Join(repoDir, ".brainignore")
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		// The fixture stands in for a real working tree, so it has to answer
		// every question one would. Leaving this out made the fixture claim a
		// branch while denying it was a repository at all.
		fakeCommandKey("git", "rev-parse", "--is-inside-work-tree"):                                                                                            {stdout: "true\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):                                                                                                   {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                                                                                                             {stdout: "aaa111\n"},
		fakeCommandKey("git", "rev-parse", "HEAD^{tree}"):                                                                                                      {stdout: "tree111\n"},
		fakeCommandKey("git", "branch", "--show-current"):                                                                                                      {stdout: "feature\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"):                                                                {stdout: "origin/main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                                                                                                         {stdout: ""},
		fakeCommandKey("git", "diff", "--unified=0", "--no-ext-diff", "--no-color", "--no-prefix", "HEAD"):                                                     {stdout: ""},
		fakeCommandKey("entire", "graph", "doctor", "--json"):                                                                                                  {stdout: `{"no_egress":true}`},
		fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"):                                                 {stdout: snapshot},
		fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--ignore-file", brainignorePath):               {stdout: snapshot},
		fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--ignore-file", brainignorePath, "--worktree"): {stdout: snapshot},
	}}
}

func fakeRunnerCalled(runner *fakeCommandRunner, name string, args ...string) bool {
	for _, call := range runner.calls {
		if call.name != name || len(call.args) != len(args) {
			continue
		}
		match := true
		for i := range args {
			if call.args[i] != args[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func semanticArgsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func semanticFixtureSnapshot(schema string) string {
	return `{"schema_version":"` + schema + `","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"caller","to_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","type":"CALLS","confidence":1}
{"record_type":"summary","warnings":[],"partial_failures":[]}
`
}

func semanticChangesRangeFixtureSnapshot() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.PrepareToken","kind":"function","name":"PrepareToken","qualified_name":"auth.PrepareToken","file_path":"internal/auth/token.go","start_line":1,"end_line":5,"signature":"func PrepareToken()","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"summary"}
`
}

func semanticDataFlowFixtureSnapshot() string {
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["ndjson"],"relation_set":["DEFINES","DATA_FLOWS"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:ts:flow.ts:function:run","kind":"function","name":"run","qualified_name":"flow.run","file_path":"flow.ts","start_line":1,"end_line":8,"signature":"function run(input: Input): string","language":"TypeScript","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:ts:flow.ts:function:normalize","kind":"function","name":"normalize","qualified_name":"flow.normalize","file_path":"flow.ts","start_line":10,"end_line":12,"signature":"function normalize(value: string): string","language":"TypeScript","stable_id_version":"1"}
{"record_type":"relation","from_id":"gh/example/repo:ts:flow.ts:function:run","to_id":"gh/example/repo:ts:flow.ts:function:normalize","type":"DATA_FLOWS","confidence":0.7,"reason":"caller parameter destructured alias forwarded into callee argument","relation_scope":"file","resolution":"exact","target_kind":"symbol","evidence":[{"kind":"destructured_alias_forward_flow","file_path":"flow.ts","start_line":1,"end_line":8,"detail":"input -> value -> normalize()"}],"warning_codes":[]}
{"record_type":"summary"}
`
}

func semanticResolvedImportFixtureSnapshot() string {
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["ndjson"],"relation_set":["IMPORTS"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"gh/example/repo:file:apps/web/src/app.ts","path":"apps/web/src/app.ts","blob":"app","language":"TypeScript","bytes":96}
{"record_type":"file","id":"gh/example/repo:file:packages/utils/src/index.ts","path":"packages/utils/src/index.ts","blob":"utils","language":"TypeScript","bytes":48}
{"record_type":"relation","from_id":"gh/example/repo:file:apps/web/src/app.ts","to_id":"gh/example/repo:file:packages/utils/src/index.ts","type":"IMPORTS","confidence":0.91,"reason":"JS/TS workspace package export resolved through nested package.json","relation_scope":"module","resolution":"import_resolved","target_kind":"file","evidence":[{"kind":"package_workspace_exports_import","file_path":"apps/web/src/app.ts","start_line":1,"end_line":1,"detail":"@acme/utils"}],"warning_codes":[]}
{"record_type":"summary"}
`
}

func semanticFixtureSnapshotWithQualifiedCallerSymbol() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/caller.go:function:auth.CallValidateToken","kind":"function","name":"CallValidateToken","qualified_name":"auth.CallValidateToken","file_path":"internal/auth/caller.go","start_line":30,"end_line":40,"signature":"func CallValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/caller.go:function:auth.CallValidateToken","to_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","type":"CALLS","confidence":1}
{"record_type":"summary"}
`
}

func semanticBoundaryFixtureSnapshot() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go","routes","tools","workflows"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/http/routes.go:route:GET /tokens/{id}","kind":"route","name":"GET /tokens/{id}","qualified_name":"GET /tokens/{id}","file_path":"internal/http/routes.go","start_line":5,"end_line":5,"signature":"GET /tokens/{id}","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/cli/root.go:cli_command:brain refresh","kind":"cli_command","name":"brain refresh","qualified_name":"brain refresh","file_path":"internal/cli/root.go","start_line":50,"end_line":60,"signature":"entire brain refresh","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:yaml:.github/workflows/test.yml:workflow:token validation","kind":"workflow","name":"token validation","qualified_name":"token validation","file_path":".github/workflows/test.yml","start_line":1,"end_line":20,"signature":"mise run check","language":"YAML","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token_test.go:test:auth.TestValidateToken","kind":"test","name":"TestValidateToken","qualified_name":"auth.TestValidateToken","file_path":"internal/auth/token_test.go","start_line":8,"end_line":18,"signature":"func TestValidateToken(t *testing.T)","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"gh/example/repo:go:internal/http/routes.go:route:GET /tokens/{id}","type":"HANDLES_ROUTE","confidence":1}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"gh/example/repo:go:internal/cli/root.go:cli_command:brain refresh","type":"HANDLES_TOOL","confidence":0.8}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"gh/example/repo:yaml:.github/workflows/test.yml:workflow:token validation","type":"HANDLES_WORKFLOW","confidence":0.7}
{"record_type":"summary"}
`
}

func semanticBoundaryFixtureSnapshotWithExtraRoute() string {
	// The extra route/relation records must land before the terminating summary
	// record, the same as a real provider stream: the summary is always last.
	base := semanticBoundaryFixtureSnapshot()
	before, after, found := strings.Cut(base, `{"record_type":"summary"}`)
	if !found {
		panic("semanticBoundaryFixtureSnapshot no longer ends with a summary record")
	}
	return before + `{"record_type":"symbol","id":"gh/example/repo:go:internal/http/routes.go:route:POST /sessions","kind":"route","name":"POST /sessions","qualified_name":"POST /sessions","file_path":"internal/http/routes.go","start_line":6,"end_line":6,"signature":"POST /sessions","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/http/session.go:function:http.CreateSession","kind":"function","name":"CreateSession","qualified_name":"http.CreateSession","file_path":"internal/http/session.go","start_line":12,"end_line":24,"signature":"func CreateSession()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/http/session.go:function:http.CreateSession","to_id":"gh/example/repo:go:internal/http/routes.go:route:POST /sessions","type":"HANDLES_ROUTE","confidence":1}
{"record_type":"summary"}` + after
}

func semanticFixtureSnapshotWithDelayedRelevantRelation() string {
	var b strings.Builder
	b.WriteString(`{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}` + "\n")
	b.WriteString(`{"record_type":"symbol","id":"target","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}` + "\n")
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, `{"record_type":"relation","from_id":"unrelated-%d","to_id":"other-%d","type":"CALLS","confidence":1}`+"\n", i, i)
	}
	b.WriteString(`{"record_type":"relation","from_id":"caller","to_id":"target","type":"CALLS","confidence":1}` + "\n")
	b.WriteString(`{"record_type":"summary"}` + "\n")
	return b.String()
}

func semanticFixtureSnapshotWithIgnoredSecret() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"public","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"secret","kind":"const","name":"SECRET_TOKEN","qualified_name":"config.SECRET_TOKEN","file_path":"secret/config.go","start_line":1,"end_line":1,"signature":"const SECRET_TOKEN","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"public","to_id":"secret","type":"ACCESSES","confidence":1}
{"record_type":"summary"}
`
}

func semanticFixtureSnapshotWithIgnoredWarnings() string {
	// The trailing summary record (not the lean header) is authoritative for
	// warnings/partial_failures: mergeSemanticSummary overrides the header with
	// it, so that is where a real provider reports them.
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"public","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"summary","warnings":[{"code":"kept","severity":"warning","path":"internal/auth/token.go","detail":"visible"},{"code":"ignored","severity":"warning","path":"secret/config.go","detail":"SECRET_TOKEN in secret/config.go"}],"partial_failures":[{"code":"ignored_failure","severity":"error","path":"secret/config.go","detail":"parse failed for SECRET_TOKEN"}]}
`
}

func semanticFixtureSnapshotWithGitHubWorkflow() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["yaml"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"workflow","kind":"workflow","name":"ci","qualified_name":"ci","file_path":".github/workflows/ci.yml","start_line":1,"end_line":20,"signature":"ci","language":"YAML","stable_id_version":"1"}
{"record_type":"summary"}
`
}

func semanticFixtureSnapshotWithIgnoredRelationID() string {
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"public","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"public","to_id":"gh/example/repo:go:secret/config.go:function:Secret","type":"CALLS","confidence":1}
{"record_type":"summary"}
`
}

func writeTestBundle(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := []byte(entries[name])
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
}

func readTestBundleEntries(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var entries []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		entries = append(entries, header.Name)
	}
	return entries
}

func readTestBundleFile(t *testing.T, path, name string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		if header.Name != name {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatalf("read bundle file: %v", err)
		}
		return buf.Bytes()
	}
	t.Fatalf("bundle file %s not found", name)
	return nil
}

func bundleSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestSemanticWarningUnmarshalProviderKeys(t *testing.T) {
	// The semantic provider emits file_path / effect_on_semantic_completeness;
	// entire-brain's canonical keys are path / effect. Both must round-trip.
	cases := []struct {
		name       string
		input      string
		wantPath   string
		wantEffect string
	}{
		{
			name:       "provider keys",
			input:      `{"code":"E_PARSE_ERROR","severity":"warning","file_path":"supabase/migrations/x.sql","effect_on_semantic_completeness":"file parsed with syntax errors","detail":"tree-sitter syntax error nodes present"}`,
			wantPath:   "supabase/migrations/x.sql",
			wantEffect: "file parsed with syntax errors",
		},
		{
			name:       "canonical keys",
			input:      `{"code":"default_branch_unknown","severity":"warning","path":"a/b.go","effect":"freshness","detail":"d"}`,
			wantPath:   "a/b.go",
			wantEffect: "freshness",
		},
		{
			name:       "canonical wins over provider",
			input:      `{"code":"c","path":"canonical","file_path":"provider","effect":"eff","effect_on_semantic_completeness":"provider-eff"}`,
			wantPath:   "canonical",
			wantEffect: "eff",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w semanticWarning
			if err := json.Unmarshal([]byte(tc.input), &w); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if w.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", w.Path, tc.wantPath)
			}
			if w.Effect != tc.wantEffect {
				t.Errorf("effect = %q, want %q", w.Effect, tc.wantEffect)
			}
		})
	}
}

func TestSemanticWarningProviderKeysRoundTripThroughHeader(t *testing.T) {
	// filterSemanticSnapshot parses the provider header then re-marshals it as the
	// stored snapshot header. The failing file path must survive that round-trip.
	header := `{"schema_version":"1.0","provider":"entire-graph","repo_key":"k","commit":"c","tree":"t","languages":["SQL"],"capabilities":[],"warnings":[],"partial_failures":[{"code":"E_PARSE_ERROR","severity":"warning","file_path":"db/x.sql","effect_on_semantic_completeness":"incomplete","detail":"tree-sitter syntax error nodes present"}]}`
	var h semanticHeader
	if err := json.Unmarshal([]byte(header), &h); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if len(h.PartialFailures) != 1 {
		t.Fatalf("partial failures = %d, want 1", len(h.PartialFailures))
	}
	if got := h.PartialFailures[0].Path; got != "db/x.sql" {
		t.Fatalf("partial failure path = %q, want db/x.sql", got)
	}
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	var h2 semanticHeader
	if err := json.Unmarshal(out, &h2); err != nil {
		t.Fatalf("re-unmarshal header: %v", err)
	}
	if got := h2.PartialFailures[0].Path; got != "db/x.sql" {
		t.Fatalf("round-tripped partial failure path = %q, want db/x.sql", got)
	}
}

func TestTrustForCompleteness(t *testing.T) {
	cases := map[string]string{"ok": "trusted", "degraded": "partial", "unsafe": "low", "": "unknown", "weird": "unknown"}
	for level, want := range cases {
		if got := semstore.TrustForCompleteness(level); got != want {
			t.Errorf("semstore.TrustForCompleteness(%q) = %q, want %q", level, got, want)
		}
	}
}

func TestCompletenessLevelFromStats(t *testing.T) {
	if got := semstore.CompletenessLevelFromStats([]byte(`{"completeness_level":"degraded","symbols":5}`)); got != "degraded" {
		t.Errorf("got %q, want degraded", got)
	}
	if got := semstore.CompletenessLevelFromStats(nil); got != "" {
		t.Errorf("nil stats: got %q, want empty", got)
	}
	if got := semstore.CompletenessLevelFromStats([]byte(`not json`)); got != "" {
		t.Errorf("bad json: got %q, want empty", got)
	}
}

// TestUnverifiedErrorDoesNotRepeatTheHostsOwnRemedy is the identity_remedy
// test's twin on the semantic side. Both call sites must reach the same
// answer for the same stderr; asserting that on one of them proves half.
func TestUnverifiedErrorDoesNotRepeatTheHostsOwnRemedy(t *testing.T) {
	t.Parallel()
	bare := `unknown command "graph" for "entire"`

	got := semanticProviderUnverifiedError("entire", []semanticWarning{{Code: "E_X", Detail: bare}}).Error()
	if !strings.Contains(got, providerdetail.InstallGraphCommand) {
		t.Fatalf("a bare missing-provider failure must still name the install command:\n%s", got)
	}

	// The single-line case the summary KEEPS: this is the one that can tell
	// the shared decision from an unconditional append.
	oneLine := `unknown command "graph"; run ` + "`" + providerdetail.InstallGraphCommand + "`"
	kept := semanticProviderUnverifiedError("entire", []semanticWarning{{Code: "E_X", Detail: oneLine}}).Error()
	if n := strings.Count(kept, providerdetail.InstallGraphCommand); n != 1 {
		t.Fatalf("the message already names the remedy; it appears %d times:\n%s", n, kept)
	}

	multi := bare + "\n\nDid you mean to run `" + providerdetail.InstallGraphCommand + "`?"
	summarised := semanticProviderUnverifiedError("entire", []semanticWarning{{Code: "E_X", Detail: multi}}).Error()
	if n := strings.Count(summarised, providerdetail.InstallGraphCommand); n != 1 {
		t.Fatalf("the summary dropped the host's copy, so exactly one of ours is owed; got %d:\n%s", n, summarised)
	}
}
