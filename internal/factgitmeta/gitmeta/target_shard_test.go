package gitmeta

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

// Regression: a commit target value shorter than the two characters TreeBasePath
// shards on used to PANIC ("slice bounds out of range [:2]") instead of erroring.
// ParseTarget enforced a 3-character minimum, but nothing else did — and the
// Target materialize.go rebuilds from a tree leaf path never goes through
// ParseTarget, so a tree carrying "commit/ab/a/k/__value" crashed the process on
// the very next write. Found by FuzzTreePath.
func TestShortTargetValueIsRejectedNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"one char", "a"},
		{"two chars", "ab"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := Target{Type: TargetCommit, Value: tc.value}
			if err := ValidateTargetValue(target); err == nil {
				t.Fatalf("ValidateTargetValue accepted a commit value of %d chars", len(tc.value))
			}
			for name, fn := range map[string]func() (string, error){
				"TreePath":      func() (string, error) { return TreePath(target, "k") },
				"ListDirPath":   func() (string, error) { return ListDirPath(target, "k") },
				"SetDirPath":    func() (string, error) { return SetDirPath(target, "k") },
				"TombstonePath": func() (string, error) { return TombstonePath(target, "k") },
				"ListEntryTomb": func() (string, error) { return ListEntryTombstonePath(target, "k", "1-abcde") },
				"SetMemberTomb": func() (string, error) { return SetMemberTombstonePath(target, "k", "m") },
			} {
				if _, err := fn(); err == nil {
					t.Errorf("%s accepted a commit value of %d chars", name, len(tc.value))
				}
			}
			// TreeBasePath returns no error, so it must at least stay total.
			if got := TreeBasePath(target); got == "" {
				t.Errorf("TreeBasePath returned an empty path for %q", tc.value)
			}
		})
	}
}

// A tree leaf naming a too-short target value is a MALFORMED TREE, caught at the
// read boundary — not something Materialize carries into State for Serialize to
// crash on. This is the end-to-end shape of the crash: the exact loop
// MetaStore.updateLocked runs on every write.
func TestMaterializeRejectsShortTargetValue(t *testing.T) {
	store := memory.NewStorage()
	root, err := BuildTree([]blobFile{{path: "commit/ab/a/k/__value", content: "v"}}, store)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	tree, err := object.GetTree(store, root)
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	state, err := Materialize(tree)
	if err == nil {
		// Pre-fix this materialized fine and the Serialize below panicked.
		out := memory.NewStorage()
		if _, serr := Serialize(state, out); serr == nil {
			t.Fatalf("a tree with a 1-character commit target round-tripped: %#v", state)
		}
		t.Fatalf("Materialize accepted a malformed tree: %#v", state)
	}
	if !errors.Is(err, ErrMalformedTree) {
		t.Fatalf("want ErrMalformedTree, got %v", err)
	}
	if !strings.Contains(err.Error(), "at least") {
		t.Fatalf("error should name the length rule, got %v", err)
	}
}

// ParseTarget's documented 3-character minimum must still hold now that the
// check lives in ValidateTargetValue.
func TestParseTargetStillEnforcesMinimumLength(t *testing.T) {
	for _, s := range []string{"commit:a", "commit:ab", "branch:x", "change-id:yz"} {
		if _, err := ParseTarget(s); err == nil {
			t.Errorf("ParseTarget(%q) accepted a value below the minimum", s)
		}
	}
	for _, s := range []string{"commit:abc", "branch:main", "project", "path:a/b"} {
		if _, err := ParseTarget(s); err != nil {
			t.Errorf("ParseTarget(%q) rejected a legal target: %v", s, err)
		}
	}
}
