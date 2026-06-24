package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const maxMCPFrameBytes = 4 * 1024 * 1024

type mcpFrameMode string

const (
	mcpFrameContentLength mcpFrameMode = "content-length"
	mcpFrameJSONLine      mcpFrameMode = "json-line"
)

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func newMCPCommand(opts Options) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve local brain tools over MCP stdio",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMCP(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), opts)
		},
	}
}

func runMCP(ctx context.Context, in io.Reader, out io.Writer, opts Options) error {
	reader := bufio.NewReader(in)
	debugLog := os.Getenv("ENTIRE_BRAIN_MCP_DEBUG_LOG")
	mcpDebugLog(debugLog, "start")
	for {
		msg, frameMode, err := readMCPMessage(reader)
		if errors.Is(err, io.EOF) {
			mcpDebugLog(debugLog, "eof")
			return nil
		}
		if errors.Is(err, errMCPRecoverable) {
			// Reply with a JSON-RPC parse error and keep serving; one bad frame
			// must not kill the whole MCP session.
			mcpDebugLog(debugLog, "parse_error: "+err.Error())
			resp := mcpMessage{JSONRPC: "2.0", Error: &mcpError{Code: -32700, Message: "parse error"}}
			if werr := writeMCPMessage(out, resp, frameMode); werr != nil {
				mcpDebugLog(debugLog, "write_error: "+werr.Error())
				return werr
			}
			continue
		}
		if err != nil {
			mcpDebugLog(debugLog, "read_error: "+err.Error())
			return err
		}
		mcpDebugLog(debugLog, "message: "+msg.Method)
		if msg.ID == nil {
			continue
		}
		response := handleMCPMessage(ctx, opts, msg)
		if err := writeMCPMessage(out, response, frameMode); err != nil {
			mcpDebugLog(debugLog, "write_error: "+err.Error())
			return err
		}
		mcpDebugLog(debugLog, "response: "+msg.Method)
		mcpDebugLogToolResult(debugLog, msg, response)
	}
}

func mcpDebugLog(path, line string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

type mcpDebugToolCallInfo struct {
	name     string
	safeArgs map[string]any
}

func mcpDebugToolCall(raw json.RawMessage) mcpDebugToolCallInfo {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return mcpDebugToolCallInfo{}
	}
	name := strings.TrimSpace(params.Name)
	if name == "" {
		return mcpDebugToolCallInfo{}
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	safe := make(map[string]any)
	for _, key := range []string{"blind_spots", "include_deletions", "location_only"} {
		if value, ok := params.Arguments[key].(bool); ok {
			safe[key] = value
		}
	}
	if name == "brain_workspace_graph" || name == "brain_workspace_regressions" || name == "brain_workspace_review" {
		if value, ok := params.Arguments["workspace"].(string); ok {
			workspace := strings.TrimSpace(value)
			if validateWorkspaceName(workspace) == nil {
				safe["workspace"] = workspace
			}
		}
	}
	return mcpDebugToolCallInfo{name: name, safeArgs: safe}
}

func mcpDebugLogToolResult(path string, msg mcpMessage, response mcpMessage) {
	if msg.Method != "tools/call" {
		return
	}
	call := mcpDebugToolCall(msg.Params)
	if call.name == "" {
		return
	}
	mcpDebugLog(path, "tool: "+call.name)
	if len(call.safeArgs) > 0 {
		if data, err := json.Marshal(call.safeArgs); err == nil {
			mcpDebugLog(path, "tool_args: "+string(data))
		}
	}
	status := "ok"
	if response.Error != nil {
		status = "error"
	}
	mcpDebugLog(path, "tool_result: "+call.name+" "+status)
}

func handleMCPMessage(ctx context.Context, opts Options, msg mcpMessage) mcpMessage {
	response := mcpMessage{JSONRPC: "2.0", ID: msg.ID}
	switch msg.Method {
	case "initialize":
		protocolVersion := mcpInitializeProtocolVersion(msg.Params)
		response.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "entire-brain", "version": opts.Version},
		}
	case "tools/list":
		response.Result = map[string]any{"tools": mcpToolDefinitions()}
	case "tools/call":
		result, err := handleMCPToolCall(ctx, opts, msg.Params)
		if err != nil {
			response.Error = &mcpError{Code: -32000, Message: err.Error()}
		} else {
			response.Result = result
		}
	default:
		response.Error = &mcpError{Code: -32601, Message: "method not found"}
	}
	return response
}

func mcpInitializeProtocolVersion(raw json.RawMessage) string {
	const fallback = "2024-11-05"
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &params) != nil || strings.TrimSpace(params.ProtocolVersion) == "" {
		return fallback
	}
	return params.ProtocolVersion
}

func mcpToolDefinitions() []map[string]any {
	stringArg := func(name, description string) map[string]any {
		return map[string]any{"type": "string", "description": description, "title": name}
	}
	integerArg := func(name, description string) map[string]any {
		return map[string]any{"type": "integer", "description": description, "title": name, "minimum": 1}
	}
	boolArg := func(name, description string) map[string]any {
		return map[string]any{"type": "boolean", "description": description, "title": name}
	}
	branchArg := func() map[string]any {
		return stringArg("branch", "Branch for facts (default: current)")
	}
	retrievalArgs := func() map[string]any {
		return map[string]any{
			"query":  stringArg("query", "Natural-language or keyword query"),
			"limit":  integerArg("limit", "Maximum results"),
			"branch": branchArg(),
		}
	}
	objectSchema := func(required []string, properties map[string]any) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	return []map[string]any{
		{
			"name":        "brain_status",
			"description": "Summarize the local brain: sources, durable-fact counts and verification, semantic provider/coverage/freshness/blind spots, and live workspace state.",
			"inputSchema": objectSchema(nil, map[string]any{}),
		},
		{
			"name":        "brain_brief",
			"description": "Build a bounded task packet from local brain context, live state, semantic context, and indexed history.",
			"inputSchema": objectSchema([]string{"task"}, map[string]any{"task": stringArg("task", "Task or bug description"), "limit": integerArg("limit", "Maximum records per section")}),
		},
		{
			"name":        "brain_query",
			"description": "Hybrid search (lexical + semantic, RRF) across the brain's facts, history, and docs. The default retrieval; results carry ids for brain_get.",
			"inputSchema": objectSchema([]string{"query"}, retrievalArgs()),
		},
		{
			"name":        "brain_search",
			"description": "Lexical keyword search across the brain's facts, history, and docs — precise keyword/identifier matching (BM25 for history and docs; token-overlap for facts).",
			"inputSchema": objectSchema([]string{"query"}, retrievalArgs()),
		},
		{
			"name":        "brain_vsearch",
			"description": "Vector (semantic) search across the brain's facts and docs (and history when a Gemma-class embedder is configured) — conceptual/paraphrased queries.",
			"inputSchema": objectSchema([]string{"query"}, retrievalArgs()),
		},
		{
			"name":        "brain_get",
			"description": "Fetch one item in full by its id (fact:… | history:… | doc:… | pattern:… | theme:…), e.g. from a search result or pattern listing.",
			"inputSchema": objectSchema([]string{"id"}, map[string]any{"id": stringArg("id", "Prefixed item id"), "branch": branchArg()}),
		},
		{
			"name":        "brain_multi_get",
			"description": "Fetch multiple items in full by their ids.",
			"inputSchema": objectSchema([]string{"ids"}, map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "title": "ids", "description": "Prefixed item ids"}, "branch": branchArg()}),
		},
		{
			"name":        "brain_context",
			"description": "Return relation-aware local semantic context.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Symbol or text query"), "limit": integerArg("limit", "Maximum symbols")}),
		},
		{
			"name":        "brain_impact",
			"description": "Traverse local semantic impact relations.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Symbol or text query"), "limit": integerArg("limit", "Maximum symbols"), "depth": integerArg("depth", "Relation depth")}),
		},
		{
			"name":        "brain_changes",
			"description": "Map local file changes to semantic symbols.",
			"inputSchema": objectSchema(nil, map[string]any{"limit": integerArg("limit", "Maximum symbols")}),
		},
		{
			"name":        "brain_code",
			"description": "Search semantic code facts (the symbol graph) by name or description — find where a symbol lives.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Symbol name or text query"), "limit": integerArg("limit", "Maximum results")}),
		},
		{
			"name":        "brain_index_status",
			"description": "Alias for brain_status focused on semantic index freshness, coverage, and counts.",
			"inputSchema": objectSchema(nil, map[string]any{}),
		},
		{
			"name":        "brain_index_repository",
			"description": "Build or refresh the local semantic index for a repository path. Local-only; does not publish artifacts.",
			"inputSchema": objectSchema(nil, map[string]any{"path": stringArg("path", "Local repository path (default: current repo)"), "profile": stringArg("profile", "Provider profile: full, fast, or syntax-only"), "sem_binary": stringArg("sem_binary", "Entire CLI binary exposing `sem` (default: entire)"), "worktree": boolArg("worktree", "Index dirty worktree content"), "force": boolArg("force", "Replace the current semantic snapshot")}),
		},
		{
			"name":        "brain_list_projects",
			"description": "List locally indexed brain projects and semantic index counts.",
			"inputSchema": objectSchema(nil, map[string]any{}),
		},
		{
			"name":        "brain_delete_project",
			"description": "Delete a local brain project by repo_key, or the current repo project when repo_key is omitted. This removes local generated brain data only.",
			"inputSchema": objectSchema(nil, map[string]any{"repo_key": stringArg("repo_key", "Repository key to delete (default: current repo)")}),
		},
		{
			"name":        "brain_search_code",
			"description": "Search indexed source symbols/snippets by name or text.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Symbol name or text query"), "limit": integerArg("limit", "Maximum results")}),
		},
		{
			"name":        "brain_search_graph",
			"description": "Search the semantic graph for matching symbols with stable pagination.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Symbol or graph text query"), "limit": integerArg("limit", "Maximum results"), "offset": integerArg("offset", "Results to skip")}),
		},
		{
			"name":        "brain_query_graph",
			"description": "Read-only semantic graph relation query using type:/from:/to: filters.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Graph query, e.g. type:CALLS from:Foo"), "limit": integerArg("limit", "Maximum relations")}),
		},
		{
			"name":        "brain_get_graph_schema",
			"description": "Return semantic graph schema metadata, counts, symbol kinds, and relation vocabulary.",
			"inputSchema": objectSchema(nil, map[string]any{}),
		},
		{
			"name":        "brain_get_code_snippet",
			"description": "Return the exact bounded source snippet for a symbol id or name.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Symbol id, name, or qualified name"), "context_lines": integerArg("context_lines", "Extra lines before and after")}),
		},
		{
			"name":        "brain_trace_path",
			"description": "Find a directed semantic relation path between two symbols.",
			"inputSchema": objectSchema([]string{"from", "to"}, map[string]any{"from": stringArg("from", "Start symbol id, name, or qualified name"), "to": stringArg("to", "Target symbol id, name, or qualified name"), "depth": integerArg("depth", "Maximum relation depth")}),
		},
		{
			"name":        "brain_dead_code",
			"description": "List function/method symbols with no incoming non-structural graph edges and no handler boundary.",
			"inputSchema": objectSchema(nil, map[string]any{"limit": integerArg("limit", "Maximum symbols")}),
		},
		{
			"name":        "brain_detect_changes",
			"description": "Alias for brain_changes: map local file changes to semantic symbols.",
			"inputSchema": objectSchema(nil, map[string]any{"limit": integerArg("limit", "Maximum symbols")}),
		},
		{
			"name":        "brain_get_architecture",
			"description": "Return graph-derived architecture metadata: schema, relation types, languages, and boundary counts.",
			"inputSchema": objectSchema(nil, map[string]any{}),
		},
		{
			"name":        "brain_ingest_traces",
			"description": "Import runtime trace JSON/NDJSON and validate dynamic edges against the static semantic graph.",
			"inputSchema": objectSchema([]string{"path"}, map[string]any{"path": stringArg("path", "Local JSON or NDJSON trace file")}),
		},
		{
			"name":        "brain_tests",
			"description": "Suggest tests relevant to a symbol or query, derived from semantic relations.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Symbol or text query"), "limit": integerArg("limit", "Maximum test suggestions")}),
		},
		{
			"name":        "brain_boundaries",
			"description": "List route, tool, or workflow boundary symbols — entry-point enumeration.",
			"inputSchema": objectSchema(nil, map[string]any{"kind": stringArg("kind", "route, tool, or workflow (default: tool)"), "limit": integerArg("limit", "Maximum boundary symbols")}),
		},
		{
			"name":        "brain_regressions",
			"description": "Flag suspected regressions: lines the session history asserts but the current tree changed (default) or, with include_deletions, deleted (file:line, expected value, confidence, provenance).",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "Task description plus the failing symbols/identifiers"), "limit": integerArg("limit", "Maximum suspected regressions"), "include_deletions": map[string]any{"type": "boolean", "description": "Also flag deleted assignments (higher recall, noisier)", "title": "include_deletions"}, "location_only": map[string]any{"type": "boolean", "description": "Return only the suspected file:line, not the expected/current values", "title": "location_only"}}),
		},
		{
			"name":        "brain_review",
			"description": "Diff-less review (versioned schema_version contract): review the current working tree against the brain's memory (no branch-vs-base diff) and return severity-ranked suspected-regression findings with provenance. The contract `entire review`'s diff-less mode and `labs investigate` are intended to bind to; those consumers are cross-repo (entireio/cli) and not yet landed. See docs/diffless_review_seam.md.",
			"inputSchema": objectSchema([]string{"query"}, map[string]any{"query": stringArg("query", "What to review plus the relevant symbols/identifiers"), "limit": integerArg("limit", "Maximum findings"), "include_deletions": map[string]any{"type": "boolean", "description": "Also flag deleted assignments (lower confidence, noisier)", "title": "include_deletions"}, "location_only": map[string]any{"type": "boolean", "description": "Return only the suspected file:line, not the expected/current values", "title": "location_only"}}),
		},
		{
			"name":        "brain_workspace_regressions",
			"description": "Flag suspected regressions across every repo in a local multi-repo workspace (each brain's memory vs that repo's current tree). Tolerates sessions-only brains; results are aggregated by repo_key.",
			"inputSchema": objectSchema([]string{"workspace", "query"}, map[string]any{"workspace": stringArg("workspace", "Workspace name"), "query": stringArg("query", "Task description plus the failing symbols/identifiers"), "limit": integerArg("limit", "Maximum suspected regressions per repo"), "include_deletions": map[string]any{"type": "boolean", "description": "Also flag deleted assignments (higher recall, noisier)", "title": "include_deletions"}, "location_only": map[string]any{"type": "boolean", "description": "Return only the suspected file:line, not the expected/current values", "title": "location_only"}}),
		},
		{
			"name":        "brain_workspace_graph",
			"description": "Return per-repo graph metadata plus shared external contracts and cross_edges for a local multi-repo workspace.",
			"inputSchema": objectSchema([]string{"workspace"}, map[string]any{"workspace": stringArg("workspace", "Workspace name"), "limit": integerArg("limit", "Maximum contracts/cross_edges")}),
		},
		{
			"name":        "brain_workspace_review",
			"description": "Cross-repo diff-less review: review each repo's current tree in a local workspace against its brain's memory and return severity-ranked suspected-regression findings per repo. The multi-brain extension of the same versioned contract; consumers are cross-repo (entireio/cli) and not yet landed. See docs/diffless_review_seam.md.",
			"inputSchema": objectSchema([]string{"workspace", "query"}, map[string]any{"workspace": stringArg("workspace", "Workspace name"), "query": stringArg("query", "What to review plus the relevant symbols/identifiers"), "limit": integerArg("limit", "Maximum findings per repo"), "include_deletions": map[string]any{"type": "boolean", "description": "Also flag deleted assignments (lower confidence, noisier)", "title": "include_deletions"}, "location_only": map[string]any{"type": "boolean", "description": "Return only the suspected file:line, not the expected/current values", "title": "location_only"}}),
		},
		{
			"name":        "brain_patterns",
			"description": "List V2 corpus patterns (task = intent+method, procedure = command workflow, risk = corrected/failed work, practice = durable judgment, theme = latent read-only/conversational practice) with strength, support, dossier/verifier state, and a top anchor. Read-only; forming a skill is a write action done via the CLI `entire brain patterns skills form`.",
			"inputSchema": objectSchema(nil, map[string]any{"type": stringArg("type", "Filter by type: task, procedure, risk, practice, or theme (empty = all)"), "scope": stringArg("scope", "Filter by scope: repo or workspace (empty = both)"), "limit": integerArg("limit", "Maximum patterns to return")}),
		},
		{
			"name":        "brain_patterns_status",
			"description": "Pattern layer freshness and counts plus the last corpus build summary (episodes, patterns, dossiers, symbol links, commits, synapses) and skill-memory (accepted/declined/updates-available).",
			"inputSchema": objectSchema(nil, map[string]any{}),
		},
	}
}

func handleMCPToolCall(ctx context.Context, opts Options, raw json.RawMessage) (map[string]any, error) {
	var params mcpToolCallParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	if err := validateMCPToolArguments(params.Name, params.Arguments); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := &cobra.Command{Use: params.Name}
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetContext(ctx)
	limit, err := mcpPositiveInt(params.Arguments, "limit", 20)
	if err != nil {
		return nil, err
	}
	query, err := mcpOptionalString(params.Arguments, "query")
	if err != nil {
		return nil, err
	}
	branch, err := mcpOptionalString(params.Arguments, "branch")
	if err != nil {
		return nil, err
	}
	switch params.Name {
	case "brain_status", "brain_index_status":
		target := "."
		if opts.Env.RepoRoot != "" {
			target = opts.Env.RepoRoot
		}
		err = runAgentStatus(ctx, cmd, opts, agentStatusOptions{json: true, failOn: semanticAuditFailOnNone}, target)
	case "brain_index_repository":
		path, stringErr := mcpOptionalString(params.Arguments, "path")
		if stringErr != nil {
			err = stringErr
			break
		}
		if strings.TrimSpace(path) == "" {
			path = "."
			if opts.Env.RepoRoot != "" {
				path = opts.Env.RepoRoot
			}
		}
		profile, profileErr := mcpOptionalString(params.Arguments, "profile")
		if profileErr != nil {
			err = profileErr
			break
		}
		semBinary, semErr := mcpOptionalString(params.Arguments, "sem_binary")
		if semErr != nil {
			err = semErr
			break
		}
		if strings.TrimSpace(semBinary) == "" {
			semBinary = "entire"
		}
		worktree, boolErr := mcpBool(params.Arguments, "worktree")
		if boolErr != nil {
			err = boolErr
			break
		}
		force, boolErr := mcpBool(params.Arguments, "force")
		if boolErr != nil {
			err = boolErr
			break
		}
		err = runSemanticIndex(ctx, cmd, opts, semanticIndexOptions{semBinary: semBinary, profile: strings.TrimSpace(profile), worktree: worktree, force: force}, path)
	case "brain_list_projects":
		err = runMCPListProjects(cmd, opts)
	case "brain_delete_project":
		repoKey, stringErr := mcpOptionalString(params.Arguments, "repo_key")
		if stringErr != nil {
			err = stringErr
			break
		}
		err = runMCPDeleteProject(ctx, cmd, opts, strings.TrimSpace(repoKey))
	case "brain_brief":
		task, stringErr := mcpOptionalString(params.Arguments, "task")
		if stringErr != nil {
			err = stringErr
			break
		}
		if strings.TrimSpace(task) == "" {
			err = errors.New("task is required")
		} else {
			err = runBrainBrief(ctx, cmd, opts, brainBriefOptions{limit: limit, json: true}, task)
		}
	case "brain_query":
		err = requireMCPQuery(query)
		if err == nil {
			err = runRetrieve(ctx, cmd, opts, query, modeHybrid, limit, branch, true, false)
		}
	case "brain_search":
		err = requireMCPQuery(query)
		if err == nil {
			err = runRetrieve(ctx, cmd, opts, query, modeLexical, limit, branch, true, false)
		}
	case "brain_vsearch":
		err = requireMCPQuery(query)
		if err == nil {
			err = runRetrieve(ctx, cmd, opts, query, modeVector, limit, branch, true, false)
		}
	case "brain_get":
		id, stringErr := mcpOptionalString(params.Arguments, "id")
		if stringErr != nil {
			err = stringErr
			break
		}
		id = strings.TrimSpace(id)
		if id == "" {
			err = errors.New("id is required")
		} else {
			err = runGet(ctx, cmd, opts, []string{id}, branch, true)
		}
	case "brain_multi_get":
		ids, sliceErr := mcpStringSlice(params.Arguments, "ids")
		if sliceErr != nil {
			err = sliceErr
			break
		}
		if len(ids) == 0 {
			err = errors.New("ids is required")
		} else {
			err = runGet(ctx, cmd, opts, ids, branch, true)
		}
	case "brain_context":
		err = requireMCPQuery(query)
		if err == nil {
			err = runSemanticContext(ctx, cmd, opts, semanticContextOptions{limit: limit, json: true}, query)
		}
	case "brain_impact":
		err = requireMCPQuery(query)
		if err == nil {
			depth, depthErr := mcpPositiveInt(params.Arguments, "depth", 1)
			if depthErr != nil {
				err = depthErr
			} else {
				err = runSemanticImpact(ctx, cmd, opts, semanticImpactOptions{limit: limit, depth: depth, json: true}, query)
			}
		}
	case "brain_changes", "brain_detect_changes":
		err = runSemanticChanges(ctx, cmd, opts, semanticChangesOptions{limit: limit, json: true})
	case "brain_code", "brain_search_code":
		err = requireMCPQuery(query)
		if err == nil {
			err = runSemanticQuery(ctx, cmd, opts, semanticQueryOptions{limit: limit, json: true}, query)
		}
	case "brain_search_graph":
		err = requireMCPQuery(query)
		if err == nil {
			offset := 0
			if _, ok := params.Arguments["offset"]; ok {
				offset, err = mcpNonNegativeInt(params.Arguments, "offset", 0)
			}
			if err == nil {
				err = runSemanticSearchGraph(cmd, opts, semanticGraphSearchOptions{limit: limit, offset: offset, json: true}, query)
			}
		}
	case "brain_query_graph":
		err = requireMCPQuery(query)
		if err == nil {
			err = runSemanticQueryGraph(cmd, opts, semanticGraphQueryOptions{limit: limit, json: true}, query)
		}
	case "brain_get_graph_schema", "brain_get_architecture":
		err = runSemanticGraphSchema(cmd, opts, semanticGraphSchemaOptions{json: true})
	case "brain_get_code_snippet":
		err = requireMCPQuery(query)
		if err == nil {
			contextLines := 0
			if _, ok := params.Arguments["context_lines"]; ok {
				contextLines, err = mcpNonNegativeInt(params.Arguments, "context_lines", 0)
			}
			if err == nil {
				err = runSemanticSnippet(cmd, opts, semanticSnippetOptions{contextLines: contextLines, json: true}, query)
			}
		}
	case "brain_trace_path":
		from, stringErr := mcpOptionalString(params.Arguments, "from")
		if stringErr != nil {
			err = stringErr
			break
		}
		to, stringErr := mcpOptionalString(params.Arguments, "to")
		if stringErr != nil {
			err = stringErr
			break
		}
		if strings.TrimSpace(from) == "" {
			err = errors.New("from is required")
			break
		}
		if strings.TrimSpace(to) == "" {
			err = errors.New("to is required")
			break
		}
		depth, depthErr := mcpPositiveInt(params.Arguments, "depth", 4)
		if depthErr != nil {
			err = depthErr
		} else {
			err = runSemanticTracePath(cmd, opts, semanticTracePathOptions{depth: depth, json: true}, from, to)
		}
	case "brain_dead_code":
		err = runSemanticDeadCode(cmd, opts, semanticDeadCodeOptions{limit: limit, json: true})
	case "brain_ingest_traces":
		path, stringErr := mcpOptionalString(params.Arguments, "path")
		if stringErr != nil {
			err = stringErr
			break
		}
		if strings.TrimSpace(path) == "" {
			err = errors.New("path is required")
		} else {
			err = runSemanticIngestTraces(cmd, opts, semanticTraceIngestOptions{json: true, restrictToRepoRoot: true}, path)
		}
	case "brain_tests":
		err = requireMCPQuery(query)
		if err == nil {
			err = runSemanticTests(ctx, cmd, opts, semanticTestsOptions{limit: limit, json: true}, query)
		}
	case "brain_boundaries":
		kind, stringErr := mcpOptionalString(params.Arguments, "kind")
		if stringErr != nil {
			err = stringErr
			break
		}
		kind = strings.TrimSpace(kind)
		if kind == "" {
			kind = "tool"
		}
		spec, specErr := inspectBoundarySpec(kind)
		if specErr != nil {
			err = specErr
		} else {
			err = runSemanticBoundary(ctx, cmd, opts, semanticBoundaryOptions{limit: limit, json: true}, spec)
		}
	case "brain_regressions":
		err = requireMCPQuery(query)
		if err == nil {
			inc, loc, boolErr := mcpRegressionBooleans(params.Arguments)
			if boolErr != nil {
				err = boolErr
				break
			}
			err = runRegressionDetect(ctx, cmd, opts, regressionDetectorOptions{limit: limit, json: true, includeDeletions: inc, locationOnly: loc}, query)
		}
	case "brain_review":
		err = requireMCPQuery(query)
		if err == nil {
			inc, loc, boolErr := mcpRegressionBooleans(params.Arguments)
			if boolErr != nil {
				err = boolErr
				break
			}
			err = runBrainReview(ctx, cmd, opts, regressionDetectorOptions{limit: limit, json: true, includeDeletions: inc, locationOnly: loc}, query)
		}
	case "brain_workspace_regressions":
		workspace, stringErr := mcpOptionalString(params.Arguments, "workspace")
		if stringErr != nil {
			err = stringErr
			break
		}
		workspace = strings.TrimSpace(workspace)
		err = requireMCPQuery(query)
		if err == nil && workspace == "" {
			err = errors.New("workspace is required")
		}
		if err == nil {
			inc, loc, boolErr := mcpRegressionBooleans(params.Arguments)
			if boolErr != nil {
				err = boolErr
				break
			}
			err = runWorkspaceRegressions(cmd, opts, regressionDetectorOptions{limit: limit, json: true, includeDeletions: inc, locationOnly: loc}, workspace, query)
		}
	case "brain_workspace_graph":
		workspace, stringErr := mcpOptionalString(params.Arguments, "workspace")
		if stringErr != nil {
			err = stringErr
			break
		}
		workspace = strings.TrimSpace(workspace)
		if workspace == "" {
			err = errors.New("workspace is required")
		} else {
			err = runWorkspaceGraph(cmd, opts, workspaceGraphOptions{limit: limit, json: true}, workspace)
		}
	case "brain_workspace_review":
		workspace, stringErr := mcpOptionalString(params.Arguments, "workspace")
		if stringErr != nil {
			err = stringErr
			break
		}
		workspace = strings.TrimSpace(workspace)
		err = requireMCPQuery(query)
		if err == nil && workspace == "" {
			err = errors.New("workspace is required")
		}
		if err == nil {
			inc, loc, boolErr := mcpRegressionBooleans(params.Arguments)
			if boolErr != nil {
				err = boolErr
				break
			}
			err = runWorkspaceReview(cmd, opts, regressionDetectorOptions{limit: limit, json: true, includeDeletions: inc, locationOnly: loc}, workspace, query)
		}
	case "brain_patterns":
		typ, typeErr := mcpOptionalString(params.Arguments, "type")
		if typeErr != nil {
			err = typeErr
			break
		}
		scope, scopeErr := mcpOptionalString(params.Arguments, "scope")
		if scopeErr != nil {
			err = scopeErr
			break
		}
		target := "."
		if opts.Env.RepoRoot != "" {
			target = opts.Env.RepoRoot
		}
		err = runPatternsList(ctx, cmd, opts, target, patternsListOptions{asJSON: true, limit: limit, typ: strings.TrimSpace(typ), scope: strings.TrimSpace(scope)})
	case "brain_patterns_status":
		target := "."
		if opts.Env.RepoRoot != "" {
			target = opts.Env.RepoRoot
		}
		err = runPatternsStatus(ctx, cmd, opts, target, true)
	default:
		err = fmt.Errorf("unknown tool: %s", params.Name)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": out.String()}}}, nil
}

func requireMCPQuery(query string) error {
	if strings.TrimSpace(query) == "" {
		return errors.New("query is required")
	}
	return nil
}

type mcpProjectSummary struct {
	RepoKey     string   `json:"repo_key"`
	BrainDir    string   `json:"brain_dir"`
	GeneratedAt string   `json:"generated_at,omitempty"`
	Semantic    bool     `json:"semantic"`
	Files       int      `json:"files,omitempty"`
	Symbols     int      `json:"symbols,omitempty"`
	Relations   int      `json:"relations,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	Profile     string   `json:"profile,omitempty"`
}

func runMCPListProjects(cmd *cobra.Command, opts Options) error {
	dirs, err := resolvePluginDirs(opts.Env)
	if err != nil {
		return err
	}
	root := filepath.Join(dirs.Data, repoStoreDirName)
	var projects []mcpProjectSummary
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() != exportManifestFileName {
			return nil
		}
		brainDir := filepath.Dir(path)
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return nil
		}
		summary := mcpProjectSummary{
			RepoKey:     manifest.RepoKey,
			BrainDir:    brainDir,
			GeneratedAt: manifest.GeneratedAt.Format(time.RFC3339),
		}
		if manifest.Sources != nil && manifest.Sources.Semantic != nil {
			semantic := manifest.Sources.Semantic
			summary.Semantic = true
			summary.Files = semantic.Files
			summary.Symbols = semantic.Symbols
			summary.Relations = semantic.Relations
			summary.Languages = nonNilStrings(semantic.Languages)
			summary.Profile = semantic.Profile
		}
		projects = append(projects, summary)
		return nil
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].RepoKey < projects[j].RepoKey })
	return writeJSON(cmd, struct {
		Projects []mcpProjectSummary `json:"projects"`
	}{Projects: projects})
}

func runMCPDeleteProject(ctx context.Context, cmd *cobra.Command, opts Options, repoKey string) error {
	var brainDir string
	var err error
	if repoKey == "" {
		target := "."
		if opts.Env.RepoRoot != "" {
			target = opts.Env.RepoRoot
		}
		repoDir, local, resolveErr := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
		if resolveErr != nil {
			return resolveErr
		}
		if !local {
			return fmt.Errorf("brain_delete_project requires a local repository path: %s", target)
		}
		storage, storageErr := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
		if storageErr != nil {
			return storageErr
		}
		repoKey = storage.Key
		brainDir = storage.BrainDir
	} else {
		brainDir, err = brainDirForKey(opts.Env, repoKey)
		if err != nil {
			return err
		}
	}
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.RemoveAll(brainDir); err != nil {
		return fmt.Errorf("delete project %s: %w", repoKey, err)
	}
	return writeJSON(cmd, struct {
		DeletedRepoKey string `json:"deleted_repo_key"`
		BrainDir       string `json:"brain_dir"`
	}{DeletedRepoKey: repoKey, BrainDir: brainDir})
}

func validateMCPToolArguments(tool string, args map[string]any) error {
	allowed := map[string]bool{}
	add := func(keys ...string) {
		for _, key := range keys {
			allowed[key] = true
		}
	}
	switch tool {
	case "brain_status", "brain_index_status", "brain_list_projects", "brain_get_graph_schema", "brain_get_architecture":
		// no arguments
	case "brain_index_repository":
		add("path", "profile", "sem_binary", "worktree", "force")
	case "brain_delete_project":
		add("repo_key")
	case "brain_brief":
		add("task", "limit")
	case "brain_query", "brain_search", "brain_vsearch":
		add("query", "limit", "branch")
	case "brain_context", "brain_code", "brain_search_code", "brain_tests":
		add("query", "limit")
	case "brain_search_graph":
		add("query", "limit", "offset")
	case "brain_query_graph":
		add("query", "limit")
	case "brain_get_code_snippet":
		add("query", "context_lines")
	case "brain_trace_path":
		add("from", "to", "depth")
	case "brain_dead_code":
		add("limit")
	case "brain_detect_changes":
		add("limit")
	case "brain_ingest_traces":
		add("path")
	case "brain_get":
		add("id", "branch")
	case "brain_multi_get":
		add("ids", "branch")
	case "brain_impact":
		add("query", "limit", "depth")
	case "brain_changes":
		add("limit")
	case "brain_boundaries":
		add("kind", "limit")
	case "brain_regressions", "brain_review":
		add("query", "limit", "include_deletions", "location_only")
	case "brain_workspace_graph":
		add("workspace", "limit")
	case "brain_workspace_regressions", "brain_workspace_review":
		add("workspace", "query", "limit", "include_deletions", "location_only")
	case "brain_patterns":
		add("type", "scope", "limit")
	case "brain_patterns_status":
		// no arguments
	default:
		return nil
	}
	for key := range args {
		if !allowed[key] {
			return fmt.Errorf("unknown argument for %s: %s", tool, key)
		}
	}
	return nil
}

func mcpOptionalString(args map[string]any, key string) (string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return "", nil
	}
	if typed, ok := value.(string); ok {
		return typed, nil
	}
	return "", fmt.Errorf("%s must be string", key)
}

func mcpBool(args map[string]any, key string) (bool, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return false, nil
	}
	boolValue, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be boolean", key)
	}
	return boolValue, nil
}

func mcpRegressionBooleans(args map[string]any) (bool, bool, error) {
	includeDeletions, err := mcpBool(args, "include_deletions")
	if err != nil {
		return false, false, err
	}
	locationOnly, err := mcpBool(args, "location_only")
	if err != nil {
		return false, false, err
	}
	return includeDeletions, locationOnly, nil
}

func mcpStringSlice(args map[string]any, key string) ([]string, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return nil, nil
	}
	raw, ok := args[key].([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", key)
		}
		// Append the trimmed value so validation (non-empty) and downstream id
		// resolution see the same string — " doc:abc " must resolve, not 404.
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, nil
}

func mcpPositiveInt(args map[string]any, key string, fallback int) (int, error) {
	value, ok := args[key]
	if !ok {
		return fallback, nil
	}
	switch typed := value.(type) {
	case float64:
		if typed >= 1 && typed <= float64(math.MaxInt) && math.Trunc(typed) == typed {
			return int(typed), nil
		}
	case int:
		if typed >= 1 {
			return typed, nil
		}
	}
	return 0, fmt.Errorf("%s must be an integer greater than zero", key)
}

func mcpNonNegativeInt(args map[string]any, key string, fallback int) (int, error) {
	value, ok := args[key]
	if !ok {
		return fallback, nil
	}
	switch typed := value.(type) {
	case float64:
		if typed >= 0 && typed <= float64(math.MaxInt) && math.Trunc(typed) == typed {
			return int(typed), nil
		}
	case int:
		if typed >= 0 {
			return typed, nil
		}
	}
	return 0, fmt.Errorf("%s must be a non-negative integer", key)
}

// errMCPRecoverable marks a single malformed/oversized frame that should be
// answered with a JSON-RPC parse error rather than terminating the session.
var errMCPRecoverable = errors.New("recoverable mcp frame error")

// readBoundedLine reads one '\n'-terminated line while capping accumulated bytes
// at max, so a newline-less giant frame cannot exhaust memory during the read
// (ReadString would buffer it unbounded). On overflow it drains the remainder of
// the line (bounded) to keep the stream aligned and returns errMCPRecoverable.
func readBoundedLine(reader *bufio.Reader, max int) (string, error) {
	var buf []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			if err == bufio.ErrBufferFull {
				drainMCPLine(reader)
			}
			return "", fmt.Errorf("%w: frame exceeds maximum size of %d bytes", errMCPRecoverable, max)
		}
		buf = append(buf, chunk...)
		switch err {
		case nil:
			return string(buf), nil
		case bufio.ErrBufferFull:
			continue
		default:
			return string(buf), err
		}
	}
}

// drainMCPLine discards the rest of an over-long line up to a bounded number of
// buffer-sized chunks, then gives up (a pathological newline-less stream stays
// memory-safe; only stream realignment is best-effort).
func drainMCPLine(reader *bufio.Reader) {
	for i := 0; i < 64; i++ {
		if _, err := reader.ReadSlice('\n'); err != bufio.ErrBufferFull {
			return
		}
	}
}

func readMCPMessage(reader *bufio.Reader) (mcpMessage, mcpFrameMode, error) {
	length := -1
	for {
		line, err := readBoundedLine(reader, maxMCPFrameBytes)
		if errors.Is(err, errMCPRecoverable) {
			return mcpMessage{}, mcpFrameJSONLine, err
		}
		if err != nil {
			return mcpMessage{}, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			trimmed := strings.TrimSpace(line)
			// readBoundedLine already caps the line at maxMCPFrameBytes (an oversize
			// NDJSON frame is reported there), so the only failure left here is a
			// malformed line, which we make recoverable instead of fatal.
			var msg mcpMessage
			if err := json.Unmarshal([]byte(trimmed), &msg); err != nil {
				return mcpMessage{}, mcpFrameJSONLine, fmt.Errorf("%w: %v", errMCPRecoverable, err)
			}
			return msg, mcpFrameJSONLine, nil
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return mcpMessage{}, "", err
			}
			if parsed < 0 {
				return mcpMessage{}, "", errors.New("Content-Length must be non-negative")
			}
			length = parsed
		}
	}
	if length < 0 {
		return mcpMessage{}, "", errors.New("missing Content-Length")
	}
	if length > maxMCPFrameBytes {
		return mcpMessage{}, "", fmt.Errorf("Content-Length exceeds maximum frame size of %d bytes", maxMCPFrameBytes)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return mcpMessage{}, "", err
	}
	var msg mcpMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return mcpMessage{}, "", err
	}
	return msg, mcpFrameContentLength, nil
}

func writeMCPMessage(out io.Writer, msg mcpMessage, frameMode mcpFrameMode) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if frameMode == mcpFrameJSONLine {
		_, err = fmt.Fprintf(out, "%s\n", data)
		return err
	}
	_, err = fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}
