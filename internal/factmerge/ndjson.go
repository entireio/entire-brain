package factmerge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxLineBytes bounds a single NDJSON record line so a corrupt store cannot
// exhaust memory while scanning.
//
// It is 16 MiB, matching the ceiling the rest of the brain applies to untrusted
// inputs it reads whole. It was 64 KiB, which the WRITER did not enforce: a fact
// whose text exceeded it serialized fine and then failed to parse back
// ("bufio.Scanner: token too long"), so the store could persist a fact-set it
// could not read. On the shared cross-member head that is worse than local
// corruption — factsync.Sync parses the pulled blob with ParseNDJSON, so ONE
// member's oversized fact permanently fails every other member's sync, with no
// way to repair it from the affected side.
//
// The bound is now enforced on BOTH sides: WriteNDJSON refuses to emit a line
// this reader could not read back (ErrRecordTooLarge), so the asymmetry cannot
// reopen.
const MaxLineBytes = 16 * 1024 * 1024

// ErrRecordTooLarge is returned by WriteNDJSON for a record whose serialized
// line would exceed MaxLineBytes, i.e. one ParseNDJSON could never read back.
// Refusing at write time keeps a store from persisting a fact-set it cannot
// load.
var ErrRecordTooLarge = errors.New("factmerge: record exceeds the maximum NDJSON line size")

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
		// bufio reports an oversized line as a bare ErrTooLong with no position,
		// which is useless for repairing a store. Name the line and the bound.
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, &ParseError{Line: line + 1, Err: fmt.Errorf("line exceeds the maximum of %d bytes", MaxLineBytes)}
		}
		return nil, err
	}
	return records, nil
}

// WriteNDJSON sorts records deterministically (see Sort) and writes them as
// newline-delimited JSON, one record per line, so the output is stable across
// rebuilds and diffs cleanly. It performs no file or path handling — callers
// supply the writer.
//
// A record whose line would exceed MaxLineBytes is refused with
// ErrRecordTooLarge rather than written: ParseNDJSON could not read it back, and
// a store that writes what it cannot read is unrecoverable from the reading side.
func WriteNDJSON(w io.Writer, records []Record) error {
	Sort(records)
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(data) > MaxLineBytes {
			return fmt.Errorf("%w: %s is %d bytes, maximum %d", ErrRecordTooLarge, record.ID, len(data), MaxLineBytes)
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
