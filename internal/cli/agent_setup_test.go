package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentGuideNameAndInstalledContent(t *testing.T) {
	opts := Options{Version: "test"}
	guide, err := execute(t, NewRootCommand(opts), "agent-guide")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := execute(t, NewRootCommand(opts), "guide")
	if err != nil || alias != guide {
		t.Fatalf("guide alias differs: %v", err)
	}
	repo := t.TempDir()
	original := "# Team rules\nKeep these instructions.\n<!-- entire-graph:begin -->\nGraph rules\n<!-- entire-graph:end -->\n"
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
	}
	opts.Env.RepoRoot = repo
	out, err := execute(t, NewRootCommand(opts), "init-agents", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Changed []string `json:"changed_files"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || len(result.Changed) != 3 {
		t.Fatalf("install: %s / %v", out, err)
	}
	installed, err := os.ReadFile(filepath.Join(repo, brainAgentGuidePath))
	if err != nil || string(installed) != guide {
		t.Fatalf("installed guide differs: %v", err)
	}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		content, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(content), original) || strings.Count(string(content), brainAgentBegin) != 1 {
			t.Fatalf("lost or duplicated text: %s", content)
		}
		info, _ := os.Stat(filepath.Join(repo, name))
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("changed permissions: %v", info.Mode())
		}
	}
	changed, err := installBrainAgentGuide(repo)
	if err != nil || len(changed) != 0 {
		t.Fatalf("second install changed files: %v / %v", changed, err)
	}
}

func TestInitAgentsPreflightPreservesAllFiles(t *testing.T) {
	for _, invalid := range []string{
		brainAgentBegin, brainAgentEnd,
		brainAgentEnd + "\n" + brainAgentBegin,
		brainAgentBegin + "\n" + brainAgentBegin + "\n" + brainAgentEnd,
		"inline " + brainAgentBegin + "\n" + brainAgentEnd,
	} {
		t.Run(invalid, func(t *testing.T) {
			repo := t.TempDir()
			agents := filepath.Join(repo, "AGENTS.md")
			claude := filepath.Join(repo, "CLAUDE.md")
			os.WriteFile(agents, []byte("keep agents\n"), 0644)
			os.WriteFile(claude, []byte(invalid), 0644)
			if _, err := installBrainAgentGuide(repo); err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("accepted malformed markers: %v", err)
			}
			got, _ := os.ReadFile(agents)
			if string(got) != "keep agents\n" {
				t.Fatal("changed AGENTS.md before validation")
			}
			if _, err := os.Stat(filepath.Join(repo, ".entire")); !os.IsNotExist(err) {
				t.Fatal("created .entire before validation")
			}
		})
	}
}

func TestInitAgentsUpdatesOnlyManagedBlock(t *testing.T) {
	before, after := "user prefix\r\n", "user suffix\r\n"
	source := before + brainAgentBegin + "\r\nstale\r\n" + brainAgentEnd + "\r\n" + after
	pointer := brainAgentBegin + "\nnew guide\n" + brainAgentEnd + "\n"
	got, err := upsertBrainAgentPointer(source, pointer)
	if err != nil || got != before+pointer+after {
		t.Fatalf("upsert = %q, %v", got, err)
	}
}

func TestInitAgentsContainedAliasesAndEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges")
	}
	t.Run("shared instructions", func(t *testing.T) {
		repo := t.TempDir()
		os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("shared instructions\n"), 0644)
		if err := os.Symlink("AGENTS.md", filepath.Join(repo, "CLAUDE.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := installBrainAgentGuide(repo); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Lstat(filepath.Join(repo, "CLAUDE.md"))
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("replaced instruction alias")
		}
		changed, err := installBrainAgentGuide(repo)
		if err != nil || len(changed) != 0 {
			t.Fatalf("not idempotent: %v / %v", changed, err)
		}
	})
	for _, target := range []string{"AGENTS.md", "CLAUDE.md", ".entire", brainAgentGuidePath} {
		t.Run(target, func(t *testing.T) {
			repo, outside := t.TempDir(), t.TempDir()
			external := filepath.Join(outside, "external.md")
			os.WriteFile(external, []byte("outside"), 0644)
			dest := external
			if target == ".entire" {
				dest = outside
			}
			if target == brainAgentGuidePath {
				os.Mkdir(filepath.Join(repo, ".entire"), 0755)
			}
			if err := os.Symlink(dest, filepath.Join(repo, target)); err != nil {
				t.Fatal(err)
			}
			if _, err := installBrainAgentGuide(repo); err == nil {
				t.Fatal("accepted escaping link")
			}
			content, _ := os.ReadFile(external)
			if string(content) != "outside" {
				t.Fatal("modified outside file")
			}
			if target != "AGENTS.md" {
				if _, err := os.Lstat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(err) {
					t.Fatal("partial install")
				}
			}
		})
	}
	t.Run("guide aliases instructions", func(t *testing.T) {
		repo := t.TempDir()
		os.Mkdir(filepath.Join(repo, ".entire"), 0755)
		os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("user rules"), 0644)
		os.Symlink("../AGENTS.md", filepath.Join(repo, brainAgentGuidePath))
		if _, err := installBrainAgentGuide(repo); err == nil {
			t.Fatal("accepted guide/instruction collision")
		}
		content, _ := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
		if string(content) != "user rules" {
			t.Fatal("overwrote user rules")
		}
	})
	t.Run("external hardlink", func(t *testing.T) {
		repo, outside := t.TempDir(), filepath.Join(t.TempDir(), "rules")
		os.WriteFile(outside, []byte("keep"), 0644)
		if err := os.Link(outside, filepath.Join(repo, "AGENTS.md")); err != nil {
			t.Skip(err)
		}
		if _, err := installBrainAgentGuide(repo); err != nil {
			t.Fatal(err)
		}
		content, _ := os.ReadFile(outside)
		if string(content) != "keep" {
			t.Fatal("wrote through external hardlink")
		}
	})
}

func TestInitAgentsRejectsDirectoryAndOversizedFiles(t *testing.T) {
	for _, directory := range []bool{false, true} {
		repo := t.TempDir()
		name := filepath.Join(repo, "CLAUDE.md")
		if directory {
			os.Mkdir(name, 0755)
		} else {
			f, err := os.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(brainInstructionLimit + 1); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		if _, err := installBrainAgentGuide(repo); err == nil {
			t.Fatal("accepted invalid file")
		}
		if _, err := os.Stat(filepath.Join(repo, ".entire")); !os.IsNotExist(err) {
			t.Fatal("partial installation")
		}
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
