package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// declared_indexes_test.go pins the cross-check between what the manifest
// declares and what the store holds, for the retrieval indexes.
//
// Reproduction: build a brain, then `rm -rf` its semantic/, history/ and docs/
// directories.
//
//	status   + sessions  + seed  + docs  x semantic  + history  ...
//	search   exit 1, "load history index: lstat …/history: no such file or directory"
//	vsearch  exit 0, `no results for "Greet"`, plus
//	         "note: the brain has no distilled facts; run `entire brain refresh`…"
//
// `+ docs` and `+ history` were both false. `vsearch` returned empty with a
// note naming the wrong cause, for a query that had produced six doc hits
// minutes earlier. Only `semantic` was caught, and only because a freshness
// probe happens to open the store.

// declaredIndexBrain builds the smallest brain that declares a doc index, a
// history index and a seed summary, and holds all three on disk.
func declaredIndexBrain(t *testing.T) (string, *exportManifest) {
	t.Helper()
	brainDir := t.TempDir()
	generation := strings.Repeat("a", 40)
	historyRel := historyDirName + "/generations/" + generation + "/" + historyIndexFileName
	seedRel := seedDirName + "/repo-overview.md"
	historyBody := mustJSON(t, historyIndex{})
	for rel, body := range map[string]string{
		docIndexPath: mustJSON(t, docIndex{Records: []docRecord{{ID: "d1", Text: "Greet formats a greeting"}}}),
		historyRel:   historyBody,
		seedRel:      "# Seeded Repository Overview\n",
	} {
		path := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A generation-addressed history index without its content digest is
	// refused before the file is ever opened, so the fixture commits the real
	// one: every assertion below has to fail on the artifact, not on a manifest
	// the reader would reject anyway.
	sum := sha256.Sum256([]byte(historyBody))
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	manifest := &exportManifest{Sources: &brainSources{
		Seed: &seedSourceManifest{GeneratedAt: now, SummaryPath: seedRel},
		History: &historySourceManifest{
			GeneratedAt: now,
			IndexPath:   historyRel,
			IndexDigest: "sha256:" + hex.EncodeToString(sum[:]),
		},
		Docs: &docSourceManifest{GeneratedAt: now, IndexPath: docIndexPath, Records: 1, Files: 1},
	}}
	return brainDir, manifest
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestDeclaredIndexesAreCheckedAgainstDisk is the cross-check itself: a whole
// brain reports nothing, and each artifact removed is reported by name.
func TestDeclaredIndexesAreCheckedAgainstDisk(t *testing.T) {
	t.Parallel()
	brainDir, manifest := declaredIndexBrain(t)
	if defects := inspectDeclaredIndexes(brainDir, manifest); len(defects) != 0 {
		t.Fatalf("a brain holding everything it declares reported defects: %+v", defects)
	}
	// The reproduction's own gesture: remove whole directories, not files.
	for _, dir := range []string{docDirName, historyDirName, seedDirName} {
		if err := os.RemoveAll(filepath.Join(brainDir, dir)); err != nil {
			t.Fatal(err)
		}
	}
	defects := inspectDeclaredIndexes(brainDir, manifest)
	if len(defects) != 3 {
		t.Fatalf("three declared artifacts were deleted, %d reported: %+v", len(defects), defects)
	}
	for _, defect := range defects {
		if !defect.Absent {
			t.Errorf("%s: a deleted artifact must read as absent, not unreadable: %v", defect.Source, defect.Err)
		}
		warning := defect.Warning()
		if !strings.Contains(warning, defect.Path) {
			t.Errorf("%s: the warning does not name the artifact: %s", defect.Source, warning)
		}
		if !strings.Contains(warning, declaredIndexRepairHint) {
			t.Errorf("%s: the warning does not name the repair: %s", defect.Source, warning)
		}
	}
}

// TestUndeclaredIndexesAreNeverReportedMissing is the guard on the distinction
// the whole file rests on. A brain that has never been refreshed holds none of
// these artifacts, and it is NOT corrupt. Get this wrong and every new brain
// starts reporting data loss on its first query.
func TestUndeclaredIndexesAreNeverReportedMissing(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	for name, manifest := range map[string]*exportManifest{
		"no manifest at all":                   nil,
		"manifest, no source":                  {},
		"manifest with an empty sources block": {Sources: &brainSources{}},
		// A source that is declared but names no artifact is not a claim this
		// check can test, so it is not one this check may fail. Docs is absent
		// from this case on purpose: its readers open a fixed path rather than
		// the manifest's, so a declared docs source IS a claim about that file
		// whether or not it repeats the path (see declaredIndexPath).
		"a source that declares no path": {Sources: &brainSources{
			History: &historySourceManifest{},
			Seed:    &seedSourceManifest{},
		}},
	} {
		if defects := inspectDeclaredIndexes(empty, manifest); len(defects) != 0 {
			t.Errorf("%s: an unbuilt brain was reported as damaged: %+v", name, defects)
		}
	}
}

// TestStatusInstantLineReportsDeletedIndexes is the surface the defect was
// reported on. instantPhaseComponents answers from manifest PRESENCE, which is
// why `+ docs` and `+ history` survived `rm -rf`.
func TestStatusInstantLineReportsDeletedIndexes(t *testing.T) {
	t.Parallel()
	brainDir, manifest := declaredIndexBrain(t)
	state := func() map[string]string {
		onboarding := brainStatusOnboarding{Components: instantPhaseComponents(manifest, setupInstantRecord{})}
		markMissingDeclaredIndexes(&onboarding, brainDir, manifest)
		out := map[string]string{}
		for _, component := range onboarding.Components {
			out[component.Name] = component.State
		}
		return out
	}
	for _, name := range []string{brainComponentSeed, brainComponentDocs, brainComponentHistory} {
		if got := state()[name]; got != "built" {
			t.Fatalf("%s: a whole brain must read as built, got %q", name, got)
		}
	}
	if err := os.RemoveAll(filepath.Join(brainDir, docDirName)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(brainDir, historyDirName)); err != nil {
		t.Fatal(err)
	}
	after := state()
	for _, name := range []string{brainComponentDocs, brainComponentHistory} {
		if after[name] != "failed" {
			t.Fatalf("%s: a deleted index still reads %q on the instant line", name, after[name])
		}
	}
	// "failed", not "missing": the manifest is proof the component WAS built,
	// and reporting a deleted index as never-built is how a corrupt brain reads
	// as a new one.
	if after[brainComponentSeed] != "built" {
		t.Fatalf("an untouched source must not be dragged down with its neighbours: %q", after[brainComponentSeed])
	}
	// A brain that declares nothing keeps the "missing" marks a new brain has.
	fresh := brainStatusOnboarding{Components: instantPhaseComponents(nil, setupInstantRecord{})}
	markMissingDeclaredIndexes(&fresh, t.TempDir(), nil)
	for _, component := range fresh.Components {
		if component.State != "missing" {
			t.Fatalf("an unbuilt brain reported %s=%s; it must read as missing, never failed", component.Name, component.State)
		}
	}
}

// TestRetrievalFailsOnADeclaredDocIndexThatIsGone is the vsearch half. The doc
// arm treated a gone index exactly like an un-built one and skipped it, so the
// verb that reaches docs FIRST returned an empty result with exit 0 while the
// verb that reaches history first exited 1. Both now answer the same way.
func TestRetrievalFailsOnADeclaredDocIndexThatIsGone(t *testing.T) {
	t.Parallel()
	brainDir, manifest := declaredIndexBrain(t)
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	// Only docs is removed: history stays whole, so nothing else can be the
	// thing that fails. This is the vector path, which does not consult history
	// at all without a fusion-eligible embedder — which is exactly why it used
	// to come back empty and cheerful.
	if err := os.RemoveAll(filepath.Join(brainDir, docDirName)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []retrievalMode{modeVector, modeLexical, modeHybrid} {
		results, err := retrieveUnified("", brainDir, "main", "Greet", 10, mode)
		if err == nil {
			t.Fatalf("mode %v: a declared doc index that is gone returned %d results and no error", mode, len(results))
		}
		if !strings.Contains(err.Error(), docIndexPath) {
			t.Fatalf("mode %v: the error does not name the missing artifact: %v", mode, err)
		}
		if !strings.Contains(err.Error(), declaredIndexRepairHint) {
			t.Fatalf("mode %v: the error does not name the repair: %v", mode, err)
		}
	}
}

// TestRetrievalStaysSilentWhenNoDocIndexWasEverDeclared is the other side of
// that line, and the one that keeps a new brain usable: retrieval over a brain
// with no docs source skips the layer without complaint, exactly as before.
func TestRetrievalStaysSilentWhenNoDocIndexWasEverDeclared(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{Sources: &brainSources{}}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []retrievalMode{modeVector, modeLexical, modeHybrid} {
		results, err := retrieveUnified("", brainDir, "main", "Greet", 10, mode)
		if err != nil {
			t.Fatalf("mode %v: a brain that declares no doc index must not be an error: %v", mode, err)
		}
		if len(results) != 0 {
			t.Fatalf("mode %v: an empty brain returned %d results", mode, len(results))
		}
	}
}

// TestHistoryIndexReadNamesADeletedGeneration covers `search`'s half. The
// hardened reader lstats every path component, so a deleted history/ surfaced
// as a raw internal path with no condition and no remedy.
func TestHistoryIndexReadNamesADeletedGeneration(t *testing.T) {
	t.Parallel()
	brainDir, manifest := declaredIndexBrain(t)
	source := manifest.Sources.History
	if _, err := loadBrainHistoryIndex(brainDir, source); err != nil {
		t.Fatalf("fixture history index is not readable: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(brainDir, historyDirName)); err != nil {
		t.Fatal(err)
	}
	_, err := loadBrainHistoryIndex(brainDir, source)
	if err == nil {
		t.Fatal("a deleted history index must still be an error")
	}
	if strings.Contains(err.Error(), "lstat ") {
		t.Fatalf("the reader still answers with a raw filesystem call: %v", err)
	}
	for _, want := range []string{source.IndexPath, "not on disk", declaredIndexRepairHint} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error is missing %q: %v", want, err)
		}
	}
	// The FTS payload fast path is the reader `search` actually takes, and a
	// defect named by one reader and left raw by the other is how the same
	// broken brain produced a sentence from one verb and an lstat from another.
	_, _, _, err = rankHistoryLexicalFromSource(brainDir, source, "history", "Greet", 5)
	if err == nil || strings.Contains(err.Error(), "lstat ") {
		t.Fatalf("the FTS fast path must name the same condition: %v", err)
	}
}

// TestDeclaredIndexDefectSeparatesAbsentFromUnreadable keeps the two situations
// apart: a file that is gone needs a rebuild, a file that will not open needs
// looking at. A directory standing where the index belongs is the cheapest
// unreadable case to construct and is a real one (an interrupted publish).
func TestDeclaredIndexDefectSeparatesAbsentFromUnreadable(t *testing.T) {
	t.Parallel()
	brainDir, manifest := declaredIndexBrain(t)
	path := filepath.Join(brainDir, filepath.FromSlash(docIndexPath))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	defect := inspectDeclaredIndex(brainDir, manifest, declaredIndexDocs)
	if defect.OK() {
		t.Fatal("a directory where the doc index belongs is not a readable index")
	}
	if defect.Absent {
		t.Fatalf("something IS there; reporting it as absent sends the reader to the wrong repair: %s", defect.Warning())
	}
	if !strings.Contains(defect.Warning(), "cannot be read") {
		t.Fatalf("the unreadable case must keep its own words: %s", defect.Warning())
	}
	if errors.Is(defect.Err, os.ErrNotExist) {
		t.Fatalf("the underlying error contradicts the classification: %v", defect.Err)
	}
}
