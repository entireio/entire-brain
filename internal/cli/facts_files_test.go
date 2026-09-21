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
