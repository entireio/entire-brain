package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpScopeTestOptions binds an MCP server to repoDir the way `entire brain mcp`
// does (ENTIRE_REPO_ROOT), with a git runner that resolves that root.
func mcpScopeTestOptions(t *testing.T, repoDir string) (Options, EntireEnv) {
	t.Helper()
	env := semanticTestEnv(t, repoDir)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                                   {stdout: repoDir + "\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                                              {stdout: "aaa111\n"},
		fakeCommandKey("git", "rev-parse", "HEAD^{tree}"):                                       {stdout: "tree111\n"},
		fakeCommandKey("git", "branch", "--show-current"):                                       {stdout: "main\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"): {stdout: "origin/main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                                          {stdout: ""},
	}}
	opts := Options{
		Version: "test-version",
		Env:     env,
		Runner:  runner,
		Now:     func() time.Time { return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC) },
	}
	return opts, env
}

// writeScopeTestBrain materializes a minimal but real brain project (manifest +
// an exported session) for repoKey, so a delete is observably destructive.
func writeScopeTestBrain(t *testing.T, env EntireEnv, repoKey string) string {
	t.Helper()
	brainDir, err := brainDirForKey(env, repoKey)
	if err != nil {
		t.Fatalf("brain dir for %s: %v", repoKey, err)
	}
	if err := os.MkdirAll(filepath.Join(brainDir, "sessions"), 0o755); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "sessions", "s.jsonl"), []byte("{\"text\":\"private history\"}\n"), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	manifest := exportManifest{
		SchemaVersion:  brainManifestSchemaVersion,
		GeneratedAt:    time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC),
		RepoKey:        repoKey,
		TranscriptMode: "compact",
		Scope:          "all",
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return brainDir
}

func mcpScopeCall(t *testing.T, opts Options, name string, args map[string]any) map[string]any {
	t.Helper()
	input := frameMCPJSON(t, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp %s: %v", name, err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected one response for %s, got %d", name, len(responses))
	}
	return responses[0]
}

func mcpScopeErrorMessage(t *testing.T, response map[string]any) string {
	t.Helper()
	errObj, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error response, got %+v", response)
	}
	return fmt.Sprint(errObj["message"])
}

// A prompt-injected agent bound to repo A must not be able to erase repo B's
// brain (exported session history plus every derived index) by naming B's key.
func TestMCPDeleteProjectRefusesForeignRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	opts, env := mcpScopeTestOptions(t, repoDir)
	victimKey := "gh/victim/other"
	victimDir := writeScopeTestBrain(t, env, victimKey)

	response := mcpScopeCall(t, opts, "brain_delete_project", map[string]any{"repo_key": victimKey, "confirm": true})
	message := mcpScopeErrorMessage(t, response)
	if !strings.Contains(message, mcpAllowCrossRepoEnv) {
		t.Fatalf("refusal must name the opt-in gate %s: %s", mcpAllowCrossRepoEnv, message)
	}
	if _, err := os.Stat(filepath.Join(victimDir, "sessions", "s.jsonl")); err != nil {
		t.Fatalf("foreign brain was erased across the repo boundary: %v", err)
	}
}

// The bound repo's own key stays deletable, by key and by omission.
func TestMCPDeleteProjectAllowsBoundRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	opts, env := mcpScopeTestOptions(t, repoDir)
	boundKey := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
	boundDir := writeScopeTestBrain(t, env, boundKey)

	response := mcpScopeCall(t, opts, "brain_delete_project", map[string]any{"repo_key": boundKey, "confirm": true})
	if errObj, ok := response["error"]; ok {
		t.Fatalf("bound repo delete was refused: %+v", errObj)
	}
	if _, err := os.Stat(boundDir); !os.IsNotExist(err) {
		t.Fatalf("bound brain dir survived delete: %v", err)
	}
}

// The operator gate restores the documented cross-repo behaviour.
func TestMCPDeleteProjectHonorsCrossRepoGate(t *testing.T) {
	repoDir := t.TempDir()
	opts, env := mcpScopeTestOptions(t, repoDir)
	victimKey := "gh/victim/other"
	victimDir := writeScopeTestBrain(t, env, victimKey)
	t.Setenv(mcpAllowCrossRepoEnv, "1")

	response := mcpScopeCall(t, opts, "brain_delete_project", map[string]any{"repo_key": victimKey, "confirm": true})
	if errObj, ok := response["error"]; ok {
		t.Fatalf("gated cross-repo delete was refused: %+v", errObj)
	}
	if _, err := os.Stat(victimDir); !os.IsNotExist(err) {
		t.Fatalf("gated cross-repo delete did not remove %s: %v", victimDir, err)
	}
}

// Enumerating every locally indexed project leaks other repos' keys, brain
// paths, and index counts to an agent bound to one repo.
func TestMCPListProjectsScopesToBoundRepo(t *testing.T) {
	repoDir := t.TempDir()
	opts, env := mcpScopeTestOptions(t, repoDir)
	boundKey := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
	writeScopeTestBrain(t, env, boundKey)
	writeScopeTestBrain(t, env, "gh/victim/other")

	response := mcpScopeCall(t, opts, "brain_list_projects", map[string]any{})
	payload := mcpTextJSONPayload(t, response)
	data, _ := json.Marshal(payload)
	if strings.Contains(string(data), "gh/victim/other") {
		t.Fatalf("brain_list_projects leaked a foreign project: %s", data)
	}
	if !strings.Contains(string(data), boundKey) {
		t.Fatalf("brain_list_projects dropped the bound project: %s", data)
	}
}

func TestMCPListProjectsHonorsCrossRepoGate(t *testing.T) {
	repoDir := t.TempDir()
	opts, env := mcpScopeTestOptions(t, repoDir)
	boundKey := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
	writeScopeTestBrain(t, env, boundKey)
	writeScopeTestBrain(t, env, "gh/victim/other")
	t.Setenv(mcpAllowCrossRepoEnv, "1")

	response := mcpScopeCall(t, opts, "brain_list_projects", map[string]any{})
	payload := mcpTextJSONPayload(t, response)
	data, _ := json.Marshal(payload)
	for _, want := range []string{boundKey, "gh/victim/other"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("gated brain_list_projects missing %q: %s", want, data)
		}
	}
}

// A workspace names repos the bound server was never given; acting on it walks
// straight out of the bound root.
func TestMCPWorkspaceToolsRefuseReposOutsideBoundRoot(t *testing.T) {
	// The bound repo IS a member here, so this isolates the locality rule from
	// the membership rule. The foreign repo sits under its own parent, which is
	// what "outside the bound root" means now that sibling checkouts under a
	// common parent are in scope.
	parent := t.TempDir()
	repoDir := filepath.Join(parent, "bound")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts, env := mcpScopeTestOptions(t, repoDir)
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	boundKey := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
	writeWorkspaceBrainRepoAt(t, env, boundKey, repoDir, session, "pkg/review_context.go", "package x\n")
	foreignRepo, foreignKey := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "outside",
		Repos: []workspaceRepo{
			{RepoKey: boundKey, Name: "bound", LocalPathHint: repoDir},
			{RepoKey: foreignKey, Name: "foreign", LocalPathHint: foreignRepo},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"brain_workspace_graph", map[string]any{"workspace": "outside", "limit": 10}},
		{"brain_workspace_regressions", map[string]any{"workspace": "outside", "query": "scopeBaseRef base scope"}},
		{"brain_workspace_review", map[string]any{"workspace": "outside", "query": "scopeBaseRef base scope"}},
	} {
		response := mcpScopeCall(t, opts, tc.tool, tc.args)
		message := mcpScopeErrorMessage(t, response)
		if !strings.Contains(message, mcpAllowCrossRepoEnv) {
			t.Fatalf("%s refusal must name the opt-in gate %s: %s", tc.tool, mcpAllowCrossRepoEnv, message)
		}
		if !strings.Contains(message, foreignKey) {
			t.Fatalf("%s refusal must name the out-of-scope repo: %s", tc.tool, message)
		}
	}
}

func TestMCPWorkspaceToolsHonorCrossRepoGate(t *testing.T) {
	repoDir := t.TempDir()
	opts, env := mcpScopeTestOptions(t, repoDir)
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	foreignRepo, foreignKey := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "outside",
		Repos:         []workspaceRepo{{RepoKey: foreignKey, Name: "foreign", LocalPathHint: foreignRepo}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	t.Setenv(mcpAllowCrossRepoEnv, "1")

	response := mcpScopeCall(t, opts, "brain_workspace_regressions", map[string]any{"workspace": "outside", "query": "scopeBaseRef base scope"})
	if errObj, ok := response["error"]; ok {
		t.Fatalf("gated workspace tool was refused: %+v", errObj)
	}
}

// The plain CLI is not the confused deputy: a human at a terminal keeps every
// cross-repo workspace verb, gate unset.
func TestWorkspaceCLIStaysCrossRepoWithoutGate(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	repoA, keyA := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "cliwide",
		Repos:         []workspaceRepo{{RepoKey: keyA, Name: "a", LocalPathHint: repoA}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	cmd := NewRootCommand(opts)
	if _, err := execute(t, cmd, "workspace", "review", "cliwide", "fix scopeBaseRef base scope", "--json"); err != nil {
		t.Fatalf("CLI workspace review must stay cross-repo: %v", err)
	}
}
