package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A framing failure must always be announced, even the fatal kind.
//
// #214 made an unparseable Content-Length deliberately fatal: the body's extent
// is unknown, so the stream cannot be resynchronised, and continuing would
// reparse the body as headers and swallow whatever request came next. That
// reasoning is right and this does not change it.
//
// What was wrong is the SHAPE of the death. Driving the server against
// `Content-Length: banana` produced:
//
//	S->C: (nothing at all)
//	      process exit code: 1
//	      stderr: strconv.Atoi: parsing "banana": invalid syntax
//
// The client sees only a closed pipe. The one place the real reason appears is
// stderr, which most MCP hosts discard — so a client that ever miscomputes a
// header length loses the session with zero diagnostics.

// TestAFatalFrameErrorIsAnnouncedBeforeTheSessionEnds is the defect itself.
func TestAFatalFrameErrorIsAnnouncedBeforeTheSessionEnds(t *testing.T) {
	for _, header := range []string{
		"Content-Length: banana\r\n\r\n",
		"Content-Length: -5\r\n\r\n",
		"Content-Length: 1e3\r\n\r\n",
	} {
		t.Run(strings.TrimSpace(header), func(t *testing.T) {
			var out bytes.Buffer
			// The session is EXPECTED to end here — that is the deliberate part.
			_ = runMCP(context.Background(), strings.NewReader(header), &out, Options{Version: "test"})

			body := out.String()
			if strings.TrimSpace(body) == "" {
				t.Fatalf("%q killed the session without writing anything; the host sees only a closed pipe", header)
			}
			if !strings.Contains(body, "-32700") {
				t.Fatalf("%q did not produce a parse error frame:\n%s", header, body)
			}
		})
	}
}

// TestAParseErrorCarriesAnExplicitNullID pins JSON-RPC 2.0 section 5: a Response
// must carry "id", and null is the value when the request could not be parsed
// well enough to have one. mcpMessage.ID is `omitempty`, so a nil interface
// dropped the member entirely and strict client validators reject that.
func TestAParseErrorCarriesAnExplicitNullID(t *testing.T) {
	var out bytes.Buffer
	_ = runMCP(context.Background(), strings.NewReader("Content-Length: 5\r\n\r\n{nope"), &out, Options{Version: "test"})

	body := out.String()
	idx := strings.Index(body, "{")
	if idx < 0 {
		t.Fatalf("no JSON in the reply:\n%s", body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body[idx:]), &raw); err != nil {
		// Content-Length framing prefixes a header; find the JSON object after it.
		if j := strings.Index(body, `{"jsonrpc"`); j >= 0 {
			if err2 := json.Unmarshal([]byte(body[j:]), &raw); err2 != nil {
				t.Fatalf("reply is not valid JSON: %v\n%s", err2, body)
			}
		} else {
			t.Fatalf("reply is not valid JSON: %v\n%s", err, body)
		}
	}
	if _, ok := raw["id"]; !ok {
		t.Fatalf(`the parse-error response omits "id"; JSON-RPC 2.0 requires it (null): %s`, body)
	}
	if string(raw["id"]) != "null" {
		t.Fatalf(`"id" = %s, want null`, raw["id"])
	}
}

// TestAParseErrorNamesItsCause covers the other half: four different framing
// failures all reported the bare string "parse error". The server knows which
// one happened; the client was told nothing.
func TestAParseErrorNamesItsCause(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{name: "missing header", frame: "X-Other: 1\r\n\r\n", want: "Content-Length"},
		{name: "malformed body", frame: "Content-Length: 5\r\n\r\n{nope", want: "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			_ = runMCP(context.Background(), strings.NewReader(tc.frame), &out, Options{Version: "test"})
			body := out.String()
			if !strings.Contains(body, "-32700") {
				t.Fatalf("no parse error for %q:\n%s", tc.frame, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Fatalf("parse error lacks cause %q: %s", tc.want, body)
			}
			if strings.Contains(body, `"message":"parse error"`) {
				t.Fatalf("%q reported the bare \"parse error\" with no cause:\n%s", tc.frame, body)
			}
		})
	}
}

// TestRecoverableFramesStillRecover guards the property #214 established, so
// naming the cause did not turn a recoverable frame fatal.
func TestRecoverableFramesStillRecover(t *testing.T) {
	good := `{"jsonrpc":"2.0","id":9,"method":"ping"}`
	input := "X-Other: 1\r\n\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(good), good)

	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(input), &out, Options{Version: "test"}); err != nil {
		t.Fatalf("a recoverable frame ended the session: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "-32700") {
		t.Fatalf("the bad frame was not refused:\n%s", body)
	}
	if !strings.Contains(body, `"id":9`) {
		t.Fatalf("the request after the bad frame was never answered:\n%s", body)
	}
}

// A line the server cannot parse must be ANSWERED, not eaten.
//
// #234 (above) fixed the shape of the responses on the Content-Length path.
// This is the adjacent hole it did not reach: whether a malformed line got a
// -32700 at all depended on its FIRST CHARACTER. Only a line starting with "{"
// reached the JSON decoder. Everything else was handed to the frame-header
// scanner, failed to be a Content-Length, and was discarded with no response,
// no log and no error — the server read to EOF and exited 0 having written
// nothing. A host that sent one waits forever for a reply to a request the
// server silently dropped.
//
// The routing goes through mcpFrameErrorResponse, the helper #234 already
// added; nothing here is a second copy of it.
func TestALineThatIsNotAFrameIsRefusedRatherThanSwallowed(t *testing.T) {
	for _, line := range []string{
		"banana\n",
		// A JSON-RPC batch over NDJSON. Its first colon sits inside the JSON, so
		// the header scanner read `[{"jsonrpc"` as a field name and dropped it.
		// The Content-Length-framed batch was already refused with -32700; this
		// is the same message over the other framing.
		"[{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}]\n",
		"\"hello\"\n",
		"123\n",
		"null\n",
	} {
		t.Run(strings.TrimSpace(line), func(t *testing.T) {
			var out bytes.Buffer
			if err := runMCP(context.Background(), strings.NewReader(line), &out, Options{Version: "test"}); err != nil {
				t.Fatalf("an unparseable line ended the session: %v", err)
			}
			body := out.String()
			if strings.TrimSpace(body) == "" {
				t.Fatalf("%q was swallowed: the server wrote nothing at all", line)
			}
			if !strings.Contains(body, "-32700") {
				t.Fatalf("%q did not produce a parse error frame:\n%s", line, body)
			}
			if !strings.Contains(body, `"id":null`) {
				t.Fatalf("%q produced a parse error without the explicit null id:\n%s", line, body)
			}
		})
	}
}

// TestAnUnparseableLineDoesNotCostTheSession pins the other half: the refusal is
// recoverable, so the request AFTER the bad line is still served, and the reply
// arrives in the framing the client was speaking.
func TestAnUnparseableLineDoesNotCostTheSession(t *testing.T) {
	var out bytes.Buffer
	input := "banana\n" + `{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n"
	if err := runMCP(context.Background(), strings.NewReader(input), &out, Options{Version: "test"}); err != nil {
		t.Fatalf("an unparseable line ended the session: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "-32700") {
		t.Fatalf("the bad line was not refused:\n%s", body)
	}
	if !strings.Contains(body, `"id":7`) {
		t.Fatalf("the request after the bad line was never answered:\n%s", body)
	}
	// A client speaking newline-delimited JSON must not be answered in LSP
	// framing, or the refusal is unreadable to it and the drop is still silent.
	if strings.Contains(body, "Content-Length:") {
		t.Fatalf("an NDJSON session was answered with Content-Length framing:\n%s", body)
	}
}

// TestHeadersTheServerDoesNotConsumeAreStillSkipped is the control. Conforming
// LSP-framed clients send Content-Type, and refusing every header this server
// does not itself read would break them — so the rule is "syntactically a header
// field", not "a header I recognise".
func TestHeadersTheServerDoesNotConsumeAreStillSkipped(t *testing.T) {
	good := `{"jsonrpc":"2.0","id":11,"method":"ping"}`
	input := "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(good), good)

	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(input), &out, Options{Version: "test"}); err != nil {
		t.Fatalf("a conforming framed request failed: %v", err)
	}
	body := out.String()
	if strings.Contains(body, "-32700") {
		t.Fatalf("a valid Content-Type header was refused as unparseable:\n%s", body)
	}
	if !strings.Contains(body, `"id":11`) {
		t.Fatalf("the request was never answered:\n%s", body)
	}
}

// TestANotificationIsStillAnsweredWithSilence guards the JSON-RPC rule the fix
// must not trade away: a message with no id gets NO response, and it is valid
// JSON, so it never reaches the framing check above.
func TestANotificationIsStillAnsweredWithSilence(t *testing.T) {
	var out bytes.Buffer
	if err := runMCP(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out, Options{Version: "test"}); err != nil {
		t.Fatalf("a notification ended the session: %v", err)
	}
	if body := strings.TrimSpace(out.String()); body != "" {
		t.Fatalf("a notification was answered:\n%s", body)
	}
}
