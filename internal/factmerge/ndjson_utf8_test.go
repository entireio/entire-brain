package factmerge

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestWriteNDJSONRefusesInvalidUTF8 pins the write-side guard found by
// FuzzNDJSONWriteReadRoundTrip: encoding/json substitutes U+FFFD for invalid
// UTF-8 instead of failing, so a record written with such a byte reads back
// changed, silently. A POSIX filesystem permits arbitrary bytes in a filename
// and fact paths come from real filenames, so this is reachable from an
// ordinary repository.
func TestWriteNDJSONRefusesInvalidUTF8(t *testing.T) {
	t.Parallel()
	base := Record{
		ID:         "f1",
		Text:       "text",
		Branch:     "main",
		Origin:     "distilled",
		Status:     "active",
		Provenance: []Anchor{{SessionID: "s"}},
		CreatedAt:  time.Unix(0, 0).UTC(),
		UpdatedAt:  time.Unix(0, 0).UTC(),
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Record)
		field  string
	}{
		{"paths", func(r *Record) { r.Paths = []string{"a\x85b.go"} }, "paths"},
		{"text", func(r *Record) { r.Text = "bad\xff" }, "text"},
		{"locus", func(r *Record) { r.Locus = []string{"sym\xfe"} }, "locus"},
		{"provenance", func(r *Record) { r.Provenance = []Anchor{{SessionID: "s\x80"}} }, "provenance.session_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := base
			tc.mutate(&record)
			var buf bytes.Buffer
			err := WriteNDJSON(&buf, []Record{record})
			if !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("WriteNDJSON err = %v, want ErrInvalidUTF8 (a lossy record must be refused, not silently mangled)", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error %q does not name the offending field %q", err, tc.field)
			}
			if buf.Len() != 0 {
				t.Fatalf("refused record still wrote %d bytes", buf.Len())
			}
		})
	}
}

// TestWriteNDJSONAcceptsValidUTF8 guards the other direction: the check must not
// reject legitimate non-ASCII content.
func TestWriteNDJSONAcceptsValidUTF8(t *testing.T) {
	t.Parallel()
	record := Record{
		ID:         "f1",
		Paths:      []string{"café/日本語.go"},
		Text:       "naïve — ünïcode ✓",
		Locus:      []string{"función"},
		Branch:     "main",
		Origin:     "distilled",
		Status:     "active",
		Provenance: []Anchor{{SessionID: "s"}},
		CreatedAt:  time.Unix(0, 0).UTC(),
		UpdatedAt:  time.Unix(0, 0).UTC(),
	}
	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, []Record{record}); err != nil {
		t.Fatalf("WriteNDJSON rejected valid UTF-8: %v", err)
	}
	got, err := ParseNDJSON(&buf)
	if err != nil {
		t.Fatalf("ParseNDJSON: %v", err)
	}
	if len(got) != 1 || got[0].Paths[0] != record.Paths[0] || got[0].Text != record.Text {
		t.Fatalf("round trip changed a valid record: %+v", got)
	}
}
