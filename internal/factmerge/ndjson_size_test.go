package factmerge

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func sizeTestRecord(text string) Record {
	return Record{
		ID:         "fact:aa",
		Paths:      []string{"a.b.c"},
		Text:       text,
		Branch:     "main",
		Origin:     "distilled",
		Status:     StatusActive,
		Provenance: []Anchor{{SessionID: "s"}},
		CreatedAt:  time.Unix(0, 0).UTC(),
		UpdatedAt:  time.Unix(0, 0).UTC(),
	}
}

// Regression: WriteNDJSON used to emit lines ParseNDJSON refused to read. A fact
// larger than the (then 64 KiB) scanner bound serialized fine and then failed to
// load with a bare "bufio.Scanner: token too long" — and on the shared head
// factsync pulls, one member's oversized fact permanently broke every other
// member's sync. Found by FuzzNDJSONWriteReadRoundTrip.
func TestWriteReadRoundTripAtRealisticSizes(t *testing.T) {
	for _, size := range []int{1, 64 * 1024, 200 * 1024, 4 * 1024 * 1024} {
		rec := sizeTestRecord(strings.Repeat("x", size))
		var buf bytes.Buffer
		if err := WriteNDJSON(&buf, []Record{rec}); err != nil {
			t.Fatalf("WriteNDJSON refused a %d-byte fact: %v", size, err)
		}
		got, err := ParseNDJSON(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("ParseNDJSON could not read back a %d-byte fact it just wrote: %v", size, err)
		}
		if len(got) != 1 || got[0].Text != rec.Text {
			t.Fatalf("%d-byte fact did not survive the round trip", size)
		}
	}
}

// The two halves must never disagree again: a record too large to read back is
// refused at WRITE time, so it can never reach the store.
func TestWriteRefusesUnreadableRecord(t *testing.T) {
	rec := sizeTestRecord(strings.Repeat("x", MaxLineBytes+1))
	var buf bytes.Buffer
	err := WriteNDJSON(&buf, []Record{rec})
	if err == nil {
		t.Fatal("WriteNDJSON accepted a record ParseNDJSON cannot read back")
	}
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("want ErrRecordTooLarge, got %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("the oversized record was partially written (%d bytes)", buf.Len())
	}
}

// An oversized line already on disk must be reported with a position and the
// bound, not as an opaque bufio error a reader cannot act on.
func TestParseReportsOversizedLineWithPosition(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"id":"fact:aa"}` + "\n")
	buf.WriteString(`{"id":"` + strings.Repeat("x", MaxLineBytes+16) + `"}` + "\n")

	_, err := ParseNDJSON(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("ParseNDJSON accepted a line beyond MaxLineBytes")
	}
	var perr *ParseError
	if !errors.As(err, &perr) {
		t.Fatalf("want a *ParseError naming the line, got %T: %v", err, err)
	}
	if perr.Line != 2 {
		t.Fatalf("oversized line reported at line %d, want 2", perr.Line)
	}
	if !strings.Contains(perr.Error(), "maximum") {
		t.Fatalf("error should name the bound, got %v", perr)
	}
}
