package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The history index and the distill preprocessor route the same transcript
// record shapes; these tests pin the formats distill handles (pi "message"
// records, opencode document-form transcripts) to the history side so the two
// readers cannot silently diverge again.

func TestExtractHistoryJSONFragmentsPiMessages(t *testing.T) {
	parse := func(line string) map[string]any {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatal(err)
		}
		return obj
	}

	assistant := parse(`{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"The root cause is the resolve transcript path fallback."}]}}`)
	fragments := extractHistoryJSONFragments(assistant)
	if len(fragments) != 1 || fragments[0].Source != "assistant_message" || !strings.Contains(fragments[0].Text, "root cause") {
		t.Errorf("pi assistant turn not indexed as narrative: %+v", fragments)
	}

	// User turns yield exactly one request fragment (scan-cache v4) — present
	// for trajectory surfaces (handoff, midtask mining), still excluded from
	// general ranking — and never a narrative fragment.
	user := parse(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"make it cohesive"}]}}`)
	if fragments := extractHistoryJSONFragments(user); len(fragments) != 1 || fragments[0].Source != "user_prompt" {
		t.Errorf("pi user turn should index as exactly one user_prompt fragment: %+v", fragments)
	}

	// toolResult content is mined for code facts like Claude tool_result blocks.
	toolResult := parse(`{"type":"message","message":{"role":"toolResult","toolName":"bash","content":[{"type":"text","text":"state.transcriptPath resolved to sessions/main"}]}}`)
	fragments = extractHistoryJSONFragments(toolResult)
	if len(fragments) == 0 || fragments[0].Source != "tool_result_fact" {
		t.Errorf("pi toolResult with code-fact signal not indexed: %+v", fragments)
	}
}

func TestScanHistoryFileOpencodeDocument(t *testing.T) {
	doc := `{
  "info": {"id": "ses_x"},
  "messages": [
    {
      "info": {"role": "user"},
      "parts": [{"type": "text", "text": "Remove the header bottom border"}]
    },
    {
      "info": {"role": "assistant"},
      "parts": [
        {"type": "tool", "tool": "edit", "state": {"output": "huge tool output"}},
        {"type": "text", "text": "The root cause is the duplicated border; removed both."}
      ]
    }
  ]
}`
	dir := t.TempDir()
	path := filepath.Join(dir, "20260604T120000Z-ses_x.jsonl")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := scanHistoryFile(dir, path)
	if err != nil {
		t.Fatalf("scanHistoryFile: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("document-form transcript indexed to nothing; the line scanner cannot parse it and the document path must take over")
	}
	for _, record := range records {
		if !strings.Contains(record.Summary, "root cause") {
			t.Errorf("unexpected record from document transcript: %+v", record)
		}
		// The assistant message object opens on line 8 of the document.
		if record.Line != 8 {
			t.Errorf("record anchored to line %d, want 8 (the message object's opening line)", record.Line)
		}
		if strings.Contains(record.Summary, "huge tool output") {
			t.Errorf("tool output leaked into the index: %+v", record)
		}
	}

	// JSONL files must still go through the line scanner.
	jsonl := filepath.Join(dir, "20260604T120001Z-jsonl.jsonl")
	line := `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"The root cause is the resolve transcript path fallback."}]}}`
	if err := os.WriteFile(jsonl, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err = scanHistoryFile(dir, jsonl)
	if err != nil {
		t.Fatalf("scanHistoryFile(jsonl): %v", err)
	}
	if len(records) == 0 {
		t.Error("JSONL transcript no longer indexed after the document-path change")
	}
}

func TestFirstUserRequestPiAndOpencode(t *testing.T) {
	pi := `{"type":"message","message":{"role":"toolResult","content":[{"type":"text","text":"ls output"}]}}` + "\n" +
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"Make the dashboard cohesive"}]}}` + "\n"
	if got := firstUserRequest(pi); got != "Make the dashboard cohesive" {
		t.Errorf("firstUserRequest(pi) = %q", got)
	}

	doc := `{
  "messages": [
    {
      "info": {"role": "assistant"},
      "parts": [{"type": "text", "text": "Hello"}]
    },
    {
      "info": {"role": "user"},
      "parts": [{"type": "text", "text": "Remove the header bottom border"}]
    }
  ]
}`
	if got := firstUserRequest(doc); got != "Remove the header bottom border" {
		t.Errorf("firstUserRequest(opencode document) = %q", got)
	}
}

// TestExtractHistoryJSONFragmentsRequestRecords covers the v4 re-introduction
// of request extraction: real user turns across dialects become request-kind
// fragments, wrapper injections never do, and request records stay excluded
// from general ranking (the v3 noise decision, unchanged).
func TestExtractHistoryJSONFragmentsRequestRecords(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // expected request text, "" = no request fragment
	}{
		{"claude user turn", `{"type":"user","message":{"content":"add retry backoff"}}`, "add retry backoff"},
		{"codex user_message", `{"type":"event_msg","payload":{"type":"user_message","message":"why does flush stay dirty"}}`, "why does flush stay dirty"},
		{"pi user turn", `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"make it cohesive"}]}}`, "make it cohesive"},
		{"wrapper filtered", `{"type":"user","message":{"content":"<local-command-caveat>Caveat: local commands"}}`, ""},
		{"tool result is not a request", `{"type":"user","message":{"content":[{"type":"tool_result","content":"blob"}]}}`, ""},
		{"assistant is not a request", `{"type":"assistant","message":{"content":[{"type":"text","text":"decided to keep it"}]}}`, ""},
	}
	for _, c := range cases {
		var obj map[string]any
		if err := json.Unmarshal([]byte(c.line), &obj); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []historyFragment
		for _, f := range extractHistoryJSONFragments(obj) {
			if f.Source == "user_prompt" {
				got = append(got, f)
			}
		}
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: unexpected request fragment %+v", c.name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].Text != c.want {
			t.Errorf("%s: request fragment = %+v, want text %q", c.name, got, c.want)
		}
		if kinds := classifyHistoryFragment(got[0]); len(kinds) != 1 || kinds[0] != "request" {
			t.Errorf("%s: user_prompt should classify as exactly [request], got %v", c.name, kinds)
		}
	}

	// The general ranking arm must keep excluding request records.
	index := historyIndex{Records: []historyRecord{
		{ID: "r1", Kind: "request", Path: "p", Line: 1, Summary: "add retry backoff to the fetcher"},
		{ID: "r2", Kind: "decision", Path: "p", Line: 2, Summary: "retry backoff doubles per attempt"},
	}}
	for _, r := range rankHistoryRecordsScored(index, "history", "retry backoff", 10, 0) {
		if r.Record.Kind == "request" {
			t.Fatalf("request record leaked into general ranking: %+v", r.Record)
		}
	}
}
