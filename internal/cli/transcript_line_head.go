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
	// text is the start of the content's text, as far as the head carries it,
	// read the way transcriptContentText reads content: a string, or the text
	// of each block of a list (its "text", or its "input_text" when the text
	// is blank), joined with spaces. Empty when the head ends, or the content
	// does, before any text.
	text string
}

// add joins one text to the head's text the way transcriptContentText joins
// blocks; blank text adds nothing.
func (h *transcriptLineHead) add(text string) {
	if strings.TrimSpace(text) != "" {
		h.text += text + " "
	}
}

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

// readTranscriptLineHead reads the record's type, role and text from a line's
// head, as far as the head carries them.
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

// content reads a turn's content the way transcriptContentText does. Blocks
// without text, such as an image or a tool result, are skipped while the head
// carries them.
func (r *transcriptHeadReader) content(h *transcriptLineHead) bool {
	switch r.next() {
	case '"':
		var text string
		whole := r.text(&text)
		h.add(text)
		return whole
	case '[':
		return r.blocks(h)
	}
	return r.skip()
}

// blocks reads the text of each block of a content list.
func (r *transcriptHeadReader) blocks(h *transcriptLineHead) bool {
	if _, err := r.dec.Token(); err != nil {
		return false
	}
	for r.dec.More() {
		if !r.block(h) {
			return false
		}
	}
	_, err := r.dec.Token()
	return err == nil
}

// block reads one content block's text: its "text", or its "input_text" when
// the text is blank, whatever order they come in. A block that is not an
// object is skipped.
func (r *transcriptHeadReader) block(h *transcriptLineHead) bool {
	if r.next() != '{' {
		return r.skip()
	}
	var text, inputText string
	whole := r.object(func(key string) bool {
		switch key {
		case "text":
			return r.text(&text)
		case "input_text":
			return r.text(&inputText)
		}
		return r.skip()
	})
	if strings.TrimSpace(text) == "" {
		text = inputText
	}
	h.add(text)
	return whole
}

// text reads a string value into out, whole or, when the head ends inside it,
// as far as the head carries it; the walk then ends. A value of another type
// is skipped and leaves out empty.
func (r *transcriptHeadReader) text(out *string) bool {
	if r.next() != '"' {
		return r.skip()
	}
	start := int(r.dec.InputOffset())
	start += bytes.IndexByte(r.head[start:], '"') + 1
	if err := r.dec.Decode(out); err != nil {
		*out = decodeJSONStringStart(r.head[start:])
		return false
	}
	return true
}

// decodeJSONStringStart decodes the start of a JSON string body that the head
// cuts off, ending before an escape or a UTF-8 sequence the cut would split.
// "" when the bytes are not a string body.
func decodeJSONStringStart(raw []byte) string {
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
