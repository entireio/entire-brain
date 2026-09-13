package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEmptyResultBlindSpotNamesAMissingBrain: the blind-spot line exists to stop
// an empty result reading as "nothing exists", and the state where that
// misreading is most costly -- no brain at all -- was the one state it said
// nothing about.
func TestEmptyResultBlindSpotNamesAMissingBrain(t *testing.T) {
	// Never built: no manifest on disk.
	note := emptyResultBlindSpot(t.TempDir())
	if note == "" {
		t.Fatal("a repository with no brain must say so; silence here is exactly the empty an agent misreads as `the brain knows nothing`")
	}
	if !strings.HasPrefix(note, "note: ") {
		t.Fatalf("the no-brain case must use the same note shape as every other blind spot, got %q", note)
	}
	if !strings.Contains(note, "no brain has been built") {
		t.Fatalf("note must name the missing brain, got %q", note)
	}
	if !strings.Contains(note, "entire brain setup") {
		t.Fatalf("note must name the command that builds one, got %q", note)
	}

	// A brain that HAS been built keeps its own note: the no-brain line must
	// not swallow the distill-coverage line it sits beside.
	built := t.TempDir()
	data, err := json.Marshal(exportManifest{
		SchemaVersion:  brainManifestSchemaVersion,
		GeneratedAt:    time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		TranscriptMode: "compact",
		Scope:          "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(built, exportManifestFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if note := emptyResultBlindSpot(built); strings.Contains(note, "no brain has been built") {
		t.Fatalf("a brain that exists must not be reported as missing, got %q", note)
	} else if note == "" {
		t.Fatal("an existing brain must still carry its coverage note")
	}
}

// TestMCPRetrievalReportsAMissingBrain is the defect over the wire.
//
// On a repository with no brain, brain_code refuses out loud ("semantic index
// missing; run `entire brain index`") while brain_search and brain_query
// answered
//
//	{"branch":"main","query":"Hello","results":[]}
//
// An agent's first call reads that as "the brain is useless here" rather than
// "there is no brain here". The note now rides on the SAME blind_spot field the
// mechanism already uses -- no second channel.
func TestMCPRetrievalReportsAMissingBrain(t *testing.T) {
	opts, _ := mcpScopeTestOptions(t, t.TempDir())

	for _, tool := range []string{"brain_search", "brain_query"} {
		payload := mcpTextJSONPayload(t, mcpScopeCall(t, opts, tool, map[string]any{"query": "Hello"}))
		results, ok := payload["results"].([]any)
		if !ok && payload["results"] != nil {
			t.Fatalf("%s: unexpected results shape %#v", tool, payload["results"])
		}
		if len(results) != 0 {
			t.Fatalf("%s: fixture must have no brain to search, got %d results", tool, len(results))
		}
		note, _ := payload["blind_spot"].(string)
		if note == "" {
			t.Fatalf("%s returned a bare empty result for a repository with no brain: %+v", tool, payload)
		}
		if !strings.Contains(note, "no brain has been built") {
			t.Fatalf("%s blind_spot must name the missing brain, got %q", tool, note)
		}
	}
}
