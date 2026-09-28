package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// render_json_contract_test.go pins the --json contract of the two commands the
// visual renderer touches. The goldens were captured from a REAL run of
// `entire-brain status --json` / `entire-brain setup --json` BEFORE the renderer
// existed. They pin serialization of the fields and values present in those
// fixtures. New optional fields that remain empty need separate populated cases.
//
// The check is a round trip through the very structs writeJSON marshals: read
// the golden, unmarshal into the report type, marshal it back with writeJSON's
// encoder, compare bytes. That catches changes to represented fields without
// depending on a live brain or clock.
//
// Regenerate deliberately (and only when the JSON contract is MEANT to change)
// with ENTIRE_BRAIN_UPDATE_GOLDEN=1 go test ./internal/cli -run JSONContract.
//
// The goldens are captured from a real run and then SCRUBBED: absolute paths are
// rewritten to /home/example/... and temp directories to /tmp/example. Re-capture
// from a live machine and you will paste your own home directory into a tracked
// file — benchmarks/agent-brain/test_publication_safety.py fails the build for
// exactly that, and it is the only thing standing between this directory and a
// published contributor path.
const goldenUpdateEnv = "ENTIRE_BRAIN_UPDATE_GOLDEN"

func checkJSONContract(t *testing.T, name string, value any) {
	t.Helper()
	path := filepath.Join("testdata", "render", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	// .gitattributes pins these to eol=lf, but normalize anyway: a checkout that
	// predates the pin (or a Windows editor re-save) can still hand us CRLF, and
	// writeJSON's encoder (json.MarshalIndent) only ever emits LF. Without this,
	// the contract check fails on checkout mechanics instead of on JSON content.
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("golden %s does not fit the report struct (a field was renamed or removed): %v", path, err)
	}
	// Re-emit through the REAL producer. Re-implementing writeJSON's encoder
	// settings here (MarshalIndent + a trailing newline) meant the contract was
	// pinned against a copy of the encoder rather than the encoder: a change to
	// writeJSON's indentation, HTML escaping, or trailing newline — every byte
	// the scripts parsing this output depend on — passed this test untouched.
	buf := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetOut(buf)
	if err := writeJSON(cmd, value); err != nil {
		t.Fatalf("writeJSON %s: %v", path, err)
	}
	got := buf.Bytes()
	if os.Getenv(goldenUpdateEnv) != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
		t.Logf("updated golden %s", path)
		return
	}
	if string(got) != string(data) {
		t.Fatalf("--json contract for %s changed.\nA field was added, renamed, dropped, or had its omitempty rule changed.\n--- golden (%d bytes) ---\n%s\n--- got (%d bytes) ---\n%s",
			path, len(data), firstDiffContext(string(data), string(got)), len(got), firstDiffContext(string(got), string(data)))
	}
}

// firstDiffContext returns the 200 bytes of a around the first byte where it
// differs from b, so a 20KB golden mismatch prints the offending field instead
// of two walls.
func firstDiffContext(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	start := max(0, i-100)
	end := min(len(a), i+100)
	return a[start:end]
}

// TestStatusJSONContractUnchanged pins `entire-brain status --json`.
func TestStatusJSONContractUnchanged(t *testing.T) {
	t.Parallel()
	var report brainStatusReport
	checkJSONContract(t, "status.json", &report)
}

// TestStatusDetailedJSONContractUnchanged pins the --details superset, which is
// the shape that carries coverage histograms, blind spots and partial failures —
// exactly the data the short/verbose split reorganises in the TEXT renderer and
// must not reorganise here.
func TestStatusDetailedJSONContractUnchanged(t *testing.T) {
	t.Parallel()
	var report brainStatusReport
	checkJSONContract(t, "status_detailed.json", &report)
}

// TestSetupJSONContractUnchanged pins `entire-brain setup --json`.
func TestSetupJSONContractUnchanged(t *testing.T) {
	t.Parallel()
	var report setupReport
	checkJSONContract(t, "setup.json", &report)
}
