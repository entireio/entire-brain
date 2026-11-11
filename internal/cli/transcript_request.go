package cli

// Transcript-reading helpers shared by the agent surface (conversation,
// history, handoff, reinforcement, recall evidence) and the brain_harness
// eval tooling. Untagged so release builds keep them (COR-2322).

import (
	"strings"

	"github.com/entireio/cli/transcript"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

func shortSessionID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// firstUserRequest recovers the first substantive user request from a session
// transcript, skipping injected wrappers (environment context, reminders, tool
// results). Returns "" when none is found. Supports the Codex (response_item /
// event_msg), Claude (user), pi (message) and opencode (document-form)
// transcript shapes.
func firstUserRequest(transcript string) string {
	// Document-form transcripts (e.g. opencode) have no parseable lines; use
	// the shared document parser the distill path uses.
	if messages, ok := parseDocumentConversation(transcript); ok {
		for _, message := range messages {
			// message.Text arrives already whitespace-collapsed from the parser.
			if message.Role != "user" || message.Text == "" || isWrapperRequest(message.Text) {
				continue
			}
			return base.TruncateString(message.Text, 280)
		}
		return ""
	}
	for _, line := range strings.Split(transcript, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		record, ok := decodeTranscriptRecord(line)
		if !ok {
			continue
		}
		text := strings.TrimSpace(transcriptUserText(record))
		if text == "" || isWrapperRequest(text) {
			continue
		}
		return base.TruncateString(strings.Join(strings.Fields(text), " "), 280)
	}
	return ""
}

func transcriptUserText(record transcriptRecord) string {
	if record.etf != nil {
		if !record.etfIs(transcript.TypeUser) {
			return ""
		}
		return strings.Join(etfTexts(record.etf), " ")
	}
	obj := record.obj
	payload := jsonMap(obj["payload"])
	switch jsonString(obj["type"]) {
	case "response_item":
		if jsonString(payload["type"]) == "message" && jsonString(payload["role"]) == "user" {
			return transcriptContentText(payload["content"])
		}
	case "event_msg":
		if jsonString(payload["type"]) == "user_message" {
			return jsonString(payload["message"])
		}
	case "user":
		// claude-code nests the turn under "message".
		return transcriptContentText(jsonMap(obj["message"])["content"])
	case "message":
		// pi wraps every turn as {"type":"message","message":{role,content}};
		// keep user turns (same blind spot distillConversationText covers).
		if msg := jsonMap(obj["message"]); jsonString(msg["role"]) == "user" {
			return transcriptContentText(msg["content"])
		}
	}
	return ""
}

func transcriptContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			pm := jsonMap(part)
			if t := firstNonEmptyString(pm["text"], pm["input_text"]); t != "" {
				b.WriteString(t)
				b.WriteByte(' ')
			}
		}
		return b.String()
	}
	return ""
}

// isWrapperRequest reports whether a user-message body is an injected wrapper
// (environment context block, system reminder, tool result, slash-command
// envelope) rather than a real request.
func isWrapperRequest(text string) bool {
	if strings.HasPrefix(text, "<") {
		return true
	}
	lower := strings.ToLower(text)
	return strings.HasPrefix(lower, "caveat:") ||
		strings.HasPrefix(lower, "[request interrupted") ||
		strings.HasPrefix(lower, "command-name") ||
		strings.Contains(text[:min(len(text), 40)], "environment_context")
}
