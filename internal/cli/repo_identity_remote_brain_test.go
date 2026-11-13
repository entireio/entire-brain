package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/repoidentity"
	"github.com/spf13/cobra"
)

func TestRepoIdentityReportsARemoteKeyedBrain(t *testing.T) {
	env := repoIdentityTestEnv(t)
	repoDir := t.TempDir()
	physical, err := canonicalLocalRepoRoot(repoDir)
	if err != nil {
		t.Fatalf("canonical root: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: physical + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "https://github.com/example/repo.git\n"},
	}}
	set, err := resolveRepoStorageSet(context.Background(), runner, env, physical)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if set.Local {
		t.Fatalf("fixture should be remote-keyed, got local key %q", set.Canonical.Key)
	}
	if err := os.MkdirAll(set.Canonical.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(set.Canonical.HeadPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(set.Canonical.HeadPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	opts := Options{Version: "test", Env: env, Runner: runner}
	if err := repoidentity.Run(context.Background(), cmd, repoIdentityConfig(opts), repoidentity.RunOptions{}, physical); err != nil {
		t.Fatalf("repo-identity: %v", err)
	}
	t.Logf("key=%s\n%s", set.Canonical.Key, out.String())
	if strings.Contains(out.String(), "no brain yet") {
		t.Fatalf("repo-identity denies the remote-keyed brain that exists:\n%s", out.String())
	}
}
