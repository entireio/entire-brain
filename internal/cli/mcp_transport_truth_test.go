package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpBlockingReader serves a prefix and then blocks, the way a peer that
// declared a body it never sends leaves the server's stdin.
type mcpBlockingReader struct {
	data    []byte
	off     int
	release chan struct{}
}

func (r *mcpBlockingReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	<-r.release
	return 0, io.EOF
}

// TestAFrameBodyThatEndsEarlyIsAnnouncedNotSwallowed.
//
// `Content-Length: 99999999` followed by a closed stdin exited 0 having written
// ZERO bytes: io.CopyN's short read returns io.EOF, and the serve loop's
// errors.Is(err, io.EOF) check runs BEFORE the branch that announces a framing
// failure, so a truncated frame was read as a clean shutdown. The client was
// left holding a request that was neither answered nor refused -- the same class
// of silent framing death the -32700-before-dying fix addressed, in the one
// branch it missed. The under-limit length is the same bug: io.ReadFull returns
// io.EOF when it read nothing at all.
func TestAFrameBodyThatEndsEarlyIsAnnouncedNotSwallowed(t *testing.T) {
	t.Parallel()

	initialize := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"over the frame limit, no body at all", initialize + "Content-Length: 99999999\r\n\r\n"},
		{"under the frame limit, no body at all", initialize + "Content-Length: 500\r\n\r\n"},
		{"under the frame limit, a partial body", initialize + "Content-Length: 500\r\n\r\nshort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runMCP(context.Background(), strings.NewReader(tc.input), &out, Options{Version: "test"})
			if err == nil {
				t.Errorf("a truncated frame ended the session as a clean shutdown")
			}
			responses := readMCPResponses(t, out.String())
			if len(responses) != 2 {
				t.Fatalf("got %d responses, want the initialize result and a framing error: %s", len(responses), out.String())
			}
			failure, _ := responses[1]["error"].(map[string]any)
			if failure == nil {
				t.Fatalf("the truncated frame was not reported: %v", responses[1])
			}
			if code, _ := failure["code"].(float64); int(code) != -32700 {
				t.Errorf("error code = %v, want -32700", failure["code"])
			}
			if message, _ := failure["message"].(string); !strings.Contains(message, "ended after") &&
				!strings.Contains(message, "exceeds maximum frame size") {
				t.Errorf("the error does not say the frame was truncated: %q", message)
			}
		})
	}
}

// TestAnOversizeFrameIsRefusedBeforeItsBodyIsDrained.
//
// With stdin held OPEN, an oversize Content-Length was worse than silent: the
// server blocked in the drain waiting for a body the peer never sent, consumed
// whatever well-formed request arrived next as body bytes, and the client waited
// forever with no deadline. The refusal is written BEFORE the drain starts, so
// the client learns the frame was refused whatever the peer does next.
func TestAnOversizeFrameIsRefusedBeforeItsBodyIsDrained(t *testing.T) {
	t.Parallel()

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`) +
		"Content-Length: 99999999\r\n\r\n"
	reader := &mcpBlockingReader{data: []byte(input), release: make(chan struct{})}
	out := &mcpSyncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runMCP(context.Background(), reader, out, Options{Version: "test"}) }()

	deadline := time.After(10 * time.Second)
	for {
		if strings.Contains(out.String(), "-32700") {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the server exited (%v) without refusing the oversize frame: %s", err, out.String())
		case <-deadline:
			t.Fatalf("the client waited 10s for a refusal that never came; wrote: %q", out.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(reader.release)
	<-done
}

// TestAParseErrorKeepsTheSessionsFraming.
//
// A client that established Content-Length framing at initialize got its -32700
// back as a BARE NDJSON LINE, spliced onto the previous frame's body with no
// header, because the reply was framed in the mode of the offending line rather
// than the session's. A strictly-LSP host mis-frames that.
func TestAParseErrorKeepsTheSessionsFraming(t *testing.T) {
	t.Parallel()

	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`) +
		"banana\n" +
		frameMCP(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(input), &out, Options{Version: "test"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	// Every reply carries a Content-Length header: exactly three of them, and no
	// bare NDJSON line anywhere.
	if headers := strings.Count(out.String(), "Content-Length: "); headers != 3 {
		t.Errorf("%d framed replies in %q, want 3 (initialize, parse error, ping)", headers, out.String())
	}
	responses := readMCPResponses(t, out.String())
	if len(responses) != 3 {
		t.Fatalf("got %d responses, want initialize + parse error + ping: %s", len(responses), out.String())
	}
	failure, _ := responses[1]["error"].(map[string]any)
	if code, _ := failure["code"].(float64); int(code) != -32700 {
		t.Errorf("second response is not the parse error: %v", responses[1])
	}
	// And the session survives it, as a recoverable frame error must.
	if _, ok := responses[2]["result"]; !ok {
		t.Errorf("the request after the bad line was not answered: %v", responses[2])
	}
}

// TestNDJSONSessionsKeepTheirOwnFraming is the other direction: a session that
// never sent a Content-Length header must not start receiving framed replies.
func TestNDJSONSessionsKeepTheirOwnFraming(t *testing.T) {
	t.Parallel()

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
		"banana\n" +
		`{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(input), &out, Options{Version: "test"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	if strings.Contains(out.String(), "Content-Length: ") {
		t.Errorf("an NDJSON session received a framed reply: %q", out.String())
	}
	if lines := strings.Count(strings.TrimRight(out.String(), "\n"), "\n") + 1; lines != 3 {
		t.Errorf("%d NDJSON replies, want 3: %q", lines, out.String())
	}
}

// TestBrainIngestTracesStaysInsideTheBoundRepository.
//
// It was the one path-taking tool with no containment: {"path":"/etc/hosts"}
// came back "invalid character '#' looking for beginning of value" while a
// missing file came back "no such file or directory", which is a file-existence
// and JSON-shape oracle for anything the server's uid can read, and a JSON file
// outside the repo was ingested and its absolute path recorded in the brain.
func TestBrainIngestTracesStaysInsideTheBoundRepository(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(root, "traces.json")
	if err := os.WriteFile(inside, []byte(`{"traces":[]}`), 0o600); err != nil {
		t.Fatalf("write trace file: %v", err)
	}
	foreign := filepath.Join(outside, "traces.json")
	if err := os.WriteFile(foreign, []byte(`{"traces":[]}`), 0o600); err != nil {
		t.Fatalf("write foreign trace file: %v", err)
	}

	// Refused: an absolute path outside the bound repository, whether or not it
	// exists -- so nothing can be learned from the difference.
	for _, path := range []string{foreign, filepath.Join(outside, "absent.json"), "/etc/hosts", "../escape.json"} {
		resolved, err := mcpResolveTracePath(root, "brain_ingest_traces", path)
		if err == nil {
			t.Errorf("path %q was accepted and resolved to %q", path, resolved)
			continue
		}
		if !strings.Contains(err.Error(), "bound repository") {
			t.Errorf("refusal for %q does not name the scope: %v", path, err)
		}
		if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "invalid character") {
			t.Errorf("refusal for %q leaks whether the file exists or parses: %v", path, err)
		}
	}

	// Accepted: inside the repository, absolute or relative to the bound root.
	for _, path := range []string{inside, "traces.json", "./traces.json"} {
		resolved, err := mcpResolveTracePath(root, "brain_ingest_traces", path)
		if err != nil {
			t.Errorf("path %q inside the bound repository was refused: %v", path, err)
			continue
		}
		if resolved != inside {
			t.Errorf("path %q resolved to %q, want %q", path, resolved, inside)
		}
	}

	// A missing path argument is still a missing-argument error, not a scope one.
	if _, err := mcpResolveTracePath(root, "brain_ingest_traces", "  "); err == nil ||
		!strings.Contains(err.Error(), "path is required") {
		t.Errorf("empty path error = %v, want \"path is required\"", err)
	}
}

// TestTracePathContainmentPolicy tests the path helper with a resolved repository root.
// Server startup and cwd root discovery are separate integration boundaries.
func TestTracePathContainmentPolicy(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "traces.json")
	if err := os.WriteFile(inside, []byte(`{"traces":[]}`), 0o600); err != nil {
		t.Fatalf("write trace file: %v", err)
	}

	// The resolved cwd repository stands in for what mcpConfigRepoRoot returns
	// for an path policy: the same value, reached without ENTIRE_REPO_ROOT.
	for _, path := range []string{"/etc/hosts", filepath.Join(t.TempDir(), "absent.json")} {
		if _, err := mcpResolveTracePath(root, "brain_ingest_traces", path); err == nil {
			t.Errorf("path policy accepted %q", path)
		} else if strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "invalid character") {
			t.Errorf("unbound refusal for %q leaks existence or parseability: %v", path, err)
		}
	}
	if _, err := mcpResolveTracePath(root, "brain_ingest_traces", inside); err != nil {
		t.Errorf("path policy refused a path inside its own repository: %v", err)
	}

	// No repository at all is the one remaining latitude, matching
	// brain_index_repository: nothing to enforce against.
	if _, err := mcpResolveTracePath("", "brain_ingest_traces", "/etc/hosts"); err != nil {
		t.Errorf("a server outside any repository must keep the index tool's latitude: %v", err)
	}
}

// Scalar arguments are validated before execution; row trimming cannot reduce them.
func TestAnOversizeStringArgumentIsRefusedBeforeItIsEchoed(t *testing.T) {
	t.Parallel()

	oversize := strings.Repeat("A", mcpStringArgMaxBytes+1)
	_, err := mcpOptionalString(map[string]any{"query": oversize}, "query")
	if err == nil {
		t.Fatal("a 1 MB scalar was accepted and would be echoed into the result")
	}
	var invalid *mcpInvalidParamsError
	if !errors.As(err, &invalid) {
		t.Errorf("an oversize argument is not reported as invalid params: %v", err)
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("the refusal does not name the bound: %v", err)
	}
	// At the bound exactly, the argument is legal.
	if _, err := mcpOptionalString(map[string]any{"query": strings.Repeat("A", mcpStringArgMaxBytes)}, "query"); err != nil {
		t.Errorf("an argument at the declared maximum was refused: %v", err)
	}
	// And the schema declares what the handler enforces.
	for _, def := range mcpToolDefinitions() {
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for name, raw := range props {
			prop, _ := raw.(map[string]any)
			if kind, _ := prop["type"].(string); kind != "string" {
				continue
			}
			if declared, ok := prop["maxLength"].(int); !ok || declared != mcpStringArgMaxBytes {
				t.Errorf("%s.%s declares maxLength %v, want %d", def["name"], name, prop["maxLength"], mcpStringArgMaxBytes)
			}
		}
	}
}

// TestBrainPatternsRejectsAValueItCannotFilterOn.
//
// scope="bogus" and type="bogus" both returned [] as a SUCCESS, so an agent
// could not tell "there are no procedures" from "procedure is not spelled that
// way" -- and would go on to report that the repository has no patterns of a
// type it never actually asked for. brain_boundaries and brain_brief validated
// theirs; this one did not.
func TestBrainPatternsRejectsAValueItCannotFilterOn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ key, value string }{
		{"type", "bogus"},
		{"scope", "bogus"},
		{"type", "Procedure"},
		{"scope", "repos"},
	} {
		allowed := mcpPatternTypes
		if tc.key == "scope" {
			allowed = mcpPatternScopes
		}
		if _, err := mcpEnumArg(map[string]any{tc.key: tc.value}, tc.key, allowed); err == nil {
			t.Errorf("%s=%q was accepted as a filter", tc.key, tc.value)
		}
	}
	for _, value := range mcpPatternTypes {
		if got, err := mcpEnumArg(map[string]any{"type": value}, "type", mcpPatternTypes); err != nil || got != value {
			t.Errorf("type=%q: got %q, %v", value, got, err)
		}
	}
	// Absent and empty still mean "no filter".
	for _, args := range []map[string]any{{}, {"type": ""}, {"type": "  "}} {
		if got, err := mcpEnumArg(args, "type", mcpPatternTypes); err != nil || got != "" {
			t.Errorf("args %v: got %q, %v", args, got, err)
		}
	}
	// The schema advertises exactly what the handler accepts.
	for _, def := range mcpToolDefinitions() {
		if def["name"] != "brain_patterns" {
			continue
		}
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for key, want := range map[string][]string{"type": mcpPatternTypes, "scope": mcpPatternScopes} {
			prop, _ := props[key].(map[string]any)
			declared, _ := prop["enum"].([]string)
			if strings.Join(declared, ",") != strings.Join(want, ",") {
				t.Errorf("brain_patterns.%s declares enum %v, want %v", key, declared, want)
			}
		}
	}
}

// TestValidationFailuresAreInvalidParamsNotServerErrors.
//
// Every failure on this surface was -32000: a limit of 99999, an unknown
// argument name, a missing required field and a genuine execution failure were
// indistinguishable to a client. -32602 is the code JSON-RPC reserves for the
// first three.
func TestValidationFailuresAreInvalidParamsNotServerErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		call map[string]any
		want string
	}{
		{"null params", nil, "params"},
		{"no tool name", map[string]any{"arguments": map[string]any{}}, "params.name"},
		{"unknown tool", map[string]any{"name": "brain_nope"}, "unknown tool"},
		{"limit past the ceiling", map[string]any{"name": "brain_search", "arguments": map[string]any{"query": "x", "limit": 99999.0}}, "between 1 and"},
		{"unknown argument", map[string]any{"name": "brain_search", "arguments": map[string]any{"query": "x", "bogus": 1.0}}, "unknown argument"},
		{"missing required argument", map[string]any{"name": "brain_search", "arguments": map[string]any{}}, "query is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := json.RawMessage("null")
			if tc.call != nil {
				data, err := json.Marshal(tc.call)
				if err != nil {
					t.Fatalf("marshal call: %v", err)
				}
				raw = data
			}
			_, err := handleMCPToolCall(context.Background(), Options{}, raw)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if code := mcpErrorCodeFor(err); code != -32602 {
				t.Errorf("code = %d, want -32602 (invalid params): %v", code, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not name the problem (%q)", err.Error(), tc.want)
			}
		})
	}
}

// mcpSyncBuffer is a bytes.Buffer safe to read while the serve loop writes.
type mcpSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *mcpSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *mcpSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestBrainListProjectsResolvesItsRepositoryLikeEveryOtherTool.
//
// It was the ONE tool on the surface that required ENTIRE_REPO_ROOT. A server
// launched from a repository with no env -- which is how a host that does not
// use `mcp --print-config` starts it -- answered 35 tools from the working
// directory and refused the 36th, telling the caller to set an environment
// variable none of the others needed.
//
// The tools whose scope ANCHOR is the bound root still refuse without one; that
// is mcpBoundAnchorTools, pinned elsewhere in this package.
func TestBrainListProjectsResolvesItsRepositoryLikeEveryOtherTool(t *testing.T) {
	t.Setenv(mcpAllowCrossRepoEnv, "")
	opts, boundDir, _ := workspaceSiblingFixture(t, "unbound")

	bound, isBound, err := mcpRepoLocalStorage(context.Background(), opts)
	if err != nil || !isBound {
		t.Fatalf("bound resolution (%v, bound=%v)", err, isBound)
	}

	// Unbound, but standing in the repository: the same answer, from the same
	// repository, as the bound server gives.
	unbound := opts
	unbound.Env.RepoRoot = ""
	t.Chdir(boundDir)
	fromCwd, resolved, err := mcpRepoLocalStorage(context.Background(), unbound)
	if err != nil {
		t.Fatalf("unbound resolution from inside a repository failed: %v", err)
	}
	if !resolved {
		t.Fatal("an path policy inside a repository refused to resolve it; brain_list_projects is dead on a cwd launch")
	}
	if fromCwd.Key != bound.Key {
		t.Errorf("cwd resolution picked %q, bound picks %q", fromCwd.Key, bound.Key)
	}
	if fromCwd.BrainDir != bound.BrainDir {
		t.Errorf("cwd resolution walks %q, bound walks %q", fromCwd.BrainDir, bound.BrainDir)
	}
}
