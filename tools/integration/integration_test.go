package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This adapter only routes the host CLI's graph verb to the real Graph binary.
// It does not fabricate Graph responses or consult the user's plugin installation.
func TestMain(m *testing.M) {
	if os.Getenv("BRAIN_INTEGRATION_HOST_MODE") == "1" {
		if len(os.Args) < 2 || os.Args[1] != "graph" {
			fmt.Fprintln(os.Stderr, "integration host only supports graph")
			os.Exit(97)
		}
		cmd := exec.Command(os.Getenv("BRAIN_INTEGRATION_GRAPH_BINARY"), os.Args[2:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if code, ok := err.(*exec.ExitError); ok {
				os.Exit(code.ExitCode())
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(98)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type productFixture struct {
	brain, graph, repo string
	env                []string
}

func newProductFixture(t *testing.T) productFixture {
	t.Helper()
	brain, graph := os.Getenv("BRAIN_INTEGRATION_BRAIN_BINARY"), os.Getenv("BRAIN_INTEGRATION_GRAPH_BINARY")
	if brain == "" || graph == "" {
		if os.Getenv("BRAIN_INTEGRATION_REQUIRED") == "1" {
			t.Fatal("both real product binaries are required")
		}
		t.Skip("set BRAIN_INTEGRATION_{BRAIN,GRAPH}_BINARY to run real product integration")
	}
	for _, path := range []string{brain, graph} {
		if !filepath.IsAbs(path) {
			t.Fatalf("binary must have an absolute path: %s", path)
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("binary unavailable: %s (%v)", path, err)
		}
	}
	root := t.TempDir()
	f := productFixture{brain: brain, graph: graph, repo: filepath.Join(root, "repo")}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ENTIRE_") || strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "XDG_") || strings.HasPrefix(key, "BRAIN_INTEGRATION_") || key == "HOME" || key == "USERPROFILE" || key == "APPDATA" || key == "LOCALAPPDATA" || key == "PATH" {
			continue
		}
		f.env = append(f.env, entry)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		f.env = append(f.env, key+"="+filepath.Join(root, key))
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	host := "entire"
	if strings.HasSuffix(self, ".exe") {
		host += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, host), data, 0700); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BRAIN_INTEGRATION_HOST_MODE=1", "BRAIN_INTEGRATION_GRAPH_BINARY="+graph,
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	if err := os.Mkdir(f.repo, 0700); err != nil {
		t.Fatal(err)
	}
	f.run(t, "git", "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(f.repo, "go.mod"), []byte("module example.com/widget\n\ngo 1.27\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "widget.go"), []byte("package widget\n\n// RenderWidget renders the cobalt widget.\nfunc RenderWidget() string { return \"cobalt\" }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.run(t, "git", "add", ".")
	f.run(t, "git", "commit", "-qm", "add widget")
	return f
}

func (f productFixture) run(t *testing.T, binary string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = f.repo, f.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s\n%s", filepath.Base(binary), args, err, stdout.Bytes(), stderr.Bytes())
	}
	return stdout.Bytes()
}

func TestRealGraphIndexAndBrainInspection(t *testing.T) {
	f := newProductFixture(t)
	f.run(t, f.brain, "refresh", "index", f.repo)
	data := f.run(t, f.brain, "inspect", "code", "RenderWidget", "--json")
	if !json.Valid(data) || !bytes.Contains(data, []byte("RenderWidget")) || !bytes.Contains(data, []byte("widget.go")) {
		t.Fatalf("Brain did not retrieve the real Graph symbol: %s", data)
	}
	f.run(t, f.brain, "remember", "Use the cobalt sentinel when rendering widgets.", "--path", "project.widget.rendering", "--json")
	data = f.run(t, f.brain, "recall", "cobalt sentinel", "--json")
	if !json.Valid(data) || !bytes.Contains(data, []byte("Use the cobalt sentinel")) {
		t.Fatalf("recorded memory was not retrievable: %s", data)
	}
	data = f.run(t, f.brain, "brief", "change RenderWidget cobalt rendering", "--json")
	if !json.Valid(data) || !bytes.Contains(data, []byte("widget.go")) {
		t.Fatalf("Brain brief lost Graph code context: %s", data)
	}
}

func TestRealProductsPreserveSharedAgentInstructions(t *testing.T) {
	for _, order := range []string{"brain-first", "graph-first"} {
		t.Run(order, func(t *testing.T) {
			f := newProductFixture(t)
			if err := os.WriteFile(filepath.Join(f.repo, "AGENTS.md"), []byte("User instructions remain.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			setup := func(product string) {
				if product == "brain" {
					f.run(t, f.brain, "setup", "--no-backfill", "--no-daemon", "--no-agent-hooks", "--agent", "none", f.repo)
				} else {
					f.run(t, f.graph, "setup", "--no-claude-hook", "--repo", f.repo)
				}
			}
			products := []string{"brain", "graph"}
			if order == "graph-first" {
				products[0], products[1] = products[1], products[0]
			}
			for _, p := range products {
				setup(p)
			}
			before := map[string][]byte{}
			for _, name := range []string{"AGENTS.md", "CLAUDE.md", ".entire/agent-guide.md"} {
				data, err := os.ReadFile(filepath.Join(f.repo, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = data
			}
			if !bytes.Contains(before["AGENTS.md"], []byte("User instructions remain.")) || bytes.Count(before["AGENTS.md"], []byte("<!-- entire-agent:begin -->")) != 1 {
				t.Fatal("setup changed user text or duplicated the managed block")
			}
			for _, mode := range []string{"--normal", "--strict"} {
				brainGuide := f.run(t, f.brain, "agent-guide", mode, "--repo", f.repo)
				graphGuide := f.run(t, f.graph, "agent-guide", mode, "--repo", f.repo)
				if !bytes.Contains(brainGuide, []byte("Graph and Brain")) || !bytes.Contains(graphGuide, []byte("Graph and Brain")) {
					t.Fatal("a product lost combined activation")
				}
				if err := compareGuideText(brainGuide, graphGuide, os.Getenv("BRAIN_INTEGRATION_REQUIRE_GUIDE_PARITY") == "1"); err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
			}
			for _, p := range products {
				setup(p)
			}
			for name, want := range before {
				got, err := os.ReadFile(filepath.Join(f.repo, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("repeated setup changed %s: %v", name, err)
				}
			}
		})
	}
}

// The pinned Graph release still includes two obsolete MCP sentences. Accept
// only these exact legacy differences until its guide correction is released.
// Strict parity can be required when validating the corrected Graph revision.
func compareGuideText(brain, graph []byte, requireParity bool) error {
	if bytes.Equal(brain, graph) {
		return nil
	}
	if requireParity {
		return fmt.Errorf("Brain and Graph must render identical guide text")
	}
	legacy := strings.NewReplacer(
		"agent on PATH. Retrieve with recall, and re-check anchors with verify.",
		"agent on PATH. Over MCP the same write is the brain_remember tool, which\nrequires path. Retrieve with recall, and re-check anchors with verify.",
		"agent on PATH; pass --path when none is guaranteed. Retrieve with recall and",
		"agent on PATH; pass --path when none is guaranteed. Over MCP the same write is\nthe brain_remember tool, which requires path. Retrieve with recall and",
	).Replace(string(brain))
	if string(graph) != legacy {
		return fmt.Errorf("Brain and Graph guide text differs beyond the pinned legacy MCP wording")
	}
	return nil
}

func TestGuideComparisonRejectsUnrelatedDrift(t *testing.T) {
	brain := []byte("agent on PATH. Retrieve with recall, and re-check anchors with verify.")
	legacy := []byte("agent on PATH. Over MCP the same write is the brain_remember tool, which\nrequires path. Retrieve with recall, and re-check anchors with verify.")
	if err := compareGuideText(brain, brain, true); err != nil {
		t.Fatal(err)
	}
	if err := compareGuideText(brain, legacy, false); err != nil {
		t.Fatal(err)
	}
	if err := compareGuideText(brain, legacy, true); err == nil {
		t.Fatal("strict parity accepted obsolete wording")
	}
	if err := compareGuideText(brain, append(legacy, []byte("\nSkip required queries.")...), false); err == nil {
		t.Fatal("accepted unrelated guide drift")
	}
}
