package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func writeTestManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, bmanifest.FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// decodersAllowedToRejectUnknownFields are the only files that may call
// DisallowUnknownFields directly. Everything reading the brain's own persisted
// state goes through brainstore.DecodeVersionedJSONBody or the dedicated
// file-backed manifest decoder, both of which force each caller to say
// whether it is guarding a rewrite or only reading — the distinction this
// package got wrong. Each entry carries its reason at the call site.
var decodersAllowedToRejectUnknownFields = map[string]string{
	"internal/manifest/read.go": "dedicated file-backed manifest decoder: strict rewrites, tolerant read retry with unknown-field reporting",
	"export.go":                 "user-authored settings, where an unknown key is usually a typo",
	"memory_abstract.go":        "untrusted model output, not state a build of ours wrote",
}

func TestPersistedStateDecodersRouteThroughTheSharedDecoder(t *testing.T) {
	t.Parallel()

	// "." plus internal/manifest, internal/factstore, internal/historystore,
	// internal/semstore, internal/docstore and internal/retrieval: the manifest
	// decoder, the durable-facts store, and the history-store, semantic-store,
	// doc-store and retrieval-engine read layers sank there (COR-2268) and
	// must stay under this guard rather than escape it by moving.
	for _, dir := range []string{".", "internal/manifest", "internal/factstore", "internal/historystore", "internal/semstore", "internal/docstore", "internal/retrieval", "patterns", "command/patternscmd", "command/watch"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if dir != "." {
				name = dir + "/" + name
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
					"brainstore.DecodeVersionedJSONBody and state whether the decode guards a rewrite, or add %s to "+
					"decodersAllowedToRejectUnknownFields with the reason it may reject a field another "+
					"build wrote", name, name)
			}
		}
	}
}

// The recovery this health report recommends has to actually work: a manifest
// carrying another build's fields stays readable and refuses rewrites, and
// removing it lets the next refresh rebuild it.
func TestBrainManifestSkewIsReadOnlyAndRecoverableByRemoval(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `{"schema_version":3,"repo_key":"gh/entirehq/devenv","retired_field":0}`)
	rebuilt := exportManifest{SchemaVersion: bmanifest.SchemaVersion, RepoKey: "gh/entirehq/devenv"}

	if _, err := bmanifest.Load(dir); err != nil {
		t.Fatalf("skewed manifest must stay readable: %v", err)
	}
	if err := writeBrainManifestAndReadme(dir, rebuilt); err == nil {
		t.Fatal("a writer must not re-encode a manifest whose fields it never saw")
	}

	if err := os.Remove(filepath.Join(dir, bmanifest.FileName)); err != nil {
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
var manifestBrainWrite = regexp.MustCompile(`(?i)write[A-Za-z]*\(\s*(outputDir|brainDir)\s*,\s*bmanifest.FileName`)

// Every file that rewrites the manifest into the brain directory gates on the
// strict reader.
//
// The tolerant reader exists for the ~280 call sites that only consume the
// manifest, but three writers used bmanifest.Load's strictness as their write
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
		if !strings.Contains(text, "bmanifest.LoadForReplace") {
			t.Errorf("%s rewrites the brain manifest but never calls bmanifest.LoadForReplace; a writer "+
				"gated on the tolerant bmanifest.Load erases fields another build wrote", name)
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

	if _, err := bmanifest.LoadForReplace(dir); err == nil {
		t.Fatal("precondition: a writer should refuse this manifest")
	}
	discarded, err := discardManifestThisBuildCannotRewrite(dir)
	if err != nil || !discarded {
		t.Fatalf("discarded=%v err=%v, want discarded with no error", discarded, err)
	}
	if _, err := os.Stat(filepath.Join(dir, bmanifest.FileName)); !os.IsNotExist(err) {
		t.Fatalf("manifest should be gone: %v", err)
	}
	rebuilt := exportManifest{SchemaVersion: bmanifest.SchemaVersion, RepoKey: "gh/entirehq/devenv"}
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
		"newer schema": {`{"schema_version":99,"future":true}`, brainstore.UnsupportedVersionCode},
		"corrupt":      {`{"schema_version":3,"repo_key":`, brainstore.StateCorruptCode},
		"wrong type":   {`{"schema_version":3,"repo_key":123}`, brainstore.StateCorruptCode},
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
			got, readErr := os.ReadFile(filepath.Join(dir, bmanifest.FileName))
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
	got, readErr := os.ReadFile(filepath.Join(dir, bmanifest.FileName))
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
			if err == nil || !strings.Contains(err.Error(), brainstore.UnsupportedVersionCode) {
				t.Fatalf("%s: err=%v, want %s", name, err, brainstore.UnsupportedVersionCode)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, bmanifest.FileName))
			if readErr != nil || string(got) != body {
				t.Fatalf("%s: bytes changed: %q err=%v", name, got, readErr)
			}
		})
	}
}
