package factmerge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
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

// ErrInvalidUTF8 is returned by WriteNDJSON for a record carrying a string that
// is not valid UTF-8.
//
// encoding/json does not fail on such a string: it substitutes U+FFFD for every
// invalid byte, so the record READS BACK DIFFERENT from the one written, with no
// error on either side. Fact paths come from real filenames, and a POSIX
// filesystem permits arbitrary bytes there, so this is reachable from an
// ordinary repository rather than only from a hostile one.
//
// Refusing at write time follows the same rule as ErrRecordTooLarge: never
// persist a record this package cannot faithfully return. Silent substitution
// would corrupt the shared cross-member head, where the mangled path would then
// merge into every other member.
var ErrInvalidUTF8 = errors.New("factmerge: record contains invalid UTF-8")

// InvalidUTF8Error names the record WriteNDJSON refused, so the caller can drop
// exactly that record and retry.
//
// WHY REFUSE RATHER THAN SKIP-AND-WARN. This writer produces the durable
// fact-set, including the shared cross-member head. A record silently dropped
// here is not a local inconvenience: it is an ABSENCE, and an absence merges
// into every other member as "this fact is not there", so one unrepresentable
// path on one machine quietly deletes a fact for everybody. A warning on stderr
// is not a control -- the sync runner and the distill pipeline are both
// non-interactive, and nothing downstream can tell a fact that was never
// distilled from one that was thrown away. Refusing keeps the store's central
// promise: what it returns is what was written.
//
// WHY IT MUST BE A TYPED ERROR. "Fail and let the caller drop it" is only a
// usable answer if the caller can identify what to drop. A formatted string
// cannot be acted on -- and a record whose own ID holds the invalid bytes has
// no printable name to match on. Index is into the slice WriteNDJSON just
// sorted, which is the caller's own slice (Sort is in place), so recovery is
// exactly: errors.As, drop records[Index], call again.
//
// WHY THE BATCH IS ALL-OR-NOTHING. Refusing mid-loop left every record sorted
// before the offender already written to w and every record after it silently
// unattempted. The caller then held a truncated fact-set with no way to tell.
// WriteNDJSON now validates and marshals the whole batch before writing a byte,
// so a failure leaves the destination untouched and the caller's retry is a
// retry, not a resume.
//
// NO MIGRATION IS NEEDED for data already stored: ParseNDJSON only ever
// returned records that round-tripped through encoding/json, so nothing on disk
// carries invalid UTF-8. This governs newly authored records only.
type InvalidUTF8Error struct {
	// RecordID is the offending record's ID, quoted for safe logging when the
	// ID itself is the invalid field.
	RecordID string
	// Index is the record's position in the sorted slice passed to WriteNDJSON.
	Index int
	// Field names the offending field ("paths", "provenance.commit", ...).
	Field string
	// Value is the invalid value, quoted when rendered.
	Value string
}

func (e *InvalidUTF8Error) Error() string {
	return fmt.Sprintf(
		"%s: record %d (%q) field %q is not valid UTF-8 (%q); encoding/json would substitute U+FFFD and the record would read back changed. Drop this record and retry, or repair the value at its source",
		ErrInvalidUTF8.Error(), e.Index, e.RecordID, e.Field, e.Value)
}

func (e *InvalidUTF8Error) Unwrap() error { return ErrInvalidUTF8 }

// invalidUTF8Field returns the name and value of the first field of record that
// is not valid UTF-8, or "" if every string round-trips.
func invalidUTF8Field(record Record) (string, string) {
	pairs := []struct {
		name  string
		value string
	}{
		{"id", record.ID},
		{"kind", record.Kind},
		{"text", record.Text},
		{"branch", record.Branch},
		{"origin", record.Origin},
		{"status", record.Status},
		{"confidence", record.Confidence},
		{"superseded_by", record.SupersededBy},
	}
	for _, pair := range pairs {
		if !utf8.ValidString(pair.value) {
			return pair.name, pair.value
		}
	}
	for _, path := range record.Paths {
		if !utf8.ValidString(path) {
			return "paths", path
		}
	}
	for _, locus := range record.Locus {
		if !utf8.ValidString(locus) {
			return "locus", locus
		}
	}
	for _, related := range record.RelatedIDs {
		if !utf8.ValidString(related) {
			return "related_ids", related
		}
	}
	for _, anchor := range record.Provenance {
		for _, pair := range []struct {
			name  string
			value string
		}{
			{"provenance.session_id", anchor.SessionID},
			{"provenance.commit", anchor.Commit},
			{"provenance.checkpoint_id", anchor.CheckpointID},
			{"provenance.turn_id", anchor.TurnID},
			{"provenance.transcript", anchor.Transcript},
		} {
			if !utf8.ValidString(pair.value) {
				return pair.name, pair.value
			}
		}
	}
	return "", ""
}

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
	scanner.Buffer(make([]byte, 0, 8*1024), MaxLineBytes+2) // reserve the optional CR and newline
	var records []Record
	line := 0
	for scanner.Scan() {
		line++
		if len(scanner.Bytes()) > MaxLineBytes {
			return nil, &ParseError{Line: line, Err: fmt.Errorf("line exceeds the maximum of %d bytes", MaxLineBytes)}
		}
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
	// Validate and marshal the WHOLE batch first: a writer that reports failure
	// must leave the destination untouched, or the caller is left holding a
	// half-written fact-set it cannot distinguish from a complete one. See
	// InvalidUTF8Error for the reasoning. The marshalled bytes are retained
	// rather than re-marshalled, which costs nothing in practice -- every caller
	// writes into a bytes.Buffer, so the serialized form is held either way.
	lines := make([][]byte, 0, len(records))
	for i, record := range records {
		if field, value := invalidUTF8Field(record); field != "" {
			return &InvalidUTF8Error{RecordID: record.ID, Index: i, Field: field, Value: value}
		}
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(data) > MaxLineBytes {
			return fmt.Errorf("%w: %s is %d bytes, maximum %d", ErrRecordTooLarge, record.ID, len(data), MaxLineBytes)
		}
		lines = append(lines, data)
	}
	for _, data := range lines {
		if _, err := w.Write(data); err != nil {
			return err
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
}
