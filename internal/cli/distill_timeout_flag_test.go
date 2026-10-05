package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestDistillCommandTimeoutValidation(t *testing.T) {
	for _, value := range []string{"0", "-1s", "1ms", ""} {
		t.Run(value, func(t *testing.T) {
			repoDir := t.TempDir()
			runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
				fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
				fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
			}}
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			opts := Options{
				Version: "test", Runner: runner, Now: func() time.Time { return now },
				Env: EntireEnv{
					RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(),
					PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir(),
				},
			}
			storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
			if err != nil {
				t.Fatal(err)
			}
			writeDistillFixtureAt(t, storage.BrainDir, now)
			var output bytes.Buffer
			cmd := NewRootCommand(opts)
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			args := []string{"distill", "--agent", "command", "--agent-command", "true", "--dry-run", "--json"}
			if value != "" {
				args = append(args, "--timeout", value)
			}
			cmd.SetArgs(args)
			err = cmd.Execute()
			if value == "0" || value == "-1s" {
				if err == nil || !strings.Contains(err.Error(), "--timeout must be greater than 0") {
					t.Fatalf("timeout %q: expected validation error, got %v\n%s", value, err, output.String())
				}
			} else if err != nil {
				t.Fatalf("timeout %q: %v\n%s", value, err, output.String())
			}
		})
	}
}
