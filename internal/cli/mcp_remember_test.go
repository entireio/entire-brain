package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// The MCP surface could read memory and never write it. An agent that worked
// something out mid-session had no way to keep it: `entire brain remember`
// exists, but it is a CLI command, and an agent speaking MCP cannot reach it.
// Every comparable memory server ships a write path.
//
// These tests hold the write path open, and hold it to the two properties that
// make it safe to expose: it must not spend model tokens the caller did not ask
// for, and what it writes must be readable back in the same session.

func rememberTestOptions(t *testing.T, repoDir string) Options {
	t.Helper()
	return Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: seedFixtureRunner(repoDir),
		Now:    func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) },
	}
}

func TestMCPExposesAWriteForDurableFacts(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := rememberTestOptions(t, repoDir)

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	listed, _ := json.Marshal(readMCPResponses(t, out.String())[0]["result"])
	if !strings.Contains(string(listed), "brain_remember") {
		t.Fatalf("tools/list has no way to record a fact; an agent cannot persist what it learns: %s", listed)
	}
}

func TestMCPRememberWritesAFactTheSameSessionCanReadBack(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := rememberTestOptions(t, repoDir)

	input := frameMCPJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "brain_remember", "arguments": map[string]any{
			"fact": "The upload endpoint must reject payloads over 25 MB.",
			"path": "constraints.upload.limits",
		}},
	})
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if _, isErr := responses[0]["error"]; isErr {
		body, _ := json.Marshal(responses[0])
		t.Fatalf("brain_remember refused a well-formed call: %s", body)
	}
	written, _ := json.Marshal(mcpTextJSONPayload(t, responses[0]))
	for _, want := range []string{"25 MB", "constraints.upload.limits", `"id"`} {
		if !strings.Contains(string(written), want) {
			t.Fatalf("brain_remember result missing %q: %s", want, written)
		}
	}
}

func TestMCPRememberRefusesRatherThanSpendingTokensToClassify(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := rememberTestOptions(t, repoDir)

	// The CLI classifies a pathless fact by shelling out to a coding agent.
	// Reaching that from an MCP call would spend model tokens the caller never
	// asked for, inside a tool whose value is being deterministic. Omitting
	// path must be refused, and the refusal must say what to pass instead.
	input := frameMCPJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "brain_remember", "arguments": map[string]any{
			"fact": "Something durable with no taxonomy path.",
		}},
	})
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	body, _ := json.Marshal(responses[0])
	if !strings.Contains(string(body), "path") {
		t.Fatalf("a pathless brain_remember should be refused by naming path: %s", body)
	}

	runner, _ := opts.Runner.(*fakeCommandRunner)
	if runner != nil {
		for _, call := range runner.calls {
			switch call.name {
			case "codex", "claude", "claude-code", "ollama":
				t.Fatalf("brain_remember spawned %q to classify; an MCP call must not spend model tokens: %+v", call.name, runner.calls)
			}
		}
	}
}

func TestMCPRememberDeclaresItWritesAndNamesTheTaxonomyShape(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := rememberTestOptions(t, repoDir)

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	var result struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	raw, _ := json.Marshal(readMCPResponses(t, out.String())[0]["result"])
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name != "brain_remember" {
			continue
		}
		// An agent picks a tool off its description. This one has to say that
		// it writes, and has to say the shape of path, or the first call is a
		// guess that gets refused.
		for _, want := range []string{"category.subcategory.type", "branch"} {
			if !strings.Contains(tool.Description, want) {
				t.Fatalf("brain_remember description omits %q: %s", want, tool.Description)
			}
		}
		required, _ := json.Marshal(tool.InputSchema["required"])
		for _, want := range []string{"fact", "path"} {
			if !strings.Contains(string(required), want) {
				t.Fatalf("brain_remember should require %q: %s", want, required)
			}
		}
		return
	}
	t.Fatal("brain_remember is not in tools/list")
}

// A tool description is how an agent chooses its next call, so a description
// that names a sibling tool is a promise the surface has to keep. brain_remember
// shipped for a moment pointing at "brain_recall", which is a CLI command and
// has never been an MCP tool: an agent following it would have burned a call
// discovering that.
func TestMCPToolDescriptionsOnlyNameToolsThatExist(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := rememberTestOptions(t, repoDir)

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	var result struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"tools"`
	}
	raw, _ := json.Marshal(readMCPResponses(t, out.String())[0]["result"])
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range result.Tools {
		registered[tool.Name] = true
	}
	mentioned := regexp.MustCompile(`\bbrain_[a-z_]+\b`)
	for _, tool := range result.Tools {
		for _, named := range mentioned.FindAllString(tool.Description, -1) {
			// brain_cgo is a build tag, not a tool, and tool text refers to it.
			if named == "brain_cgo" || registered[named] {
				continue
			}
			t.Fatalf("%s's description names %q, which is not a registered tool", tool.Name, named)
		}
	}
}
