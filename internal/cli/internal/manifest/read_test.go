package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func writeTestManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The real-world regression: manifests written before commit cc5c5798 carry
// sources.seed.history_coverage.checkpointed_unexported_commits, a field since
// retired at the same schema version 3. Every such brain read as corrupt, which
// turned off all sources and left the memory worker retrying forever.
func TestLoadBrainManifest_RetiredFieldFromOlderBuildStillReads(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `{
	  "schema_version": 3,
	  "repo_key": "gh/entirehq/devenv",
	  "sources": {
	    "seed": {
	      "history_coverage": {
	        "path": "seed/history-gaps.md",
	        "total_commits": 34,
	        "checkpointed_unexported_commits": 0
	      }
	    }
	  }
	}`)

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("manifest from an older build must load, not read as corrupt: %v", err)
	}
	if got.RepoKey != "gh/entirehq/devenv" {
		t.Fatalf("known fields lost: %+v", got)
	}
	if got.Sources == nil || got.Sources.Seed == nil || got.Sources.Seed.HistoryCoverage == nil {
		t.Fatalf("nested known fields lost: %+v", got.Sources)
	}
	if got.Sources.Seed.HistoryCoverage.TotalCommits != 34 {
		t.Fatalf("sibling of the retired field lost: %+v", got.Sources.Seed.HistoryCoverage)
	}
}

// The reader getting more forgiving must not make the writer forgiving: a
// rewrite that cannot round-trip the bytes it observed still has to refuse.
func TestLoadBrainManifestForReplace_StillRefusesUnknownFields(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `{"schema_version":3,"repo_key":"gh/entirehq/devenv","retired_field":0}`)

	if _, err := Load(dir); err != nil {
		t.Fatalf("the reader must accept it: %v", err)
	}
	if _, err := LoadForReplace(dir); err == nil || !strings.Contains(err.Error(), brainstore.StateCorruptCode) {
		t.Fatalf("the writer must refuse to erase it, got: %v", err)
	}
}

// TestWriterRefusalExplainsItselfInsteadOfSayingCorrupt pins COR-2162.
//
// A writer refusing an unknown field is correct: re-encoding from a struct
// that never saw the field would erase what another build recorded. What was
// wrong is how it read. "brain manifest cannot be parsed: memory_state_corrupt"
// describes damage, and the manifest is undamaged and fully readable — a
// reader on the same bytes succeeds. On a real machine that wording left a
// component failed for four days while nothing was broken.
func TestWriterRefusalExplainsItselfInsteadOfSayingCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	// schema_version must be in range, or the version guards answer first and
	// this test would pass without ever reaching the unknown-field path.
	// The probe field must be one no release will ever claim. "suppressed_warnings"
	// was a real candidate for the manifest and has since become a real field,
	// which turned this guard red on the branch that added it -- the test was
	// asserting "unknown" about a name the product was still free to take.
	body := fmt.Sprintf(`{"schema_version":%d,"zz_field_no_release_will_define":["x"]}`, SchemaVersion)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	open := func() *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}

	// A reader is unaffected: the bytes are readable, which is the whole point.
	if _, _, dropped, err := DecodeFile(open(), true); err != nil {
		t.Fatalf("a reader must still read this manifest, got: %v", err)
	} else if !dropped {
		t.Fatal("the reader should report that it dropped an unknown field")
	}

	_, _, _, err := DecodeFile(open(), false)
	if err == nil {
		t.Fatal("a writer must refuse rather than silently erase the unknown field")
	}
	msg := err.Error()
	if !strings.Contains(msg, "zz_field_no_release_will_define") {
		t.Errorf("refusal must name the field the reader has to act on: %v", err)
	}
	if !strings.Contains(msg, "NOT damaged") {
		t.Errorf("refusal must say the manifest is not damaged: %v", err)
	}
	if strings.Contains(msg, "cannot be parsed") {
		t.Errorf("refusal still claims the manifest cannot be parsed, but a reader just parsed it: %v", err)
	}
	// The code is part of the maintenance taxonomy and many consumers match on
	// it, so improving the sentence must not retype the error.
	if !strings.Contains(msg, brainstore.StateCorruptCode) {
		t.Errorf("error code changed; that is a contract change, not a wording fix: %v", err)
	}
}
