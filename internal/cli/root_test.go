package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestFormatJSONRendersStructuredErrors(t *testing.T) {
	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd, "search", "alpha", "--format", "json", "--limit", "0")
	if err == nil {
		t.Fatalf("search succeeded unexpectedly:\n%s", out)
	}
	var envelope commandJSONError
	if decodeErr := json.Unmarshal([]byte(out), &envelope); decodeErr != nil {
		t.Fatalf("--format json error output was not JSON: %v\n%s", decodeErr, out)
	}
	if envelope.Code != "command_failed" || envelope.Message == "" {
		t.Fatalf("unexpected JSON error envelope: %+v", envelope)
	}

	cmd = NewRootCommand(Options{Version: "test-version"})
	out, err = execute(t, cmd, "search", "alpha", "--format", "cli", "--json", "--limit", "0")
	if err == nil {
		t.Fatalf("search succeeded unexpectedly:\n%s", out)
	}
	if json.Valid([]byte(out)) {
		t.Fatalf("--format cli should force plain error output even with --json:\n%s", out)
	}
}

func TestRootWithoutCommandShowsHelp(t *testing.T) {
	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd)
	if err != nil {
		t.Fatalf("execute root: %v", err)
	}
	for _, want := range []string{
		"local, inspectable repository brain",
		"Usage:",
		// Commands are organized into use-case groups instead of one flat list.
		"Create the brain:",
		"Explore the brain:",
		"Maintain & share:",
		"brief",
		"status",
		"refresh",
		"inspect",
		"mcp",
		"version",
		"workspace",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("root output missing %q:\n%s", want, out)
		}
	}
	// Absent from top-level help: hidden plugin/config commands and the dropped
	// completion generator; removed redundant aliases; and the build stages that
	// now live under `refresh` (sessions, index, seed).
	for _, absent := range []string{
		"  doctor ", "  config ", "  completion ",
		"  context ", "  impact ", "  changes ",
		"  tests ", "  routes ", "  tools ", "  workflows ",
		"  index ", "  seed ", "  sessions ", "  history-index ",
	} {
		if strings.Contains(out, absent) {
			t.Fatalf("root output should not list %q:\n%s", absent, out)
		}
	}
}

func TestRedundantAliasesAreRemoved(t *testing.T) {
	// The duplicate commands are unregistered (not just hidden): invoking them
	// now fails. Canonical paths are the retrieval verbs (query/search/vsearch/
	// get/multi-get) and `inspect <sub>` for symbol-graph navigation.
	for _, alias := range []string{"context", "impact", "changes", "tests", "routes", "tools", "workflows"} {
		cmd := NewRootCommand(Options{Version: "test-version"})
		if _, err := execute(t, cmd, alias); err == nil {
			t.Fatalf("alias %q should be removed (expected unknown-command error)", alias)
		}
	}
}

func TestRefreshExposesBuildStageSubcommands(t *testing.T) {
	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd, "refresh", "--help")
	if err != nil {
		t.Fatalf("refresh --help: %v\n%s", err, out)
	}
	for _, want := range []string{"sessions", "index", "seed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("refresh --help missing stage %q:\n%s", want, out)
		}
	}
	// The renamed stage is `sessions`, not `history-index`.
	if strings.Contains(out, "history-index") {
		t.Fatalf("refresh --help should not mention history-index:\n%s", out)
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
		"plugin config dir: warn (creatable_unproven: " + filepath.Join(xdg, "config", "entire"),
		"plugin data dir: warn (creatable_unproven: " + filepath.Join(xdg, "data", "entire", "plugins", "data", pluginDataName),
		"plugin state dir: warn (creatable_unproven: " + filepath.Join(xdg, "state", "entire"),
		"plugin cache dir: warn (creatable_unproven: " + filepath.Join(xdg, "cache", "entire"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
	for _, path := range []string{filepath.Join(xdg, "config", "entire"), filepath.Join(xdg, "data", "entire"), filepath.Join(xdg, "state", "entire"), filepath.Join(xdg, "cache", "entire")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("doctor must not create %s: %v", path, err)
		}
	}
}

func TestDoctorDoesNotCreateOrProbePluginDataDir(t *testing.T) {
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
	if !strings.Contains(out, "plugin data dir: warn (creatable_unproven:") {
		t.Fatalf("doctor output missing read-only directory status:\n%s", out)
	}
	for _, suffix := range []string{"config", "data", "state", "cache"} {
		if _, err := os.Lstat(filepath.Join(dirs, suffix)); !os.IsNotExist(err) {
			t.Fatalf("doctor must not create or probe %s: %v", suffix, err)
		}
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
