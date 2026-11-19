package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestBrainIgnoreAcceptsAUTF8ByteOrderMark pins how loadBrainIgnore treats a
// leading byte-order mark.
//
// PowerShell's `Set-Content -Encoding utf8` writes one, so a .brainignore
// created on Windows usually begins with it. U+FEFF is not whitespace to
// unicode.IsSpace, so the TrimSpace in the parser left it attached to the
// first line and that line matched nothing. Nothing reported it: the
// repository asked for a path to be kept out of the index and out of an
// agent's context, and the path was indexed anyway.
//
// Exactly one mark, at byte zero, which is all git accepts in an exclude
// file. A second mark, or one further down, is pattern text -- worth holding
// because "strip every U+FEFF anywhere" would silently repair a genuinely
// malformed file instead of leaving it visible.
//
// Each case asserts the parsed pattern list, not just the verdict on one
// path. A verdict alone is too coarse to see the comment case: an unstripped
// mark turns "# a comment" into a pattern rather than a comment, which no
// query about zzcustom/ can detect, so that case would pass with the fix
// reverted and prove nothing.
//
// Unlike the git-ceiling tests there is no external oracle here: .brainignore
// is brain's own format, not git's, so these cases state brain's rule rather
// than checking it against another implementation.
func TestBrainIgnoreAcceptsAUTF8ByteOrderMark(t *testing.T) {
	t.Parallel()
	const bom = "\xef\xbb\xbf"

	for _, tc := range []struct {
		name         string
		body         string
		wantPatterns []string
		ignored      bool
	}{
		{"no mark", "zzcustom/\n", []string{"zzcustom/"}, true},
		{"one mark at byte zero", bom + "zzcustom/\n", []string{"zzcustom/"}, true},
		{"a marked comment is still a comment", bom + "# a comment\nzzcustom/\n", []string{"zzcustom/"}, true},
		{"a second mark stays pattern text", bom + bom + "zzcustom/\n", []string{bom + "zzcustom/"}, false},
		{"a mark on a later line stays pattern text", "unrelated/\n" + bom + "zzcustom/\n", []string{"unrelated/", bom + "zzcustom/"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ".brainignore"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			ignore, err := loadBrainIgnore(dir)
			if err != nil {
				t.Fatalf("loadBrainIgnore: %v", err)
			}
			if !slices.Equal(ignore.patterns, tc.wantPatterns) {
				t.Fatalf("parsed patterns %q, want %q", ignore.patterns, tc.wantPatterns)
			}
			if got := ignore.Ignored("zzcustom/lib.js"); got != tc.ignored {
				t.Fatalf("Ignored(%q) = %v, want %v", "zzcustom/lib.js", got, tc.ignored)
			}
		})
	}
}
