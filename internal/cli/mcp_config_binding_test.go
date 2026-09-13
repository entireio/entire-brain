package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// mcpBoundAnchorTools are the four tools whose SCOPE ANCHOR is the bound
// repository root, and which therefore refuse to act without one: deleting a
// brain is irreversible, and the workspace tools fan out to whatever
// workspaceScopeRoot computes from the anchor's parent. An anchor picked up from
// an MCP host's working directory -- routinely $HOME, or / -- is never a
// statement about which repository the server serves, so these fail closed.
//
// brain_list_projects is deliberately NOT here. It used to be, and it was the
// only tool on the whole surface that required ENTIRE_REPO_ROOT: a server
// launched from a repository with no env answered 35 tools from the working
// directory and refused the 36th. Listing has no widening branch -- the listing
// it produces is scoped to the one repository it resolved, exactly as
// brain_status and brain_patterns already are -- so it resolves its target the
// way they do. See mcpRepoLocalStorage.
var mcpBoundAnchorTools = []string{
	"brain_workspace_graph",
	"brain_workspace_regressions",
	"brain_workspace_review",
	"brain_delete_project",
}

// mcpRepoScopedTools is every tool that needs a repository to act on, anchor
// tools plus the listing. A config that leaves the server unbound and is not
// started inside a repository ships a surface that is 5/36 dead on arrival.
var mcpRepoScopedTools = append([]string{"brain_list_projects"}, mcpBoundAnchorTools...)

// mcpPrintedServerEnv runs `entire-brain mcp --print-config` and returns the env
// block a host would register verbatim.
func mcpPrintedServerEnv(t *testing.T, opts Options) map[string]string {
	t.Helper()
	var out bytes.Buffer
	if err := printMCPServerConfig(context.Background(), &out, opts); err != nil {
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
	return server.Env
}

// TestPrintMCPServerConfigBindsARepository: the printed entry must name the
// repository the server serves.
//
// It used to emit `"env": {}`. An empty env is not a neutral default -- it is
// the unbound state, and the unbound state is the one in which every repo-scoped
// tool refuses.
func TestPrintMCPServerConfigBindsARepository(t *testing.T) {
	opts, boundDir, _ := workspaceSiblingFixture(t, "printed")

	env := mcpPrintedServerEnv(t, opts)
	root, ok := env[envRepoRoot]
	if !ok {
		t.Fatalf("printed env must set %s, got %#v", envRepoRoot, env)
	}
	want, err := canonicalLocalRepoRoot(boundDir)
	if err != nil {
		t.Fatalf("canonical repository root: %v", err)
	}
	if root != want {
		t.Fatalf("printed %s = %q, want the canonical repository root %q", envRepoRoot, root, want)
	}
}

// A directory that is not a repository has no binding to print. Emitting one
// anyway would hand the host a root the server accepts and then cannot resolve
// storage for, turning a config problem into a tool error.
func TestPrintMCPServerConfigOmitsABindingOutsideARepository(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		Version: "test-version",
		Env:     semanticTestEnv(t, dir),
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stderr: "fatal: not a git repository", err: errors.New("exit status 128")},
		}},
	}

	if env := mcpPrintedServerEnv(t, opts); len(env) != 0 {
		t.Fatalf("a non-repository must print no binding, got %#v", env)
	}
}

// TestPrintedMCPConfigMakesEveryScopedToolReachable is the defect as an agent
// experiences it: register the server exactly as --print-config recommends, then
// call the advertised surface.
//
// Before the fix the printed env was empty, so all five of these answered
//
//	-32000: <tool> is scoped to the MCP server's bound repository ...
//
// The binding is the ONLY thing taken from the printed config here; everything
// else is the ambient environment a host would already provide.
func TestPrintedMCPConfigMakesEveryScopedToolReachable(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	opts, _, _ := workspaceSiblingFixture(t, "printed")

	hostOpts := opts
	hostOpts.Env = opts.Env
	hostOpts.Env.RepoRoot = mcpPrintedServerEnv(t, opts)[envRepoRoot]

	for _, tool := range mcpRepoScopedTools {
		response := mcpScopeCall(t, hostOpts, tool, mcpBoundScopeToolArgs(t, hostOpts, tool, "printed"))
		if _, failed := response["error"]; !failed {
			continue
		}
		message := mcpScopeErrorMessage(t, response)
		if strings.Contains(message, "is scoped to the MCP server's bound repository") {
			t.Fatalf("%s is dead under the server's own recommended config: %s", tool, message)
		}
		t.Fatalf("%s failed under the recommended config: %s", tool, message)
	}
}

// mcpBoundScopeToolArgs is scopeToolArgs with the one argument that must name a
// real repository: brain_delete_project's key. The scoping test suite points it
// at a foreign project on purpose; here the point is that the BOUND project is
// reachable, so it names the bound key.
func mcpBoundScopeToolArgs(t *testing.T, opts Options, tool, workspace string) map[string]any {
	t.Helper()
	if tool != "brain_delete_project" {
		return scopeToolArgs(tool, workspace)
	}
	storage, bound, err := mcpBoundRepoStorage(context.Background(), opts)
	if err != nil || !bound {
		t.Fatalf("bound storage (%v, bound=%v)", err, bound)
	}
	return map[string]any{"repo_key": storage.Key, "confirm": true}
}

// TestMCPUnboundScopeRefusalNamesRepoRootBeforeTheOptOut pins the second half of
// the defect: the refusal used to name ONLY
// ENTIRE_BRAIN_MCP_ALLOW_CROSS_REPO=1, i.e. it answered a missing setting by
// recommending that the reader disable the confused-deputy protection. An agent
// follows that literally.
//
// ENTIRE_REPO_ROOT is the fix and must be named first; the opt-out may still
// appear, but only as the deliberate cross-repo choice.
func TestMCPUnboundScopeRefusalNamesRepoRootBeforeTheOptOut(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	opts, _, _ := workspaceSiblingFixture(t, "unbound")
	opts.Env.RepoRoot = ""

	for _, tool := range mcpBoundAnchorTools {
		message := mcpScopeErrorMessage(t, mcpScopeCall(t, opts, tool, scopeToolArgs(tool, "unbound")))
		root := strings.Index(message, envRepoRoot)
		gate := strings.Index(message, mcpAllowCrossRepoEnv)
		if root < 0 {
			t.Fatalf("%s: an unbound server's refusal must name %s, the setting that actually fixes it: %s", tool, envRepoRoot, message)
		}
		if gate < 0 {
			t.Fatalf("%s: refusal must still name the opt-out %s: %s", tool, mcpAllowCrossRepoEnv, message)
		}
		if root > gate {
			t.Fatalf("%s: refusal names the security opt-out before the fix: %s", tool, message)
		}
	}
}

// A genuine cross-repo request is a different failure with a different cause,
// and must still say so -- naming the bound repository, the opt-out, and the
// binding that would point the server at the repository the caller meant.
func TestMCPCrossRepoRefusalStillNamesTheBoundRepositoryAndTheGate(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	repoDir := t.TempDir()
	opts, env := mcpScopeTestOptions(t, repoDir)
	victimKey := "gh/victim/other"
	writeScopeTestBrain(t, env, victimKey)

	message := mcpScopeErrorMessage(t, mcpScopeCall(t, opts, "brain_delete_project",
		map[string]any{"repo_key": victimKey, "confirm": true}))
	for _, want := range []string{mcpAllowCrossRepoEnv, envRepoRoot, "names a different project"} {
		if !strings.Contains(message, want) {
			t.Fatalf("cross-repo refusal must mention %q: %s", want, message)
		}
	}
}

// An unbound brain_delete_project used to report a cross-repo mismatch --
// `repo_key "x" names a different project` -- when there was no bound project
// for it to differ from. The reader hunted for a mismatch that did not exist.
func TestMCPUnboundDeleteProjectReportsTheMissingBindingNotAMismatch(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	opts, env := mcpScopeTestOptions(t, t.TempDir())
	boundKey := mcpBoundScopeToolArgs(t, opts, "brain_delete_project", "")["repo_key"].(string)
	writeScopeTestBrain(t, env, boundKey)
	opts.Env.RepoRoot = ""

	message := mcpScopeErrorMessage(t, mcpScopeCall(t, opts, "brain_delete_project",
		map[string]any{"repo_key": boundKey, "confirm": true}))
	if strings.Contains(message, "names a different project") {
		t.Fatalf("unbound refusal blames a mismatch that cannot exist: %s", message)
	}
	if !strings.Contains(message, mcpUnboundRepoDetail) {
		t.Fatalf("unbound refusal must report the missing binding: %s", message)
	}
}
