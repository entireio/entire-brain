package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestProgressMarkersConsistentOnTTY locks in the marker set the spinner mode
// renders: "+" done, "!" failed, "-" skipped. Skip used to print the non-TTY
// "prefix: … skipped" form unconditionally, leaving one prefix-styled line in
// an otherwise marker-styled list.
func TestProgressMarkersConsistentOnTTY(t *testing.T) {
	var buf bytes.Buffer
	p := &refreshProgress{out: &buf, prefix: "refresh", spinner: true}

	p.Begin("step one").Finish(nil)
	p.Skip("step two: nothing to do")
	p.Begin("step three").Finish(errors.New("boom"))

	out := buf.String()
	for _, want := range []string{"+ step one done\n", "- step two: nothing to do skipped\n", "! step three failed\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("TTY output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "refresh: step two") {
		t.Errorf("TTY skip must not use the non-TTY prefix form:\n%s", out)
	}
}

func TestProgressSkipNonTTYKeepsPrefixForm(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "refresh") // bytes.Buffer is not a TTY
	p.Skip("seed baseline")
	if got := buf.String(); got != "refresh: seed baseline skipped\n" {
		t.Fatalf("non-TTY skip = %q", got)
	}
}

func TestProgressThrottlesExportSessionCountChangesNonTTY(t *testing.T) {
	var buf bytes.Buffer
	task := newProgress(&buf, "refresh").Begin("export sessions")
	task.Update("export sessions: reading checkpoint metadata: 10/100 metadata files, 1 session")
	task.Update("export sessions: reading checkpoint metadata: 11/100 metadata files, 2 sessions")
	task.Update("export sessions: reading checkpoint metadata: 12/100 metadata files, 3 sessions")

	out := buf.String()
	if got := strings.Count(out, "reading checkpoint metadata"); got != 1 {
		t.Fatalf("rapid count-only updates emitted %d lines, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "10/100 metadata files, 1 session") {
		t.Fatalf("first phase update was not emitted:\n%s", out)
	}
}
