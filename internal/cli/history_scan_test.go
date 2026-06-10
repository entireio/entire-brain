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

	// User turns are skipped, mirroring codex user_message routing.
	user := parse(`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"make it cohesive"}]}}`)
	if fragments := extractHistoryJSONFragments(user); len(fragments) != 0 {
		t.Errorf("pi user turn should not be indexed: %+v", fragments)
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
