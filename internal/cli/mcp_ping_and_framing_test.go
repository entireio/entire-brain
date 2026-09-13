package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Two ways an MCP session died that it should have survived.
//
// Carries #186 and #185, reimplemented against current main — mcp.go has moved
// a long way since (#118, #156, #158) and both patches conflict.

// --- ping (#186) ---------------------------------------------------------

// MCP defines ping as a liveness check every server must answer, with an empty
// result. main fell through to the default arm and replied
//
//	{"code": -32601, "message": "method not found"}
//
// A client that pings — the spec's own recommended keepalive — reads that as a
// broken server and drops a session that was working fine.
func TestPingIsAnswered(t *testing.T) {
	resp := dispatchMCPMessage(context.Background(), Options{Version: "test"},
		mcpMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "ping"})

	if resp.Error != nil {
		t.Fatalf("ping answered with an error: code=%d message=%q", resp.Error.Code, resp.Error.Message)
	}
	m, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("ping result is %T, want map[string]any", resp.Result)
	}
	if len(m) != 0 {
		t.Fatalf("ping result should be empty, got %v", m)
	}
}

// TestUnknownMethodIsStillMethodNotFound pins that adding ping did not turn the
// default arm into a catch-all that answers anything.
func TestUnknownMethodIsStillMethodNotFound(t *testing.T) {
	resp := dispatchMCPMessage(context.Background(), Options{Version: "test"},
		mcpMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "definitely/not/a/method"})
	if resp.Error == nil {
		t.Fatal("an unknown method was answered instead of refused")
	}
	if resp.Error.Code != -32601 {
		t.Fatalf("unknown method code = %d, want -32601", resp.Error.Code)
	}
}

// --- framing (#185) ------------------------------------------------------
//
// RECOVERABILITY IS ABOUT RESYNCHRONISATION, NOT SEVERITY. The serve loop
// answers errMCPRecoverable with -32700 and keeps reading, which is only
// correct when the reader is left at the start of the NEXT frame. These tests
// pin both halves of that rule — what must recover, and what must NOT.

// TestRecoverableFramingErrorsAreResynchronisable covers the cases where the
// body's extent is known, so the stream can be resumed.
func TestRecoverableFramingErrorsAreResynchronisable(t *testing.T) {
	big := strings.Repeat("x", 64)
	cases := []struct {
		name  string
		frame string
		why   string
	}{
		{
			name:  "no Content-Length header",
			frame: "X-Other: 1\r\n\r\n",
			why:   "nothing past the headers was consumed, so the stream is already at a boundary",
		},
		{
			name:  "body is valid JSON this server does not implement",
			frame: fmt.Sprintf("Content-Length: %d\r\n\r\n[{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}]", len(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)),
			why:   "a JSON-RPC batch — the case a CONFORMING client reaches first; the body was read in full",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readMCPMessage(bufio.NewReader(strings.NewReader(tc.frame)))
			if err == nil {
				t.Fatalf("frame was accepted; it should have been refused (%s)", tc.why)
			}
			if !errors.Is(err, errMCPRecoverable) {
				t.Fatalf("killed the session instead of refusing one frame (%s): %v", tc.why, err)
			}
		})
	}

	// An oversized body is recoverable too, but it is reported to the serve
	// loop as its own type rather than as errMCPRecoverable, because the loop
	// has to WRITE THE REFUSAL BEFORE IT DRAINS: a peer that declares
	// 99,999,999 bytes and sends none used to leave the server blocked in the
	// drain and the client waiting forever. The session-level property is the
	// one that matters, so it is asserted end to end.
	t.Run("oversized body that can be drained", func(t *testing.T) {
		body := strings.Repeat("x", maxMCPFrameBytes+len(big))
		var oversize *mcpOversizeFrameError
		_, _, err := readMCPMessage(bufio.NewReader(strings.NewReader(
			fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))))
		if !errors.As(err, &oversize) {
			t.Fatalf("an oversized frame must be refusable, not fatal: %v", err)
		}
		input := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body) +
			frameMCP(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
		var out bytes.Buffer
		if err := runMCP(context.Background(), strings.NewReader(input), &out, Options{Version: "test"}); err != nil {
			t.Fatalf("the session died on a drainable frame: %v", err)
		}
		responses := readMCPResponses(t, out.String())
		if len(responses) != 2 {
			t.Fatalf("got %d responses, want the refusal and the next request's answer: %q", len(responses), out.String())
		}
		if failure, _ := responses[0]["error"].(map[string]any); failure == nil {
			t.Errorf("the oversized frame was not refused: %v", responses[0])
		}
		if _, ok := responses[1]["result"]; !ok {
			t.Errorf("the request after the drained frame was not answered: %v", responses[1])
		}
	})
}

// TestUnresynchronisableFramingErrorsStayFatal is the other half, and the
// reason this is not simply "mark everything recoverable". When the length is
// unparseable the body's extent is unknown, so continuing would reparse the
// body as headers and silently swallow whatever followed — the host then hangs
// waiting for a reply to a request the server ate. Disconnecting is correct.
func TestUnresynchronisableFramingErrorsStayFatal(t *testing.T) {
	for _, frame := range []string{
		"Content-Length: banana\r\n\r\n",
		"Content-Length: -5\r\n\r\n",
	} {
		t.Run(strings.TrimSpace(frame), func(t *testing.T) {
			_, _, err := readMCPMessage(bufio.NewReader(strings.NewReader(frame)))
			if err == nil {
				t.Fatalf("%q was accepted", frame)
			}
			if errors.Is(err, errMCPRecoverable) {
				t.Fatalf("%q was marked recoverable, but the body boundary is unknown — "+
					"resuming would reparse the body as headers and swallow the next request", frame)
			}
		})
	}
}

// TestAnOversizedFrameDoesNotSwallowTheNextRequest is the concrete failure the
// drain prevents: mark the oversized frame recoverable WITHOUT draining it and
// the 4 MiB body is reparsed as headers, eating the real request behind it.
func TestAnOversizedFrameDoesNotSwallowTheNextRequest(t *testing.T) {
	oversized := strings.Repeat("x", maxMCPFrameBytes+1)
	good := `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	input := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(oversized), oversized) +
		fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(good), good)

	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(input), &out,
		Options{Version: "test"}); err != nil {
		t.Fatalf("the session died on an oversized frame: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "-32700") {
		t.Fatalf("the oversized frame was not refused:\n%s", body)
	}
	if !strings.Contains(body, `"id":7`) {
		t.Fatalf("the request AFTER the oversized frame was swallowed — "+
			"the body was not drained before resuming:\n%s", body)
	}
}

// TestSessionSurvivesABatchBody is #185's headline case end to end: a JSON-RPC
// batch, then an ordinary request, over one session.
func TestSessionSurvivesABatchBody(t *testing.T) {
	batch := `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`
	good := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	input := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(batch), batch) +
		fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(good), good)

	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(input), &out,
		Options{Version: "test"}); err != nil {
		t.Fatalf("the session died on a batch body: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "-32700") {
		t.Fatalf("the batch body was not answered with a parse error:\n%s", body)
	}
	if !strings.Contains(body, `"id":2`) {
		t.Fatalf("the request after the batch body was never answered:\n%s", body)
	}
}
