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
	"strconv"
	"strings"

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
	return []map[string]any{
		{
			"name":        "brain_stale",
			"description": "Report local semantic brain freshness for the current repository.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "brain_brief",
			"description": "Build a bounded task packet from local brain context, live state, semantic context, and indexed history.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"task"}, "properties": map[string]any{"task": stringArg("task", "Task or bug description"), "limit": integerArg("limit", "Maximum records per section")}},
		},
		{
			"name":        "brain_query",
			"description": "Search local semantic symbols.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{"query": stringArg("query", "Symbol or text query"), "limit": integerArg("limit", "Maximum symbols")}},
		},
		{
			"name":        "brain_context",
			"description": "Return relation-aware local semantic context.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{"query": stringArg("query", "Symbol or text query"), "limit": integerArg("limit", "Maximum symbols")}},
		},
		{
			"name":        "brain_impact",
			"description": "Traverse local semantic impact relations.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{"query": stringArg("query", "Symbol or text query"), "limit": integerArg("limit", "Maximum symbols"), "depth": integerArg("depth", "Relation depth")}},
		},
		{
			"name":        "brain_changes",
			"description": "Map local file changes to semantic symbols.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"limit": integerArg("limit", "Maximum symbols")}},
		},
		{
			"name":        "brain_history",
			"description": "Search indexed Entire session history, decisions, validation notes, tool paths, or architecture facts.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{"query": stringArg("query", "History query"), "kind": stringArg("kind", "history, decisions, sessions, validation, tool-paths, or architecture")}},
		},
		{
			"name":        "brain_regressions",
			"description": "Flag suspected regressions: lines the session history asserts but the current tree dropped or changed (with file:line, expected value, confidence, and provenance).",
			"inputSchema": map[string]any{"type": "object", "required": []string{"query"}, "properties": map[string]any{"query": stringArg("query", "Task description plus the failing symbols/identifiers"), "limit": integerArg("limit", "Maximum suspected regressions")}},
		},
	}
}

func handleMCPToolCall(ctx context.Context, opts Options, raw json.RawMessage) (map[string]any, error) {
	var params mcpToolCallParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := &cobra.Command{Use: params.Name}
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	limit, err := mcpPositiveInt(params.Arguments, "limit", 20)
	if err != nil {
		return nil, err
	}
	query := mcpString(params.Arguments, "query")
	switch params.Name {
	case "brain_stale":
		target := "."
		if opts.Env.RepoRoot != "" {
			target = opts.Env.RepoRoot
		}
		err = runSemanticStale(ctx, cmd, opts, target, true)
	case "brain_brief":
		task := mcpString(params.Arguments, "task")
		if strings.TrimSpace(task) == "" {
			err = errors.New("task is required")
		} else {
			err = runBrainBrief(ctx, cmd, opts, brainBriefOptions{limit: limit, json: true}, task)
		}
	case "brain_query":
		err = requireMCPQuery(query)
		if err == nil {
			err = runSemanticQuery(ctx, cmd, opts, semanticQueryOptions{limit: limit, json: true}, query)
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
	case "brain_changes":
		err = runSemanticChanges(ctx, cmd, opts, semanticChangesOptions{limit: limit, json: true})
	case "brain_history":
		err = requireMCPQuery(query)
		if err == nil {
			kind := strings.TrimSpace(mcpString(params.Arguments, "kind"))
			if kind == "" {
				kind = "history"
			}
			if !validMCPHistoryKind(kind) {
				err = fmt.Errorf("kind must be history, decisions, sessions, validation, tool-paths, or architecture")
			} else {
				err = runBrainHistoryInspect(ctx, cmd, opts, kind, query, true)
			}
		}
	case "brain_regressions":
		err = requireMCPQuery(query)
		if err == nil {
			err = runRegressionDetect(ctx, cmd, opts, regressionDetectorOptions{limit: limit, json: true}, query)
		}
	default:
		err = fmt.Errorf("unknown tool: %s", params.Name)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": out.String()}}}, nil
}

func validMCPHistoryKind(kind string) bool {
	switch kind {
	case "history", "decisions", "sessions", "validation", "tool-paths", "architecture":
		return true
	default:
		return false
	}
}

func requireMCPQuery(query string) error {
	if strings.TrimSpace(query) == "" {
		return errors.New("query is required")
	}
	return nil
}

func mcpString(args map[string]any, key string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return ""
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
