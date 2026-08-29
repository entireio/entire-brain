package repoid

import (
	"errors"
	"net/url"
	"path"
	"testing"
)

// TestPathEscapeIsNotTheRule records WHY this package exists, and is the assertion
// the first attempt at this fix was missing: it pins the behaviour of the escape
// function that was mistaken for a defence. url.PathEscape leaves the dot segments
// completely alone, so an escaped repo id can still re-address the request.
func TestPathEscapeIsNotTheRule(t *testing.T) {
	// "." and ".." are the ones a normalizer actually collapses; "..." is rejected
	// because a normalizer MAY treat it as a dot segment, not because path.Clean does.
	for _, id := range []string{".", ".."} {
		if got := url.PathEscape(id); got != id {
			t.Fatalf("url.PathEscape(%q) = %q; this test encodes the premise of the package and the premise changed", id, got)
		}
		target := "/api/v1/repos/" + url.PathEscape(id) + "/brain/mcp"
		cleaned := path.Clean(target)
		if cleaned == target {
			t.Errorf("escaped id %q produced %q, which is already normal form; expected it to be rewritable", id, target)
		}
		t.Logf("escaped id %q yields %q, which a normalizing hop rewrites to %q", id, target, cleaned)
	}
	for _, id := range []string{".", "..", "..."} {
		if err := Validate(id); err == nil {
			t.Errorf("Validate(%q) = nil; escaping it is not enough, so the rule must reject it", id)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	hostile := map[string]string{
		"empty":              "",
		"blank":              "   ",
		"dot":                ".",
		"dotdot":             "..",
		"dotdot padded":      " .. ",
		"triple dot segment": "...",
		"slash":              "a/b",
		"leading slash":      "/admin",
		"trailing slash":     "repo/",
		"backslash":          `a\b`,
		"encoded slash":      "..%2f..",
		"query":              "x?admin=1",
		"fragment":           "x#frag",
		"space":              "repo 1",
		"percent":            "repo%41",
		"nul":                "repo\x00",
		"newline":            "repo\n",
		"traversal chain":    "../../admin",
	}
	for name, id := range hostile {
		t.Run(name, func(t *testing.T) {
			err := Validate(id)
			if err == nil {
				t.Fatalf("Validate(%q) = nil, want a refusal", id)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate(%q) = %v; want it to wrap ErrInvalid", id, err)
			}
		})
	}
}

func TestValidateAcceptsAndYieldsNormalFormSegment(t *testing.T) {
	for _, id := range []string{
		"01JABCDEFGHJKMNPQRSTVWXYZ0",
		"repo-123",
		"a.b.c",
		"repo_1~x",
	} {
		t.Run(id, func(t *testing.T) {
			if err := Validate(id); err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", id, err)
			}
			// The two properties every caller relies on: concatenating the id raw is
			// the same as escaping it, and the resulting path cannot be moved by a
			// normalizing hop.
			if escaped := url.PathEscape(id); escaped != id {
				t.Errorf("accepted id %q escapes to %q, so raw concatenation is not safe", id, escaped)
			}
			target := "/api/v1/repos/" + id + "/brain/mcp"
			if cleaned := path.Clean(target); cleaned != target {
				t.Errorf("accepted id %q yields %q, which a normalizing hop rewrites to %q", id, target, cleaned)
			}
		})
	}
}
