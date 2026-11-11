package cli

import (
	"strings"
	"testing"
)

// Git for Windows defaults to core.autocrlf=true, so the same commit checks out
// with CRLF line endings there and LF everywhere else. These pin what that does
// and — more importantly — what it does NOT do, because the two halves pull in
// opposite directions and only one of them is safe.
//
// Structure is CRLF-insensitive, and that is load-bearing: evidenceBlocks finds
// blank lines with strings.TrimSpace, which eats the \r, and closes fences the
// same way. A Windows checkout therefore yields the same blocks in the same
// order as a Linux one.
//
// Content is NOT, and that is deliberate (see evidenceBlocks' comment: the
// original CRLF is preserved so code fences and table rows survive verbatim).
// The consequence is the part worth knowing: evidenceHash over the same logical
// source yields a DIFFERENT digest on a CRLF checkout. That is fine while a
// fact and its anchor stay on one machine, and it is exactly the kind of thing
// that bites once facts move between machines — a bundle or a facts sync from a
// Linux teammate carries anchors whose hashes cannot match the Windows
// checkout of the same commit.
//
// So these tests do not assert that CRLF "works". They assert where the
// boundary is, so that a future change to normalization is a deliberate
// decision rather than an accident.

const crlfEvidenceLF = "first paragraph line one\nfirst paragraph line two\n" +
	"\n" +
	"```go\n" +
	"func main() {}\n" +
	"```\n" +
	"\n" +
	"second paragraph\n"

func crlfVariant(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func TestEvidenceBlocksAreCRLFInsensitiveInStructure(t *testing.T) {
	t.Parallel()

	lf := crlfEvidenceLF
	crlf := crlfVariant(lf)

	lfBlocks := evidenceBlocks(lf)
	crlfBlocks := evidenceBlocks(crlf)

	if len(lfBlocks) != len(crlfBlocks) {
		t.Fatalf("block count differs: LF=%d CRLF=%d — a CRLF checkout must not change how source is divided",
			len(lfBlocks), len(crlfBlocks))
	}
	if len(lfBlocks) == 0 {
		t.Fatal("fixture produced no blocks; it no longer exercises anything")
	}

	for i := range lfBlocks {
		lfText := lf[lfBlocks[i][0]:lfBlocks[i][1]]
		crlfText := crlf[crlfBlocks[i][0]:crlfBlocks[i][1]]
		if got, want := strings.ReplaceAll(crlfText, "\r\n", "\n"), lfText; got != want {
			t.Fatalf("block %d differs once \\r is removed:\n LF:   %q\n CRLF: %q", i, want, got)
		}
	}
}

// The other half of the boundary, asserted positively so nobody has to infer it
// from the absence of a test: the digests differ. If a future change normalizes
// line endings before hashing, this test fails and whoever made that change
// gets to decide whether cross-platform anchor portability is now a promise.
func TestEvidenceHashIsCRLFSensitive(t *testing.T) {
	t.Parallel()

	lf := crlfEvidenceLF
	crlf := crlfVariant(lf)

	if lf == crlf {
		t.Fatal("fixture has no line endings to convert")
	}
	if evidenceHash([]byte(lf)) == evidenceHash([]byte(crlf)) {
		t.Fatal("evidenceHash no longer distinguishes CRLF from LF: line endings are now normalized " +
			"before hashing, which makes anchors portable across checkouts. That is a reasonable " +
			"change — update the comment above and the cross-machine expectations that depend on it.")
	}
}
