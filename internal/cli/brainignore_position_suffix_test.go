package cli

import (
	"strings"
	"testing"
)

// A compiler, linter or parser names its location as "<path>:<line>:<col>",
// and the tokenizer only trimmed a TRAILING colon -- so ".env:3:5" survived
// as one token and matched no exact-file pattern: Ignored() compares against
// ".env", and ".env:3:5" is not equal to it, does not contain "/.env", and
// does not end in ".env".
//
// That is the shape these warnings actually take, and such an error routinely
// quotes the offending SOURCE LINE. So a parse failure inside an ignored
// .env, .pem, .key or a user's own secrets pattern was written into the
// snapshot warnings and printed by `brain status`.
//
// Measured before the fix: "/repo/.env" withheld, "/repo/.env:3:5" NOT.
func TestIgnoredPathsStayWithheldWithALineColumnSuffix(t *testing.T) {
	t.Parallel()

	ig := brainIgnore{repoDir: "/repo", patterns: []string{"secrets.yaml", "vault/"}}

	for _, tc := range []struct {
		text string
		want bool
	}{
		// The built-in secret patterns, with and without a position.
		{"/repo/.env", true},
		{"/repo/.env:3:5", true},
		{"/repo/.env:3", true},
		{"/repo/config/prod.key:12:1", true},
		{"/repo/certs/server.pem:1:1", true},
		// A user's own exact-file pattern.
		{"/repo/secrets.yaml", true},
		{"/repo/secrets.yaml:7:2", true},
		// Directory patterns already worked; they must keep working.
		{"/repo/vault/x.go:7:2", true},
		// An ordinary source file is NOT withheld, or the filter would hide
		// every real warning.
		{"/repo/internal/cli/semantic.go:42:7", false},
		{"/repo/main.go", false},
	} {
		if got := ig.MentionsIgnoredPath(tc.text); got != tc.want {
			t.Errorf("MentionsIgnoredPath(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}

	// The realistic case: the warning text quotes the secret itself.
	leak := `parse /repo/.env:3:5: unexpected token in AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI`
	if !ig.MentionsIgnoredPath(leak) {
		t.Errorf("a parse error quoting an ignored file's contents must be withheld:\n  %s", leak)
	}
}

// Only trailing DIGIT groups are stripped. A colon followed by anything else
// may be part of the filename, and over-trimming would withhold warnings about
// files the user never ignored.
func TestPositionSuffixTrimOnlyEatsDigits(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"/repo/.env:3:5":        "/repo/.env",
		"/repo/.env:3":          "/repo/.env",
		"/repo/.env":            "/repo/.env",
		"/repo/weird:name.go":   "/repo/weird:name.go",
		"/repo/weird:name.go:4": "/repo/weird:name.go",
		"/repo/a:1:2:3":         "/repo/a",
		":5":                    ":5",
		"/repo/trailing:":       "/repo/trailing:",
	} {
		if got := trimPathPositionSuffix(in); got != want {
			t.Errorf("trimPathPositionSuffix(%q) = %q, want %q", in, got, want)
		}
	}

	// Non-vacuity: the tokenizer must actually offer the trimmed form.
	toks := brainIgnorePathTokens("parse /repo/.env:3:5: bad")
	joined := strings.Join(toks, " ")
	if !strings.Contains(joined, "/repo/.env ") && !strings.HasSuffix(joined, "/repo/.env") {
		t.Errorf("tokens must include the bare path, got %v", toks)
	}
}
