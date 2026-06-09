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

	// Docs preserve internal blank lines so markdown structure (the paragraph break
	// after the heading) survives into the indexed text.
	if !strings.Contains(clean[0].Text, "\n\n") {
		t.Fatalf("doc chunk dropped the internal blank line: %q", clean[0].Text)
	}

	// distill's numbered rendering must be unchanged: still prefixed, still no blanks.
	numbered := chunkLines(in, 3000, true)
	if !strings.Contains(numbered[0].Text, "\t") {
		t.Fatalf("numbered chunk lost its line-number prefix: %q", numbered[0].Text)
	}
	if strings.Contains(numbered[0].Text, "\n\n") {
		t.Fatalf("numbered transcript chunk should still drop blank lines: %q", numbered[0].Text)
	}
}

// When the doc FTS index can't be opened, retrieval falls back to rankDocsLexical,
// which scores docs by query-term overlap so the doc source doesn't vanish.
func TestRankDocsLexicalFallbackScoresByTermOverlap(t *testing.T) {
	index := docIndex{Records: []docRecord{
		{ID: "d1", Text: "checkpoint advance committed ref"},
		{ID: "d2", Text: "entirely unrelated content"},
	}}
	out := rankDocsLexical(index, "checkpoint advance", 10)
	if len(out) == 0 || out[0].Record.ID != "d1" {
		t.Fatalf("expected d1 (term overlap) first, got %+v", out)
	}
	if got := rankDocsLexical(index, "zzqqxxnomatch", 10); len(got) != 0 {
		t.Fatalf("expected no matches for a term-disjoint query, got %+v", got)
	}
}
