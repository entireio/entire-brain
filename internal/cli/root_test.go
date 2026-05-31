package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/config"
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
	configDir := t.TempDir()
	dataDir := t.TempDir()
	stateDir := t.TempDir()
	cacheDir := t.TempDir()
	if err := config.Save(configDir, config.Config{Greeting: "hello test"}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			CLIVersion:      "cli-test",
			RepoRoot:        "/tmp/repo",
			PluginConfigDir: configDir,
			PluginDataDir:   dataDir,
			PluginStateDir:  stateDir,
			PluginCacheDir:  cacheDir,
		},
	})

	out, err := execute(t, cmd)
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	for _, want := range []string{
		"entire-brain",
		"version: test-version",
		"entire cli: cli-test",
		"repo root: /tmp/repo",
		"plugin config: " + configDir,
		"plugin data: " + dataDir,
		"plugin state: " + stateDir,
		"plugin cache: " + cacheDir,
		"greeting: hello test",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("root output missing %q:\n%s", want, out)
		}
	}
}

func TestRootStatusWorksWithoutEntireEnvironment(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv(xdgConfigHome, filepath.Join(xdg, "config"))
	t.Setenv(xdgDataHome, filepath.Join(xdg, "data"))
	t.Setenv(xdgStateHome, filepath.Join(xdg, "state"))
	t.Setenv(xdgCacheHome, filepath.Join(xdg, "cache"))

	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd)
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	for _, want := range []string{
		"plugin config: " + filepath.Join(xdg, "config", "entire"),
		"plugin data: " + filepath.Join(xdg, "data", "entire"),
		"plugin state: " + filepath.Join(xdg, "state", "entire"),
		"plugin cache: " + filepath.Join(xdg, "cache", "entire"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("root output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorUsesXDGFallbacks(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv(xdgConfigHome, filepath.Join(xdg, "config"))
	t.Setenv(xdgDataHome, filepath.Join(xdg, "data"))
	t.Setenv(xdgStateHome, filepath.Join(xdg, "state"))
	t.Setenv(xdgCacheHome, filepath.Join(xdg, "cache"))

	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, want := range []string{
		"ENTIRE_PLUGIN_DATA_DIR=<unset>",
		"plugin config dir: writable (" + filepath.Join(xdg, "config", "entire") + ")",
		"plugin data dir: writable (" + filepath.Join(xdg, "data", "entire") + ")",
		"plugin state dir: writable (" + filepath.Join(xdg, "state", "entire") + ")",
		"plugin cache dir: writable (" + filepath.Join(xdg, "cache", "entire") + ")",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorCreatesWritablePluginDataDir(t *testing.T) {
	dirs := t.TempDir()
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: dirs + "/config",
			PluginDataDir:   dirs + "/data",
			PluginStateDir:  dirs + "/state",
			PluginCacheDir:  dirs + "/cache",
		},
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
	configDir := t.TempDir()
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env:     EntireEnv{PluginConfigDir: configDir},
	})

	out, err := execute(t, cmd, "config", "init")
	if err != nil {
		t.Fatalf("config init: %v", err)
	}
	if !strings.Contains(out, "brain.json") {
		t.Fatalf("config init output missing path:\n%s", out)
	}

	cmd = NewRootCommand(Options{
		Version: "test-version",
		Env:     EntireEnv{PluginConfigDir: configDir},
	})
	out, err = execute(t, cmd, "config", "show")
	if err != nil {
		t.Fatalf("config show: %v", err)
	}
	if !strings.Contains(out, `"greeting": "Hello from Entire Brain"`) {
		t.Fatalf("config show output missing default greeting:\n%s", out)
	}
}
