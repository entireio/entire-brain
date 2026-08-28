package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// providerLocalKeyFixtureSnapshot mirrors what entire-graph emits for a
// repository whose remotes do not match a github.com pattern: "local/<base>",
// with no disambiguating hash.
func providerLocalKeyFixtureSnapshot(repoDir string) string {
	key := "local/" + filepath.Base(filepath.Clean(repoDir))
	return `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + key + `","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"` + key + `:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"caller","to_id":"` + key + `:go:internal/auth/token.go:function:auth.ValidateToken","type":"CALLS","confidence":1}
`
}

// semanticRepoKeyFixtureRunner is semanticFixtureRunner with the repository's
// origin remote replaced (or removed, when originURL is empty), so the brain
// derives a storage key that the provider cannot reproduce.
func semanticRepoKeyFixtureRunner(repoDir, originURL, snapshot string) *fakeCommandRunner {
	runner := semanticFixtureRunner(repoDir, snapshot)
	if originURL == "" {
		runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{
			stderr: "error: No such remote 'origin'\n",
			err:    errors.New("exit status 2"),
		}
	} else {
		runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: originURL + "\n"}
	}
	// entire-graph lists every remote URL (origin first) when deriving its key.
	runner.responses[fakeCommandKey("git", "config", "--get-regexp", `^remote\..*\.url$`)] = fakeCommandResponse{
		stdout: func() string {
			if originURL == "" {
				return ""
			}
			return "remote.origin.url " + originURL + "\n"
		}(),
	}
	return runner
}

func semanticStorageKeyForTest(t *testing.T, runner CommandRunner, env EntireEnv, repoDir string) string {
	t.Helper()
	key, err := repoStorageKey(context.Background(), runner, env.PluginConfigDir, repoDir)
	if err != nil {
		t.Fatalf("repo storage key: %v", err)
	}
	return key
}

func persistedSnapshotRepoKey(t *testing.T, env EntireEnv, repoKey string) string {
	t.Helper()
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, filepath.FromSlash(repoKey))
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	var header semanticHeader
	if err := json.Unmarshal([]byte(line), &header); err != nil {
		t.Fatalf("parse persisted header: %v", err)
	}
	return header.RepoKey
}

// TestSemanticIndexAcceptsProviderKeyForRepoWithoutRemote is the reported
// first-run bug: `entire-brain refresh index` in a git repository with no
// remote failed with
//
//	semantic snapshot repo_key "local/repo" does not match current repo "local/repo-<hash>"
//
// because the brain compared the provider's provenance label against its own
// storage key, which carries a path hash the provider never emits.
func TestSemanticIndexAcceptsProviderKeyForRepoWithoutRemote(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticRepoKeyFixtureRunner(repoDir, "", providerLocalKeyFixtureSnapshot(repoDir))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}

	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index a repository with no remote: %v", err)
	}

	storageKey := semanticStorageKeyForTest(t, runner, env, repoDir)
	if !strings.HasPrefix(storageKey, "local/") {
		t.Fatalf("storage key = %q, want a local/ key", storageKey)
	}
	// The brain's own artifacts must carry the brain's canonical key, so every
	// later read (snapshot summary, bundle manifest) compares like with like.
	if got := persistedSnapshotRepoKey(t, env, storageKey); got != storageKey {
		t.Fatalf("persisted snapshot repo_key = %q, want normalized storage key %q", got, storageKey)
	}
}

// TestSemanticIndexAcceptsProviderKeyForNonGitHubRemote covers the wider blast
// radius of the same defect: entire-graph only recognizes github.com, so ANY
// other forge (gitlab, bitbucket, self-hosted) also produced "local/<base>"
// while the brain derived "<slug>/<owner>/<name>".
func TestSemanticIndexAcceptsProviderKeyForNonGitHubRemote(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticRepoKeyFixtureRunner(repoDir, "https://gitlab.com/acme/widget.git", providerLocalKeyFixtureSnapshot(repoDir))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}

	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index a repository with a non-GitHub remote: %v", err)
	}
	if got := persistedSnapshotRepoKey(t, env, "gl/acme/widget"); got != "gl/acme/widget" {
		t.Fatalf("persisted snapshot repo_key = %q, want gl/acme/widget", got)
	}
}

// TestSemanticIndexRejectsForeignProviderRepoKey keeps the guard: a snapshot
// that belongs to a DIFFERENT repository is still refused, and the refusal is
// actionable — it names both keys and a remedy.
func TestSemanticIndexRejectsForeignProviderRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticRepoKeyFixtureRunner(repoDir, "", semanticFixtureSnapshot("1.0")) // header says gh/example/repo
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}

	err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil {
		t.Fatal("expected a foreign provider repo_key to be rejected")
	}
	storageKey := semanticStorageKeyForTest(t, runner, env, repoDir)
	message := err.Error()
	for _, want := range []string{
		"gh/example/repo",                 // the key the provider reported
		storageKey,                        // the key this brain expects
		"local/" + filepath.Base(repoDir), // the key the provider should have reported
		"--graph-binary",                  // remedy
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("mismatch error is not actionable: %q missing from %q", want, message)
		}
	}
}

func TestProviderRepoKeyMirrorsEntireGraphRule(t *testing.T) {
	for name, tc := range map[string]struct {
		remotes []string
		repoDir string
		want    string
	}{
		"no remote":            {remotes: nil, repoDir: "/src/My Repo", want: "local/My Repo"},
		"ssh github":           {remotes: []string{"git@github.com:entireio/cli.git"}, repoDir: "/src/cli", want: "gh/entireio/cli"},
		"https github":         {remotes: []string{"https://github.com/Entireio/CLI"}, repoDir: "/src/cli", want: "gh/Entireio/CLI"},
		"http github":          {remotes: []string{"http://github.com/o/r.git"}, repoDir: "/src/r", want: "gh/o/r"},
		"ssh url github":       {remotes: []string{"ssh://git@github.com/o/r"}, repoDir: "/src/r", want: "gh/o/r"},
		"trailing slash":       {remotes: []string{"https://github.com/o/r/"}, repoDir: "/src/r", want: "gh/o/r"},
		"nested path rejected": {remotes: []string{"https://github.com/o/r/extra"}, repoDir: "/src/r", want: "local/r"},
		"gitlab falls back":    {remotes: []string{"https://gitlab.com/acme/widget.git"}, repoDir: "/src/widget", want: "local/widget"},
		"second remote wins":   {remotes: []string{"https://gitlab.com/a/b", "git@github.com:o/r.git"}, repoDir: "/src/b", want: "gh/o/r"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := providerRepoKeyFromRemotes(tc.remotes, tc.repoDir); got != tc.want {
				t.Fatalf("providerRepoKeyFromRemotes = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProviderRepoKeyMatchesInstalledEntireGraph is the differential test that
// keeps the mirrored rule honest against the real provider. It is skipped when
// no `entire` binary is installed.
func TestProviderRepoKeyMatchesInstalledEntireGraph(t *testing.T) {
	bin, err := exec.LookPath("entire")
	if err != nil {
		t.Skip("entire binary not installed")
	}
	repoDir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	for _, origin := range []string{"", "https://gitlab.com/acme/widget.git", "git@github.com:entireio/cli.git"} {
		remotes := []string(nil)
		if origin == "" {
			_ = exec.Command("git", "-C", repoDir, "remote", "remove", "origin").Run()
		} else {
			_ = exec.Command("git", "-C", repoDir, "remote", "remove", "origin").Run()
			run("remote", "add", "origin", origin)
			remotes = []string{origin}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		cmd := exec.CommandContext(ctx, bin, "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")
		cmd.Dir = repoDir
		out, err := cmd.Output()
		cancel()
		if err != nil {
			t.Skipf("entire graph snapshot unavailable: %v", err)
		}
		line, _, _ := strings.Cut(string(out), "\n")
		var header semanticHeader
		if err := json.Unmarshal([]byte(line), &header); err != nil {
			t.Fatalf("parse provider header: %v", err)
		}
		if got := providerRepoKeyFromRemotes(remotes, repoDir); got != header.RepoKey {
			t.Fatalf("origin %q: mirrored rule = %q, installed provider = %q", origin, got, header.RepoKey)
		}
	}
}
