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
	if !strings.Contains(string(data), "brain_query") || !strings.Contains(string(data), "brain_impact") || !strings.Contains(string(data), "brain_code") {
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

func TestMCPToolsListIncludesQMDRetrievalSurface(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	result, ok := responses[1]["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list result has unexpected shape: %+v", responses[1])
	}
	rawTools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list missing tools array: %+v", result)
	}
	names := map[string]bool{}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tool has unexpected shape: %+v", raw)
		}
		name, ok := tool["name"].(string)
		if !ok {
			t.Fatalf("tool missing string name: %+v", tool)
		}
		names[name] = true
	}
	for _, want := range []string{"brain_search", "brain_vsearch", "brain_query", "brain_get", "brain_multi_get"} {
		if !names[want] {
			t.Fatalf("tools/list missing QMD retrieval tool %q; got %v", want, names)
		}
	}
}

func TestMCPToolsListAdvertisesStaleBlindSpots(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses[0]["result"])
	for _, want := range []string{"brain_stale", "blind_spots", "semantic blind spots"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("tools/list missing %q: %s", want, data)
		}
	}
}

func TestMCPToolSchemasRejectAdditionalProperties(t *testing.T) {
	for _, tool := range mcpToolDefinitions() {
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing inputSchema", tool["name"])
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s schema does not close additionalProperties: %+v", tool["name"], schema)
		}
	}
}

func TestMCPRejectsInvalidBooleanArguments(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"scope regression","location_only":"true"}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_stale","arguments":{"blind_spots":"yes"}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	for i, want := range []string{"location_only must be boolean", "blind_spots must be boolean"} {
		errObj, ok := responses[i]["error"].(map[string]any)
		if !ok {
			t.Fatalf("response %d missing error: %+v", i, responses[i])
		}
		if !strings.Contains(fmt.Sprint(errObj["message"]), want) {
			t.Fatalf("response %d error = %+v, want %q", i, errObj, want)
		}
	}
}

func TestMCPRejectsInvalidStringAndUnknownArguments(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_query","arguments":{"query":7}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_workspace_review","arguments":{"workspace":7,"query":"x"}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_boundaries","arguments":{"kind":7}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"brain_multi_get","arguments":{"ids":["fact:x",7]}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"x","locationOnly":true}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	wants := []string{
		"query must be string",
		"workspace must be string",
		"kind must be string",
		"ids must be an array of strings",
		"unknown argument for brain_regressions: locationOnly",
	}
	if len(responses) != len(wants) {
		t.Fatalf("responses = %d, want %d", len(responses), len(wants))
	}
	for i, want := range wants {
		errObj, ok := responses[i]["error"].(map[string]any)
		if !ok {
			t.Fatalf("response %d missing error: %+v", i, responses[i])
		}
		if !strings.Contains(fmt.Sprint(errObj["message"]), want) {
			t.Fatalf("response %d error = %+v, want %q", i, errObj, want)
		}
	}
}

func TestMCPDebugLogIncludesToolCallNameAndSafeBooleanArgsOnly(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("ENTIRE_BRAIN_MCP_DEBUG_LOG", logPath)
	input := frameMCP(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"secret-query-value","location_only":true,"include_deletions":true}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	text := string(data)
	for _, want := range []string{"message: tools/call", "tool: brain_regressions", `tool_args: {"include_deletions":true,"location_only":true}`} {
		if !strings.Contains(text, want) {
			t.Fatalf("debug log missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "secret-query-value") {
		t.Fatalf("debug log leaked tool arguments: %s", text)
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

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"fix scopeBaseRef base scope review","limit":5}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"fix scopeBaseRef base scope review","limit":5,"location_only":true}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses)
	for _, want := range []string{"review_context.go", "changed", "master..HEAD"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("brain_regressions result missing %q: %s", want, data)
		}
	}
	locData, _ := json.Marshal(responses[1])
	if !strings.Contains(string(locData), "review_context.go") {
		t.Fatalf("brain_regressions location_only dropped file location: %s", locData)
	}
	if strings.Contains(string(locData), "master..HEAD") || strings.Contains(string(locData), `scopeBaseRef+`) {
		t.Fatalf("brain_regressions location_only leaked expected/current values: %s", locData)
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
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_review","arguments":{"query":"review scopeBaseRef base scope"}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_review","arguments":{"query":"review scopeBaseRef base scope","location_only":true}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	listData, _ := json.Marshal(responses[0]["result"])
	if !strings.Contains(string(listData), "brain_review") || !strings.Contains(string(listData), "location_only") {
		t.Fatalf("tools/list missing brain_review/location_only: %s", listData)
	}
	callData, _ := json.Marshal(responses[1])
	// schema_version is the load-bearing contract field cross-repo consumers bind to; a silent rename
	// (e.g. to schemaVersion) or a dropped field must fail here, not pass CI green.
	for _, want := range []string{"diff-less", "Suspected regression", "review_context.go", "master..HEAD", "schema_version"} {
		if !strings.Contains(string(callData), want) {
			t.Fatalf("brain_review result missing %q: %s", want, callData)
		}
	}
	// location_only must still localize the finding but NOT leak the expected/current values.
	locData, _ := json.Marshal(responses[2])
	if !strings.Contains(string(locData), "review_context.go") {
		t.Fatalf("brain_review --location-only dropped the file location: %s", locData)
	}
	if strings.Contains(string(locData), "master..HEAD") || strings.Contains(string(locData), `scopeBaseRef+`) {
		t.Fatalf("brain_review location_only leaked expected/current values: %s", locData)
	}
}

func TestMCPBrainWorkspaceReviewTool(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}

	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	regressed := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	repoA := writeWorkspaceBrainRepo(t, env, "gh/example/repoa", session, "pkg/review_context.go", regressed)
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: "gh/example/repoa", Name: "a", LocalPathHint: repoA}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_workspace_review","arguments":{"workspace":"related","query":"review scopeBaseRef base scope"}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_workspace_regressions","arguments":{"workspace":"related","query":"fix scopeBaseRef base scope","location_only":true}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	listData, _ := json.Marshal(responses[0]["result"])
	for _, want := range []string{"brain_workspace_review", "brain_workspace_regressions", "location_only"} {
		if !strings.Contains(string(listData), want) {
			t.Fatalf("tools/list missing %q: %s", want, listData)
		}
	}
	callData, _ := json.Marshal(responses[1])
	for _, want := range []string{"diff-less", "Suspected regression", "review_context.go", "gh/example/repoa"} {
		if !strings.Contains(string(callData), want) {
			t.Fatalf("brain_workspace_review result missing %q: %s", want, callData)
		}
	}
	regData, _ := json.Marshal(responses[2])
	for _, want := range []string{"review_context.go", "gh/example/repoa", "anomalies"} {
		if !strings.Contains(string(regData), want) {
			t.Fatalf("brain_workspace_regressions result missing %q: %s", want, regData)
		}
	}
	if strings.Contains(string(regData), "master..HEAD") || strings.Contains(string(regData), `scopeBaseRef+`) {
		t.Fatalf("brain_workspace_regressions location_only leaked expected/current values: %s", regData)
	}
}

func TestMCPBrainWorkspaceToolRequiresWorkspace(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	opts := Options{Version: "test-version", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now}
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_workspace_review","arguments":{"query":"x"}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	if !strings.Contains(out.String(), "workspace is required") {
		t.Fatalf("expected workspace-required error: %s", out.String())
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

func TestMCPQMDRetrievalToolsUseLocalFacts(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	facts := []factRecord{
		{ID: factRecordID("qmd retrieval alpha contract", paths), Text: "qmd retrieval alpha contract", Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
		{ID: factRecordID("qmd retrieval beta contract", paths), Text: "qmd retrieval beta contract", Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
	}
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_search","arguments":{"query":"qmd retrieval alpha","limit":1}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_vsearch","arguments":{"query":"qmd retrieval alpha","limit":1}}}`) +
		frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_get","arguments":{"id":%q}}}`, facts[0].ID)) +
		frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"brain_multi_get","arguments":{"ids":[%q,"fact:missing"]}}}`, facts[1].ID))
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 4 {
		t.Fatalf("responses = %d", len(responses))
	}
	data, _ := json.Marshal(responses)
	for _, want := range []string{"qmd retrieval alpha contract", "qmd retrieval beta contract", "fact:missing"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("QMD MCP retrieval results missing %q: %s", want, data)
		}
	}
	for i, response := range responses {
		if response["error"] != nil {
			t.Fatalf("response %d returned error: %+v", i+1, response)
		}
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

func TestMCPBrainBriefAndQueryToolsUseIndexedHistory(t *testing.T) {
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
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_query","arguments":{"query":"media playback verification"}}}`)
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
