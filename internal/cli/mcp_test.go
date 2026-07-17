package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

func TestMCPBrainReviewToolDefinitionGolden(t *testing.T) {
	var review map[string]any
	for _, definition := range mcpToolDefinitions() {
		if definition["name"] == "brain_review" {
			review = definition
			break
		}
	}
	if review == nil {
		t.Fatal("brain_review tool definition missing")
	}
	got, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal brain_review tool definition: %v", err)
	}
	want, err := os.ReadFile("testdata/mcp_brain_review_tool.golden.json")
	if err != nil {
		t.Fatalf("read brain_review tool definition golden: %v", err)
	}
	if !bytes.Equal(append(got, '\n'), want) {
		t.Fatalf("brain_review tool definition changed\n got: %s\nwant: %s", got, want)
	}

	// Exact local o200k_base evidence: this definition is 1,026 -> 810 bytes
	// and 223 -> 166 tokens; the full tools/list result is 16,080 -> 15,864
	// bytes and 3,411 -> 3,354 tokens. The pinned tokenizer asset (SHA-256
	// 446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d)
	// is deliberately not a production or test dependency.
	if len(got) != 810 {
		t.Fatalf("brain_review tool definition bytes = %d, want 810", len(got))
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

func TestMCPProjectManagementTools(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	repoKey := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                                                {stdout: repoDir + "\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                                                           {stdout: "aaa111\n"},
		fakeCommandKey("git", "rev-parse", "HEAD^{tree}"):                                                    {stdout: "tree111\n"},
		fakeCommandKey("git", "branch", "--show-current"):                                                    {stdout: "main\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"):              {stdout: "origin/main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                                                       {stdout: ""},
		fakeCommandKey("entire", "sem", "doctor", "--json"):                                                  {stdout: `{"no_egress":true}`},
		fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"): {stdout: workspaceGraphSnapshot(repoKey, "HandleMCP")},
	}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC) }}
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`) +
		frameMCPJSON(t, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "brain_index_repository", "arguments": map[string]any{"path": repoDir}}}) +
		frameMCP(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_list_projects","arguments":{}}}`) +
		frameMCPJSON(t, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "brain_delete_project", "arguments": map[string]any{"repo_key": repoKey}}})
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	listData, _ := json.Marshal(responses[0]["result"])
	for _, want := range []string{"brain_index_repository", "brain_list_projects", "brain_delete_project"} {
		if !strings.Contains(string(listData), want) {
			t.Fatalf("tools/list missing %q: %s", want, listData)
		}
	}
	projectPayload := mcpTextJSONPayload(t, responses[2])
	projectData, _ := json.Marshal(projectPayload)
	if !strings.Contains(string(projectData), repoKey) || !strings.Contains(string(projectData), `"semantic":true`) {
		t.Fatalf("brain_list_projects missing indexed project: %s", projectData)
	}
	brainDir, err := brainDirForKey(env, repoKey)
	if err != nil {
		t.Fatalf("brain dir: %v", err)
	}
	if _, err := os.Stat(brainDir); !os.IsNotExist(err) {
		t.Fatalf("brain_delete_project did not remove %s: %v", brainDir, err)
	}
}

func TestMCPWorkspaceGraphReturnsCrossEdges(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := filepath.ToSlash(filepath.Join("local", localRepoKey(repoA)))
	indexWorkspaceGraphRepo(t, cmd, opts, runner, repoA, keyA, "HandleMCPA")
	repoB := t.TempDir()
	keyB := filepath.ToSlash(filepath.Join("local", localRepoKey(repoB)))
	indexWorkspaceGraphRepo(t, cmd, opts, runner, repoB, keyB, "HandleMCPB")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "mcpgraph",
		Repos: []workspaceRepo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_workspace_graph","arguments":{"workspace":"mcpgraph","limit":10}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	payload := mcpTextJSONPayload(t, responses[0])
	data, _ := json.Marshal(payload)
	for _, want := range []string{`"workspace":"mcpgraph"`, `"contracts"`, `"cross_edges"`, `external:config:kubernetes/image/shared:latest`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("brain_workspace_graph payload missing %q: %s", want, data)
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
	for _, want := range []string{"brain_status", "semantic provider/coverage/freshness/blind spots"} {
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

func TestMCPToolSchemasMatchArgumentValidator(t *testing.T) {
	allPublicKeys := map[string]bool{"__unexpected": true}
	toolKeys := map[string]map[string]bool{}
	for _, tool := range mcpToolDefinitions() {
		name, ok := tool["name"].(string)
		if !ok || name == "" {
			t.Fatalf("tool missing name: %+v", tool)
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing inputSchema", name)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing properties: %+v", name, schema)
		}
		keys := map[string]bool{}
		for key := range props {
			keys[key] = true
			allPublicKeys[key] = true
		}
		required, _ := schema["required"].([]string)
		for _, key := range required {
			if !keys[key] {
				t.Fatalf("%s requires non-schema property %q", name, key)
			}
		}
		toolKeys[name] = keys
	}
	for name, keys := range toolKeys {
		schemaArgs := map[string]any{}
		for key := range keys {
			schemaArgs[key] = true
		}
		if err := validateMCPToolArguments(name, schemaArgs); err != nil {
			t.Fatalf("%s validator rejected advertised schema args %v: %v", name, keys, err)
		}
		for key := range allPublicKeys {
			if keys[key] {
				continue
			}
			if err := validateMCPToolArguments(name, map[string]any{key: true}); err == nil {
				t.Fatalf("%s validator accepted non-schema arg %q", name, key)
			}
		}
	}
}

func TestMCPToolRequiredArgumentsAreEnforced(t *testing.T) {
	requiredValue := func(key string) any {
		switch key {
		case "ids":
			return []string{"fact:required-field-probe"}
		case "id":
			return "fact:required-field-probe"
		case "workspace":
			return "required-field-workspace"
		default:
			return "required field probe"
		}
	}

	var input strings.Builder
	var wants []string
	id := 1
	for _, tool := range mcpToolDefinitions() {
		name, ok := tool["name"].(string)
		if !ok || name == "" {
			t.Fatalf("tool missing name: %+v", tool)
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing inputSchema", name)
		}
		required, _ := schema["required"].([]string)
		for _, omitted := range required {
			args := map[string]any{}
			for _, key := range required {
				if key == omitted {
					continue
				}
				args[key] = requiredValue(key)
			}
			msg := map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"method":  "tools/call",
				"params": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}
			data, err := json.Marshal(msg)
			if err != nil {
				t.Fatalf("marshal %s missing %s: %v", name, omitted, err)
			}
			input.WriteString(frameMCP(string(data)))
			wants = append(wants, fmt.Sprintf("%s is required", omitted))
			id++
		}
	}
	if len(wants) == 0 {
		t.Fatal("no required MCP arguments advertised")
	}

	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input.String()), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
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

func TestMCPQMDRetrievalSchemasExposeBranchAndNonEmptyMultiGet(t *testing.T) {
	byName := map[string]map[string]any{}
	for _, tool := range mcpToolDefinitions() {
		name, _ := tool["name"].(string)
		byName[name] = tool
	}
	wantRequired := map[string]string{
		"brain_query":     "query",
		"brain_search":    "query",
		"brain_vsearch":   "query",
		"brain_get":       "id",
		"brain_multi_get": "ids",
	}
	for _, name := range []string{"brain_query", "brain_search", "brain_vsearch", "brain_get", "brain_multi_get"} {
		schema, ok := byName[name]["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing inputSchema", name)
		}
		required, ok := schema["required"].([]string)
		if !ok || len(required) != 1 || required[0] != wantRequired[name] {
			t.Fatalf("%s required = %#v, want [%s]", name, schema["required"], wantRequired[name])
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing properties: %+v", name, schema)
		}
		branch, ok := props["branch"].(map[string]any)
		if !ok || branch["type"] != "string" {
			t.Fatalf("%s missing string branch property: %+v", name, props["branch"])
		}
		if name == "brain_query" || name == "brain_search" || name == "brain_vsearch" {
			limit, ok := props["limit"].(map[string]any)
			if !ok || limit["type"] != "integer" || limit["minimum"] != 1 {
				t.Fatalf("%s missing positive integer limit property: %+v", name, props["limit"])
			}
		}
		if name == "brain_multi_get" {
			ids, ok := props["ids"].(map[string]any)
			if !ok || ids["type"] != "array" || ids["minItems"] != 1 {
				t.Fatalf("%s missing non-empty ids array property: %+v", name, props["ids"])
			}
		}
	}
}

func TestMCPRejectsInvalidBooleanArguments(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"scope regression","location_only":"true"}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_review","arguments":{"query":"x","include_deletions":"yes"}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	for i, want := range []string{"location_only must be boolean", "include_deletions must be boolean"} {
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
		frameMCP(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"x","locationOnly":true}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"brain_get","arguments":{"id":"fact:x","branch":7}}}`)
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
		"branch must be string",
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

func TestMCPDebugLogIncludesToolCallNameAndSafeArgsOnly(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("ENTIRE_BRAIN_MCP_DEBUG_LOG", logPath)
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"secret-query-value","limit":"bad","location_only":true,"include_deletions":true}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	text := string(data)
	for _, want := range []string{"message: tools/call", "response: tools/call", "tool: brain_regressions", `tool_args: {"include_deletions":true,"location_only":true}`, "tool_result: brain_regressions error"} {
		if !strings.Contains(text, want) {
			t.Fatalf("debug log missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "secret-query-value") {
		t.Fatalf("debug log leaked tool arguments: %s", text)
	}
}

func TestMCPDebugLogIncludesSuccessfulWorkspaceRadarResult(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("ENTIRE_BRAIN_MCP_DEBUG_LOG", logPath)

	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
	session := `{"text":"pkg/resolve.go must set state.TranscriptPath = resolved so later reads work"}`
	body := "package x\nfunc resolve(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\n"
	repoA, repoAKey := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/resolve.go", body)
	if err := writeWorkspaceManifest(env, workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: repoAKey, Name: "a", LocalPathHint: repoA}},
	}); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_workspace_regressions","arguments":{"workspace":"related","query":"secret-query-value","include_deletions":true,"location_only":true}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	text := string(data)
	for _, want := range []string{"tool: brain_workspace_regressions", `tool_args: {"include_deletions":true,"location_only":true,"workspace":"related"}`, "tool_result: brain_workspace_regressions ok"} {
		if !strings.Contains(text, want) {
			t.Fatalf("debug log missing %q: %s", want, text)
		}
	}
	for _, forbidden := range []string{"secret-query-value", "state.TranscriptPath = resolved", "_ = state.TranscriptPath"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("debug log leaked %q: %s", forbidden, text)
		}
	}
}

func TestMCPDebugLogDoesNotTreatNotificationsAsExecutedTools(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp.log")
	t.Setenv("ENTIRE_BRAIN_MCP_DEBUG_LOG", logPath)
	input := frameMCP(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"secret-query-value","location_only":true}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "tool: brain_regressions") || strings.Contains(text, "tool_result: brain_regressions") {
		t.Fatalf("notification was logged as an executed tool: %s", text)
	}
	if strings.Contains(text, "secret-query-value") {
		t.Fatalf("debug log leaked tool arguments: %s", text)
	}
}

func TestMCPDebugLogReviewToolsRedactAndLogSuccess(t *testing.T) {
	t.Run("brain_review", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "mcp.log")
		t.Setenv("ENTIRE_BRAIN_MCP_DEBUG_LOG", logPath)

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

		input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_review","arguments":{"query":"secret-review-query","include_deletions":true,"location_only":true}}}`)
		var out bytes.Buffer
		if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
			t.Fatalf("mcp: %v", err)
		}
		assertMCPDebugLogReviewRedacted(t, logPath, "brain_review", `tool_args: {"include_deletions":true,"location_only":true}`, []string{"secret-review-query", "master..HEAD", `scopeBaseRef+`})
	})

	t.Run("brain_workspace_review", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "mcp.log")
		t.Setenv("ENTIRE_BRAIN_MCP_DEBUG_LOG", logPath)

		env := semanticTestEnv(t, t.TempDir())
		runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
		opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
		session := `{"text":"pkg/resolve.go must set state.TranscriptPath = resolved so later reads work"}`
		body := "package x\nfunc resolve(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\n"
		repoA, repoAKey := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/resolve.go", body)
		if err := writeWorkspaceManifest(env, workspaceManifest{
			SchemaVersion: workspaceSchemaVersion,
			Name:          "related",
			Repos:         []workspaceRepo{{RepoKey: repoAKey, Name: "a", LocalPathHint: repoA}},
		}); err != nil {
			t.Fatalf("write workspace: %v", err)
		}

		input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_workspace_review","arguments":{"workspace":"related","query":"secret-workspace-review-query","include_deletions":true,"location_only":true}}}`)
		var out bytes.Buffer
		if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
			t.Fatalf("mcp: %v", err)
		}
		assertMCPDebugLogReviewRedacted(t, logPath, "brain_workspace_review", `tool_args: {"include_deletions":true,"location_only":true,"workspace":"related"}`, []string{"secret-workspace-review-query", "state.TranscriptPath = resolved", "_ = state.TranscriptPath"})
	})
}

func assertMCPDebugLogReviewRedacted(t *testing.T, logPath, tool, expectedArgs string, forbidden []string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	text := string(data)
	for _, want := range []string{"tool: " + tool, expectedArgs, "tool_result: " + tool + " ok"} {
		if !strings.Contains(text, want) {
			t.Fatalf("debug log missing %q: %s", want, text)
		}
	}
	for _, leak := range forbidden {
		if strings.Contains(text, leak) {
			t.Fatalf("debug log leaked %q: %s", leak, text)
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

func TestMCPBrainRegressionsDeletionLocationOnlyKeepsAllAssignmentSites(t *testing.T) {
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
	sessionLine := `{"type":"agent_message","message":"pkg/resolve_transcript.go must set state.TranscriptPath = resolved so later reads work"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(sessionLine), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir pkg: %v", err)
	}
	body := "package x\nfunc ok(state *State) string {\n\tresolved := compute()\n\tstate.TranscriptPath = resolved\n\treturn resolved\n}\nfunc missOne(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\nfunc missTwo(state *State) string {\n\tresolved := compute()\n\treturn resolved\n}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "resolve_transcript.go"), []byte(body), 0o600); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_regressions","arguments":{"query":"fix TranscriptPath resolved","limit":5,"include_deletions":true,"location_only":true}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses[0])
	for _, want := range []string{"resolve_transcript.go", "missOne", "missTwo"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("brain_regressions deletion location_only missing %q: %s", want, data)
		}
	}
	if strings.Contains(string(data), "state.TranscriptPath = resolved") || strings.Contains(string(data), "current") && strings.Contains(string(data), "_ = state.TranscriptPath") {
		t.Fatalf("brain_regressions deletion location_only leaked expected/current values: %s", data)
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
	callPayload := mcpTextJSONPayload(t, responses[1])
	assertMCPReviewPayloadContract(t, callPayload, "review scopeBaseRef base scope")
	assertMCPReviewFindingContract(t, firstPayloadObject(t, callPayload, "findings"), false)
	// location_only must still localize the finding but NOT leak the expected/current values.
	locPayload := mcpTextJSONPayload(t, responses[2])
	locFinding := firstPayloadObject(t, locPayload, "findings")
	assertMCPReviewFindingContract(t, locFinding, true)
	locData, _ := json.Marshal(locPayload)
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
	repoA, repoAKey := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", regressed)
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: repoAKey, Name: "a", LocalPathHint: repoA}},
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
	callPayload := mcpTextJSONPayload(t, responses[1])
	if callPayload["workspace"] != "related" {
		t.Fatalf("workspace review payload workspace = %#v, want related: %+v", callPayload["workspace"], callPayload)
	}
	assertNumberField(t, callPayload, "schema_version", reviewReportSchemaVersion)
	assertStringContains(t, callPayload, "mode", "diff-less")
	assertStringContains(t, callPayload, "summary", "Cross-repo diff-less review")
	results := payloadArray(t, callPayload, "results")
	if len(results) != 1 {
		t.Fatalf("workspace review results = %d, want 1: %+v", len(results), callPayload)
	}
	result, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("workspace review result is not object: %+v", results[0])
	}
	if result["repo_key"] != repoAKey {
		t.Fatalf("workspace review repo_key = %#v, want %q in %+v", result["repo_key"], repoAKey, result)
	}
	assertStringContains(t, result, "summary", "suspected regression")
	assertMCPReviewFindingContract(t, firstPayloadObject(t, result, "findings"), false)
	regData, _ := json.Marshal(responses[2])
	for _, want := range []string{"review_context.go", repoAKey, "anomalies"} {
		if !strings.Contains(string(regData), want) {
			t.Fatalf("brain_workspace_regressions result missing %q: %s", want, regData)
		}
	}
	if strings.Contains(string(regData), "master..HEAD") || strings.Contains(string(regData), `scopeBaseRef+`) {
		t.Fatalf("brain_workspace_regressions location_only leaked expected/current values: %s", regData)
	}
}

func TestMCPBrainWorkspaceRegressionsDeletionLocationOnlyAndReviewRedaction(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}

	session := `{"text":"pkg/resolve.go must set state.TranscriptPath = resolved so later reads work"}`
	body := "package x\nfunc missOne(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\nfunc missTwo(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\n"
	repoA, repoAKey := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/resolve.go", body)
	if err := writeWorkspaceManifest(env, workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: repoAKey, Name: "a", LocalPathHint: repoA}},
	}); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_workspace_regressions","arguments":{"workspace":"related","query":"fix TranscriptPath resolved","include_deletions":true,"location_only":true}}}`) +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_workspace_review","arguments":{"workspace":"related","query":"fix TranscriptPath resolved","include_deletions":true,"location_only":true}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	regData, _ := json.Marshal(responses[0])
	for _, want := range []string{"resolve.go", repoAKey, "anomalies", "location only", "missOne", "missTwo", "related_locations"} {
		if !strings.Contains(string(regData), want) {
			t.Fatalf("brain_workspace_regressions deletion result missing %q: %s", want, regData)
		}
	}
	if strings.Contains(string(regData), "state.TranscriptPath = resolved") || strings.Contains(string(regData), "_ = state.TranscriptPath") {
		t.Fatalf("brain_workspace_regressions location_only leaked expected/current values: %s", regData)
	}
	reviewData, _ := json.Marshal(responses[1])
	for _, want := range []string{"resolve.go", repoAKey, "diff-less", "Suspected regression"} {
		if !strings.Contains(string(reviewData), want) {
			t.Fatalf("brain_workspace_review deletion result missing %q: %s", want, reviewData)
		}
	}
	for _, forbidden := range []string{"state.TranscriptPath = resolved", "_ = state.TranscriptPath", "history shows"} {
		if strings.Contains(string(reviewData), forbidden) {
			t.Fatalf("brain_workspace_review location_only leaked %q: %s", forbidden, reviewData)
		}
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
	branch := "feature/mcp"
	facts := []factRecord{
		{ID: factRecordID("qmd retrieval alpha contract", paths), Text: "qmd retrieval alpha contract", Paths: paths, Branch: branch, Status: factStatusActive, UpdatedAt: now},
		{ID: factRecordID("qmd retrieval beta contract", paths), Text: "qmd retrieval beta contract", Paths: paths, Branch: branch, Status: factStatusActive, UpdatedAt: now},
	}
	if err := writeFacts(storage.BrainDir, branch, facts); err != nil {
		t.Fatal(err)
	}

	input := frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_search","arguments":{"query":"qmd retrieval alpha","limit":1,"branch":%q}}}`, branch)) +
		frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_vsearch","arguments":{"query":"qmd retrieval beta","limit":1,"branch":%q}}}`, branch)) +
		frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_query","arguments":{"query":"qmd retrieval alpha","limit":1,"branch":%q}}}`, branch)) +
		frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"brain_get","arguments":{"id":%q,"branch":%q}}}`, facts[0].ID, branch)) +
		frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"brain_multi_get","arguments":{"ids":[%q,"fact:missing"],"branch":%q}}}`, facts[1].ID, branch))
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 5 {
		t.Fatalf("responses = %d", len(responses))
	}
	for i, response := range responses {
		if response["error"] != nil {
			t.Fatalf("response %d returned error: %+v", i+1, response)
		}
	}
	assertMCPRetrievalResult(t, responses[0], branch, facts[0].ID, "qmd retrieval alpha contract")
	assertMCPRetrievalResult(t, responses[1], branch, facts[1].ID, "qmd retrieval beta contract")
	assertMCPRetrievalResult(t, responses[2], branch, facts[0].ID, "qmd retrieval alpha contract")
	assertMCPRetrievalResult(t, responses[3], "", facts[0].ID, "qmd retrieval alpha contract")
	assertMCPRetrievalResult(t, responses[4], "", facts[1].ID, "qmd retrieval beta contract")
	payload := mcpTextJSONPayload(t, responses[4])
	missing, ok := payload["missing"].([]any)
	if !ok || len(missing) != 1 || missing[0] != "fact:missing" {
		t.Fatalf("multi-get missing = %#v", payload["missing"])
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

func TestMCPIndexRepositoryRejectsSemBinaryArgument(t *testing.T) {
	// sem_binary used to be an attacker-controllable executable name; it must no
	// longer be an accepted argument so an untrusted/prompt-injected client cannot
	// run an arbitrary binary through the indexer.
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_index_repository","arguments":{"sem_binary":"/bin/evil"}}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("responses = %d", len(responses))
	}
	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok || !strings.Contains(fmt.Sprint(errObj["message"]), "unknown argument for brain_index_repository: sem_binary") {
		t.Fatalf("expected unknown-argument rejection, got %+v", responses[0])
	}
}

func TestMCPResolveIndexPath(t *testing.T) {
	root := "/repo/root"
	// No bound root: path passes through, no containment.
	if p, cr := mcpResolveIndexPath(EntireEnv{}, "/somewhere"); p != "/somewhere" || cr != "" {
		t.Fatalf("unbound root: got (%q,%q)", p, cr)
	}
	if p, cr := mcpResolveIndexPath(EntireEnv{}, ""); p != "." || cr != "" {
		t.Fatalf("unbound empty: got (%q,%q)", p, cr)
	}
	// Bound root: empty and "." both resolve to the root, with containment on.
	for _, in := range []string{"", ".", "  "} {
		if p, cr := mcpResolveIndexPath(EntireEnv{RepoRoot: root}, in); p != root || cr != root {
			t.Fatalf("input %q should resolve to root with containment: got (%q,%q)", in, p, cr)
		}
	}
	// Relative path resolves inside the root (not CWD).
	if p, cr := mcpResolveIndexPath(EntireEnv{RepoRoot: root}, "sub/pkg"); p != filepath.Join(root, "sub/pkg") || cr != root {
		t.Fatalf("relative path: got (%q,%q)", p, cr)
	}
	// Absolute path is kept, but containment still applies. Use an
	// OS-appropriate absolute path so filepath.IsAbs is true on Windows too
	// (a Unix-style "/elsewhere" is not absolute without a drive letter).
	absElsewhere := "/elsewhere"
	if runtime.GOOS == "windows" {
		absElsewhere = `C:\elsewhere`
	}
	if p, cr := mcpResolveIndexPath(EntireEnv{RepoRoot: root}, absElsewhere); p != absElsewhere || cr != root {
		t.Fatalf("absolute path: got (%q,%q)", p, cr)
	}
	// Opt-out drops containment.
	t.Setenv("ENTIRE_BRAIN_MCP_ALLOW_ANY_PATH", "1")
	if p, cr := mcpResolveIndexPath(EntireEnv{RepoRoot: root}, absElsewhere); p != absElsewhere || cr != "" {
		t.Fatalf("opt-out: got (%q,%q)", p, cr)
	}
}

func TestEnforceIndexContainment(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sub")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := enforceIndexContainment("", outside); err != nil {
		t.Fatalf("empty containRoot disables check: %v", err)
	}
	if err := enforceIndexContainment(root, inside); err != nil {
		t.Fatalf("inside root should pass: %v", err)
	}
	if err := enforceIndexContainment(root, root); err != nil {
		t.Fatalf("root itself should pass: %v", err)
	}
	if err := enforceIndexContainment(root, outside); err == nil {
		t.Fatalf("outside root should be rejected")
	}
}

func TestMCPHandlerRecoversFromPanic(t *testing.T) {
	// A panic in a handler must become a JSON-RPC internal error (-32603), not a
	// crash of the long-lived stdio server.
	orig := dispatchMCPMessage
	t.Cleanup(func() { dispatchMCPMessage = orig })
	dispatchMCPMessage = func(ctx context.Context, opts Options, msg mcpMessage) mcpMessage {
		panic("boom")
	}
	resp := handleMCPMessage((&cobra.Command{}).Context(), Options{Version: "test-version"}, mcpMessage{JSONRPC: "2.0", ID: 7, Method: "tools/call"})
	if resp.Error == nil || resp.Error.Code != -32603 {
		t.Fatalf("expected -32603 internal error, got %+v", resp)
	}
	if resp.ID != 7 {
		t.Fatalf("response id = %v, want 7", resp.ID)
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
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_status","arguments":{}}}`)
	var out bytes.Buffer
	if err := runMCP(cmd.Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses[0]["result"])
	if !strings.Contains(string(data), `"severity\": \"ok\"`) {
		t.Fatalf("status result = %s", data)
	}
}

func frameMCP(payload string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(payload), payload)
}

func frameMCPJSON(t *testing.T, payload map[string]any) string {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal MCP payload: %v", err)
	}
	return frameMCP(string(data))
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

func mcpTextJSONPayload(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result object: %+v", response)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("response missing content: %+v", response)
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("response content item is not an object: %+v", content[0])
	}
	text, ok := item["text"].(string)
	if !ok {
		t.Fatalf("response content text is not a string: %+v", item)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode MCP text JSON %q: %v", text, err)
	}
	return payload
}

func assertMCPReviewPayloadContract(t *testing.T, payload map[string]any, query string) {
	t.Helper()
	assertNumberField(t, payload, "schema_version", reviewReportSchemaVersion)
	if payload["query"] != query {
		t.Fatalf("query = %#v, want %q in payload %+v", payload["query"], query, payload)
	}
	assertStringContains(t, payload, "mode", "diff-less")
	assertStringContains(t, payload, "summary", "Diff-less review")
	assertStringContains(t, payload, "repo_path", "")
	assertStringContains(t, payload, "brain_path", "")
	if generatedAt, ok := payload["generated_at"].(string); !ok || generatedAt == "" {
		t.Fatalf("generated_at missing string in payload %+v", payload)
	}
}

func assertMCPReviewFindingContract(t *testing.T, finding map[string]any, locationOnly bool) {
	t.Helper()
	if finding["severity"] != "medium" {
		t.Fatalf("finding severity = %#v, want medium in %+v", finding["severity"], finding)
	}
	if finding["file"] != "pkg/review_context.go" {
		t.Fatalf("finding file = %#v, want pkg/review_context.go in %+v", finding["file"], finding)
	}
	assertNumberField(t, finding, "line", 3)
	assertStringContains(t, finding, "title", "Suspected regression")
	assertStringContains(t, finding, "evidence", "session")
	detail, ok := finding["detail"].(string)
	if !ok || detail == "" {
		t.Fatalf("finding detail missing string in %+v", finding)
	}
	if locationOnly {
		if strings.Contains(detail, "history shows") || strings.Contains(detail, "master..HEAD") || strings.Contains(detail, "scopeBaseRef") {
			t.Fatalf("location-only finding detail leaked expected/current values: %+v", finding)
		}
		return
	}
	for _, want := range []string{"history shows", "master..HEAD", "scopeBaseRef"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("finding detail missing %q in %+v", want, finding)
		}
	}
}

func firstPayloadObject(t *testing.T, payload map[string]any, key string) map[string]any {
	t.Helper()
	items := payloadArray(t, payload, key)
	if len(items) == 0 {
		t.Fatalf("%s is empty in payload %+v", key, payload)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("%s[0] is not object: %+v", key, items[0])
	}
	return item
}

func payloadArray(t *testing.T, payload map[string]any, key string) []any {
	t.Helper()
	items, ok := payload[key].([]any)
	if !ok {
		t.Fatalf("%s missing array in payload %+v", key, payload)
	}
	return items
}

func assertNumberField(t *testing.T, payload map[string]any, key string, want int) {
	t.Helper()
	got, ok := payload[key].(float64)
	if !ok || int(got) != want {
		t.Fatalf("%s = %#v, want %d in payload %+v", key, payload[key], want, payload)
	}
}

func assertStringContains(t *testing.T, payload map[string]any, key, want string) {
	t.Helper()
	got, ok := payload[key].(string)
	if !ok {
		t.Fatalf("%s missing string in payload %+v", key, payload)
	}
	if want != "" && !strings.Contains(got, want) {
		t.Fatalf("%s = %q, want it to contain %q in payload %+v", key, got, want, payload)
	}
}

func assertMCPRetrievalResult(t *testing.T, response map[string]any, branch, id, text string) {
	t.Helper()
	payload := mcpTextJSONPayload(t, response)
	if branch != "" && payload["branch"] != branch {
		t.Fatalf("branch = %#v, want %q in payload %+v", payload["branch"], branch, payload)
	}
	results, ok := payload["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("results missing or empty: %+v", payload)
	}
	for _, result := range results {
		row, ok := result.(map[string]any)
		if !ok {
			continue
		}
		if row["id"] == id && strings.Contains(fmt.Sprint(row["text"]), text) {
			return
		}
	}
	t.Fatalf("result %q containing %q not found in %+v", id, text, payload)
}
