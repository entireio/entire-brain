package factmerge

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNDJSONRecordSizeBoundary(t *testing.T) {
	base := Record{ID: "boundary"}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{-1, 0, 1} {
		record := base
		record.Text = strings.Repeat("x", MaxLineBytes-len(raw)+delta)
		var buf bytes.Buffer
		err := WriteNDJSON(&buf, []Record{record})
		if delta > 0 {
			if !errors.Is(err, ErrRecordTooLarge) {
				t.Fatalf("oversize write: %v", err)
			}
			raw, _ := json.Marshal(record)
			if _, err := ParseNDJSON(bytes.NewReader(raw)); err == nil {
				t.Fatal("oversize read accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, ending := range []string{"", "\n", "\r\n"} {
			input := append(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte(ending)...)
			got, err := ParseNDJSON(bytes.NewReader(input))
			if err != nil {
				t.Fatalf("delta=%d ending=%q: %v", delta, ending, err)
			}
			if len(got) != 1 || got[0].Text != record.Text {
				t.Fatal("round trip changed record")
			}
		}
	}
}
