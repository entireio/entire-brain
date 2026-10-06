package factmerge

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func utf8TestRecord(id string) Record {
	return Record{ID: id, Kind: "note", Text: "ok", Branch: "main", Status: "active"}
}

// TestWriteNDJSONWritesNothingWhenOneRecordIsUnwritable is the batch-abort fix.
//
// The UTF-8 refusal was applied INSIDE the write loop, so a batch stopped
// half-written: every record sorted before the bad one had already been handed
// to the writer, and every record after it was silently never attempted. A
// distill run that produced one fact with an invalid-UTF-8 path therefore
// aborted, and whatever the caller did with the partially-filled buffer was
// wrong either way -- truncated if it kept it, all-or-nothing lost if it did
// not.
//
// A writer that reports failure must leave the destination untouched, so the
// caller's retry is a retry and not a resume.
func TestWriteNDJSONWritesNothingWhenOneRecordIsUnwritable(t *testing.T) {
	bad := utf8TestRecord("bbb")
	bad.Paths = []string{"pkg/\xff\xfe.go"}
	records := []Record{utf8TestRecord("aaa"), bad, utf8TestRecord("ccc")}

	var buf bytes.Buffer
	err := WriteNDJSON(&buf, records)
	if err == nil {
		t.Fatal("an unwritable record was accepted")
	}
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("error = %v, want it to wrap ErrInvalidUTF8", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a failed WriteNDJSON left %d bytes behind; the batch must be all-or-nothing, got:\n%s", buf.Len(), buf.String())
	}
}

// TestWriteNDJSONErrorIdentifiesTheRecordToDrop: refusing the batch is only a
// usable answer if the caller can act on it. A formatted string cannot be acted
// on -- the caller would have to parse an error message to learn which of its
// records to drop, and a record whose own ID is the invalid field has no
// printable name at all. The typed error carries the record itself.
func TestWriteNDJSONErrorIdentifiesTheRecordToDrop(t *testing.T) {
	bad := utf8TestRecord("bbb")
	bad.Paths = []string{"pkg/\xff\xfe.go"}
	records := []Record{utf8TestRecord("aaa"), bad, utf8TestRecord("ccc")}

	var buf bytes.Buffer
	err := WriteNDJSON(&buf, records)
	var invalid *InvalidUTF8Error
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v (%T), want an *InvalidUTF8Error a caller can inspect", err, err)
	}
	if invalid.RecordID != "bbb" {
		t.Errorf("RecordID = %q, want %q", invalid.RecordID, "bbb")
	}
	if invalid.Field != "paths" {
		t.Errorf("Field = %q, want %q", invalid.Field, "paths")
	}
	// Index is into the SORTED slice WriteNDJSON just produced, which is the
	// caller's slice: it sorts in place. So the caller can drop exactly one
	// element without re-deriving which.
	if invalid.Index < 0 || invalid.Index >= len(records) {
		t.Fatalf("Index = %d, out of range for %d records", invalid.Index, len(records))
	}
	if records[invalid.Index].ID != "bbb" {
		t.Fatalf("records[%d].ID = %q, want the offending record %q", invalid.Index, records[invalid.Index].ID, "bbb")
	}

	// The documented recovery: drop that one record and retry. The retry must
	// then write every remaining record.
	kept := append(append([]Record{}, records[:invalid.Index]...), records[invalid.Index+1:]...)
	buf.Reset()
	if err := WriteNDJSON(&buf, kept); err != nil {
		t.Fatalf("retry after dropping the named record: %v", err)
	}
	got := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1
	if got != len(kept) {
		t.Fatalf("retry wrote %d lines, want %d", got, len(kept))
	}
}

// TestWriteNDJSONErrorNamesAnUnprintableRecordID: when the invalid bytes are in
// the ID itself, the message must still be readable and must not smuggle raw
// invalid bytes into a log line.
func TestWriteNDJSONErrorNamesAnUnprintableRecordID(t *testing.T) {
	bad := utf8TestRecord("id-\xff\xfe")
	var buf bytes.Buffer
	err := WriteNDJSON(&buf, []Record{bad})
	var invalid *InvalidUTF8Error
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want an *InvalidUTF8Error", err)
	}
	if invalid.Field != "id" {
		t.Errorf("Field = %q, want %q", invalid.Field, "id")
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("error message carries raw invalid UTF-8: %q", err.Error())
	}
}

// TestWriteNDJSONOversizeRecordAlsoLeavesNothingBehind: the size ceiling had the
// same mid-write shape, so it gets the same all-or-nothing guarantee.
func TestWriteNDJSONOversizeRecordAlsoLeavesNothingBehind(t *testing.T) {
	huge := utf8TestRecord("zzz")
	huge.Text = strings.Repeat("x", MaxLineBytes+1)
	records := []Record{utf8TestRecord("aaa"), huge}

	var buf bytes.Buffer
	err := WriteNDJSON(&buf, records)
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("error = %v, want ErrRecordTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a failed WriteNDJSON left %d bytes behind; the batch must be all-or-nothing", buf.Len())
	}
}
