package factmerge

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The fact-set NDJSON blob is the shared, cross-member wire format (factsync
// pulls it from the hosted head and parses it with ParseNDJSON), so it becomes
// untrusted input the moment the brain is shared. These harnesses assert the two
// properties that matter for it: arbitrary bytes never panic, and anything the
// writer emits the reader can read back.
//
//	go test ./internal/factmerge -run xxx -fuzz FuzzParseNDJSON

func FuzzParseNDJSON(f *testing.F) {
	f.Add([]byte(`{"id":"fact:aa","paths":["a.b.c"],"text":"t","branch":"main","origin":"distilled","status":"active","provenance":[{"session_id":"s"}],"created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}` + "\n"))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte(`{"id":`))
	f.Add([]byte("\x00\x01 not json"))
	f.Add([]byte(`{"created_at":"not-a-time"}`))
	f.Add(append([]byte(`{"id":"`), append(bytes.Repeat([]byte("x"), 70*1024), []byte(`"}`)...)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		records, err := ParseNDJSON(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Anything the parser accepted must survive a write/read round trip;
		// otherwise the store can persist a fact-set it cannot read back.
		var buf bytes.Buffer
		if err := WriteNDJSON(&buf, records); err != nil {
			t.Fatalf("WriteNDJSON rejected records ParseNDJSON accepted: %v", err)
		}
		again, err := ParseNDJSON(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("ParseNDJSON rejected its own WriteNDJSON output: %v", err)
		}
		if len(again) != len(records) {
			t.Fatalf("round trip changed the record count: %d -> %d", len(records), len(again))
		}
	})
}

// FuzzNDJSONWriteReadRoundTrip drives the round trip from the STRUCT side: any
// record the store is willing to write must be readable back. A writer that can
// emit a line its own reader rejects is a store that bricks itself — and on the
// shared head, one member's oversized fact would brick every other member's sync.
func FuzzNDJSONWriteReadRoundTrip(f *testing.F) {
	f.Add("fact:aa", "a.b.c", "short text", "main", 1)
	f.Add("fact:bb", "a.b.c", strings.Repeat("x", 100*1024), "main", 1)
	f.Add("fact:cc", "a.b.c", "   unicode  ", "main", 3)

	f.Fuzz(func(t *testing.T, id, path, text, branch string, anchors int) {
		if anchors < 0 || anchors > 8 {
			return
		}
		rec := Record{
			ID:        id,
			Paths:     []string{path},
			Text:      text,
			Branch:    branch,
			Origin:    "distilled",
			Status:    StatusActive,
			CreatedAt: time.Unix(0, 0).UTC(),
			UpdatedAt: time.Unix(0, 0).UTC(),
		}
		for i := 0; i < anchors; i++ {
			rec.Provenance = append(rec.Provenance, Anchor{SessionID: "s"})
		}
		var buf bytes.Buffer
		if err := WriteNDJSON(&buf, []Record{rec}); err != nil {
			return // a record the writer refuses never reaches the store
		}
		got, err := ParseNDJSON(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("the reader rejected a record the writer emitted (%d bytes): %v", buf.Len(), err)
		}
		if len(got) != 1 || !reflect.DeepEqual(got[0], rec) {
			t.Fatalf("round trip changed the record:\nwant %#v\ngot  %#v", rec, got)
		}
	})
}

// FuzzRecordIdentity pins the content-addressing helpers: normalization must be
// idempotent and the derived id must be a pure function of the normalized form.
func FuzzRecordIdentity(f *testing.F) {
	f.Add("Some Fact Text", "a.b.c,d.e.f")
	f.Add("  spaced   out  ", "A.B.C,,zzz")
	f.Add("  ", "x")
	f.Fuzz(func(t *testing.T, text, rawPaths string) {
		paths := NormalizePaths(strings.Split(rawPaths, ","))
		if got := NormalizePaths(paths); !reflect.DeepEqual(got, paths) {
			t.Fatalf("NormalizePaths is not idempotent: %#v -> %#v", paths, got)
		}
		if NormalizeText(NormalizeText(text)) != NormalizeText(text) {
			t.Fatalf("NormalizeText is not idempotent for %q", text)
		}
		if RecordID(text, paths) != RecordID(NormalizeText(text), paths) {
			t.Fatalf("RecordID is not a function of the normalized text: %q", text)
		}
	})
}
