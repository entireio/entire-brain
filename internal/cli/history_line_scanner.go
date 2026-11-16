package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// historyLineScanner reads a line-oriented transcript exactly the way a
// bufio.Scanner with ScanLines and a maxLine buffer does, with one difference:
// a line too long for the buffer is skipped instead of ending the scan.
//
// The plain scanner stopped at the first such line with bufio.ErrTooLong, and
// every caller treated that as "this transcript cannot be read", so one
// oversized line cost the whole session: its history records, its
// conversation exchanges, and every expansion of them. Those lines are real
// and common. A Claude Code image Read stores the base64 image twice, and a
// Codex command stores its output twice (stdout and aggregated_output), so a
// screenshot or a large command output of about half a megabyte is enough.
//
// A skipped line still counts as one line, so every line after it keeps the
// number it would have had: history record IDs, exchange ranges and the
// expansion's range re-read all anchor to line numbers. Its bytes are still
// read and discarded, never buffered, so a reader that hashes the stream (the
// conversation digest) still covers them.
type historyLineScanner struct {
	scanner *bufio.Scanner
	maxLine int
	// discarding is set while the split function drops the rest of a line
	// that did not fit the buffer.
	discarding bool
	// skipToken is set by the split function when the token it returns is
	// the placeholder for a skipped line.
	skipToken bool
	oversized bool
	skipped   int
}

// historyOversizedLineToken stands in for a skipped line. It is empty and
// non-nil, so a caller that forgets to check Oversized sees an empty line,
// which every transcript reader already ignores.
var historyOversizedLineToken = []byte{}

func newHistoryLineScanner(r io.Reader) *historyLineScanner {
	return newBoundedHistoryLineScanner(r, historyMaxLineBytes)
}

func newBoundedHistoryLineScanner(r io.Reader, maxLine int) *historyLineScanner {
	s := &historyLineScanner{maxLine: maxLine}
	s.scanner = bufio.NewScanner(r)
	s.scanner.Buffer(make([]byte, 0, min(64*1024, maxLine)), maxLine)
	s.scanner.Split(s.split)
	return s
}

func (s *historyLineScanner) split(data []byte, atEOF bool) (int, []byte, error) {
	if s.discarding {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			s.discarding = false
			s.skipToken = true
			return i + 1, historyOversizedLineToken, nil
		}
		if atEOF {
			// The oversized line was the last one and had no newline.
			s.discarding = false
			s.skipToken = true
			return len(data), historyOversizedLineToken, nil
		}
		return len(data), nil, nil
	}
	advance, token, err := bufio.ScanLines(data, atEOF)
	if advance == 0 && token == nil && err == nil && len(data) >= s.maxLine {
		// A full buffer with no line end: this is the line a plain scanner
		// rejects with bufio.ErrTooLong. Drop what is buffered and the rest of
		// the line as it arrives.
		s.discarding = true
		return len(data), nil, nil
	}
	return advance, token, err
}

// Scan advances to the next line, skipped or not. It returns false at the end
// of the input or on a read error; Err reports which.
func (s *historyLineScanner) Scan() bool {
	s.oversized = false
	s.skipToken = false
	if !s.scanner.Scan() {
		return false
	}
	if s.skipToken {
		s.skipToken = false
		s.oversized = true
		s.skipped++
	}
	return true
}

// Text returns the current line. It is empty for a skipped line.
func (s *historyLineScanner) Text() string { return s.scanner.Text() }

// Oversized reports that the current line was too long to read and was
// skipped.
func (s *historyLineScanner) Oversized() bool { return s.oversized }

// Skipped is the number of lines skipped so far.
func (s *historyLineScanner) Skipped() int { return s.skipped }

func (s *historyLineScanner) Err() error { return s.scanner.Err() }

// historySkippedLinesWarning names the transcript that lost lines, so a reader
// can tell which session's history is partial and why.
func historySkippedLinesWarning(rel string, skipped int) string {
	noun := "lines"
	if skipped == 1 {
		noun = "line"
	}
	return fmt.Sprintf("history skipped %d transcript %s of %d bytes or more in %s; indexed the rest of the session", skipped, noun, historyMaxLineBytes, rel)
}
