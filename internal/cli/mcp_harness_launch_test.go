package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpPrintedServerEnvWithNotes prints the entry and returns both halves a
// reader gets: the env block a host registers, and whatever went to stderr.
func mcpPrintedServerEnvWithNotes(t *testing.T, opts Options) (map[string]string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := printMCPServerConfig(context.Background(), &out, &errOut, opts); err != nil {
		t.Fatalf("print config: %v", err)
	}
	var config struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(out.Bytes(), &config); err != nil {
		t.Fatalf("output must be valid JSON a host can paste: %v\n%s", err, out.String())
	}
	server, ok := config.MCPServers[mcpServerName]
	if !ok {
		t.Fatalf("no %q entry: %s", mcpServerName, out.String())
	}
	return server.Env, errOut.String()
}

// prunedHostEnv is the EntireEnv a server gets from a host that passes ONLY the
// entry's own `env` block through -- which is what Codex and Cursor do.
//
// Measured on this machine: Codex starts an MCP server with 14 environment
// variables and Cursor with 15, in both cases HOME/PATH/PWD/SHELL/USER/TERM and
// a few shell leftovers plus the entry's declared env. Neither passes ENTIRE_*
// or XDG_* through. Claude Code passes the full parent environment (69 vars
// here), which is why this only ever reproduced on two of the three harnesses.
func prunedHostEnv(printed map[string]string) EntireEnv {
	return EntireEnv{
		RepoRoot:        printed[envRepoRoot],
		PluginConfigDir: printed[envPluginConfigDir],
		PluginDataDir:   printed[envPluginDataDir],
		PluginStateDir:  printed[envPluginStateDir],
		PluginCacheDir:  printed[envPluginCacheDir],
	}
}

// TestPrintedMCPConfigCarriesTheStoreToAPrunedHost is the defect as a user meets
// it: the CLI and the agent read different brains for the same repository.
//
// `entire brain mcp --print-config` printed only ENTIRE_REPO_ROOT. Registered
// with Codex or Cursor -- neither of which passes the parent environment to an
// MCP server -- the server had no ENTIRE_PLUGIN_*_DIR and no XDG_* left to
// re-derive one from, so resolvePluginDirs fell through to the default
// $HOME/.local/share/entire path while the CLI that printed the entry was
// reading somewhere else entirely. Nothing errors: the agent calls brain_query
// and is told "no brain has been built for this repository", which is a
// confident wrong answer about a repository that has one.
func TestPrintedMCPConfigCarriesTheStoreToAPrunedHost(t *testing.T) {
	// Leave the package's XDG pin so the wrong answer this test must not accept
	// -- the default store -- is a sandboxed directory nothing else shares,
	// with the home redirected on every platform rather than only on POSIX.
	useDefaultStoreFallback(t)

	t.Run("explicit plugin dirs", func(t *testing.T) {
		opts, boundDir, _ := workspaceSiblingFixture(t, "printed-store")
		assertPrintedEntryReachesTheSameBrain(t, opts, boundDir)
	})

	// The variable a normal user actually sets. XDG_DATA_HOME is an input to
	// resolvePluginDirs and is dropped by the same hosts, so an entry that
	// echoed the raw ENTIRE_PLUGIN_* variables would still be wrong here --
	// they are empty. Only the RESOLVED dirs survive the pruning.
	t.Run("XDG-configured store", func(t *testing.T) {
		opts, boundDir, _ := workspaceSiblingFixture(t, "printed-store-xdg")
		xdgData := t.TempDir()
		t.Setenv(xdgDataHome, xdgData)
		opts.Env.PluginConfigDir = ""
		opts.Env.PluginDataDir = ""
		opts.Env.PluginStateDir = ""
		opts.Env.PluginCacheDir = ""
		assertPrintedEntryReachesTheSameBrain(t, opts, boundDir)

		printed, _ := mcpPrintedServerEnvWithNotes(t, opts)
		if data := printed[envPluginDataDir]; !strings.HasPrefix(data, xdgData) {
			t.Errorf("printed %s = %q, want a path under the XDG data home %q; "+
				"a host that drops XDG_DATA_HOME cannot re-derive it",
				envPluginDataDir, data, xdgData)
		}
	})
}

// assertPrintedEntryReachesTheSameBrain registers the printed entry the way a
// pruning host does and checks it lands on the brain the printing process reads.
func assertPrintedEntryReachesTheSameBrain(t *testing.T, opts Options, boundDir string) {
	t.Helper()
	ctx := context.Background()

	want, err := resolvePluginDirs(opts.Env)
	if err != nil {
		t.Fatalf("resolve the printing process's plugin dirs: %v", err)
	}
	wantStorage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, boundDir)
	if err != nil {
		t.Fatalf("resolve the printing process's brain: %v", err)
	}

	printed, _ := mcpPrintedServerEnvWithNotes(t, opts)
	hostEnv := prunedHostEnv(printed)

	got, err := resolvePluginDirs(hostEnv)
	if err != nil {
		t.Fatalf("a pruned host could not resolve the printed entry's plugin dirs: %v", err)
	}
	if got != want {
		t.Errorf("printed entry resolves to %+v, want the store the CLI reads %+v", got, want)
	}

	gotStorage, err := repoStoragePaths(ctx, opts.Runner, hostEnv, boundDir)
	if err != nil {
		t.Fatalf("a pruned host could not resolve the printed entry's brain: %v", err)
	}
	if gotStorage.BrainDir != wantStorage.BrainDir {
		t.Errorf("the agent would read %q while the CLI reads %q -- same repository, two brains",
			gotStorage.BrainDir, wantStorage.BrainDir)
	}
}

// The entry must never point the server at the default store just because the
// printing process happened to be using it -- but it must point there
// EXPLICITLY when that is the store, so a pruned host does not have to guess.
func TestPrintedMCPConfigPinsTheDefaultStoreExplicitly(t *testing.T) {
	home := useDefaultStoreFallback(t)

	opts, _, _ := workspaceSiblingFixture(t, "printed-default")
	opts.Env.PluginConfigDir = ""
	opts.Env.PluginDataDir = ""
	opts.Env.PluginStateDir = ""
	opts.Env.PluginCacheDir = ""

	printed, _ := mcpPrintedServerEnvWithNotes(t, opts)
	wantData := filepath.Join(home, ".local", "share", xdgRootDir, "plugins", "data", pluginDataName)
	if got := printed[envPluginDataDir]; got != wantData {
		t.Errorf("printed %s = %q, want %q", envPluginDataDir, got, wantData)
	}
	for _, name := range []string{envPluginConfigDir, envPluginStateDir, envPluginCacheDir} {
		if printed[name] == "" {
			t.Errorf("printed entry omits %s; a pruned host has nothing left to resolve it from", name)
		}
	}
}

// TestMCPUnresolvedRepoDetailDoesNotBlameTheWorkingDirectory: a missing git is
// not a missing repository, and the repo-local gate said it was.
//
// Reproduced through the real server: launched with the four plugin dirs and
// nothing else -- no PATH -- with its working directory inside a real git
// repository, brain_status resolved that repository and answered from it, while
// brain_list_projects in the SAME PROCESS refused with "the server has no bound
// repository and its working directory is not inside one". The working directory
// was inside one. An agent that believes the message edits a config to fix a
// problem it does not have.
func TestMCPUnresolvedRepoDetailDoesNotBlameTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("git absent", func(t *testing.T) {
		t.Parallel()
		detail := mcpUnresolvedRepoDetail(ctx, Options{Runner: gitAbsentRunner{}})
		if strings.Contains(detail, "working directory is not inside one") {
			t.Errorf("git is missing, so nothing is known about the working directory:\n%s", detail)
		}
		if !strings.Contains(detail, "git could not be run") {
			t.Errorf("detail does not name the real cause:\n%s", detail)
		}
	})

	t.Run("git present, not a repository", func(t *testing.T) {
		t.Parallel()
		detail := mcpUnresolvedRepoDetail(ctx, Options{Runner: gitPresentNoRepoRunner{}})
		if !strings.Contains(detail, "working directory is not inside one") {
			t.Errorf("git answered, so the working directory IS the problem:\n%s", detail)
		}
		if strings.Contains(detail, "git could not be run") {
			t.Errorf("git answered --version; it can be run:\n%s", detail)
		}
	})
}

// --print-config exits 0 whether or not it found a repository to bind, and the
// JSON alone never says which happened. A reader who pipes it into .mcp.json and
// later hits five refusing tools has no way back to the cause, so the cause is
// printed -- on stderr, so the redirect still works.
func TestPrintMCPServerConfigNotesWhyItPrintedNoBinding(t *testing.T) {
	t.Parallel()

	t.Run("git present, not a repository", func(t *testing.T) {
		t.Parallel()
		opts := Options{
			Version: "test-version",
			Env:     semanticTestEnv(t, t.TempDir()),
			Runner:  gitPresentNoRepoRunner{},
		}
		env, notes := mcpPrintedServerEnvWithNotes(t, opts)
		if _, ok := env[envRepoRoot]; ok {
			t.Fatalf("a non-repository must print no binding, got %#v", env)
		}
		if !strings.Contains(notes, "not inside a git repository") {
			t.Errorf("silence about a missing binding; stderr was %q", notes)
		}
		if !strings.Contains(notes, "brain_list_projects") {
			t.Errorf("the note does not say which tools this costs; stderr was %q", notes)
		}
	})

	t.Run("git absent", func(t *testing.T) {
		t.Parallel()
		opts := Options{
			Version: "test-version",
			Env:     semanticTestEnv(t, t.TempDir()),
			Runner:  gitAbsentRunner{},
		}
		_, notes := mcpPrintedServerEnvWithNotes(t, opts)
		if !strings.Contains(notes, "install git") {
			t.Errorf("with git missing the remedy is to install it; stderr was %q", notes)
		}
		if strings.Contains(notes, "not inside a git repository") {
			t.Errorf("git could not be run, so this directory was never judged; stderr was %q", notes)
		}
	})
}

// A successful print says nothing on stderr: the note is a diagnosis, not a
// banner, and a tool that always warns is a tool nobody reads.
func TestPrintMCPServerConfigIsQuietWhenItBinds(t *testing.T) {
	opts, _, _ := workspaceSiblingFixture(t, "printed-quiet")
	if _, notes := mcpPrintedServerEnvWithNotes(t, opts); strings.TrimSpace(notes) != "" {
		t.Errorf("a bound entry must print no note, got %q", notes)
	}
}

// The entry has to stay something a host will actually accept. Claude Code and
// Cursor both read this exact JSON shape from a file, so the env block must be
// flat string->string with no nesting a TOML or JSON schema would reject.
func TestPrintedMCPConfigEnvIsFlatStrings(t *testing.T) {
	opts, _, _ := workspaceSiblingFixture(t, "printed-flat")
	var out bytes.Buffer
	if err := printMCPServerConfig(context.Background(), &out, io.Discard, opts); err != nil {
		t.Fatalf("print config: %v", err)
	}
	var config struct {
		MCPServers map[string]struct {
			Env map[string]json.RawMessage `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(out.Bytes(), &config); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	for name, raw := range config.MCPServers[mcpServerName].Env {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Errorf("env %q is not a string (%s); hosts reject non-string env values", name, raw)
			continue
		}
		if !filepath.IsAbs(s) {
			t.Errorf("env %q = %q must be absolute: a pruned host has no working directory to resolve it against", name, s)
		}
	}
}

// An agent plans against the tool description, so a bound the description does
// not mention is a bound the agent walks into.
//
// brain_refresh says "Calls are capped at 60 seconds" AND names
// brain_index_repository as the separate long-running step to use instead for a
// large repository -- while brain_index_repository carries the same 60s cap and
// said nothing about it. An agent following that advice on a repository too big
// for one minute is routed from a disclosed limit to an undisclosed identical
// one, and the failure arrives as a surprise mid-index.
func TestIndexToolDescriptionDisclosesItsTimeout(t *testing.T) {
	t.Parallel()

	descriptions := map[string]string{}
	for _, def := range mcpToolDefinitions() {
		name, _ := def["name"].(string)
		desc, _ := def["description"].(string)
		descriptions[name] = desc
	}

	for _, name := range []string{"brain_refresh", "brain_index_repository"} {
		desc, ok := descriptions[name]
		if !ok {
			t.Fatalf("%s is not on the surface", name)
		}
		if !strings.Contains(desc, "capped at 60 seconds") {
			t.Errorf("%s enforces a 60s cap its description never states:\n%s", name, desc)
		}
	}

	// The cap is real, and it is the same one for both.
	if mcpIndexTimeout != mcpRefreshTimeout {
		t.Errorf("the descriptions state one shared cap but the code has two: index=%s refresh=%s",
			mcpIndexTimeout, mcpRefreshTimeout)
	}
	if mcpIndexTimeout != 60*time.Second {
		t.Errorf("descriptions say 60 seconds, code says %s", mcpIndexTimeout)
	}
}

// The server's own instructions are the contract an agent reads before its
// first call, and two of their claims were stronger than the implementation.
//
//   - "{path, returned, total}" invited reading `total` as how many rows exist.
//     It is len(rows the tool produced), which the caller's own limit already
//     capped: a repository with 154 routes queried at limit=100 reported
//     {returned: 57, total: 100}, so "43 dropped" was 97 dropped.
//   - "a wider list is not cut to a narrower one's width" is exactly what
//     mcpTrimSharedWidth -- the documented fallback for a document too wide to
//     share fairly -- does. Lists of 154/76/264 rows all came back at 57.
func TestServerInstructionsDoNotOverstateTheBudget(t *testing.T) {
	t.Parallel()

	if strings.Contains(mcpServerInstructions, "a wider list is not cut to a narrower one's width") {
		t.Error("the shared-width fallback does exactly this; the instructions must not deny it")
	}
	if !strings.Contains(mcpServerInstructions, "shared width") {
		t.Error("the instructions do not mention the shared-width fallback at all")
	}
	if !strings.Contains(mcpServerInstructions, "not the number that exist") {
		t.Error("the instructions do not say that `total` is capped by the caller's own limit")
	}
}
