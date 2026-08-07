package factmerge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// MaxLineBytes bounds a single NDJSON record line so a corrupt store cannot
// exhaust memory while scanning.
const MaxLineBytes = 64 * 1024

// ParseError reports a malformed line in an NDJSON fact stream. It carries the
// 1-based line number so callers that know the file name can reconstruct a
// path-qualified message; Unwrap exposes the underlying decode error.
type ParseError struct {
	Line int
	Err  error
}

func (e *ParseError) Error() string { return fmt.Sprintf("parse line %d: %v", e.Line, e.Err) }

func (e *ParseError) Unwrap() error { return e.Err }

// ParseNDJSON reads newline-delimited JSON fact records from r. Blank lines are
// skipped; a malformed line is a hard error (a *ParseError) so a corrupt store
// is surfaced rather than silently dropping records. It performs no file or
// path handling — callers open the reader.
func ParseNDJSON(r io.Reader) ([]Record, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 8*1024), MaxLineBytes)
	var records []Record
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(text), &record); err != nil {
			return nil, &ParseError{Line: line, Err: err}
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// WriteNDJSON sorts records deterministically (see Sort) and writes them as
// newline-delimited JSON, one record per line, so the output is stable across
// rebuilds and diffs cleanly. It performs no file or path handling — callers
// supply the writer.
func WriteNDJSON(w io.Writer, records []Record) error {
	Sort(records)
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}
