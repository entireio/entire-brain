package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// semanticMultiRemoteFixtureRunner is semanticFixtureRunner with the fixture's
// supported remote names emitted in deterministic Git-config order.
// providerRemoteURLs hoists origin, exactly as entire-graph's gitutil.RemoteURLs
// does.
func semanticMultiRemoteFixtureRunner(repoDir, snapshot string, remotes map[string]string) *fakeCommandRunner {
	runner := semanticFixtureRunner(repoDir, snapshot)
	if origin, ok := remotes["origin"]; ok {
		runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: origin + "\n"}
	} else {
		runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{
			stderr: "error: No such remote 'origin'\n",
			err:    errors.New("exit status 2"),
		}
	}
	var b strings.Builder
	for _, name := range []string{"origin", "upstream", "fork", "github"} {
		if url, ok := remotes[name]; ok {
			b.WriteString("remote." + name + ".url " + url + "\n")
		}
	}
	runner.responses[fakeCommandKey("git", "config", "--get-regexp", `^remote\..*\.url$`)] = fakeCommandResponse{stdout: b.String()}
	return runner
}

// TestSemanticIndexAcceptsGitHubRemoteNotNamedOrigin is the reason this mirror
// reads EVERY remote rather than just `origin`.
//
// entire-graph derives its key from gitutil.RemoteURLs — all configured remote
// URLs, origin hoisted to the front — and takes the FIRST github.com match. So a
// repository whose GitHub remote is named `upstream` (the standard fork layout,
// and the layout of any repo that keeps a mirror as origin) gets
// gh/<owner>/<name> from the provider.
//
// A mirror that reads only `origin` computes local/<basename> for that repo,
// matches neither the provider's key nor the brain's storage key, and refuses a
// perfectly valid snapshot — reintroducing the very bug the mirror exists to fix,
// just for a narrower set of repositories.
func TestSemanticIndexAcceptsGitHubRemoteNotNamedOrigin(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	// origin is a non-GitHub mirror; the GitHub remote is `upstream`.
	remotes := map[string]string{
		"origin":   "https://gitlab.com/acme/widget.git",
		"upstream": "git@github.com:acme/widget.git",
	}
	// entire-graph therefore stamps the GitHub key, not local/<base>.
	snapshot := `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/acme/widget","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/acme/widget:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"summary","warnings":[],"partial_failures":[]}
`
	runner := semanticMultiRemoteFixtureRunner(repoDir, snapshot, remotes)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}

	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index a repo whose GitHub remote is named upstream: %v", err)
	}
	// Brain's storage key still comes from origin; the artifact is normalized to it.
	if got := persistedSnapshotRepoKey(t, env, "gl/acme/widget"); got != "gl/acme/widget" {
		t.Fatalf("persisted snapshot repo_key = %q, want gl/acme/widget", got)
	}
}

// TestExpectedProviderRepoKeyReadsEveryRemote is the unit-level statement of the
// same rule, including the ordering guarantee: origin is consulted first, so a
// GitHub origin still wins over a GitHub upstream.
func TestExpectedProviderRepoKeyReadsEveryRemote(t *testing.T) {
	repoDir := t.TempDir()
	for name, tc := range map[string]struct {
		remotes map[string]string
		want    string
	}{
		"github upstream, non-github origin": {
			remotes: map[string]string{"origin": "https://gitlab.com/acme/widget.git", "upstream": "git@github.com:acme/widget.git"},
			want:    "gh/acme/widget",
		},
		"github upstream, no origin": {
			remotes: map[string]string{"upstream": "https://github.com/acme/widget.git"},
			want:    "gh/acme/widget",
		},
		"origin wins over upstream": {
			remotes: map[string]string{"origin": "git@github.com:acme/origin-repo.git", "upstream": "git@github.com:acme/upstream-repo.git"},
			want:    "gh/acme/origin-repo",
		},
		"no github remote at all": {
			remotes: map[string]string{"origin": "https://gitlab.com/acme/widget.git"},
			want:    "local/" + filepath.Base(repoDir),
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := semanticMultiRemoteFixtureRunner(repoDir, "", tc.remotes)
			if got := expectedProviderRepoKey(context.Background(), runner, repoDir); got != tc.want {
				t.Fatalf("expectedProviderRepoKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDoctorReportedRepoKeyIsAuthoritative: when the installed provider reports
// its own repo_key from `graph doctor --json`, that answer beats the brain's
// reimplementation of the provider rule. The provider is the process that will
// stamp the header, so it cannot be wrong about its own key — whereas the mirror
// is a copy that can drift the moment entire-graph's rule changes.
//
// Here the provider uses a rule the brain has never heard of. The mirror would
// compute local/<base> and reject the snapshot; the handshake accepts it.
func TestDoctorReportedRepoKeyIsAuthoritative(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	const futureKey = "sr/acme/widget" // a forge slug this brain's mirror does not know
	snapshot := `{"schema_version":"1.0","provider":"entire-graph","provider_version":"9.9.9","repo_key":"` + futureKey + `","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"` + futureKey + `:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"summary","warnings":[],"partial_failures":[]}
`
	runner := semanticMultiRemoteFixtureRunner(repoDir, snapshot, map[string]string{"origin": "https://sr.ht/~acme/widget"})
	runner.responses[fakeCommandKey("entire", "graph", "doctor", "--json")] = fakeCommandResponse{
		stdout: `{"no_egress":true,"repo_key":"` + futureKey + `"}`,
	}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}

	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("provider that reports its own repo_key was rejected: %v", err)
	}
}

// TestDoctorReportedRepoKeyStillRejectsAForeignSnapshot: the handshake widens
// what is accepted, it does not disable the guard. When the doctor names this
// repository's key, a snapshot carrying a DIFFERENT key is still refused — and
// the refusal names the doctor's answer, since that is now the authority.
func TestDoctorReportedRepoKeyStillRejectsAForeignSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticMultiRemoteFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"), map[string]string{}) // header says gh/example/repo
	runner.responses[fakeCommandKey("entire", "graph", "doctor", "--json")] = fakeCommandResponse{
		stdout: `{"no_egress":true,"repo_key":"local/somewhere-else"}`,
	}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}

	err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil {
		t.Fatal("a snapshot whose repo_key contradicts the provider's own doctor answer was accepted")
	}
	for _, want := range []string{"gh/example/repo", "local/somewhere-else", "--graph-binary"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("mismatch error is not actionable: %q missing from %q", want, err.Error())
		}
	}
}

// TestLiveHeaderRepoKeyCheckCannotFail documents why validateLiveSemanticHeader
// no longer compares repo_key.
//
// scanSemanticStream rewrites header.RepoKey to the brain's storage key before
// the header is ever handed on, so comparing it against that same storage key
// downstream compared a value with itself. A check that cannot fail is worse
// than no check: it reads as protection and provides none. The real assertions
// are validateSemanticProviderRepoKey (the provider's untrusted spelling) and
// readSemanticSnapshotSummary / validateImportedBundle (untrusted on-disk keys).
func TestLiveHeaderRepoKeyCheckCannotFail(t *testing.T) {
	const storageKey = "gl/acme/widget"
	var out strings.Builder
	res, err := scanSemanticStream(
		strings.NewReader(providerLocalKeyFixtureSnapshot("/tmp/widget")),
		&out,
		semanticStreamScanConfig{repoKey: storageKey, repoDir: "/tmp/widget"},
	)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.header.RepoKey != storageKey {
		t.Fatalf("header repo_key = %q; scanSemanticStream must normalize it to the storage key %q", res.header.RepoKey, storageKey)
	}
	if res.providerRepoKey != "local/widget" {
		t.Fatalf("provider repo_key = %q, want the provider's original spelling preserved for the real check", res.providerRepoKey)
	}
}
