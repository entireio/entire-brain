package cli

import (
	"os"
	"path/filepath"
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
	if err != nil || len(traces) != 2 {
		t.Fatalf("array: len=%d err=%v", len(traces), err)
	}

	nd := writeTraceFile(t, `{"from":"A","to":"B"}`+"\n"+`{"from":"C","to":"D"}`+"\n")
	traces, err = readRuntimeTraces(nd)
	if err != nil || len(traces) != 2 {
		t.Fatalf("ndjson: len=%d err=%v", len(traces), err)
	}
}

func TestReadRuntimeTracesRejectsOversizedFile(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES", "64")
	big := writeTraceFile(t, "["+strings.Repeat(`{"from":"A","to":"B"},`, 100)+`{"from":"x","to":"y"}]`)
	if _, err := readRuntimeTraces(big); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatalf("oversized trace file should be rejected, got %v", err)
	}
}
