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
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/entireio/entire-brain/internal/apiurl"
	"github.com/entireio/entire-brain/internal/brainwire"
	"github.com/entireio/entire-brain/internal/httpx"
	"github.com/entireio/entire-brain/internal/repoid"
)

// mcpProtocolVersion is the MCP protocol version the client requests on initialize.
const mcpProtocolVersion = "2024-11-05"

// ErrNoEgress is returned by every remote call when the egress gate is set — the
// hosted brain must not be contacted from a local-only/no-egress workspace.
var ErrNoEgress = errors.New("hostedbrain: remote brain access disabled by ENTIRE_BRAIN_NO_EGRESS/LOCAL_ONLY")

// Typed transport errors so callers can react to the distinct HTTP outcomes without
// string-matching: an unauthenticated token, a repo the caller cannot pull, and a
// server with the hosted brain not configured. Each wraps the response body for
// diagnostics; match with errors.Is.
var (
	ErrUnauthorized  = errors.New("hostedbrain: unauthorized (401)")
	ErrForbidden     = errors.New("hostedbrain: forbidden — no pull access to this repo (403)")
	ErrNotConfigured = errors.New("hostedbrain: hosted brain not configured (503)")
)

// Client talks to a repo's hosted brain MCP endpoint. BaseURL is the entire-api
// origin; Token is the member's bearer token (same as factsync.HTTPServer). BaseURL
// must clear the shared scheme floor (apiurl.Validate): https, or http only to a
// loopback host.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// hostedRequestTimeout bounds one hosted-brain RPC end to end.
//
// The realistic caller is the CLI or the workspace daemon passing
// context.Background(): with no caller deadline, this client's own bound is the only
// thing standing between a wedged hosted endpoint — one that completes the dial and
// then never responds — and a permanently hung process. http.DefaultClient has no
// timeout at all, so it can never provide one. http.Client.Timeout covers the whole
// exchange (dial, request, response headers, body), which is what a stall after a
// successful dial needs; a dial-phase bound alone would not fire.
//
// The hosted tools are bounded reads (search / get / status), so a minute is
// generous; factsync.HTTPServer uses a longer bound because it uploads a whole
// fact-set. This mirrors cli.publishRequestTimeout, the same shape already used on
// the hosted publish path.
//
// It is a var, not a const, so tests can shorten it.
var hostedRequestTimeout = 60 * time.Second

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return apiurl.WithoutRedirects(c.HTTP)
	}
	// The phase bounds (dial, TLS handshake, response header) come from the shared
	// bounded transport; hostedRequestTimeout is this caller's own end-to-end bound.
	// A fresh Client per call is free — the transport, and so the connection pool,
	// is shared.
	return apiurl.WithoutRedirects(httpx.Client(hostedRequestTimeout))
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
	// Two independent guards on the same line, both about the bearer token this
	// request carries.
	//
	// Scheme floor: the hosted MCP query surface is an egress chokepoint — the
	// query and the token authorizing it must not cross plaintext to a
	// non-loopback host.
	//
	// PathEscape: concatenated raw, a "/", "?", "#" or dot segment in the repo
	// id rewrites the request target, sending that same token somewhere it was
	// never meant to go. The id arrives from --repo-id or the environment,
	// neither of which this package controls.
	//
	// The local is `endpoint`, not `url`, because `url` shadows the package
	// PathEscape comes from.
	base, err := apiurl.Validate(c.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("hostedbrain: %w", err)
	}
	// The repo id is interpolated into the request target by concatenation, so it
	// must be exactly one safe path segment — otherwise a "/", "?", "#", or dot
	// segment rewrites the target and sends the caller's bearer token to an
	// endpoint it never asked for. Escaping alone is NOT enough: "." and ".." are
	// unreserved, so url.PathEscape("..") == "..", and "/api/v1/repos/../brain/mcp"
	// collapses to /api/v1/brain/mcp at any normalizing hop. repoid.Validate states
	// the rule instead, and it is the SAME rule the fact-set transport and `brain
	// publish` apply — one implementation in internal/repoid, not three copies.
	// Refuse before the request exists, so a bad id costs zero egress.
	if err := repoid.Validate(repoID); err != nil {
		return nil, fmt.Errorf("hostedbrain: %s: %w", method, err)
	}
	endpoint := base + "/api/v1/repos/" + repoID + "/brain/mcp"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
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
		// httpx renders the body rather than pasting it: bounded read, the JSON
		// envelope's own explanation when the endpoint answered, an HTML page's title
		// when something in FRONT of the endpoint did, and control characters
		// stripped either way. Pasting the raw prefix (what this used to do) was
		// right for the documented envelope and wrong for a gateway's error page —
		// kilobytes of markup, or a peer's terminal escapes, printed at the member.
		//
		// The typed sentinels are unchanged and still wrapped with %w: callers branch
		// on errors.Is, and the detail is additive. A body with nothing to say adds
		// no separator, so a silent refusal no longer ends in a bare ": ".
		// The token this request carried must not survive into the message: a peer
		// that echoes the Authorization header would otherwise get it written to the
		// member's stderr. See httpx.Redact.
		detail := httpx.Redact(httpx.ErrorSuffix(resp), c.Token)
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, fmt.Errorf("%w%s", ErrUnauthorized, detail)
		case http.StatusForbidden:
			return nil, fmt.Errorf("%w%s", ErrForbidden, detail)
		case http.StatusServiceUnavailable:
			return nil, fmt.Errorf("%w%s", ErrNotConfigured, detail)
		default:
			return nil, fmt.Errorf("hostedbrain: %s %s: unexpected status %s%s", method, repoID, resp.Status, detail)
		}
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *jsonrpcError   `json:"error"`
	}
	if err := httpx.DecodeJSONBody(resp, &out); err != nil {
		return nil, fmt.Errorf("hostedbrain: decode %s response: %w", method, err)
	}
	if out.Error != nil {
		out.Error.Message = httpx.Redact(httpx.ErrorDetailFromBody([]byte(out.Error.Message)), c.Token)
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
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("hostedbrain: decode tools/call %s: %w", name, err)
	}
	if res.IsError {
		detail := ""
		for _, content := range res.Content {
			if content.Type == "text" && content.Text != "" {
				detail = httpx.Redact(httpx.SuffixFromBody([]byte(content.Text)), c.Token)
				break
			}
		}
		return "", fmt.Errorf("hostedbrain: tool %s failed%s", name, detail)
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
	var results []SearchResult
	return results, c.callInto(ctx, repoID, "brain_search", args, &results)
}

// GetResult is the shape brain_get returns: Found + the fact on a hit, or Found=false +
// the queried id on a miss (a miss is a normal outcome, not an error).
type GetResult struct {
	Found bool   `json:"found"`
	Fact  Fact   `json:"fact"`
	ID    string `json:"id"`
}

// Get fetches one active fact by id from the hosted fact-set.
func (c *Client) Get(ctx context.Context, repoID, branch, id string) (GetResult, error) {
	args := map[string]any{"id": id}
	if branch != "" {
		args["branch"] = branch
	}
	var out GetResult
	return out, c.callInto(ctx, repoID, "brain_get", args, &out)
}

// MultiGetResult is the shape brain_multi_get returns: the found active facts and the
// ids that were not found (unknown or non-active).
type MultiGetResult struct {
	Facts   []Fact   `json:"facts"`
	Missing []string `json:"missing"`
}

// MultiGet batch-fetches active facts by id.
func (c *Client) MultiGet(ctx context.Context, repoID, branch string, ids []string) (MultiGetResult, error) {
	args := map[string]any{"ids": ids}
	if branch != "" {
		args["branch"] = branch
	}
	var out MultiGetResult
	return out, c.callInto(ctx, repoID, "brain_multi_get", args, &out)
}

// StatusResult is the shape brain_status returns: per-status counts for a branch.
type StatusResult struct {
	Branch     string `json:"branch"`
	Active     int    `json:"active"`
	Superseded int    `json:"superseded"`
	Retracted  int    `json:"retracted"`
	Total      int    `json:"total"`
}

// Status summarizes the branch fact-set.
func (c *Client) Status(ctx context.Context, repoID, branch string) (StatusResult, error) {
	args := map[string]any{}
	if branch != "" {
		args["branch"] = branch
	}
	var out StatusResult
	return out, c.callInto(ctx, repoID, "brain_status", args, &out)
}

// callInto calls a tool and decodes its text content block into dst. An empty content
// block leaves dst at its zero value (no error) — the caller's zero value is a valid
// "nothing" for these shapes.
func (c *Client) callInto(ctx context.Context, repoID, name string, args map[string]any, dst any) error {
	text, err := c.CallTool(ctx, repoID, name, args)
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(text), dst); err != nil {
		return fmt.Errorf("hostedbrain: decode %s result: %w", name, err)
	}
	return nil
}

// noEgress reports whether the egress gate is set, matching the CLI's fail-closed
// securityToggleEnabled semantics for ENTIRE_BRAIN_NO_EGRESS / ENTIRE_BRAIN_LOCAL_ONLY:
// an empty/false-ish value is off; anything else (including an unrecognized value) is
// on, so a misconfigured gate fails closed (no egress).
func noEgress() bool {
	return toggleOn("ENTIRE_BRAIN_NO_EGRESS") || toggleOn("ENTIRE_BRAIN_LOCAL_ONLY")
}

// toggleWarned dedups the unrecognized-value warning (mirrors the CLI's
// securityToggleEnabled) so a garbage env value cannot flood stderr.
var toggleWarned sync.Map

func toggleOn(name string) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	switch strings.ToLower(raw) {
	case "", "0", "false", "no", "off", "disable", "disabled":
		return false
	case "1", "true", "yes", "on", "enable", "enabled":
		return true
	default:
		// Fail closed AND say so, matching the CLI's securityToggleEnabled: a
		// typo'd value silently enabling the gate is safe, but the operator must
		// be told their config is not what they wrote.
		if _, seen := toggleWarned.LoadOrStore(name, struct{}{}); !seen {
			fmt.Fprintf(os.Stderr, "warning: %s is not a recognized boolean; treating as enabled (fail-closed)\n", name)
		}
		return true
	}
}
