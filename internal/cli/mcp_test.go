package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestMCPInitializeAndToolsList(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	if responses[0]["error"] != nil {
		t.Fatalf("initialize error: %+v", responses[0])
	}
	data, _ := json.Marshal(responses[1]["result"])
	if !strings.Contains(string(data), "brain_query") || !strings.Contains(string(data), "brain_impact") {
		t.Fatalf("tools/list missing tools: %s", data)
	}
}

func TestMCPBrainQueryToolUsesLocalSemanticJSON(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_query","arguments":{"query":"ValidateToken","limit":5}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("responses = %d", len(responses))
	}
	data, _ := json.Marshal(responses[0]["result"])
	if !strings.Contains(string(data), "ValidateToken") || strings.Contains(string(data), "http://") {
		t.Fatalf("query result = %s", data)
	}
}

func TestMCPBrainContextImpactAndChangesToolsUseLocalSemanticJSON(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_context","arguments":{"query":"ValidateToken","limit":5}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_impact","arguments":{"query":"ValidateToken","depth":1,"limit":5}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_changes","arguments":{"limit":5}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 3 {
		t.Fatalf("responses = %d", len(responses))
	}
	for _, response := range responses {
		if response["error"] != nil {
			t.Fatalf("tool error: %+v", response)
		}
	}
	data, _ := json.Marshal(responses)
	for _, want := range []string{"ValidateToken", "relations", "internal/auth/token.go"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("wrapper results missing %q: %s", want, data)
		}
	}
}

func TestMCPToolCallRejectsInvalidIntegerArguments(t *testing.T) {
	for _, input := range []string{
		`{"limit":0}`,
		`{"limit":-1}`,
		`{"limit":"bad"}`,
		`{"limit":1.9}`,
	} {
		t.Run(input, func(t *testing.T) {
			payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_query","arguments":{"query":"ValidateToken",%s}}}`, strings.TrimPrefix(strings.TrimSuffix(input, "}"), "{"))
			var out bytes.Buffer
			err := runMCP((&cobra.Command{}).Context(), strings.NewReader(frameMCP(payload)), &out, Options{Version: "test-version"})
			if err != nil {
				t.Fatalf("mcp: %v", err)
			}
			responses := readMCPResponses(t, out.String())
			data, _ := json.Marshal(responses[0]["error"])
			if !strings.Contains(string(data), "limit must be an integer greater than zero") {
				t.Fatalf("error = %s", data)
			}
		})
	}
}

func TestMCPToolCallRejectsInvalidDepth(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_impact","arguments":{"query":"ValidateToken","depth":0}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses[0]["error"])
	if !strings.Contains(string(data), "depth must be an integer greater than zero") {
		t.Fatalf("error = %s", data)
	}
}

func TestMCPRejectsOversizedAndNegativeFrames(t *testing.T) {
	if _, err := readMCPMessage(bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n", maxMCPFrameBytes+1)))); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("oversized frame err = %v", err)
	}
	if _, err := readMCPMessage(bufio.NewReader(strings.NewReader("Content-Length: -1\r\n\r\n"))); err == nil || !strings.Contains(err.Error(), "non-negative") {
		t.Fatalf("negative frame err = %v", err)
	}
}

func TestMCPBrainStaleUsesEnvRepoRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	otherDir := t.TempDir()
	if err := os.Chdir(otherDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_stale","arguments":{}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses[0]["result"])
	if !strings.Contains(string(data), `"severity\": \"ok\"`) {
		t.Fatalf("stale result = %s", data)
	}
}

func frameMCP(payload string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(payload), payload)
}

func readMCPResponses(t *testing.T, data string) []map[string]any {
	t.Helper()
	reader := bufio.NewReader(strings.NewReader(data))
	var responses []map[string]any
	for {
		msg, err := readMCPMessage(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		raw, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		responses = append(responses, decoded)
	}
	return responses
}
