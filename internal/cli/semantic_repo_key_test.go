package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// semanticProviderKeyFixtureRunner mirrors semanticFixtureRunner but lets a test
// pick the `origin` remote AND the repo_key the provider stamps into the
// snapshot header. Every pre-existing semantic fixture uses a github.com origin,
// which is the one remote shape where Brain's storage key and entire-graph's
// provider key happen to coincide — so the whole suite was blind to the seam.
func semanticProviderKeyFixtureRunner(repoDir, remote, providerRepoKey string) *fakeCommandRunner {
	brainignorePath := filepath.Join(repoDir, ".brainignore")
	snapshot := semanticFixtureSnapshotForRepoKey(providerRepoKey)
	originResponse := fakeCommandResponse{stdout: remote + "\n"}
	if remote == "" {
		originResponse = fakeCommandResponse{err: errors.New("fatal: No such remote 'origin'")}
	}
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                                                                                                  {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):                                                                                                   originResponse,
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

func semanticFixtureSnapshotForRepoKey(repoKey string) string {
	return strings.ReplaceAll(semanticFixtureSnapshot("1.1"), "gh/example/repo", repoKey)
}

// TestSemanticIndexAcceptsProviderRepoKeyForEveryRemoteShape pins the seam
// contract: entire-graph derives repo_key from the repository alone
// (github remote -> gh/<owner>/<name>, otherwise local/<basename>), while Brain
// derives a storage key that is config- and path-dependent. Brain must accept
// the provider's spelling for EVERY remote shape, not just the github one where
// the two rules coincide.
func TestSemanticIndexAcceptsProviderRepoKeyForEveryRemoteShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		remote string
	}{
		{name: "github", remote: "git@github.com:example/repo.git"},
		{name: "no-remote", remote: ""},
		{name: "gitlab", remote: "https://gitlab.com/acme/widget.git"},
		{name: "bitbucket", remote: "git@bitbucket.org:acme/widget.git"},
		{name: "self-hosted", remote: "https://git.corp.internal/acme/widget.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			providerKey := providerSemanticRepoKey(repoDir, tc.remote)
			runner := semanticProviderKeyFixtureRunner(repoDir, tc.remote, providerKey)
			cmd := &cobra.Command{Use: "index"}
			err := runSemanticIndex(cmd.Context(), cmd, Options{
				Version: "test",
				Env:     env,
				Runner:  runner,
				Now:     time.Now,
			}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
			if err != nil {
				t.Fatalf("index with remote %q rejected the provider repo_key %q: %v", tc.remote, providerKey, err)
			}
		})
	}
}

// TestProviderSemanticRepoKeyMatchesGraphContract is the golden-vector table for
// the documented provider rule. entire-graph's own contract test asserts the
// same vectors, so a change on either side breaks a test on both.
func TestProviderSemanticRepoKeyMatchesGraphContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ remote, want string }{
		{"git@github.com:example/repo.git", "gh/example/repo"},
		{"https://github.com/example/repo.git", "gh/example/repo"},
		{"https://github.com/example/repo", "gh/example/repo"},
		{"ssh://git@github.com/example/repo.git", "gh/example/repo"},
		{"http://github.com/example/repo.git", "gh/example/repo"},
		{"https://github.com/example/nested/repo.git", "local/widgets"},
		{"https://gitlab.com/acme/widget.git", "local/widgets"},
		{"git@bitbucket.org:acme/widget.git", "local/widgets"},
		{"https://git.corp.internal/acme/widget.git", "local/widgets"},
		{"", "local/widgets"},
	} {
		got := providerSemanticRepoKey(filepath.Join("/tmp", "widgets"), tc.remote)
		if got != tc.want {
			t.Fatalf("providerSemanticRepoKey(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

// TestSemanticRepoKeyMismatchErrorIsActionable pins the operator-facing message.
// A skewed installed entire-graph can still produce a key Brain cannot derive;
// when that happens the failure must name both keys, say where each came from,
// and state the remedy — never a bare "does not match".
func TestSemanticRepoKeyMismatchErrorIsActionable(t *testing.T) {
	t.Parallel()
	identity := semanticRepoIdentity{
		StorageKey:  "gl/acme/widget",
		ProviderKey: "local/widget",
		RepoDir:     "/repos/widget",
		GraphBinary: "entire",
	}
	err := validateLiveSemanticHeader(
		semanticHeader{RepoKey: "totally/other", Commit: "aaa111", Tree: "tree111"},
		identity, "aaa111", "tree111", false,
	)
	if err == nil {
		t.Fatal("expected a repo_key mismatch error")
	}
	msg := err.Error()
	for _, want := range []string{
		"totally/other",
		"local/widget",
		"gl/acme/widget",
		"entire graph version",
		"--graph-binary",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("mismatch error is not actionable: missing %q in %q", want, msg)
		}
	}
}

// TestSemanticIndexPrefersProviderReportedRepoKey pins the handshake: a
// provider that reports repo_key from `graph doctor --json` is authoritative,
// because it is the process that stamps the header. A provider that reports
// nothing (an older build) leaves Brain on the contract rule.
func TestSemanticIndexPrefersProviderReportedRepoKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		doctorJSON     string
		headerRepoKey  string
		wantIndexError bool
	}{
		{
			name:          "reported key wins over the contract rule",
			doctorJSON:    `{"no_egress":true,"repo_key":"provider/custom/key"}`,
			headerRepoKey: "provider/custom/key",
		},
		{
			name:          "older provider reports nothing and the contract rule applies",
			doctorJSON:    `{"no_egress":true}`,
			headerRepoKey: "",
		},
		{
			name:           "a reported key that the header contradicts is still rejected",
			doctorJSON:     `{"no_egress":true,"repo_key":"provider/custom/key"}`,
			headerRepoKey:  "somewhere/else",
			wantIndexError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			headerKey := tc.headerRepoKey
			if headerKey == "" {
				headerKey = providerSemanticRepoKey(repoDir, "")
			}
			runner := semanticProviderKeyFixtureRunner(repoDir, "", headerKey)
			runner.responses[fakeCommandKey("entire", "graph", "doctor", "--json")] = fakeCommandResponse{stdout: tc.doctorJSON}
			cmd := &cobra.Command{Use: "index"}
			err := runSemanticIndex(cmd.Context(), cmd, Options{
				Version: "test", Env: env, Runner: runner, Now: time.Now,
			}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
			if tc.wantIndexError {
				if err == nil {
					t.Fatal("expected a repo_key mismatch, got success")
				}
				return
			}
			if err != nil {
				t.Fatalf("index: %v", err)
			}
		})
	}
}
