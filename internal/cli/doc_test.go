package cli

import (
	"strings"
	"testing"
)

// Doc chunks must hold clean markdown — not the "<lineNo>\t" rendering distill
// uses — or the line-number prefix pollutes embeddings and hides headings.
func TestChunkLinesCleanForDocsPreservesHeadings(t *testing.T) {
	in := "# Title\n\nSome body text.\nMore detail here.\n"

	clean := chunkLines(in, 3000, false)
	if len(clean) == 0 {
		t.Fatal("expected at least one chunk")
	}
	if strings.Contains(clean[0].Text, "\t") {
		t.Fatalf("doc chunk carries a line-number prefix: %q", clean[0].Text)
	}
	if h := firstHeading(clean[0].Text); h != "Title" {
		t.Fatalf("firstHeading should read the heading from clean text, got %q", h)
	}

	// distill's numbered rendering must be unchanged.
	numbered := chunkLines(in, 3000, true)
	if !strings.Contains(numbered[0].Text, "\t") {
		t.Fatalf("numbered chunk lost its line-number prefix: %q", numbered[0].Text)
	}
}
