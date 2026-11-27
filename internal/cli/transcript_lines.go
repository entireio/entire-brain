package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// transcriptLines bounds retained bytes while draining oversized physical lines.
// The cap excludes LF and an optional preceding CR.
type transcriptLines struct {
	r               *bufio.Reader
	limit           int
	line            int
	skipped         int
	lastUnsafeLine  int
	firstUnsafeLine int
	text            string
	err             error
}

func newTranscriptLines(r io.Reader, limit int) *transcriptLines {
	return &transcriptLines{r: bufio.NewReaderSize(r, 64*1024), limit: limit}
}

func (s *transcriptLines) Scan() bool {
	s.firstUnsafeLine = 0
	for s.err == nil {
		var data []byte
		var prefix []byte
		oversized := false
		for {
			fragment, err := s.r.ReadSlice('\n')
			if !oversized {
				if len(data)+len(fragment) > s.limit+2 {
					// Preserve metadata only when discarding an oversized line;
					// normal lines need no separate prefix allocation or copy.
					prefix = append(prefix, data[:min(len(data), 4096)]...)
					prefix = append(prefix, fragment[:min(len(fragment), 4096-len(prefix))]...)
					oversized = true
					data = nil
				} else {
					data = append(data, fragment...)
				}
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil && err != io.EOF {
				s.err = err
				return false
			}
			if len(fragment) == 0 && len(data) == 0 && !oversized && err == io.EOF {
				s.err = io.EOF
				return false
			}
			s.line++
			if err == io.EOF {
				s.err = io.EOF
			}
			data = []byte(strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"))
			if oversized || len(data) > s.limit {
				if !oversized {
					prefix = data[:min(len(data), 4096)]
				}
				s.skipped++
				if !transcriptNoisePrefix(prefix) {
					s.lastUnsafeLine = s.line
					if s.firstUnsafeLine == 0 {
						s.firstUnsafeLine = s.line
					}
				}
				break
			}
			s.text = string(data)
			return true
		}
	}
	return false
}
func (s *transcriptLines) Text() string { return s.text }
func (s *transcriptLines) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}

// A truncated first line is JSONL, not evidence of a pretty JSON document.
func transcriptDocumentProbe(r *bufio.Reader) (string, error) {
	p, err := r.Peek(historyMaxLineBytes + 2)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return "", err
	}
	first, _, found := strings.Cut(strings.TrimLeft(string(p), " \t\r\n"), "\n")
	if (!found && len(p) > historyMaxLineBytes) || len(strings.TrimSuffix(first, "\r")) > historyMaxLineBytes {
		return "", nil
	}
	return strings.TrimSpace(first), nil
}

// Only positive leading metadata can make a discarded line safe. Decoding is
// confined to the retained prefix; large or incomplete preceding values fail closed.
func transcriptNoisePrefix(prefix []byte) bool {
	d := json.NewDecoder(bytes.NewReader(prefix))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return false
	}
	kind, payloadKind := "", ""
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return false
		}
		switch key {
		case "type":
			if d.Decode(&kind) != nil {
				return false
			}
		case "payload":
			t, err = d.Token()
			if err != nil || t != json.Delim('{') {
				return false
			}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return false
				}
				if k == "type" {
					if d.Decode(&payloadKind) != nil {
						return false
					}
				} else {
					var value json.RawMessage
					if d.Decode(&value) != nil {
						return false
					}
				}
				if kind == "event_msg" && payloadKind == "item_completed" {
					return true
				}
			}
			if _, err = d.Token(); err != nil {
				return false
			}
		default:
			var value json.RawMessage
			if d.Decode(&value) != nil {
				return false
			}
		}
		if kind == "compacted" || kind == "event_msg" && payloadKind == "item_completed" {
			return true
		}
	}
	return false
}
