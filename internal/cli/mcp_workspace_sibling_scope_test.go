package cli

import (
	"os"
	"path/filepath"
	"runtime"
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
	opts.Runner = &workspaceScopeRunner{CommandRunner: opts.Runner}
	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	body := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"

	boundKey := testLocalRepoStorageKey(t, boundDir)
	siblingKey = testLocalRepoStorageKey(t, siblingDir)
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
	strangerKey := testLocalRepoStorageKey(t, strangerDir)
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

	// The filesystem root is host-shaped. On POSIX a bare separator IS the
	// root; on Windows it is only drive-RELATIVE ("\" means "the root of the
	// current drive"), so it must be absolutized to the volume root ("D:\")
	// before the no-widening rule can be asserted against it. Comparing the
	// bare separator to the result is what made this assertion fail on Windows
	// even though workspaceScopeRoot behaved correctly there.
	root, err := filepath.Abs(string(filepath.Separator))
	if err != nil {
		t.Fatalf("resolve the filesystem root: %v", err)
	}
	if got := workspaceScopeRoot(root); got != root {
		t.Fatalf("workspaceScopeRoot(%q) = %q; a repo at the filesystem root must not widen", root, got)
	}

	// The case the guard actually exists for: a checkout one level below the
	// root. Widening by one would hand the whole volume to the fan-out, so this
	// path must stay its own scope. Plain widening returns the root here, which
	// is why this assertion -- not the root itself -- is what pins the rule.
	atRoot := filepath.Join(root, "cli")
	if got := workspaceScopeRoot(atRoot); got != atRoot {
		t.Fatalf("workspaceScopeRoot(%q) = %q; widening to %q would put the whole volume in scope", atRoot, got, root)
	}
}

// filepathCaseInsensitiveHost mirrors path/filepath's own rule: it folds case
// when comparing path elements on Windows and compares them byte-for-byte
// everywhere else. It is not a switch that skips a test -- every row of the
// table below runs on every host; it only records what "the same path" means
// on this one.
var filepathCaseInsensitiveHost = runtime.GOOS == "windows"

// TestSamePathOnDiskUsesHostPathSemantics pins the comparison that decides
// whether the bound repository is a member of a workspace by path. Getting it
// wrong is not cosmetic: a false negative refuses a legitimate workspace, and a
// false positive is the confused-deputy hole the membership check exists to
// close. The table runs on every host; only the case row's expectation is
// host-defined, because path case-sensitivity itself is.
func TestSamePathOnDiskUsesHostPathSemantics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resolved := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		resolved = r
	}
	sibling := filepath.Join(filepath.Dir(resolved), "not-the-same")
	// A hint in a manifest can name a checkout that is not on this disk (moved,
	// not cloned yet). Symlink resolution cannot canonicalize such a path, so
	// this is where a raw string comparison and the host's real path rules come
	// apart -- and it is exactly the spelling difference Windows produces.
	gone := filepath.Join(resolved, "moved-away", "cli")

	for _, tc := range []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", resolved, resolved, true},
		{"trailing separator", resolved, resolved + string(filepath.Separator), true},
		{"redundant dot segment", resolved, filepath.Join(resolved, ".", "."), true},
		{"round trip through a child", resolved, filepath.Join(resolved, "child", ".."), true},
		{"forward slashes", resolved, filepath.ToSlash(resolved), true},
		{"different directory", resolved, sibling, false},
		{"child is not the same directory", resolved, filepath.Join(resolved, "child"), false},
		{"parent is not the same directory", resolved, filepath.Dir(resolved), false},
		{"absent path, trailing separator", gone, gone + string(filepath.Separator), true},
		{"absent path, different directory", gone, filepath.Join(resolved, "moved-away", "entiredb"), false},
		// Windows compares paths case-insensitively and POSIX does not, so the
		// rule here is the host's own rule, applied consistently.
		{"different case", resolved, strings.ToUpper(resolved), filepathCaseInsensitiveHost},
		{"absent path, different case", gone, strings.ToUpper(gone), filepathCaseInsensitiveHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := samePathOnDisk(tc.a, tc.b); got != tc.want {
				t.Fatalf("samePathOnDisk(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := samePathOnDisk(tc.b, tc.a); got != tc.want {
				t.Fatalf("samePathOnDisk(%q, %q) = %v, want %v (comparison must be symmetric)", tc.b, tc.a, got, tc.want)
			}
		})
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
	farKey := testLocalRepoStorageKey(t, farDir)
	writeWorkspaceBrainRepoAt(t, env, farKey, farDir, session, "pkg/review_context.go", "package x\n")

	boundKey := testLocalRepoStorageKey(t, boundDir)
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
