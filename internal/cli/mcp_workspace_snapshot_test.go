package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Resolve each fixture checkout independently, as git does for sibling repos.
type workspaceScopeRunner struct {
	CommandRunner
	beforeRun func()
}

func (r *workspaceScopeRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if r.beforeRun != nil {
		f := r.beforeRun
		r.beforeRun = nil
		f()
	}
	if name == "git" && strings.Join(args, " ") == "rev-parse --show-toplevel" {
		return []byte(dir + "\n"), nil, nil
	}
	return r.CommandRunner.Run(ctx, dir, name, args...)
}

func TestMCPUnboundProjectAndWorkspaceAccessFailsClosed(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	opts, _, _ := workspaceSiblingFixture(t, "unbound")
	victim := writeScopeTestBrain(t, opts.Env, "gh/victim/other")
	opts.Env.RepoRoot = ""
	for _, tool := range []string{"brain_list_projects", "brain_delete_project", "brain_workspace_graph", "brain_workspace_regressions", "brain_workspace_review"} {
		response := mcpScopeCall(t, opts, tool, scopeToolArgs(tool, "unbound"))
		if msg := mcpScopeErrorMessage(t, response); !strings.Contains(msg, mcpAllowCrossRepoEnv) {
			t.Fatalf("%s: %s", tool, msg)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("foreign brain modified: %v", err)
	}
	response := mcpScopeCall(t, opts, "brain_delete_project", map[string]any{"confirm": true})
	if msg := mcpScopeErrorMessage(t, response); !strings.Contains(msg, mcpAllowCrossRepoEnv) {
		t.Fatal(msg)
	}
	t.Setenv(mcpAllowCrossRepoEnv, "1")
	for _, tool := range []string{"brain_list_projects", "brain_workspace_graph", "brain_workspace_regressions", "brain_workspace_review", "brain_delete_project"} {
		response := mcpScopeCall(t, opts, tool, scopeToolArgs(tool, "unbound"))
		if errObj, ok := response["error"]; ok {
			t.Fatalf("explicit override %s: %v", tool, errObj)
		}
	}
}

func TestMCPWorkspaceRejectsForgedCheckoutIdentity(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	for _, member := range []int{0, 1} {
		opts, _, _ := workspaceSiblingFixture(t, "forged")
		manifest, err := loadWorkspaceManifest(opts.Env, "forged")
		if err != nil {
			t.Fatal(err)
		}
		manifest.Repos[member].RepoKey = "gh/victim/other"
		victim := writeScopeTestBrain(t, opts.Env, "gh/victim/other")
		if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{"brain_workspace_graph", "brain_workspace_regressions", "brain_workspace_review"} {
			response := mcpScopeCall(t, opts, tool, scopeToolArgs(tool, "forged"))
			if msg := mcpScopeErrorMessage(t, response); !strings.Contains(msg, "does not match local checkout identity") {
				t.Fatalf("%s: %s", tool, msg)
			}
		}
		entries, err := os.ReadDir(victim)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "lock") {
				t.Fatalf("foreign brain locked: %s", entry.Name())
			}
		}
	}
}

func TestMCPWorkspaceExecutesValidatedManifestSnapshot(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	for _, tool := range []string{"brain_workspace_graph", "brain_workspace_regressions", "brain_workspace_review"} {
		t.Run(tool, func(t *testing.T) {
			opts, _, siblingKey := workspaceSiblingFixture(t, "snapshot")
			manifest, err := loadWorkspaceManifest(opts.Env, "snapshot")
			if err != nil {
				t.Fatal(err)
			}
			foreignKey := "gh/victim/other"
			writeScopeTestBrain(t, opts.Env, foreignKey)
			manifest.Repos = append(manifest.Repos, workspaceRepo{RepoKey: foreignKey, LocalPathHint: filepath.Join(t.TempDir(), "foreign")})
			// The first identity lookup runs after the manifest is loaded. Replace its
			// file then: a second load during execution would consume the foreign key.
			changed := false
			opts.Runner = &workspaceScopeRunner{CommandRunner: opts.Runner, beforeRun: func() {
				if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
					t.Fatal(err)
				}
				changed = true
			}}
			response := mcpScopeCall(t, opts, tool, scopeToolArgs(tool, "snapshot"))
			if errObj, ok := response["error"]; ok {
				t.Fatalf("snapshot rejected: %v", errObj)
			}
			data, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if !changed || strings.Contains(string(data), foreignKey) || !strings.Contains(string(data), siblingKey) {
				t.Fatalf("execution did not retain validated members: %s", data)
			}
		})
	}
}

func scopeToolArgs(tool, workspace string) map[string]any {
	switch tool {
	case "brain_list_projects":
		return map[string]any{}
	case "brain_delete_project":
		return map[string]any{"repo_key": "gh/victim/other", "confirm": true}
	case "brain_workspace_graph":
		return map[string]any{"workspace": workspace}
	default:
		return map[string]any{"workspace": workspace, "query": "scope"}
	}
}
