package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// transcriptLineHead is what the first bytes of a transcript line say about
// the record, for a line too long to read whole (historyLineScanner.Head).
// Every dialect transcriptUserText reads puts the record's type, the role and
// the first content block before the bulk of the line, which is what makes a
// head enough.
type transcriptLineHead struct {
	recordType  string // top-level "type"
	payloadType string // codex "payload.type"
	role        string // claude and pi "message.role", codex "payload.role"
	// content is set once the turn's content starts inside the head: a
	// string, or a list whose first block is described by blockType and text.
	content   bool
	blockType string
	// text is the start of the content's first text, as far as the head
	// carries it; empty when the first block has no text.
	text string
}

// transcriptLineHeadTextBytes bounds the text a head keeps: the wrapper and
// injected-request checks only look at how the text starts.
const transcriptLineHeadTextBytes = 256

// unreadLineIsUserTurn reports whether a line too long to read is probably a
// real user turn, judged from its head with the dialects and filters of
// conversationSubstantiveRequest: a claude, Entire Transcript Format, pi or
// codex user message whose first block is not a tool result and whose text,
// as far as the head shows it, is not an injected wrapper. A user pasting a
// large screenshot is the usual case. A tool result (a screenshot Read), an
// assistant line, a line whose head does not show its type, role and content,
// and anything else is not one.
func unreadLineIsUserTurn(head []byte) bool {
	return readTranscriptLineHead(head).userTurn()
}

func (h transcriptLineHead) userTurn() bool {
	switch h.recordType {
	case "user": // claude-code nests the turn under "message"; the format does not
	case "message": // pi
		if h.role != "user" {
			return false
		}
	case "response_item": // codex
		if h.payloadType != "message" || h.role != "user" {
			return false
		}
	case "event_msg": // codex
		if h.payloadType != "user_message" {
			return false
		}
	default:
		return false
	}
	if !h.content || h.blockType == "tool_result" {
		return false
	}
	text := strings.TrimSpace(h.text)
	return text == "" || (!isWrapperRequest(text) && !isConversationInjectedRequest(text))
}

// requestStart is the start of the turn's request text, normalized like a
// readable request, so a readable copy of the same turn starts with it. Empty
// when the head shows no text.
func (h transcriptLineHead) requestStart() string {
	return normalizeConversationText(h.text)
}

// readTranscriptLineHead reads the record's type, role and first content
// block from a line's head, stopping where the head ends or the content
// starts, whichever comes first.
func readTranscriptLineHead(head []byte) transcriptLineHead {
	r := transcriptHeadReader{head: head, dec: json.NewDecoder(bytes.NewReader(head))}
	var h transcriptLineHead
	r.object(func(key string) bool {
		switch key {
		case "type":
			return r.str(&h.recordType)
		case "content": // the format, and unversioned turns
			return r.content(&h)
		case "message", "payload":
			if r.next() != '{' {
				return r.skip()
			}
			return r.object(func(key string) bool {
				switch key {
				case "type":
					return r.str(&h.payloadType)
				case "role":
					return r.str(&h.role)
				case "content", "message": // codex event_msg carries its text as "message"
					return r.content(&h)
				}
				return r.skip()
			})
		}
		return r.skip()
	})
	return h
}

// transcriptHeadReader walks the JSON of a line's head. Every method returns
// false when the head ends, or stops being the JSON it claims, before the
// value does; the walk then stops with what it has read so far.
type transcriptHeadReader struct {
	head []byte
	dec  *json.Decoder
}

// next returns the first byte of the next value, or 0 when the head ends
// first.
func (r *transcriptHeadReader) next() byte {
	for _, c := range r.head[r.dec.InputOffset():] {
		switch c {
		case ' ', '\t', '\r', '\n', ':', ',':
			continue
		}
		return c
	}
	return 0
}

// object walks the members of the object that comes next, handing each key to
// member, which must read or skip its value.
func (r *transcriptHeadReader) object(member func(key string) bool) bool {
	if tok, err := r.dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for r.dec.More() {
		tok, err := r.dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok || !member(key) {
			return false
		}
	}
	_, err := r.dec.Token()
	return err == nil
}

func (r *transcriptHeadReader) skip() bool {
	var discard json.RawMessage
	return r.dec.Decode(&discard) == nil
}

// str reads a string value into out; a value of another type is skipped.
func (r *transcriptHeadReader) str(out *string) bool {
	if r.next() != '"' {
		return r.skip()
	}
	tok, err := r.dec.Token()
	s, ok := tok.(string)
	if err != nil || !ok {
		return false
	}
	*out = s
	return true
}

// content reads the start of a turn's content: a string, or the first block of
// a list. It always ends the walk: nothing after the first block matters.
func (r *transcriptHeadReader) content(h *transcriptLineHead) bool {
	switch r.next() {
	case '"':
		h.content = true
		h.text = r.text()
	case '[':
		if _, err := r.dec.Token(); err != nil {
			return false
		}
		if r.next() != '{' {
			return false
		}
		h.content = true
		r.object(func(key string) bool {
			switch key {
			case "type":
				return r.str(&h.blockType)
			case "text", "input_text":
				h.text = r.text()
				return false
			}
			return r.skip()
		})
	}
	return false
}

// text reads the start of the string that comes next: all of it when the head
// carries it, else as much as the head shows, up to
// transcriptLineHeadTextBytes.
func (r *transcriptHeadReader) text() string {
	if r.next() != '"' {
		return ""
	}
	start := int(r.dec.InputOffset())
	start += bytes.IndexByte(r.head[start:], '"') + 1
	var s string
	if err := r.dec.Decode(&s); err == nil {
		return s
	}
	raw := r.head[start:]
	if len(raw) > transcriptLineHeadTextBytes {
		raw = raw[:transcriptLineHeadTextBytes]
	}
	// Do not end inside an escape sequence, the longest is \uXXXX, or inside
	// a UTF-8 sequence, which would decode as a replacement character.
	if i := bytes.LastIndexByte(raw, '\\'); i >= 0 && i >= len(raw)-6 {
		raw = raw[:i]
	}
	for n := 0; n < utf8.UTFMax && len(raw) > 0 && !utf8.Valid(raw); n++ {
		raw = raw[:len(raw)-1]
	}
	quoted := make([]byte, 0, len(raw)+2)
	quoted = append(append(append(quoted, '"'), raw...), '"')
	if err := json.Unmarshal(quoted, &s); err != nil {
		return ""
	}
	return s
}
