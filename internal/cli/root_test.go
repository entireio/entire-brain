package cli

import (
	"bytes"
	"encoding/json"
	"os"
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
	// A plugin directory that has not been created yet is the normal state of
	// a fresh install, not a finding: what doctor owes the reader here is the
	// PATH it resolved from the XDG fallback, and the promise that it did not
	// create it. See doctor_honest_reporting_test.go for the state mapping.
	for _, want := range []string{
		"ENTIRE_PLUGIN_DATA_DIR=<unset>",
		"plugin config dir: ok (" + filepath.Join(xdg, "config", "entire"),
		"plugin data dir: ok (" + filepath.Join(xdg, "data", "entire", "plugins", "data", pluginDataName),
		"plugin state dir: ok (" + filepath.Join(xdg, "state", "entire"),
		"plugin cache dir: ok (" + filepath.Join(xdg, "cache", "entire"),
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
	if !strings.Contains(out, "plugin data dir: ok ("+dirs+"/data: not created yet") {
		t.Fatalf("doctor output missing read-only directory status:\n%s", out)
	}
	for _, suffix := range []string{"config", "data", "state", "cache"} {
		if _, err := os.Lstat(filepath.Join(dirs, suffix)); !os.IsNotExist(err) {
			t.Fatalf("doctor must not create or probe %s: %v", suffix, err)
		}
	}
}

// configShowTestEnv pins the process-level switches `config show` reports, so
// the assertions below do not depend on the developer's or CI runner's shell.
func configShowTestEnv(t *testing.T) EntireEnv {
	t.Helper()
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_BRAIN_GRAPH_BINARY", "")
	root := t.TempDir()
	return EntireEnv{
		PluginConfigDir: filepath.Join(root, "config"),
		PluginDataDir:   filepath.Join(root, "data"),
		PluginStateDir:  filepath.Join(root, "state"),
		PluginCacheDir:  filepath.Join(root, "cache"),
		RepoRoot:        filepath.Join(root, "repo"),
	}
}

func TestConfigInitAndShow(t *testing.T) {
	env := configShowTestEnv(t)
	cmd := NewRootCommand(Options{Version: "test-version", Env: env})

	out, err := execute(t, cmd, "config", "init")
	if err != nil {
		t.Fatalf("config init: %v", err)
	}
	if !strings.Contains(out, "brain.json") {
		t.Fatalf("config init output missing path:\n%s", out)
	}

	cmd = NewRootCommand(Options{Version: "test-version", Env: env})
	out, err = execute(t, cmd, "config", "show")
	if err != nil {
		t.Fatalf("config show: %v", err)
	}

	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("config show did not emit JSON: %v\n%s", err, out)
	}
	// The scaffold this replaced: `config show` reported the hello-world
	// greeting as if it were the plugin's configuration, and `scripts/install.sh`
	// writes that file as a documented install step.
	if strings.Contains(out, "greeting") || strings.Contains(out, "Hello from Entire Brain") {
		t.Fatalf("config show still reports the scaffold greeting:\n%s", out)
	}
	wantFile := filepath.Join(env.PluginConfigDir, "brain.json")
	if report["config_file"] != wantFile {
		t.Fatalf("config_file = %v, want %q", report["config_file"], wantFile)
	}
	if report["config_file_exists"] != true {
		t.Fatalf("config_file_exists = %v after config init, want true", report["config_file_exists"])
	}
	dirs, ok := report["directories"].(map[string]any)
	if !ok {
		t.Fatalf("config show did not report the plugin directories:\n%s", out)
	}
	for _, dir := range []struct {
		key  string
		want string
	}{
		{"config", env.PluginConfigDir},
		{"data", env.PluginDataDir},
		{"state", env.PluginStateDir},
		{"cache", env.PluginCacheDir},
	} {
		if dirs[dir.key] != dir.want {
			t.Fatalf("directories.%s = %v, want %q", dir.key, dirs[dir.key], dir.want)
		}
	}
	if want := filepath.Join(env.PluginDataDir, repoStoreDirName); report["brain_store"] != want {
		t.Fatalf("brain_store = %v, want %q", report["brain_store"], want)
	}
	if report["repo_root"] != env.RepoRoot {
		t.Fatalf("repo_root = %v, want %q", report["repo_root"], env.RepoRoot)
	}
	slugs, ok := report["domain_slugs"].(map[string]any)
	if !ok || len(slugs) != 0 {
		t.Fatalf("domain_slugs = %v, want an empty object on a fresh config", report["domain_slugs"])
	}
	if report["no_egress"] != false {
		t.Fatalf("no_egress = %v, want false", report["no_egress"])
	}
	if report["mcp_graph_binary"] != "entire" {
		t.Fatalf("mcp_graph_binary = %v, want the default %q", report["mcp_graph_binary"], "entire")
	}
}

// Every field `config show` prints must track the thing it names, or the report
// is decoration. These are the three that change under a reader.
func TestConfigShowTracksLiveConfiguration(t *testing.T) {
	env := configShowTestEnv(t)
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "1")
	t.Setenv("ENTIRE_BRAIN_GRAPH_BINARY", "/opt/entire/bin/entire")

	// No `config init`: the file is genuinely absent, and the report must say so
	// rather than presenting defaults as if they were written.
	cmd := NewRootCommand(Options{Version: "test-version", Env: env})
	out, err := execute(t, cmd, "config", "show")
	if err != nil {
		t.Fatalf("config show: %v\n%s", err, out)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("config show did not emit JSON: %v\n%s", err, out)
	}
	if report["config_file_exists"] != false {
		t.Fatalf("config_file_exists = %v with no config written, want false", report["config_file_exists"])
	}
	if report["no_egress"] != true {
		t.Fatalf("no_egress = %v under ENTIRE_BRAIN_LOCAL_ONLY=1, want true", report["no_egress"])
	}
	if report["mcp_graph_binary"] != "/opt/entire/bin/entire" {
		t.Fatalf("mcp_graph_binary = %v, want the ENTIRE_BRAIN_GRAPH_BINARY override", report["mcp_graph_binary"])
	}

	// A persisted host slug is the one real setting brain.json carries, and it
	// is what a reader debugging a repo key needs to see.
	if _, err := config.Update(env.PluginConfigDir, func(cfg *config.Config) error {
		cfg.DomainSlugs = map[string]string{"git.example.invalid": "ab"}
		return nil
	}); err != nil {
		t.Fatalf("persist domain slug: %v", err)
	}
	cmd = NewRootCommand(Options{Version: "test-version", Env: env})
	out, err = execute(t, cmd, "config", "show")
	if err != nil {
		t.Fatalf("config show: %v\n%s", err, out)
	}
	report = nil
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("config show did not emit JSON: %v\n%s", err, out)
	}
	if report["config_file_exists"] != true {
		t.Fatalf("config_file_exists = %v after a write, want true", report["config_file_exists"])
	}
	slugs, ok := report["domain_slugs"].(map[string]any)
	if !ok || slugs["git.example.invalid"] != "ab" {
		t.Fatalf("domain_slugs = %v, want the persisted git.example.invalid slug", report["domain_slugs"])
	}
}
