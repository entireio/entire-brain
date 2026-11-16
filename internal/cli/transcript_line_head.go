package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// transcriptLineHead is what the first bytes of a transcript line say about
// the record, for a line too long to read whole (historyLineScanner.Head).
// Every dialect transcriptUserText reads puts the record's type and role
// before the content, which is what makes a head useful; whether the head
// also reaches the content's text depends on what comes first.
type transcriptLineHead struct {
	recordType  string // top-level "type"
	payloadType string // codex "payload.type"
	role        string // claude and pi "message.role", codex "payload.role"
	// text is the start of the content's first non-blank text, as far as the
	// head carries it, read the way transcriptContentText reads content: a
	// string, or the "text" or "input_text" of any block of a list. Empty when
	// the head ends, or the content does, before any text.
	text string
}

// transcriptLineHeadTextBytes bounds the text a head keeps: the wrapper and
// injected-request checks only look at how the text starts.
const transcriptLineHeadTextBytes = 256

// unreadLineIsUserTurn reports whether a line too long to read is a real user
// turn, judged from its head the way conversationSubstantiveRequest judges a
// readable line: a claude, Entire Transcript Format, pi or codex user record
// whose text, as far as the head shows it, starts with something other than
// an injected wrapper. A user pasting a large screenshot after their text is
// the usual case. A tool result (a screenshot Read), an assistant line, and a
// head that ends before the record's type, role or first text are not: when
// the head cannot show that the line is a request, the scan treats it as
// content of the open exchange, as it did before.
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
	text := strings.TrimSpace(h.text)
	return text != "" && !isWrapperRequest(text) && !isConversationInjectedRequest(text)
}

// requestStart is the start of the turn's request text, normalized like a
// readable request, so a readable copy of the same turn starts with it.
func (h transcriptLineHead) requestStart() string {
	return normalizeConversationText(h.text)
}

// readTranscriptLineHead reads the record's type, role and first text from a
// line's head, stopping at the first text or where the head ends.
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
// value does, or when the walk has found the text it looks for; the walk then
// stops with what it has read so far.
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

// content looks for the first non-blank text of a turn's content. Blocks
// before it, such as an image or a tool result, are skipped while the head
// carries them.
func (r *transcriptHeadReader) content(h *transcriptLineHead) bool {
	switch r.next() {
	case '"':
		return r.textInto(h)
	case '[':
		if _, err := r.dec.Token(); err != nil {
			return false
		}
		for r.dec.More() {
			if r.next() != '{' {
				if !r.skip() {
					return false
				}
				continue
			}
			if !r.object(func(key string) bool {
				if key == "text" || key == "input_text" {
					return r.textInto(h)
				}
				return r.skip()
			}) {
				return false
			}
		}
		_, err := r.dec.Token()
		return err == nil
	}
	return r.skip()
}

// textInto reads a text value. Non-blank text, whole or as far as the head
// shows it, goes into h.text and ends the walk; blank or non-string text is
// passed over.
func (r *transcriptHeadReader) textInto(h *transcriptLineHead) bool {
	if r.next() != '"' {
		return r.skip()
	}
	start := int(r.dec.InputOffset())
	start += bytes.IndexByte(r.head[start:], '"') + 1
	var s string
	if err := r.dec.Decode(&s); err == nil {
		if strings.TrimSpace(s) == "" {
			return true
		}
		h.text = s
		return false
	}
	h.text = decodeJSONStringStart(r.head[start:])
	return false
}

// decodeJSONStringStart decodes the start of a JSON string body that the head
// cuts off, up to transcriptLineHeadTextBytes, ending before an escape or a
// UTF-8 sequence the cut would split. "" when the bytes are not a string body.
func decodeJSONStringStart(raw []byte) string {
	if len(raw) > transcriptLineHeadTextBytes {
		raw = raw[:transcriptLineHeadTextBytes]
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		size := 2
		if i+1 < len(raw) && raw[i+1] == 'u' {
			size = 6
		}
		if i+size > len(raw) {
			raw = raw[:i]
			break
		}
		i += size - 1
	}
	for n := 0; n < utf8.UTFMax && len(raw) > 0 && !utf8.Valid(raw); n++ {
		raw = raw[:len(raw)-1]
	}
	quoted := make([]byte, 0, len(raw)+2)
	quoted = append(append(append(quoted, '"'), raw...), '"')
	var s string
	if err := json.Unmarshal(quoted, &s); err != nil {
		return ""
	}
	return s
}
