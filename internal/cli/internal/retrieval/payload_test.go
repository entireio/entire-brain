package retrieval

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/entireio/entire-brain/factmerge"
)

func TestRetrievalResultExcerptCentersDenseQueryTerms(t *testing.T) {
	text := strings.Repeat("irrelevant preface ", 50) +
		"inspect code and inspect context JSON are compact by default; use get for full details " +
		strings.Repeat("irrelevant suffix ", 50)
	excerpt := ResultExcerpt(text, "inspect context JSON compact full details", 180)
	for _, want := range []string{"inspect context", "JSON", "compact", "full details"} {
		if !strings.Contains(excerpt, want) {
			t.Fatalf("query-centered excerpt missing %q: %q", want, excerpt)
		}
	}
	if len(excerpt) > 186 { // bounded window plus leading/trailing ellipses
		t.Fatalf("excerpt is not bounded: %d bytes: %q", len(excerpt), excerpt)
	}
	unicodeExcerpt := ResultExcerpt(strings.Repeat("界", 300)+" compact context details", "compact context", 80)
	if !utf8.ValidString(unicodeExcerpt) {
		t.Fatalf("excerpt split UTF-8: %q", unicodeExcerpt)
	}
	// Unicode lowercase can change byte length (K is three UTF-8 bytes but
	// lowercases to one-byte k). Search offsets must still point into the
	// original string and center the actual match.
	foldedExcerpt := ResultExcerpt(strings.Repeat("K", 100)+" TARGET context details", "target context", 64)
	if !utf8.ValidString(foldedExcerpt) || !strings.Contains(foldedExcerpt, "TARGET context") {
		t.Fatalf("case-folded Unicode offset missed the query window: %q", foldedExcerpt)
	}
}

func TestPrintRetrievalCaveatsIncludesStructuredContext(t *testing.T) {
	var out strings.Builder
	PrintResultCaveats(&out, Result{Caveats: []Caveat{
		{
			Kind:    CaveatStaleLocus,
			Message: "Current code no longer contains every recorded path.",
			Paths:   []string{"internal/old.go", "pkg/removed.go"},
		},
		{
			Kind:       CaveatUnresolvedReview,
			Message:    "A pending supersede proposal requires review.",
			ReviewID:   "review:abc",
			Action:     factmerge.ActionSupersede,
			Confidence: 0.55,
		},
	}})
	text := out.String()
	for _, want := range []string{
		"Current code no longer contains every recorded path.",
		"paths=internal/old.go,pkg/removed.go",
		"review=review:abc",
		"action=supersede",
		"confidence=0.55",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("caveat output missing %q:\n%s", want, text)
		}
	}
}
