package cli

import (
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
	result, err := writeFactFiles(dir, facts)
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
	result, err := writeFactFiles(dir, hostile)
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
	result, err := writeFactFiles(dir, []factRecord{
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
	first, err := writeFactFiles(t.TempDir(), facts)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	second, err := writeFactFiles(t.TempDir(), facts)
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

	first, err := writeFactFiles(dir, []factRecord{keep, drop})
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
	if _, err := writeFactFiles(dir, []factRecord{keep}); err != nil {
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
	if _, err := writeFactFiles(dir, []factRecord{fact}); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if _, err := writeFactFiles(dir, nil); err != nil {
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
	manifest := `{"files":["../outside.md","` + filepath.Join("..", "..", "etc", "hosts") + `","/etc/hosts"]}`
	if err := os.WriteFile(filepath.Join(dir, factFilesManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeFactFiles(dir, nil); err != nil {
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

	result, err := writeFactFiles(dir, []factRecord{a, b})
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
