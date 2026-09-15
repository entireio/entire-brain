package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type versionedTestDoc struct {
	SchemaVersion int    `json:"schema_version"`
	Known         string `json:"known"`
}

// A document written by a different build of our own writer at a supported
// schema version carries fields this build does not declare. For a reader that
// is routine skew, not corruption.
func TestDecodeVersionedJSONBody_TolerantModeAcceptsUnknownFields(t *testing.T) {
	t.Parallel()

	var got versionedTestDoc
	if _, err := decodeVersionedJSONBody([]byte(`{"schema_version":3,"known":"kept","retired_field":0}`), &got, true); err != nil {
		t.Fatalf("unknown field must not fail a reader: %v", err)
	}
	if got.Known != "kept" {
		t.Fatalf("known field lost during tolerant decode: %+v", got)
	}
}

// Writers keep the opposite answer: re-encoding from a struct that never saw a
// field erases it, so an unrecognised field must stop the write.
func TestDecodeVersionedJSONBody_StrictModeStillGuardsWriters(t *testing.T) {
	t.Parallel()

	var got versionedTestDoc
	_, err := decodeVersionedJSONBody([]byte(`{"schema_version":3,"known":"kept","retired_field":0}`), &got, false)
	if !isUnknownFieldError(err) {
		t.Fatalf("a writer must refuse a field it cannot round-trip, got: %v", err)
	}
}

// Tolerating unknown fields may not widen into tolerating malformed bytes or a
// second document, in either mode.
func TestDecodeStrictVersionedJSON_RejectsRealCorruption(t *testing.T) {
	t.Parallel()

	for name, data := range map[string]string{
		"malformed":     `{"schema_version":3,`,
		"trailing":      `{"schema_version":3}{"schema_version":3}`,
		"wrong type":    `{"schema_version":3,"known":123}`,
		"not an object": `["schema_version"]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got versionedTestDoc
			if _, err := decodeStrictVersionedJSON([]byte(data), &got, 3, "test document"); err == nil {
				t.Fatalf("%s must be rejected", name)
			} else if !strings.Contains(err.Error(), memoryErrStateCorrupt) {
				t.Fatalf("%s must classify as %s, got: %v", name, memoryErrStateCorrupt, err)
			}

			var tolerant versionedTestDoc
			if _, err := decodeVersionedJSONBody([]byte(data), &tolerant, true); err == nil {
				t.Fatalf("%s must be rejected by the tolerant reader too", name)
			}
		})
	}
}

// A newer schema version keeps its own distinct, non-corrupt classification.
func TestDecodeStrictVersionedJSON_NewerVersionStillUnsupported(t *testing.T) {
	t.Parallel()

	var got versionedTestDoc
	_, err := decodeStrictVersionedJSON([]byte(`{"schema_version":9}`), &got, 3, "test document")
	if err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("newer schema must stay %s, got: %v", memoryErrUnsupportedVersion, err)
	}
}

// The tolerant retry re-decodes into a target the aborted strict pass already
// touched. Composite fields must come back exactly as a single clean decode
// would leave them, with nothing merged in from the first attempt.
func TestDecodeVersionedJSONBody_RetryDoesNotMergeIntoPartialTarget(t *testing.T) {
	t.Parallel()

	type doc struct {
		SchemaVersion int               `json:"schema_version"`
		Items         []string          `json:"items"`
		Labels        map[string]string `json:"labels"`
	}

	var got, want doc
	if _, err := decodeVersionedJSONBody([]byte(`{"schema_version":3,"items":["a","b"],"labels":{"k":"v"},"retired_field":1}`), &got, true); err != nil {
		t.Fatalf("unknown field must not fail: %v", err)
	}
	if _, err := decodeVersionedJSONBody([]byte(`{"schema_version":3,"items":["a","b"],"labels":{"k":"v"}}`), &want, true); err != nil {
		t.Fatalf("clean document must not fail: %v", err)
	}
	if len(got.Items) != len(want.Items) || len(got.Labels) != len(want.Labels) {
		t.Fatalf("retry merged into a partial target: got %+v, want %+v", got, want)
	}
	for i := range want.Items {
		if got.Items[i] != want.Items[i] {
			t.Fatalf("retry merged into a partial target: got %+v, want %+v", got, want)
		}
	}
}

// Trailing data keeps its own classification rather than being folded into the
// generic parse failure, because callers report it differently.
func TestDecodeVersionedJSONBody_TrailingDataIsDistinct(t *testing.T) {
	t.Parallel()

	var got versionedTestDoc
	if _, err := decodeVersionedJSONBody([]byte(`{"schema_version":3} {"schema_version":3}`), &got, true); !errors.Is(err, errTrailingJSONData) {
		t.Fatalf("want errTrailingJSONData, got: %v", err)
	}
}

func writeTestManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, exportManifestFileName), []byte(body), 0o600); err != nil {
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

	got, err := loadBrainManifest(dir)
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

	if _, err := loadBrainManifest(dir); err != nil {
		t.Fatalf("the reader must accept it: %v", err)
	}
	if _, err := loadBrainManifestForReplace(dir); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("the writer must refuse to erase it, got: %v", err)
	}
}

// encoding/json exposes no typed error for an unknown field, so the tolerant
// path discriminates on the stdlib's message. Pin it: if the stdlib ever rewords
// it, this fails loudly here instead of silently re-bricking every brain.
func TestUnknownFieldErrorTextIsStillStdlibContract(t *testing.T) {
	t.Parallel()

	// Nested targets too: the fallback must recognise the error wherever in the
	// document the unknown field sits, or a manifest like ours (the retired
	// field lives under sources.seed.history_coverage) is still called corrupt.
	for name, body := range map[string]string{
		"top level": `{"nope":1}`,
		"nested":    `{"nested":{"nope":1}}`,
		"deep":      `{"nested":{"nope":{"a":[1,2]}}}`,
	} {
		var target struct {
			Nested struct {
				Known string `json:"known"`
			} `json:"nested"`
		}
		decoder := json.NewDecoder(strings.NewReader(body))
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&target)
		if err == nil {
			t.Fatalf("%s: expected an unknown-field error from the stdlib decoder", name)
		}
		if !isUnknownFieldError(err) {
			t.Fatalf("%s: stdlib unknown-field error text changed; isUnknownFieldError no longer matches it: %v", name, err)
		}
	}
}

// decodersAllowedToRejectUnknownFields are the only files that may call
// DisallowUnknownFields directly. Everything reading the brain's own persisted
// state goes through decodeVersionedJSONBody or the dedicated file-backed
// manifest decoder, both of which force each caller to say
// whether it is guarding a rewrite or only reading — the distinction this
// package got wrong. Each entry carries its reason at the call site.
var decodersAllowedToRejectUnknownFields = map[string]string{
	"brain_manifest_read.go": "dedicated file-backed manifest decoder: strict rewrites, tolerant read retry with unknown-field reporting",
	"export.go":              "user-authored settings, where an unknown key is usually a typo",
	"memory_abstract.go":     "untrusted model output, not state a build of ours wrote",
	"versioned_state.go":     "the shared decoder itself",
}

func TestPersistedStateDecodersRouteThroughTheSharedDecoder(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(source), "DisallowUnknownFields()") {
			continue
		}
		if _, ok := decodersAllowedToRejectUnknownFields[name]; !ok {
			t.Errorf("%s calls DisallowUnknownFields() directly; read persisted brain state through "+
				"decodeVersionedJSONBody and state whether the decode guards a rewrite, or add %s to "+
				"decodersAllowedToRejectUnknownFields with the reason it may reject a field another "+
				"build wrote", name, name)
		}
	}
}

// The tolerant reader must say when it dropped fields, so status and doctor can
// recommend a refresh rather than quietly narrowing another build's manifest.
func TestDecodeVersionedJSONBody_ReportsDroppedUnknownFields(t *testing.T) {
	t.Parallel()

	var got versionedTestDoc
	dropped, err := decodeVersionedJSONBody([]byte(`{"schema_version":3,"known":"kept","retired_field":0}`), &got, true)
	if err != nil || !dropped {
		t.Fatalf("dropped=%v err=%v, want dropped with no error", dropped, err)
	}

	var clean versionedTestDoc
	dropped, err = decodeVersionedJSONBody([]byte(`{"schema_version":3,"known":"kept"}`), &clean, true)
	if err != nil || dropped {
		t.Fatalf("dropped=%v err=%v, want no drop and no error", dropped, err)
	}
}

// The recovery this health report recommends has to actually work: a manifest
// carrying another build's fields stays readable and refuses rewrites, and
// removing it lets the next refresh rebuild it.
func TestBrainManifestSkewIsReadOnlyAndRecoverableByRemoval(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `{"schema_version":3,"repo_key":"gh/entirehq/devenv","retired_field":0}`)
	rebuilt := exportManifest{SchemaVersion: brainManifestSchemaVersion, RepoKey: "gh/entirehq/devenv"}

	if _, err := loadBrainManifest(dir); err != nil {
		t.Fatalf("skewed manifest must stay readable: %v", err)
	}
	if err := writeBrainManifestAndReadme(dir, rebuilt); err == nil {
		t.Fatal("a writer must not re-encode a manifest whose fields it never saw")
	}

	if err := os.Remove(filepath.Join(dir, exportManifestFileName)); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(dir, rebuilt); err != nil {
		t.Fatalf("removal must clear the way for a rebuild: %v", err)
	}
}

// manifestBrainWrite matches a write of the manifest back into the brain
// directory: a write-named helper taking the brain dir and the manifest name.
// It deliberately does not match a manifest packed into a bundle tar, which
// copies a freshly built value rather than rewriting live state.
var manifestBrainWrite = regexp.MustCompile(`(?i)write[A-Za-z]*\(\s*(outputDir|brainDir)\s*,\s*exportManifestFileName`)

// Every file that rewrites the manifest into the brain directory gates on the
// strict reader.
//
// The tolerant reader exists for the ~280 call sites that only consume the
// manifest, but three writers used loadBrainManifest's strictness as their write
// guard and two of them are nowhere near writeBrainManifestAndReadme. Making the
// reader tolerant silently disarmed those two: the bytes they write are
// marshalled from a struct, so a field the reader dropped would be erased. This
// discovers writers by scanning, so a new one cannot quietly inherit the wrong
// reader.
func TestEveryManifestWriterGatesOnTheStrictReader(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	writers := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(source)
		if !manifestBrainWrite.MatchString(text) {
			continue
		}
		writers++
		if !strings.Contains(text, "loadBrainManifestForReplace") {
			t.Errorf("%s rewrites the brain manifest but never calls loadBrainManifestForReplace; a writer "+
				"gated on the tolerant loadBrainManifest erases fields another build wrote", name)
		}
	}
	if writers < 3 {
		t.Fatalf("found %d manifest writers, want at least the 3 known ones; the detection pattern has drifted", writers)
	}
}

// A forced refresh rebuilds a manifest this build cannot rewrite, so a brain
// that is readable but frozen recovers without anyone deleting a file by hand.
func TestDiscardManifestThisBuildCannotRewrite_RecoversASkewedManifest(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `{"schema_version":3,"repo_key":"gh/entirehq/devenv","retired_field":0}`)

	if _, err := loadBrainManifestForReplace(dir); err == nil {
		t.Fatal("precondition: a writer should refuse this manifest")
	}
	discarded, err := discardManifestThisBuildCannotRewrite(dir)
	if err != nil || !discarded {
		t.Fatalf("discarded=%v err=%v, want discarded with no error", discarded, err)
	}
	if _, err := os.Stat(filepath.Join(dir, exportManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("manifest should be gone: %v", err)
	}
	rebuilt := exportManifest{SchemaVersion: brainManifestSchemaVersion, RepoKey: "gh/entirehq/devenv"}
	if err := writeBrainManifestAndReadme(dir, rebuilt); err != nil {
		t.Fatalf("the rebuild must now succeed: %v", err)
	}
}

// --force does not overrule the version check. Down-converting a manifest from
// a newer schema is the loss that check exists to prevent, so those bytes stay.
func TestDiscardManifestThisBuildCannotRewrite_KeepsWhatItMustNotDelete(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body     string
		wantCode string
	}{
		"newer schema": {`{"schema_version":99,"future":true}`, memoryErrUnsupportedVersion},
		"corrupt":      {`{"schema_version":3,"repo_key":`, memoryErrStateCorrupt},
		"wrong type":   {`{"schema_version":3,"repo_key":123}`, memoryErrStateCorrupt},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := writeTestManifest(t, tc.body)
			discarded, err := discardManifestThisBuildCannotRewrite(dir)
			if discarded {
				t.Fatalf("%s must not be discarded", name)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("%s: err=%v, want %s", name, err, tc.wantCode)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, exportManifestFileName))
			if readErr != nil || string(got) != tc.body {
				t.Fatalf("%s: bytes changed: %q err=%v", name, got, readErr)
			}
		})
	}
}

// A manifest this build can already rewrite is left exactly where it is, so a
// forced refresh does not throw away a perfectly good one on the way past.
func TestDiscardManifestThisBuildCannotRewrite_LeavesAGoodManifestAlone(t *testing.T) {
	t.Parallel()

	body := `{"schema_version":3,"repo_key":"gh/entirehq/devenv"}`
	dir := writeTestManifest(t, body)

	discarded, err := discardManifestThisBuildCannotRewrite(dir)
	if discarded || err != nil {
		t.Fatalf("discarded=%v err=%v, want neither", discarded, err)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, exportManifestFileName))
	if readErr != nil || string(got) != body {
		t.Fatalf("bytes changed: %q err=%v", got, readErr)
	}
}

// An absent manifest is the ordinary first-run case, not something to fail on.
func TestDiscardManifestThisBuildCannotRewrite_ToleratesAnAbsentManifest(t *testing.T) {
	t.Parallel()

	discarded, err := discardManifestThisBuildCannotRewrite(t.TempDir())
	if discarded || err != nil {
		t.Fatalf("discarded=%v err=%v, want neither", discarded, err)
	}
}

// --force may only discard the one thing it claims to: a manifest this build
// cannot rewrite. A version the reader already calls unsupported is corruption,
// and corruption is a diagnosis for the operator, not bytes to delete on a
// guess. checkedVersionedJSONHeader alone does not catch these, because it only
// rejects versions NEWER than this build.
func TestDiscardManifestThisBuildCannotRewrite_KeepsOutOfRangeVersions(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"negative": `{"schema_version":-1,"repo_key":"gh/entirehq/devenv","retired_field":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := writeTestManifest(t, body)
			discarded, err := discardManifestThisBuildCannotRewrite(dir)
			if discarded {
				t.Fatalf("%s schema version must not be deleted", name)
			}
			if err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
				t.Fatalf("%s: err=%v, want %s", name, err, memoryErrUnsupportedVersion)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, exportManifestFileName))
			if readErr != nil || string(got) != body {
				t.Fatalf("%s: bytes changed: %q err=%v", name, got, readErr)
			}
		})
	}
}
