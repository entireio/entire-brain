package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	if !strings.Contains(string(data), "brain_query") || !strings.Contains(string(data), "brain_impact") || !strings.Contains(string(data), "brain_history") {
		t.Fatalf("tools/list missing tools: %s", data)
	}
}

func TestMCPToolsListIncludesRegressions(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses[1]["result"])
	for _, want := range []string{"brain_regressions", "include_deletions"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("tools/list missing %q: %s", want, data)
		}
	}
}

func TestMCPBrainRegressionsTool(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths(cmd.Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	// History asserts the invariant and names the file; the current tree has the regressed literal.
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	sessionLine := `{"type":"agent_message","message":"in pkg/review_context.go the scope range is scopeBaseRef+\"..HEAD\""}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(sessionLine), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "review_context.go"),
		[]byte("package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"), 0o600); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"fix scopeBaseRef base scope review","limit":5}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	data, _ := json.Marshal(readMCPResponses(t, out.String()))
	for _, want := range []string{"review_context.go", "changed", "master..HEAD"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("brain_regressions result missing %q: %s", want, data)
		}
	}
}

func TestMCPBrainReviewTool(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths(cmd.Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	sessionLine := `{"type":"agent_message","message":"in pkg/review_context.go the scope range is scopeBaseRef+\"..HEAD\""}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(sessionLine), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "review_context.go"),
		[]byte("package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"), 0o600); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_review","arguments":{"query":"review scopeBaseRef base scope"}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	listData, _ := json.Marshal(responses[0]["result"])
	if !strings.Contains(string(listData), "brain_review") {
		t.Fatalf("tools/list missing brain_review: %s", listData)
	}
	callData, _ := json.Marshal(responses[1])
	for _, want := range []string{"diff-less", "Suspected regression", "review_context.go"} {
		if !strings.Contains(string(callData), want) {
			t.Fatalf("brain_review result missing %q: %s", want, callData)
		}
	}
}

func TestMCPInitializeEchoesClientProtocolVersion(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	result := responses[0]["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
}

func TestMCPInitializeSupportsJSONLineFraming(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	if strings.HasPrefix(out.String(), "Content-Length:") {
		t.Fatalf("json-line input should produce json-line output, got %q", out.String())
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("responses = %d", len(responses))
	}
	result := responses[0]["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
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

func TestMCPBrainBriefAndHistoryToolsUseIndexedHistory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths(cmd.Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	sessionLine := `{"type":"agent_message","message":"Decision: keep media playback verification for YouTube song goals; reaching YouTube alone is not success."}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(sessionLine), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_brief","arguments":{"task":"restore YouTube media playback verification","limit":5}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_history","arguments":{"kind":"decisions","query":"media playback verification"}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	data, _ := json.Marshal(responses)
	for _, want := range []string{"brain", "media playback verification", "reaching YouTube alone is not success"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("history wrapper results missing %q: %s", want, data)
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

func TestMCPRecoversFromMalformedJSONLineFrame(t *testing.T) {
	// A malformed json-line frame must NOT terminate the session: it should yield a
	// JSON-RPC parse error (-32700) and the next valid frame must still be served.
	input := `{"jsonrpc":"2.0","id":1,"method":` + "\n" + // truncated => invalid JSON
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n"
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("session should survive a malformed frame, got err: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 2 {
		t.Fatalf("expected parse-error + initialize responses, got %d: %q", len(responses), out.String())
	}
	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != -32700 {
		t.Fatalf("first response should be parse error -32700, got %v", responses[0])
	}
	if responses[1]["result"] == nil {
		t.Fatalf("second response should be a valid initialize result, got %v", responses[1])
	}
}

func TestMCPRejectsOversizedAndNegativeFrames(t *testing.T) {
	if _, _, err := readMCPMessage(bufio.NewReader(strings.NewReader(fmt.Sprintf("Content-Length: %d\r\n\r\n", maxMCPFrameBytes+1)))); err == nil || !strings.Contains(err.Error(), "exceeds maximum") {
		t.Fatalf("oversized frame err = %v", err)
	}
	if _, _, err := readMCPMessage(bufio.NewReader(strings.NewReader("Content-Length: -1\r\n\r\n"))); err == nil || !strings.Contains(err.Error(), "non-negative") {
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
		msg, _, err := readMCPMessage(reader)
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
