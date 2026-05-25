package cli

import "testing"

func TestEnvFromOS(t *testing.T) {
	t.Setenv(envCLIVersion, "cli-test")
	t.Setenv(envRepoRoot, "/tmp/repo")
	t.Setenv(envPluginDataDir, "/tmp/data")

	env := EnvFromOS()
	if env.CLIVersion != "cli-test" {
		t.Fatalf("CLIVersion = %q", env.CLIVersion)
	}
	if env.RepoRoot != "/tmp/repo" {
		t.Fatalf("RepoRoot = %q", env.RepoRoot)
	}
	if env.PluginDataDir != "/tmp/data" {
		t.Fatalf("PluginDataDir = %q", env.PluginDataDir)
	}
}
