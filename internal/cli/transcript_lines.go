package cli

import (
	"bufio"
	"io"
	"strings"
)

// transcriptLines bounds retained bytes while draining oversized physical lines.
// The cap excludes LF and an optional preceding CR.
type transcriptLines struct {
	r       *bufio.Reader
	limit   int
	line    int
	skipped int
	text    string
	err     error
}

func newTranscriptLines(r io.Reader, limit int) *transcriptLines {
	return &transcriptLines{r: bufio.NewReaderSize(r, 64*1024), limit: limit}
}

func (s *transcriptLines) Scan() bool {
	for s.err == nil {
		var data []byte
		oversized := false
		for {
			fragment, err := s.r.ReadSlice('\n')
			if !oversized {
				if len(data)+len(fragment) > s.limit+2 {
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
				s.skipped++
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
