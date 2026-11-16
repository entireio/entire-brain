package cli

import (
	"strings"
	"testing"
)

// A head is the first bytes of a line too long to read; the bulk of every case
// here is past it, as it is for a real oversized line.
func TestUnreadLineIsUserTurn(t *testing.T) {
	t.Parallel()
	bulk := strings.Repeat("A", 8*1024)
	const headBytes = 4 * 1024
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"claude pasted screenshot, text first", `{"parentUuid":"p","isSidechain":false,"cwd":"/repo","type":"user","message":{"role":"user","content":[{"type":"text","text":"What does this show?"},{"type":"image","source":{"type":"base64","data":"` + bulk + `"}}]}}`, true},
		{"claude pasted screenshot, text past the head", `{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"` + bulk + `"}},{"type":"text","text":"What does this show?"}]}}`, false},
		{"claude small image, then text", `{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"iVBOR"}},{"type":"text","text":"What does this show? ` + bulk + `"}]}}`, true},
		{"claude small image, then a reminder", `{"type":"user","message":{"role":"user","content":[{"type":"image","source":{"data":"iVBOR"}},{"type":"text","text":"<system-reminder>` + bulk + `"}]}}`, false},
		{"claude blank text, then text", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"  "},{"type":"text","text":"What does this show? ` + bulk + `"}]}}`, true},
		{"claude request split across text blocks", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Please review"},{"type":"text","text":"this log: ` + bulk + `"}]}}`, true},
		{"claude wrapper split across text blocks", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Stop hook"},{"type":"text","text":"feedback: ` + bulk + `"}]}}`, false},
		{"claude long blank text, then text", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + strings.Repeat(" ", 300) + `"},{"type":"text","text":"What does this show? ` + bulk + `"}]}}`, true},
		{"block with blank text and an input_text", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":" ","input_text":"What does this show? ` + bulk + `"}]}}`, true},
		{"block whose text is a reminder after its input_text", `{"type":"user","message":{"role":"user","content":[{"input_text":"What does this show?","text":"<system-reminder>x"},{"type":"image","source":{"data":"` + bulk + `"}}]}}`, false},
		{"claude small tool result, then user text", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"},{"type":"text","text":"Now look at this: ` + bulk + `"}]}}`, true},
		{"claude tool result, content before type", `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","content":[{"type":"image","source":{"data":"` + bulk + `"}}],"type":"tool_result"}]}}`, false},
		{"claude pasted text", `{"type":"user","message":{"role":"user","content":"Please review this log: ` + bulk + `"}}`, true},
		{"claude pasted text with escapes", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"line one\nline \"two\" é` + bulk + `"}]}}`, true},
		{"claude screenshot read (tool result)", `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":[{"type":"image","source":{"data":"` + bulk + `"}}]}]}}`, false},
		{"claude command output wrapper", `{"type":"user","message":{"role":"user","content":"<local-command-stdout>` + bulk + `"}}`, false},
		{"claude stop hook feedback", `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Stop hook feedback: ` + bulk + `"}]}}`, false},
		{"claude assistant write", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"content":"` + bulk + `"}}]}}`, false},
		{"transcript format user", `{"v":1,"type":"user","content":[{"text":"What does this show?"},{"type":"image","data":"` + bulk + `"}]}`, true},
		{"pi user", `{"type":"message","id":"m1","message":{"role":"user","content":[{"type":"text","text":"What does this show? ` + bulk + `"}]}}`, true},
		{"pi role after its content", `{"type":"message","message":{"content":[{"type":"text","text":"What does this show?"}],"role":"user","details":"` + bulk + `"}}`, true},
		{"pi tool result", `{"type":"message","id":"m1","message":{"role":"toolResult","content":[{"type":"text","text":"` + bulk + `"}]}}`, false},
		{"codex user message", `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"What does this show?"},{"type":"input_image","image_url":"data:` + bulk + `"}]}}`, true},
		{"codex environment context", `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>` + bulk + `"}]}}`, false},
		{"codex command output", `{"type":"response_item","payload":{"type":"function_call_output","call_id":"c","output":"` + bulk + `"}}`, false},
		{"codex user event", `{"type":"event_msg","payload":{"type":"user_message","message":"What does this show?","images":["data:` + bulk + `"]}}`, true},
		{"codex command event", `{"type":"event_msg","payload":{"type":"exec_command_end","aggregated_output":"` + bulk + `"}}`, false},
		{"type past the head", `{"meta":"` + bulk + `","type":"user","message":{"role":"user","content":"hi"}}`, false},
		{"not json", `user: ` + bulk, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.line) <= headBytes {
				t.Fatal("case must run past the head")
			}
			head := readTranscriptLineHead([]byte(tc.line[:headBytes]))
			if got := unreadLineIsUserTurn([]byte(tc.line[:headBytes])); got != tc.want {
				t.Fatalf("unreadLineIsUserTurn = %v, want %v (head %+v)", got, tc.want, head)
			}
			if !tc.want {
				return
			}
			// What the head reads is how the whole line's request, read as a
			// readable line, starts.
			record, ok := decodeTranscriptRecord(tc.line)
			if !ok {
				t.Fatal("the whole line does not decode")
			}
			request := conversationSubstantiveRequest(record)
			if start := head.requestStart(); start == "" || !strings.HasPrefix(request, start) {
				t.Fatalf("request start %q does not begin the line's request %.80q", start, request)
			}
		})
	}
}

// A head can end inside an escape sequence; the text before it still counts.
func TestUnreadLineHeadEndingInsideAnEscape(t *testing.T) {
	t.Parallel()
	head := `{"type":"user","message":{"role":"user","content":"Stop hook feedback: \u00`
	if got := readTranscriptLineHead([]byte(head)).requestStart(); got != "Stop hook feedback:" {
		t.Fatalf("text = %q, want the readable start", got)
	}
	if unreadLineIsUserTurn([]byte(head)) {
		t.Fatal("an injected stop-hook turn was read as a user turn")
	}
	// A complete escaped backslash at the cut stays, and so does the text.
	for _, tc := range []struct{ body, want string }{
		{`C:\\`, `C:\`},
		{`C:\\\`, `C:\`},
		{`a\"b\`, `a"b`},
		{`a\u00e9\u00`, "a\u00e9"},
	} {
		if got := decodeJSONStringStart([]byte(tc.body)); got != tc.want {
			t.Fatalf("decodeJSONStringStart(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
	wrapped := `{"type":"user","message":{"role":"user","content":"<local-command-stdout>C:\\`
	if unreadLineIsUserTurn([]byte(wrapped)) {
		t.Fatal("a wrapper cut after an escaped backslash was read as a user turn")
	}
	// Or inside a multi-byte character: the text stops before it rather than
	// ending in a replacement character.
	cut := `{"type":"user","message":{"role":"user","content":"Here is caf` + "\xc3"
	if got := readTranscriptLineHead([]byte(cut)).requestStart(); got != "Here is caf" {
		t.Fatalf("request start = %q, want the text before the cut character", got)
	}
}

// The scanner keeps the head of each line it skips, and only of those.
func TestHistoryLineScannerKeepsHeadOfSkippedLine(t *testing.T) {
	t.Parallel()
	content := oversizedLineTranscript()
	lines := strings.Split(content, "\n")
	scanner := newHistoryLineScanner(strings.NewReader(content))
	for i := 0; scanner.Scan(); i++ {
		head := scanner.Head()
		if !scanner.Oversized() {
			if head != nil {
				t.Fatalf("line %d: head %q on a line that was read", i+1, head)
			}
			continue
		}
		if len(head) != historyOversizedLineHeadBytes || string(head) != lines[i][:historyOversizedLineHeadBytes] {
			t.Fatalf("line %d: head is %d bytes, want the line's first %d", i+1, len(head), historyOversizedLineHeadBytes)
		}
	}
	if err := scanner.Err(); err != nil || scanner.Skipped() != 1 {
		t.Fatalf("err = %v skipped = %d, want one skipped line", err, scanner.Skipped())
	}
}
