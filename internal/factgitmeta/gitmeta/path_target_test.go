package gitmeta

import (
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/storage/memory"
)

// Regression: path targets were exempt from ValidateTargetValue entirely, so no
// rule applied to their segments. A value carrying an empty segment ("/",
// "a//b", a leading or trailing slash) encoded to a tree path with an EMPTY
// component — "path///__target__/k/__value" — which BuildTree then refuses. It
// refuses the whole Serialize call, not just the offending record, so one such
// record makes the entire store unwritable for every writer that materializes it.
// Found by FuzzTreePath.
func TestPathTargetValueSegmentsAreValidated(t *testing.T) {
	for _, value := range []string{"/", "//", "a//b", "/a", "a/", "", "a/./b", "a/../b", "a\x00b"} {
		target := Target{Type: TargetPath, Value: value}
		if err := ValidateTargetValue(target); err == nil {
			t.Errorf("ValidateTargetValue accepted path value %q", value)
			continue
		}
		if _, err := TreePath(target, "k"); err == nil {
			t.Errorf("TreePath accepted path value %q", value)
		}
	}
}

// Legal path values — the reason path targets are exempt from the no-slash rule
// in the first place — must keep working, and must still serialize.
func TestLegalPathTargetValuesStillSerialize(t *testing.T) {
	for _, value := range []string{"a", "src/main.rs", "a/b/c", "~weird", "__reserved/x", "a.b/c.d"} {
		target := Target{Type: TargetPath, Value: value}
		if err := ValidateTargetValue(target); err != nil {
			t.Errorf("ValidateTargetValue rejected the legal path value %q: %v", value, err)
			continue
		}
		path, err := TreePath(target, "k")
		if err != nil {
			t.Errorf("TreePath rejected the legal path value %q: %v", value, err)
			continue
		}
		for _, comp := range strings.Split(path, "/") {
			if comp == "" {
				t.Errorf("path value %q encoded to %q, which has an empty component", value, path)
			}
		}
		state := State{Strings: []StringVal{{Target: target, Key: "k", Value: "v"}}}
		if _, err := Serialize(state, memory.NewStorage()); err != nil {
			t.Errorf("Serialize rejected the legal path value %q: %v", value, err)
		}
	}
}

// ParseTarget must reject the same values, so a malformed path target cannot
// enter through the API boundary either.
func TestParseTargetRejectsMalformedPathValues(t *testing.T) {
	for _, s := range []string{"path://", "path:/a", "path:a//b", "path:a/"} {
		if _, err := ParseTarget(s); err == nil {
			t.Errorf("ParseTarget(%q) accepted a malformed path target", s)
		}
	}
	if _, err := ParseTarget("path:src/main.rs"); err != nil {
		t.Errorf("ParseTarget rejected a legal path target: %v", err)
	}
}
