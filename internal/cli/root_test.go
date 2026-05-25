package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/entireio/entire-plugin-template/internal/config"
	"github.com/spf13/cobra"
)

func execute(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()

	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

func TestRootStatusShowsEntireEnvironment(t *testing.T) {
	dataDir := t.TempDir()
	if err := config.Save(dataDir, config.Config{Greeting: "hello test"}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			CLIVersion:    "cli-test",
			RepoRoot:      "/tmp/repo",
			PluginDataDir: dataDir,
		},
	})

	out, err := execute(t, cmd)
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	for _, want := range []string{
		"version: test-version",
		"entire cli: cli-test",
		"repo root: /tmp/repo",
		"plugin data: " + dataDir,
		"greeting: hello test",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("root output missing %q:\n%s", want, out)
		}
	}
}

func TestRootStatusWorksWithoutEntireEnvironment(t *testing.T) {
	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd)
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	if !strings.Contains(out, "plugin data: <unset>") {
		t.Fatalf("root output missing unset plugin data:\n%s", out)
	}
}

func TestDoctorRequiresPluginDataDir(t *testing.T) {
	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd, "doctor")
	if err == nil {
		t.Fatal("doctor returned nil error without plugin data dir")
	}
	if !strings.Contains(out, "ENTIRE_PLUGIN_DATA_DIR=<unset>") {
		t.Fatalf("doctor output missing environment dump:\n%s", out)
	}
}

func TestDoctorCreatesWritablePluginDataDir(t *testing.T) {
	dataDir := t.TempDir() + "/plugin-data"
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env:     EntireEnv{PluginDataDir: dataDir},
	})

	out, err := execute(t, cmd, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "plugin data dir: writable") {
		t.Fatalf("doctor output missing writable status:\n%s", out)
	}
}

func TestConfigInitAndShow(t *testing.T) {
	dataDir := t.TempDir()
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env:     EntireEnv{PluginDataDir: dataDir},
	})

	out, err := execute(t, cmd, "config", "init")
	if err != nil {
		t.Fatalf("config init: %v", err)
	}
	if !strings.Contains(out, "config.json") {
		t.Fatalf("config init output missing path:\n%s", out)
	}

	cmd = NewRootCommand(Options{
		Version: "test-version",
		Env:     EntireEnv{PluginDataDir: dataDir},
	})
	out, err = execute(t, cmd, "config", "show")
	if err != nil {
		t.Fatalf("config show: %v", err)
	}
	if !strings.Contains(out, `"greeting": "Hello from an Entire plugin"`) {
		t.Fatalf("config show output missing default greeting:\n%s", out)
	}
}
