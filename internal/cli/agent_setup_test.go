package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/agentsetup"
)

func TestAgentSetupReadsActualBrainRecords(t *testing.T) {
	for _, remote := range []string{"", "https://github.com/Team/Repo.git", "git@gitlab.com:team/sub/repo.git", "git@custom.example.com:team/repo.git", "entire://server/gh/team/repo", "git@rewrite:Team/Repo.git"} {
		t.Run(remote, func(t *testing.T) {
			repo := t.TempDir()
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			global := filepath.Join(t.TempDir(), "gitconfig")
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			if remote == "git@rewrite:Team/Repo.git" {
				included := filepath.Join(t.TempDir(), "included")
				if err := os.WriteFile(included, []byte("[url \"https://github.com/\"]\n\tinsteadOf = git@rewrite:\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(global, []byte("[include]\n\tpath = "+included+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			env := EntireEnv{PluginStateDir: t.TempDir(), PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir()}
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %s %v", out, err)
			}
			if remote != "" {
				if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", remote).CombinedOutput(); err != nil {
					t.Fatalf("git remote: %s %v", out, err)
				}
			}
			storage, err := repoStoragePaths(context.Background(), ExecRunner{}, env, repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeSetupRecord(filepath.Dir(storage.HeadPath), setupRecord{SchemaVersion: setupBackfillStateVersion, Workspace: "fixture", Agent: "none"}); err != nil {
				t.Fatal(err)
			}
			guide, err := agentsetup.Preview(repo, "graph", agentsetup.Options{StateDir: env.PluginStateDir, ConfigDir: env.PluginConfigDir, DataDir: env.PluginDataDir, ListPlugins: func() (string, error) {
				return "Managed plugin directory: /fixture\n\n  brain v1 → /fixture/brain\n", nil
			}})
			if err != nil || guide != agentsetup.CombinedGuide {
				t.Fatalf("actual setup record not recognized: %v\n%s", err, guide)
			}
		})
	}
}
func TestAgentGuideAliasAndOutsideRepository(t *testing.T) {
	t.Chdir(t.TempDir())
	opts := Options{Version: "test"}
	guide, err := execute(t, NewRootCommand(opts), "agent-guide")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := execute(t, NewRootCommand(opts), "guide")
	if err != nil || alias != guide {
		t.Fatal("alias differs", err)
	}
	if !strings.HasPrefix(guide, "# Entire repository agent guide — Brain") {
		t.Fatal(guide)
	}
	if _, err := execute(t, NewRootCommand(opts), "init-agents"); err == nil {
		t.Fatal("initialized outside repository without explicit target")
	}
}

func TestCapabilitiesWithoutRepository(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Version: "test-build", Env: EntireEnv{RepoRoot: filepath.Join(dir, "missing")}}
	out, err := execute(t, NewRootCommand(opts), "capabilities", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var c brainCapabilities
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c.Version != "test-build" || c.SchemaVersion != 1 || c.Build.BrainCGO != brainCGOBuild || c.Query.DefaultMode != "hybrid" {
		t.Fatalf("capabilities: %+v", c)
	}
	if len(c.Sources) != 4 {
		t.Fatalf("sources: %+v", c.Sources)
	}
	for _, source := range c.Sources {
		if source.Name == retrievalSourceConversation && (source.Default || !source.Experimental || source.SemanticCompiled != brainCGOBuild) {
			t.Fatalf("conversation availability overstated: %+v", source)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("capabilities created repository state")
	}
	help, err := execute(t, NewRootCommand(opts), "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Set up your agent:", "init-agents", "agent-guide", "capabilities"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q", want)
		}
	}
	for _, line := range strings.Split(help, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "guide" {
			t.Fatal("old guide name remains in top-level list")
		}
	}
}
