package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workspaceSiblingFixture builds the layout every multi-repo workspace on this
// machine actually has -- sibling checkouts under one parent:
//
//	<parent>/cli        <- the MCP server is bound here
//	<parent>/entiredb   <- a sibling member of the same workspace
//
// This is the normal case, not an edge case: `entire brain workspace add` is
// pointed at each checkout in turn, and checkouts live side by side.
func workspaceSiblingFixture(t *testing.T, workspaceName string) (opts Options, boundDir, siblingKey string) {
	t.Helper()
	parent := t.TempDir()
	boundDir = filepath.Join(parent, "cli")
	siblingDir := filepath.Join(parent, "entiredb")
	for _, dir := range []string{boundDir, siblingDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opts, env := mcpScopeTestOptions(t, boundDir)
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	body := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"

	boundKey := filepath.ToSlash(filepath.Join("local", localRepoKey(boundDir)))
	siblingKey = filepath.ToSlash(filepath.Join("local", localRepoKey(siblingDir)))
	writeWorkspaceBrainRepoAt(t, env, boundKey, boundDir, session, "pkg/review_context.go", body)
	writeWorkspaceBrainRepoAt(t, env, siblingKey, siblingDir, session, "pkg/review_context.go", body)

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          workspaceName,
		Repos: []workspaceRepo{
			{RepoKey: boundKey, Name: "cli", LocalPathHint: boundDir},
			{RepoKey: siblingKey, Name: "entiredb", LocalPathHint: siblingDir},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	return opts, boundDir, siblingKey
}

// TestMCPWorkspaceToolsAllowSiblingCheckouts: requiring every member to live
// INSIDE the bound repository root refused the only layout workspaces are ever
// built from. A sibling checkout is never inside its sibling, so all three
// workspace tools returned "outside the bound repository root" for the setup
// they exist to serve -- the feature was scoped to nothing.
func TestMCPWorkspaceToolsAllowSiblingCheckouts(t *testing.T) {
	opts, _, _ := workspaceSiblingFixture(t, "siblings")
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"brain_workspace_graph", map[string]any{"workspace": "siblings", "limit": 10}},
		{"brain_workspace_regressions", map[string]any{"workspace": "siblings", "query": "scopeBaseRef base scope"}},
		{"brain_workspace_review", map[string]any{"workspace": "siblings", "query": "scopeBaseRef base scope"}},
	} {
		response := mcpScopeCall(t, opts, tc.tool, tc.args)
		if errObj, ok := response["error"]; ok {
			t.Fatalf("%s refused the standard sibling-checkout workspace: %+v", tc.tool, errObj)
		}
	}
}

// TestMCPWorkspaceToolsRefuseWorkspaceWithoutTheBoundRepo keeps the
// confused-deputy protection that widening the locality rule must not lose.
//
// Membership is now what carries it: an agent bound to repo A may fan out over
// a workspace only if A is a MEMBER of it. Naming some other workspace -- even
// one whose repos happen to sit next door -- is exactly the "agent in repo A
// acting on unrelated repo B" case, and is refused.
func TestMCPWorkspaceToolsRefuseWorkspaceWithoutTheBoundRepo(t *testing.T) {
	parent := t.TempDir()
	boundDir := filepath.Join(parent, "cli")
	strangerDir := filepath.Join(parent, "unrelated")
	for _, dir := range []string{boundDir, strangerDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opts, env := mcpScopeTestOptions(t, boundDir)
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	strangerKey := filepath.ToSlash(filepath.Join("local", localRepoKey(strangerDir)))
	writeWorkspaceBrainRepoAt(t, env, strangerKey, strangerDir, session, "pkg/review_context.go", "package x\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "not-mine",
		Repos:         []workspaceRepo{{RepoKey: strangerKey, Name: "unrelated", LocalPathHint: strangerDir}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	for _, tool := range []string{"brain_workspace_graph", "brain_workspace_regressions", "brain_workspace_review"} {
		args := map[string]any{"workspace": "not-mine"}
		if tool != "brain_workspace_graph" {
			args["query"] = "scopeBaseRef base scope"
		} else {
			args["limit"] = float64(10)
		}
		message := mcpScopeErrorMessage(t, mcpScopeCall(t, opts, tool, args))
		if !strings.Contains(message, mcpAllowCrossRepoEnv) {
			t.Fatalf("%s refusal must name the opt-in gate: %s", tool, message)
		}
		if !strings.Contains(message, "not-mine") {
			t.Fatalf("%s refusal must name the workspace: %s", tool, message)
		}
	}
}

// TestWorkspaceScopeRootWidensToTheParentButNotToTheFilesystemRoot pins the
// widening rule, including the degenerate case: a repository checked out at the
// top of a volume must not put the whole filesystem in scope.
func TestWorkspaceScopeRootWidensToTheParent(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "cli")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	got := workspaceScopeRoot(repo)
	want := parent
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		want = resolved
	}
	if got != want {
		t.Fatalf("workspaceScopeRoot(%q) = %q, want the parent %q", repo, got, want)
	}

	root := string(filepath.Separator)
	if got := workspaceScopeRoot(root); got != root {
		t.Fatalf("workspaceScopeRoot(%q) = %q; a repo at the filesystem root must not widen", root, got)
	}
}

// TestMCPWorkspaceMemberOutsideTheParentIsRefused: widening to the parent is
// not the same as no rule. A member two directories away is still out of scope
// even when the bound repo is a member of the workspace.
func TestMCPWorkspaceMemberOutsideTheParentIsRefused(t *testing.T) {
	opts, boundDir, _ := workspaceSiblingFixture(t, "siblings")
	env := opts.Env
	farDir := t.TempDir() // a different parent entirely
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	farKey := filepath.ToSlash(filepath.Join("local", localRepoKey(farDir)))
	writeWorkspaceBrainRepoAt(t, env, farKey, farDir, session, "pkg/review_context.go", "package x\n")

	boundKey := filepath.ToSlash(filepath.Join("local", localRepoKey(boundDir)))
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "reaches-out",
		Repos: []workspaceRepo{
			{RepoKey: boundKey, Name: "cli", LocalPathHint: boundDir},
			{RepoKey: farKey, Name: "far", LocalPathHint: farDir},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	message := mcpScopeErrorMessage(t, mcpScopeCall(t, opts, "brain_workspace_graph", map[string]any{"workspace": "reaches-out", "limit": float64(10)}))
	if !strings.Contains(message, farKey) {
		t.Fatalf("refusal must name the out-of-scope repo: %s", message)
	}
	if !strings.Contains(message, mcpAllowCrossRepoEnv) {
		t.Fatalf("refusal must name the opt-in gate: %s", message)
	}
}
