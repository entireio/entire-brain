package cli

import (
	"encoding/json"
	"strings"
)

// documentMessage is one message of a document-form transcript as extracted by
// parseDocumentConversation.
type documentMessage struct {
	Role string // the message's info.role ("user", "assistant", ...); "" when absent
	Text string // whitespace-collapsed conversation text; "" for tool-only messages
	Line int    // 1-based line of the message object's opening brace in the document
}

// parseDocumentConversation extracts conversation messages from a document-form
// transcript whose shape is {info, messages: [{info, parts: [{type, text}]}]}.
// It returns ok=false for JSONL transcripts and unknown document shapes so the
// caller can fall back to line-oriented parsing.
func parseDocumentConversation(content string) ([]documentMessage, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	firstLine, _, _ := strings.Cut(trimmed, "\n")
	if json.Valid([]byte(firstLine)) {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(content))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	line, pos := 1, 0
	lineAt := func(offset int) int {
		line += strings.Count(content[pos:offset], "\n")
		pos = offset
		return line
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		if key != "messages" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, false
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return nil, false
		}
		var messages []documentMessage
		for dec.More() {
			start := int(dec.InputOffset())
			for start < len(content) && isJSONWhitespaceOrComma(content[start]) {
				start++
			}
			msgLine := lineAt(start)
			var msg struct {
				Info struct {
					Role string `json:"role"`
				} `json:"info"`
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			if err := dec.Decode(&msg); err != nil {
				return nil, false
			}
			var words []string
			for _, part := range msg.Parts {
				if part.Type != "text" || part.Text == "" {
					continue
				}
				words = append(words, strings.Fields(part.Text)...)
			}
			messages = append(messages, documentMessage{
				Role: msg.Info.Role,
				Text: strings.Join(words, " "),
				Line: msgLine,
			})
		}
		for _, message := range messages {
			if message.Text != "" {
				return messages, true
			}
		}
		return nil, false
	}
	return nil, false
}

func isJSONWhitespaceOrComma(b byte) bool {
	return b == ',' || b == ' ' || b == '\t' || b == '\r' || b == '\n'
}
