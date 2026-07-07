// Package hostedbrain is the runner/agent-side client for a repo's HOSTED brain
// served over MCP (entire-api's POST /repos/{repo_id}/brain/mcp, P1.M3). Where
// factsync.HTTPServer syncs the shared fact-set, this QUERIES it: it drives the
// remote JSON-RPC surface (initialize / tools/list / tools/call) so a member's agent
// can run the curated read tools (brain_search, brain_get, brain_multi_get,
// brain_status) against the shared brain over the wire.
//
// It respects the egress gate (ENTIRE_BRAIN_NO_EGRESS / ENTIRE_BRAIN_LOCAL_ONLY):
// every remote call is refused with ErrNoEgress when the gate is set, so a
// local-only workspace never reaches out. On initialize it negotiates the brain wire
// schema via brainwire.CheckCompatibility — a hosted major the local reader cannot
// consume is a hard error before any facts are read.
package hostedbrain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ashtom/entire-brain/internal/brainwire"
)

// mcpProtocolVersion is the MCP protocol version the client requests on initialize.
const mcpProtocolVersion = "2024-11-05"

// ErrNoEgress is returned by every remote call when the egress gate is set — the
// hosted brain must not be contacted from a local-only/no-egress workspace.
var ErrNoEgress = errors.New("hostedbrain: remote brain access disabled by ENTIRE_BRAIN_NO_EGRESS/LOCAL_ONLY")

// Client talks to a repo's hosted brain MCP endpoint. BaseURL is the entire-api
// origin; Token is the member's bearer token (same as factsync.HTTPServer).
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// ServerInfo is the hosted brain's initialize serverInfo, including the brain wire
// schema version the client negotiates against.
type ServerInfo struct {
	Name               string `json:"name"`
	Version            string `json:"version"`
	BrainSchemaVersion string `json:"brainSchemaVersion"`
}

// Tool is one entry from tools/list.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Fact / SearchResult mirror the hosted brainquery projection — declared locally
// because the two repos are separate modules (the wire is JSON, not shared types).
type Fact struct {
	ID     string   `json:"id"`
	Paths  []string `json:"paths"`
	Kind   string   `json:"kind"`
	Text   string   `json:"text"`
	Status string   `json:"status"`
}

type SearchResult struct {
	Fact  Fact `json:"fact"`
	Score int  `json:"score"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonrpcError) Error() string {
	return fmt.Sprintf("hostedbrain: rpc error %d: %s", e.Code, e.Message)
}

// rpc sends one JSON-RPC request to the repo's brain MCP endpoint and returns the raw
// result (or a *jsonrpcError for a JSON-RPC error, or a transport error). It is the
// single egress chokepoint: refused up front when the no-egress gate is set.
func (c *Client) rpc(ctx context.Context, repoID, method string, params any) (json.RawMessage, error) {
	if noEgress() {
		return nil, ErrNoEgress
	}
	reqBody := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		reqBody["params"] = params
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/api/v1/repos/" + repoID + "/brain/mcp"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("hostedbrain: %s %s: %w", method, repoID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("hostedbrain: %s %s: unexpected status %s: %s", method, repoID, resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *jsonrpcError   `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("hostedbrain: decode %s response: %w", method, err)
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

// Initialize handshakes with the hosted brain and negotiates the wire schema. It
// returns the serverInfo and any compatibility warning (a newer-but-compatible remote
// minor); an incompatible major is a hard error. A remote that advertises no schema
// version is accepted (older server) with no negotiation.
func (c *Client) Initialize(ctx context.Context, repoID string) (ServerInfo, string, error) {
	raw, err := c.rpc(ctx, repoID, "initialize", map[string]any{"protocolVersion": mcpProtocolVersion})
	if err != nil {
		return ServerInfo{}, "", err
	}
	var res struct {
		ServerInfo ServerInfo `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return ServerInfo{}, "", fmt.Errorf("hostedbrain: decode initialize: %w", err)
	}
	warn := ""
	if v := res.ServerInfo.BrainSchemaVersion; v != "" {
		ok, w, cerr := brainwire.CheckCompatibility(v)
		if cerr != nil {
			return res.ServerInfo, "", cerr
		}
		if !ok {
			return res.ServerInfo, "", fmt.Errorf("hostedbrain: incompatible hosted brain schema %q", v)
		}
		warn = w
	}
	return res.ServerInfo, warn, nil
}

// ListTools returns the hosted brain's advertised tool surface.
func (c *Client) ListTools(ctx context.Context, repoID string) ([]Tool, error) {
	raw, err := c.rpc(ctx, repoID, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("hostedbrain: decode tools/list: %w", err)
	}
	return res.Tools, nil
}

// CallTool invokes a tool and returns the text of its first content block (the shape
// the hosted tools return: a single JSON-encoded text block). A tool that produced no
// content returns "".
func (c *Client) CallTool(ctx context.Context, repoID, name string, args map[string]any) (string, error) {
	raw, err := c.rpc(ctx, repoID, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("hostedbrain: decode tools/call %s: %w", name, err)
	}
	if len(res.Content) == 0 {
		return "", nil
	}
	return res.Content[0].Text, nil
}

// Search is the convenience wrapper over the brain_search tool: it calls the tool and
// decodes the ranked results. branch "" defaults server-side (main); limit <= 0 uses
// the server default.
func (c *Client) Search(ctx context.Context, repoID, branch, query string, limit int) ([]SearchResult, error) {
	args := map[string]any{"query": query}
	if branch != "" {
		args["branch"] = branch
	}
	if limit > 0 {
		args["limit"] = limit
	}
	text, err := c.CallTool(ctx, repoID, "brain_search", args)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	var results []SearchResult
	if err := json.Unmarshal([]byte(text), &results); err != nil {
		return nil, fmt.Errorf("hostedbrain: decode search results: %w", err)
	}
	return results, nil
}

// noEgress reports whether the egress gate is set, matching the CLI's fail-closed
// securityToggleEnabled semantics for ENTIRE_BRAIN_NO_EGRESS / ENTIRE_BRAIN_LOCAL_ONLY:
// an empty/false-ish value is off; anything else (including an unrecognized value) is
// on, so a misconfigured gate fails closed (no egress).
func noEgress() bool {
	return toggleOn("ENTIRE_BRAIN_NO_EGRESS") || toggleOn("ENTIRE_BRAIN_LOCAL_ONLY")
}

func toggleOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "", "0", "false", "no", "off", "disable", "disabled":
		return false
	default:
		return true
	}
}
