package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Durable facts lived only inside the store, reachable through recall and the
// MCP tools. Wanting to grep them, read one in an editor, or diff two branches
// meant writing a program. These tests hold the plain-file projection to the
// properties that make it worth having: ordinary tools work on it, the output
// stays under its directory, and it never pretends to be a second source of
// truth.

func factFixture(id, text string, paths []string) factRecord {
	return factRecord{
		ID:        id,
		Paths:     paths,
		Kind:      "invariant",
		Text:      text,
		Branch:    "main",
		Origin:    "authored",
		Status:    "active",
		CreatedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}
}

func TestFactFilesAreGreppableAndReadable(t *testing.T) {
	dir := t.TempDir()
	facts := []factRecord{
		factFixture("fact:57e03e4afc229b3c", "Retries are capped at 3; the 4th failure must page.", []string{"constraints.retry.policy"}),
		factFixture("fact:e07be9baabcc1122", "The upload endpoint rejects payloads over 25 MB.", []string{"constraints.upload.limits"}),
	}
	result, err := writeFactFiles(dir, "main", facts)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if result.Written != 2 {
		t.Fatalf("written = %d, want 2", result.Written)
	}

	// The taxonomy is the directory layout, so `ls` and tab-completion follow
	// the same structure `recall` does.
	want := filepath.Join("constraints", "retry", "policy", "fact-57e03e4afc22.md")
	if result.Files[0] != want && result.Files[1] != want {
		t.Fatalf("expected %s among %v", want, result.Files)
	}

	body, err := os.ReadFile(filepath.Join(dir, want))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(body)
	// The fact sentence must be on its own line, unindented and unwrapped, or
	// `grep -rn` returns metadata instead of the thing you searched for.
	if !strings.Contains(text, "\nRetries are capped at 3; the 4th failure must page.\n") {
		t.Fatalf("fact text is not a plain grep-able line:\n%s", text)
	}
	for _, field := range []string{"id: fact:", "kind: invariant", "status: active", "created_at: 2026-09-21"} {
		if !strings.Contains(text, field) {
			t.Fatalf("front matter missing %q:\n%s", field, text)
		}
	}
}

func TestFactFilesStayUnderTheExportDirectory(t *testing.T) {
	dir := t.TempDir()
	// This is a structural invariant, not a live-threat test, and saying so
	// matters: splitting the taxonomy on "." already destroys ".." before a
	// segment exists, and filepath.Join cleans under the root, so no input
	// below escapes even with the sanitiser removed. The assertion guards the
	// property against a future change to how paths are split or joined.
	hostile := []factRecord{
		factFixture("fact:aaa1", "escape via dots", []string{"../../etc"}),
		factFixture("fact:bbb2", "escape via separators", []string{"a/../../b.c"}),
		factFixture("fact:ccc3", "absolute", []string{"/etc/passwd"}),
	}
	result, err := writeFactFiles(dir, "main", hostile)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, rel := range result.Files {
		full, err := filepath.EvalSymlinks(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("resolve %s: %v", rel, err)
		}
		if !strings.HasPrefix(full, root+string(os.PathSeparator)) {
			t.Fatalf("%s escaped the export directory: %s not under %s", rel, full, root)
		}
	}
}

func TestFactFilesPutUnfiledFactsSomewhereVisible(t *testing.T) {
	dir := t.TempDir()
	// A fact with no taxonomy path must not land in the export root: the root
	// is the category list, and an unfiled fact mixed into it is invisible.
	result, err := writeFactFiles(dir, "main", []factRecord{
		factFixture("fact:nopath", "A fact filed nowhere.", nil),
		factFixture("fact:blank", "A fact with a blank path.", []string{"   "}),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, rel := range result.Files {
		if !strings.HasPrefix(rel, "unfiled"+string(os.PathSeparator)) {
			t.Fatalf("%s should be under unfiled/", rel)
		}
	}
}

func TestFactFilesAreDeterministic(t *testing.T) {
	facts := []factRecord{
		factFixture("fact:bbb", "second", []string{"b.b.b"}),
		factFixture("fact:aaa", "first", []string{"a.a.a"}),
	}
	first, err := writeFactFiles(t.TempDir(), "main", facts)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	second, err := writeFactFiles(t.TempDir(), "main", facts)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if strings.Join(first.Files, "|") != strings.Join(second.Files, "|") {
		t.Fatalf("manifest is not stable:\n%v\n%v", first.Files, second.Files)
	}
	// Sorted, so a diff of two exports is a diff of the facts and not of map
	// iteration order.
	for i := 1; i < len(first.Files); i++ {
		if first.Files[i-1] > first.Files[i] {
			t.Fatalf("manifest is not sorted: %v", first.Files)
		}
	}
}

func TestFactFileRenderSurvivesAwkwardTaxonomySegments(t *testing.T) {
	// Spaces, punctuation and unicode all appear in real taxonomies. They must
	// become a usable directory name rather than a quoting problem.
	cases := map[string]string{
		"retry policy":   "retry-policy",
		"API/v2":         "API-v2",
		"caché":          "cach",
		"--leading--":    "leading",
		".":              "",
		"..":             "",
		"":               "",
		"already_fine-1": "already_fine-1",
	}
	for in, want := range cases {
		if got := sanitizeFactPathSegment(in); got != want {
			t.Fatalf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// A retracted fact's file must not survive the next export. `facts files`
// documents that "the directory is rewritten on each run"; if stale files
// linger, a grep over the export returns things the brain no longer believes
// and presents them as current — which is worse than not having the export,
// because it looks authoritative.
func TestExportRemovesAFactThatIsNoLongerCurrent(t *testing.T) {
	dir := t.TempDir()
	keep := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports are numbered from 8000.", Status: factStatusActive}
	drop := factRecord{ID: "fact:bbbb2222", Paths: []string{"constraints.retry.policy"}, Text: "Retries stop after three attempts.", Status: factStatusActive}

	first, err := writeFactFiles(dir, "main", []factRecord{keep, drop})
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	if first.Written != 2 {
		t.Fatalf("first export wrote %d files", first.Written)
	}
	stale := filepath.Join(dir, "constraints", "retry", "policy", factFileName(drop.ID))
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The second fact was retracted, so it is no longer in the input.
	if _, err := writeFactFiles(dir, "main", []factRecord{keep}); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a fact removed from the brain still has a file in the export")
	}
	// And the fact that is still current must survive.
	live := filepath.Join(dir, "architecture", "api", "ports", factFileName(keep.ID))
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a current fact's file was removed: %v", err)
	}
}

// The export directory is chosen by the user with --dir. Clearing it would be
// the obvious fix and a destructive one: pointed at a directory holding
// anything else, it would delete work that has nothing to do with the brain.
// Only files a previous export recorded writing may be removed.
func TestExportNeverRemovesAFileItDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "NOTES.md")
	if err := os.WriteFile(mine, []byte("my own notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "architecture", "api", "ports")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file that sits exactly where an exported fact would, and is not one.
	theirs := filepath.Join(nested, "fact-deadbeef9999.md")
	if err := os.WriteFile(theirs, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}

	fact := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports are numbered from 8000.", Status: factStatusActive}
	if _, err := writeFactFiles(dir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, err := writeFactFiles(dir, "main", nil); err != nil {
		t.Fatalf("second export: %v", err)
	}

	for _, path := range []string{mine, theirs} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the export deleted %s, which it did not write", filepath.Base(path))
		}
	}
}

// The manifest is a JSON file sitting inside a directory the user controls, so
// its contents are input, not a trusted record. An entry pointing outside the
// export root would turn the next export into a delete of whatever it names.
func TestExportEscapingManifestEntryIsRefused(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.md")
	if err := os.WriteFile(outside, []byte("not in the export"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "export")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Slash-separated literals, not filepath.Join: on Windows Join emits
	// backslashes, which are invalid JSON escapes — the manifest would be
	// corrupt rather than escaping, and the test would exercise the
	// corrupt-manifest path instead of the containment check.
	manifest := `{"branch":"main","files":["../outside.md","../../etc/hosts","/etc/hosts","..\\..\\windows\\system32\\drivers\\etc\\hosts"]}`
	if err := os.WriteFile(filepath.Join(dir, factFilesManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeFactFiles(dir, "main", nil); err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a manifest entry escaped the export root and deleted %s", outside)
	}
}

// Filenames use a 12-character prefix of the fact id. A collision inside one
// taxonomy directory is improbable; silently losing a fact to it is not
// acceptable at any probability, because the export would report both as
// written and one of them would simply not be there.
func TestExportRefusesTwoFactsThatWouldShareAFile(t *testing.T) {
	dir := t.TempDir()
	// Same first 12 characters after the prefix, same taxonomy directory.
	a := factRecord{ID: "fact:abcdef012345AAAA", Paths: []string{"architecture.api.ports"}, Text: "First.", Status: factStatusActive}
	b := factRecord{ID: "fact:abcdef012345BBBB", Paths: []string{"architecture.api.ports"}, Text: "Second.", Status: factStatusActive}
	if factFileName(a.ID) != factFileName(b.ID) {
		t.Fatalf("the fixture does not collide: %s vs %s", factFileName(a.ID), factFileName(b.ID))
	}

	result, err := writeFactFiles(dir, "main", []factRecord{a, b})
	if err == nil {
		t.Fatalf("a colliding export reported success, writing %d file(s) for 2 facts", result.Written)
	}
	// The error has to name both facts, or nobody can act on it.
	for _, id := range []string{a.ID, b.ID} {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("the error does not name %s: %v", id, err)
		}
	}
}

// A symlink planted at a path component redirects a write or a delete out of
// --dir entirely. No string check catches it: every component is a plain name
// and the escape happens in the filesystem. This is the guard every other
// place in this codebase that writes under a user-named directory applies.
func TestExportRefusesToWriteThroughASymlinkedComponent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "export")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The first taxonomy segment is a symlink pointing out of the export.
	if err := os.Symlink(outside, filepath.Join(dir, "architecture")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fact := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	_, err := writeFactFiles(dir, "main", []factRecord{fact})
	if err == nil {
		t.Fatal("the export wrote through a symlinked path component")
	}

	// And nothing may have landed outside.
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the export wrote %d entr(ies) outside --dir", len(entries))
	}
}

// The same for deletes: a manifest entry whose parent is a symlink would
// otherwise redirect os.Remove outside the export root.
func TestPruneRefusesToDeleteThroughASymlinkedComponent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "fact-deadbeef1234.md")
	if err := os.WriteFile(victim, []byte("somebody else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "export")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "architecture")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A manifest naming a path that resolves through the symlink. Every
	// component is a plain name, so the textual checks admit it.
	manifest := `{"files":["architecture/fact-deadbeef1234.md"]}`
	if err := os.WriteFile(filepath.Join(dir, factFilesManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeFactFiles(dir, "main", nil); err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatal("the prune deleted a file outside --dir through a symlinked component")
	}
}

// The directory guard checks the taxonomy components; it does not check the
// file. A symlink planted at the fact file itself is a separate escape, and
// os.WriteFile follows it — writing a fact's contents over whatever it points
// at, outside --dir.
func TestExportRefusesToWriteThroughASymlinkedFile(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim.md")
	if err := os.WriteFile(victim, []byte("somebody else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "export")
	fact := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	nested := filepath.Join(dir, "architecture", "api", "ports")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// Every directory component is real; only the leaf is a symlink.
	if err := os.Symlink(victim, filepath.Join(nested, factFileName(fact.ID))); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := writeFactFiles(dir, "main", []factRecord{fact}); err == nil {
		t.Fatal("the export wrote through a symlinked fact file")
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "somebody else's file" {
		t.Fatalf("the export overwrote a file outside --dir: %q", data)
	}
}

// The manifest is a file in the export directory like any other, so it can be
// redirected by a planted symlink the same way a fact file can. It was the one
// write left unguarded after the others were fixed.
func TestManifestIsNotWrittenThroughASymlink(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim.json")
	if err := os.WriteFile(victim, []byte("somebody else's file"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "export")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, factFilesManifestName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fact := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	_, writeErr := writeFactFiles(dir, "main", []factRecord{fact})

	data, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "somebody else's file" {
		t.Fatalf("the manifest was written through a symlink, overwriting %s: %q", victim, data)
	}
	// And it has to be reported. The facts were written, but with no manifest
	// this export can never be reconciled — proceeding in silence would leave
	// every file it wrote permanently orphaned.
	if writeErr == nil {
		t.Fatal("a manifest that could not be written was not reported; the export is unreconcilable and says nothing")
	}
	if !strings.Contains(writeErr.Error(), factFilesManifestName) {
		t.Fatalf("the error does not name the manifest: %v", writeErr)
	}
}

// An export that fails partway leaves files on disk. If the manifest does not
// cover them, no later run can ever remove them — they are orphaned forever,
// and a grep over the export keeps returning them.
func TestPartialExportStillRecordsWhatItWrote(t *testing.T) {
	dir := t.TempDir()
	good := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	// Two facts colliding on one filename: the first is written, then the
	// export fails — the exact partial-failure shape.
	collideA := factRecord{ID: "fact:bbbbbbbbbbbbAAAA", Paths: []string{"constraints.retry.policy"}, Text: "First.", Status: factStatusActive}
	collideB := factRecord{ID: "fact:bbbbbbbbbbbbBBBB", Paths: []string{"constraints.retry.policy"}, Text: "Second.", Status: factStatusActive}

	if _, err := writeFactFiles(dir, "main", []factRecord{good, collideA, collideB}); err == nil {
		t.Fatal("the colliding export reported success")
	}
	orphan := filepath.Join(dir, "architecture", "api", "ports", factFileName(good.ID))
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("setup: the first fact was not written: %v", err)
	}

	// The manifest must now cover the file the failed run left behind, so the
	// next export removes it.
	manifest, manifestErr := readFactFilesManifest(dir)
	if manifestErr != nil {
		t.Fatalf("read manifest: %v", manifestErr)
	}
	found := false
	for _, rel := range manifest.Files {
		if strings.Contains(rel, factFileName(good.ID)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failed export left %s on disk and out of the manifest: %v", orphan, manifest.Files)
	}

	// And the next successful export with no facts removes it.
	if _, err := writeFactFiles(dir, "main", nil); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if _, err := os.Stat(orphan); err == nil {
		t.Fatal("the orphaned file survived the next export")
	}
}

// A partial failure must not make the export forget what earlier runs wrote.
// Recording only the files this run managed to write drops the previous
// entries, and everything a successful earlier export left behind becomes
// unremovable.
func TestPartialExportDoesNotForgetEarlierFiles(t *testing.T) {
	dir := t.TempDir()
	first := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	if _, err := writeFactFiles(dir, "main", []factRecord{first}); err != nil {
		t.Fatalf("first export: %v", err)
	}
	firstFile := filepath.Join(dir, "architecture", "api", "ports", factFileName(first.ID))
	if _, err := os.Stat(firstFile); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// A second export that writes one new file and then fails.
	second := factRecord{ID: "fact:cccc3333", Paths: []string{"conventions.style.naming"}, Text: "Names are lower case.", Status: factStatusActive}
	collideA := factRecord{ID: "fact:dddddddddddd1111", Paths: []string{"constraints.retry.policy"}, Text: "A.", Status: factStatusActive}
	collideB := factRecord{ID: "fact:dddddddddddd2222", Paths: []string{"constraints.retry.policy"}, Text: "B.", Status: factStatusActive}
	if _, err := writeFactFiles(dir, "main", []factRecord{second, collideA, collideB}); err == nil {
		t.Fatal("the colliding export reported success")
	}

	// Both the earlier file and the one this run wrote must be reconcilable.
	manifest, manifestErr := readFactFilesManifest(dir)
	if manifestErr != nil {
		t.Fatalf("read manifest: %v", manifestErr)
	}
	for _, want := range []string{factFileName(first.ID), factFileName(second.ID)} {
		found := false
		for _, rel := range manifest.Files {
			if strings.Contains(rel, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is on disk and missing from the manifest: %v", want, manifest.Files)
		}
	}

	if _, err := writeFactFiles(dir, "main", nil); err != nil {
		t.Fatalf("third export: %v", err)
	}
	if _, err := os.Stat(firstFile); err == nil {
		t.Fatal("a file from the first export survived; the partial record forgot it")
	}
}

// Treating a corrupt manifest as empty permanently orphans everything a
// previous export wrote: nothing can reconcile against a record that has been
// silently discarded, and the directory quietly stops meaning what the command
// says it means. Refusing is recoverable in one step.
func TestCorruptManifestIsRefusedNotIgnored(t *testing.T) {
	dir := t.TempDir()
	fact := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	if _, err := writeFactFiles(dir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("first export: %v", err)
	}
	written := filepath.Join(dir, "architecture", "api", "ports", factFileName(fact.ID))
	if _, err := os.Stat(written); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, factFilesManifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := writeFactFiles(dir, "main", nil)
	if err == nil {
		t.Fatal("a corrupt manifest was treated as empty; the previously exported file is now unremovable")
	}
	// The error has to name the file and the way out, or the user is stuck.
	if !strings.Contains(err.Error(), factFilesManifestName) {
		t.Fatalf("the error does not name the manifest: %v", err)
	}
	if !strings.Contains(err.Error(), "delete") {
		t.Fatalf("the error does not say how to recover: %v", err)
	}
	// And it must not have destroyed anything on the way out.
	if _, err := os.Stat(written); err != nil {
		t.Fatal("the refused export removed a file")
	}
}

// An empty or whitespace-only manifest is not corruption — it is what a
// truncated write leaves — and must read as "nothing recorded" rather than
// blocking every future export.
func TestEmptyManifestIsNotTreatedAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{"", "   \n"} {
		if err := os.WriteFile(filepath.Join(dir, factFilesManifestName), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readFactFilesManifest(dir); err != nil {
			t.Fatalf("an empty manifest was refused: %v", err)
		}
	}
}

// Every path check in the export is relative to --dir, so a symlink AT --dir is
// invisible to all of them — and this command both writes and deletes. `export`
// refuses a symlinked output directory for the same reason.
func TestExportRefusesASymlinkedOutputDirectory(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fact := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	_, err := writeFactFiles(link, "main", []factRecord{fact})
	if err == nil {
		t.Fatal("the export wrote into a symlinked output directory")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("the error does not say why: %v", err)
	}
	entries, readErr := os.ReadDir(real)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused export still wrote %d entr(ies)", len(entries))
	}
}

// A file where the export directory should be is a mistake worth naming rather
// than a confusing MkdirAll failure.
func TestExportRefusesAnOutputPathThatIsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := writeFactFiles(path, "main", nil)
	if err == nil {
		t.Fatal("a file was accepted as the export directory")
	}
	// Our message, not the platform's: MkdirAll's wording for "there is a file
	// there" differs between Unix and Windows, so relying on it made this test
	// pass on one and fail on the other.
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("the error does not say why: %v", err)
	}
}

// The directory check has to describe the state actually written into, not the
// state a moment earlier. Checking before MkdirAll leaves a window in which dir
// can be replaced by a symlink, and every path check after that point is
// relative to dir and would follow it.
//
// The window itself is not reproducible from a test without instrumenting the
// syscall, so this pins the property that closes it: the check runs against a
// directory that already exists, which is the post-creation state.
func TestExportChecksTheDirectoryItWillActuallyWriteInto(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// A symlink that already exists is what MkdirAll leaves untouched — it
	// succeeds on a link to an existing directory — so a check placed only
	// before it would be the one that mattered, and one placed after it must
	// still catch this.
	fact := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	_, err := writeFactFiles(link, "main", []factRecord{fact})
	if err == nil {
		t.Fatal("the export wrote into a symlinked directory that MkdirAll had accepted")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("the error does not say why: %v", err)
	}
	entries, readErr := os.ReadDir(real)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused export wrote %d entr(ies) through the link", len(entries))
	}
}

// A manifest failure during error recovery must not be swallowed: the files are
// on disk and nothing records them, which is the silent-orphan failure this
// file has been bitten by twice.
func TestPartialExportReportsAFailedManifestWrite(t *testing.T) {
	dir := t.TempDir()
	// Plant a symlinked manifest so recording the partial export fails, and
	// force a partial export with a filename collision.
	victim := filepath.Join(t.TempDir(), "victim.json")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, factFilesManifestName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	good := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports.", Status: factStatusActive}
	a := factRecord{ID: "fact:bbbbbbbbbbbbAAAA", Paths: []string{"constraints.retry.policy"}, Text: "A.", Status: factStatusActive}
	b := factRecord{ID: "fact:bbbbbbbbbbbbBBBB", Paths: []string{"constraints.retry.policy"}, Text: "B.", Status: factStatusActive}

	_, err := writeFactFiles(dir, "main", []factRecord{good, a, b})
	if err == nil {
		t.Fatal("the colliding export reported success")
	}
	// Both halves have to survive: the reason the export failed, and the fact
	// that its files are now untracked.
	if !strings.Contains(err.Error(), "both map to") {
		t.Fatalf("the original failure was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "not tracked") {
		t.Fatalf("the manifest failure was swallowed, so the orphaned files are unreported: %v", err)
	}
}

// The manifest is keyed by --dir, and the facts come from the current branch.
// Exporting one branch and then another into the same directory made the second
// run treat the first branch's files as stale and delete them — destroying an
// export silently, on the workflow this file's header advertises.
func TestExportDoesNotDeleteAnotherBranchesFiles(t *testing.T) {
	dir := t.TempDir()
	onMain := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	onFeature := factRecord{ID: "fact:bbbb2222", Paths: []string{"constraints.retry.policy"}, Text: "Retries stop at three.", Status: factStatusActive}

	if _, err := writeFactFiles(dir, "main", []factRecord{onMain}); err != nil {
		t.Fatalf("main export: %v", err)
	}
	mainFile := filepath.Join(dir, "architecture", "api", "ports", factFileName(onMain.ID))
	if _, err := os.Stat(mainFile); err != nil {
		t.Fatalf("setup: %v", err)
	}

	result, err := writeFactFiles(dir, "feature", []factRecord{onFeature})
	if err != nil {
		t.Fatalf("feature export: %v", err)
	}
	if _, err := os.Stat(mainFile); err != nil {
		t.Fatal("exporting a second branch into the same directory deleted the first branch's files")
	}
	// Silence would be worse than the deletion in one way: the directory now
	// holds two branches' facts and nothing says so.
	if len(result.Warnings) == 0 {
		t.Fatal("the directory now holds two branches' exports and nothing said so")
	}
	if !strings.Contains(strings.Join(result.Warnings, " "), "main") {
		t.Fatalf("the warning does not name the other branch: %v", result.Warnings)
	}
}

// And re-exporting the SAME branch must still reconcile, or the branch check
// has disabled pruning rather than scoping it.
func TestExportStillPrunesWithinOneBranch(t *testing.T) {
	dir := t.TempDir()
	keep := factRecord{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "Ports start at 8000.", Status: factStatusActive}
	drop := factRecord{ID: "fact:bbbb2222", Paths: []string{"constraints.retry.policy"}, Text: "Retries stop at three.", Status: factStatusActive}

	if _, err := writeFactFiles(dir, "main", []factRecord{keep, drop}); err != nil {
		t.Fatalf("first export: %v", err)
	}
	stale := filepath.Join(dir, "constraints", "retry", "policy", factFileName(drop.ID))
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := writeFactFiles(dir, "main", []factRecord{keep}); err != nil {
		t.Fatalf("second export: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("a retracted fact's file survived a same-branch re-export")
	}
}

// A warning on the result is only useful if the command prints it.
func TestFactsFilesCommandPrintsCrossBranchWarnings(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)
	dir := t.TempDir()

	// A previous export recorded for a different branch.
	if _, err := writeFactFiles(dir, "some-other-branch", []factRecord{
		{ID: "fact:aaaa1111", Paths: []string{"architecture.api.ports"}, Text: "From elsewhere.", Status: factStatusActive},
	}); err != nil {
		t.Fatalf("seed export: %v", err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		return writeFacts(brainDir, branch, []factRecord{
			{ID: "fact:bbbb2222", Paths: []string{"constraints.retry.policy"}, Text: "Here.", Branch: branch, Status: factStatusActive, CreatedAt: now, UpdatedAt: now},
		})
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}

	cmd := newFactsFilesCommand(opts)
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("dir", dir); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("facts files: %v", err)
	}
	if !strings.Contains(errOut.String(), "some-other-branch") {
		t.Fatalf("the command did not report that the directory holds another branch's export: %q", errOut.String())
	}
}
