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
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

const maxMCPFrameBytes = 4 * 1024 * 1024

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
	for {
		msg, err := readMCPMessage(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if msg.ID == nil {
			continue
		}
		response := handleMCPMessage(ctx, opts, msg)
		if err := writeMCPMessage(out, response); err != nil {
			return err
		}
	}
}

func handleMCPMessage(ctx context.Context, opts Options, msg mcpMessage) mcpMessage {
	response := mcpMessage{JSONRPC: "2.0", ID: msg.ID}
	switch msg.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": "2024-11-05",
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

func readMCPMessage(reader *bufio.Reader) (mcpMessage, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return mcpMessage{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return mcpMessage{}, err
			}
			if parsed < 0 {
				return mcpMessage{}, errors.New("Content-Length must be non-negative")
			}
			length = parsed
		}
	}
	if length < 0 {
		return mcpMessage{}, errors.New("missing Content-Length")
	}
	if length > maxMCPFrameBytes {
		return mcpMessage{}, fmt.Errorf("Content-Length exceeds maximum frame size of %d bytes", maxMCPFrameBytes)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return mcpMessage{}, err
	}
	var msg mcpMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return mcpMessage{}, err
	}
	return msg, nil
}

func writeMCPMessage(out io.Writer, msg mcpMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}
