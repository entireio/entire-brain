package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMemoryStatusPlainRendererHasDeterministicJSONSectionParity(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	fixed := time.Date(2026, 8, 10, 7, 0, 0, 0, time.UTC)
	opts.Now = func() time.Time { return fixed }
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")

	// Use a forward-version manifest so parity also covers partial status and
	// its typed issue without allowing the renderer to rewrite newer state.
	future := []byte(`{"schema_version":4,"future":{"preserve":true}}` + "\n")
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	if err := os.WriteFile(manifestPath, future, 0o600); err != nil {
		t.Fatal(err)
	}

	jsonOut, err := execute(t, newMemoryStatusCommand(opts), "--json")
	if err != nil {
		t.Fatalf("JSON status: %v\n%s", err, jsonOut)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonOut), &sections); err != nil {
		t.Fatalf("decode JSON status: %v\n%s", err, jsonOut)
	}
	wantLabels := make([]string, 0, len(sections))
	for key := range sections {
		wantLabels = append(wantLabels, key)
	}
	sort.Strings(wantLabels)

	plainOne, err := execute(t, newMemoryStatusCommand(opts))
	if err != nil {
		t.Fatalf("plain status: %v\n%s", err, plainOne)
	}
	plainTwo, err := execute(t, newMemoryStatusCommand(opts))
	if err != nil {
		t.Fatalf("repeat plain status: %v\n%s", err, plainTwo)
	}
	if plainOne != plainTwo {
		t.Fatalf("plain status is nondeterministic\nfirst:\n%s\nsecond:\n%s", plainOne, plainTwo)
	}

	var gotLabels []string
	for _, line := range strings.Split(strings.TrimSuffix(plainOne, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, " ") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 1 {
			t.Fatalf("top-level status line has no label: %q", line)
		}
		gotLabels = append(gotLabels, line[:colon])
	}
	if strings.Join(gotLabels, "\n") != strings.Join(wantLabels, "\n") {
		t.Fatalf("plain/JSON top-level section mismatch\nplain=%q\njson=%q\n%s", gotLabels, wantLabels, plainOne)
	}
	for _, section := range []string{"manifest_health", "schemas", "locks", "install", "coordinator", "worker_log", "canonical_sessions", "reconciliation", "issues"} {
		if _, ok := sections[section]; !ok || !strings.Contains(plainOne, section+":") {
			t.Fatalf("section %q missing from one format\nplain:\n%s\njson:%s", section, plainOne, jsonOut)
		}
	}
	if !strings.Contains(plainOne, "  state: \"unsupported\"") || !strings.Contains(plainOne, memoryErrUnsupportedVersion) {
		t.Fatalf("forward-version health missing from plain output:\n%s", plainOne)
	}
	if after, err := os.ReadFile(manifestPath); err != nil || string(after) != string(future) {
		t.Fatalf("status changed forward-version manifest: err=%v got=%q want=%q", err, after, future)
	}
}
