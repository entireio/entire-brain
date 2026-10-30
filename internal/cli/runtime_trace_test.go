package cli

import (
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeTraceFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "traces.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadRuntimeTracesArrayAndNDJSON(t *testing.T) {
	arr := writeTraceFile(t, `[{"from":"A","to":"B","type":"CALLS"},{"from":"B","to":"C"}]`)
	traces, err := readRuntimeTraces(arr)
	want := []semanticRuntimeTrace{{From: "A", To: "B", Type: "CALLS"}, {From: "B", To: "C"}}
	if err != nil || !slices.Equal(traces, want) {
		t.Fatalf("array: traces=%+v err=%v, want %+v", traces, err, want)
	}

	nd := writeTraceFile(t, `{"from":"A","to":"B"}`+"\n"+`{"from":"C","to":"D"}`+"\n")
	traces, err = readRuntimeTraces(nd)
	want = []semanticRuntimeTrace{{From: "A", To: "B"}, {From: "C", To: "D"}}
	if err != nil || !slices.Equal(traces, want) {
		t.Fatalf("ndjson: traces=%+v err=%v, want %+v", traces, err, want)
	}
}

func TestReadRuntimeTracesRejectsOversizedFile(t *testing.T) {

	big := writeTraceFile(t, "")
	if err := os.Truncate(big, semstore.TraceMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := readRuntimeTraces(big); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("oversized trace file should be rejected, got %v", err)
	}
}
