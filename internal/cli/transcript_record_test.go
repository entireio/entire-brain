package cli

import (
	"slices"
	"strings"
	"testing"
)

// mustDecodeTranscriptRecord decodes one transcript line the way the readers
// do, failing the test when it is not a JSON object.
func mustDecodeTranscriptRecord(t *testing.T, line string) transcriptRecord {
	t.Helper()
	record, ok := decodeTranscriptRecord(line)
	if !ok {
		t.Fatalf("not a transcript record: %q", line)
	}
	return record
}

// Lines in the Entire Transcript Format go to the CLI's transcript package
// whatever order their fields come in, and so do user and assistant records
// with no wrapped turn, including ones the package rejects; native lines and
// lines with a Claude Code wrapper stay generic JSON.
func TestDecodeTranscriptRecordRoutesByFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, line string
		etf        bool
	}{
		{"format line as the CLI writes it", `{"v":1,"agent":"codex","type":"assistant","content":[{"type":"text","text":"hi"}]}`, true},
		{"format line with v last", `{"agent":"codex","content":[{"type":"text","text":"hi"}],"type":"assistant","v":1}`, true},
		{"format line naming message in its content", `{"v":1,"type":"assistant","content":[{"type":"text","text":"the \"message\" key"},{"type":"tool_use","name":"x","input":{"message":"hi"}}]}`, true},
		{"claude-code native", `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`, false},
		{"codex rollout", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}`, false},
		{"pi message", `{"type":"message","message":{"role":"assistant","content":[]}}`, false},
		{"another format version reads as an unversioned turn", `{"v":2,"type":"assistant","content":[]}`, true},
		{"non-integer v reads as an unversioned turn", `{"v":1.5,"type":"assistant","content":[]}`, true},
		{"known field of the wrong type reads as an unversioned turn", `{"v":1,"type":"assistant","agent":42,"content":[]}`, true},
		{"rejected line of another type", `{"v":2,"type":"system","content":"hi"}`, false},
		{"format head with a wrapped turn", `{"v":1,"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`, false},
		{"unversioned turn with message null", `{"type":"assistant","message":null,"content":[{"type":"text","text":"hi"}]}`, true},
		{"unversioned turn with no message", `{"type":"user","content":[{"text":"hi"}]}`, true},
		{"unversioned other type", `{"type":"system","content":"hi"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record := mustDecodeTranscriptRecord(t, tt.line)
			if got := record.etf != nil; got != tt.etf {
				t.Fatalf("decoded as format line = %v, want %v", got, tt.etf)
			}
			if (record.etf == nil) == (record.obj == nil) {
				t.Fatalf("want exactly one of etf and obj set, got etf=%v obj=%v", record.etf, record.obj)
			}
		})
	}
	for _, line := range []string{`not json`, `[1,2]`, `"text"`} {
		if _, ok := decodeTranscriptRecord(line); ok {
			t.Errorf("decodeTranscriptRecord(%q) ok, want not a record", line)
		}
	}
	if record, ok := decodeTranscriptRecord(`null`); !ok || record.etf != nil || len(record.obj) != 0 {
		t.Errorf("null = %+v, %v; want an empty native record", record, ok)
	}
}

// The transcript package normalizes v1 quirks: string content is one text
// block, null content is no blocks, and a user text block with no "type" is
// text. Every reader sees the same text the hand-written readers did.
func TestTranscriptReadersReadFormatNormalizations(t *testing.T) {
	t.Parallel()
	stringContent := mustDecodeTranscriptRecord(t, `{"v":1,"agent":"codex","type":"assistant","content":"plain answer"}`)
	if got := conversationAssistantNarrative(stringContent); !slices.Equal(got, []string{"plain answer"}) {
		t.Errorf("narrative of string content = %q", got)
	}
	if got := distillConversationText(stringContent); got != "plain answer" {
		t.Errorf("distill text of string content = %q", got)
	}
	var narrative []string
	for _, fragment := range extractHistoryJSONFragments(stringContent) {
		if fragment.Source == "assistant_message" {
			narrative = append(narrative, fragment.Text)
		}
	}
	if !slices.Equal(narrative, []string{"plain answer"}) {
		t.Errorf("history narrative of string content = %q", narrative)
	}

	nullContent := mustDecodeTranscriptRecord(t, `{"v":1,"agent":"codex","type":"assistant","content":null,"output_tokens":12}`)
	if got := conversationAssistantNarrative(nullContent); got != nil {
		t.Errorf("narrative of null content = %q, want none", got)
	}
	if got := toolCallsFromRecord(nullContent); got != nil {
		t.Errorf("tool calls of null content = %+v, want none", got)
	}
	if got := extractHistoryJSONFragments(nullContent); got != nil {
		t.Errorf("history fragments of null content = %+v, want none", got)
	}

	untypedUser := mustDecodeTranscriptRecord(t, `{"v":1,"agent":"claude-code","type":"user","content":[{"text":"always use"},{"text":"tabs"}]}`)
	if got := strings.TrimSpace(transcriptUserText(untypedUser)); got != "always use tabs" {
		t.Errorf("user text = %q", got)
	}
	if got := distillConversationText(untypedUser); got != "always use tabs" {
		t.Errorf("distill text of user line = %q", got)
	}
	if got := conversationSubstantiveRequest(untypedUser); got != "always use tabs" {
		t.Errorf("conversation request = %q", got)
	}
}

// A user or assistant line the transcript package rejects (another "v", or a
// known field of the wrong JSON type) has its top-level content read as the
// readers always did, rather than dropped as an unrecognized native record.
func TestTranscriptReadersReadRejectedFormatLines(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"wrong-typed agent": `{"v":1,"agent":42,"type":"user","content":"Fix the bug"}`,
		"another version":   `{"v":2,"agent":"codex","type":"user","content":[{"text":"Fix the bug"}]}`,
		"wrong-typed model": `{"v":1,"agent":"codex","type":"user","model":7,"content":"Fix the bug"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			record := mustDecodeTranscriptRecord(t, line)
			if got := conversationSubstantiveRequest(record); got != "Fix the bug" {
				t.Errorf("conversation request = %q", got)
			}
			if got := strings.TrimSpace(transcriptUserText(record)); got != "Fix the bug" {
				t.Errorf("user text = %q", got)
			}
			if got := distillConversationText(record); got != "Fix the bug" {
				t.Errorf("distill text = %q", got)
			}
		})
	}

	assistant := mustDecodeTranscriptRecord(t, `{"v":2,"agent":"codex","type":"assistant","content":[{"type":"text","text":"Running the tests."},{"type":"tool_use","id":"c1","name":"exec_command","input":{"cmd":"go test ./..."},"result":{"output":"Process exited with code 1\nFAIL","status":"success"}}]}`)
	if got := conversationAssistantNarrative(assistant); !slices.Equal(got, []string{"Running the tests."}) {
		t.Errorf("narrative = %q", got)
	}
	if got := conversationToolNames(assistant); !slices.Equal(got, []string{"exec_command"}) {
		t.Errorf("tool names = %q", got)
	}
	if got := toolCallsFromRecord(assistant); len(got) != 1 {
		t.Errorf("tool calls = %+v, want one", got)
	}
}

// A wrapped turn's tool_use block can inline its tool's output, as the format
// does. History mines it for code facts on either route, as it did when both
// shared one content reader.
func TestHistoryMinesInlinedResultsInWrappedTurns(t *testing.T) {
	t.Parallel()
	const result = `{"output":"state.transcriptPath = \"sessions/main\"","status":"success"}`
	for name, line := range map[string]string{
		"wrapped turn": `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"state.go"},"result":` + result + `}]}}`,
		"format line":  `{"v":1,"agent":"claude-code","type":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"state.go"},"result":` + result + `}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var facts []string
			for _, fragment := range extractHistoryJSONFragments(mustDecodeTranscriptRecord(t, line)) {
				if fragment.Source == "tool_result_fact" {
					facts = append(facts, fragment.Text)
				}
			}
			if len(facts) != 1 || !strings.Contains(facts[0], "state.transcriptPath") {
				t.Errorf("tool result facts = %q, want the inlined output's", facts)
			}
		})
	}
}
