package cli

import (
	"os"
	"path/filepath"
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

func TestRankDocsLexicalPrefersCurrentOverHistorical(t *testing.T) {
	index := docIndex{Records: []docRecord{
		{ID: "old", Text: "agent benchmark workflow", Historical: true},
		{ID: "current", Text: "agent benchmark workflow"},
	}}
	out := rankDocsLexical(index, "agent benchmark workflow", 2)
	if len(out) != 2 || out[0].Record.ID != "current" || out[1].Record.ID != "old" {
		t.Fatalf("current docs should rank before historical docs: %+v", out)
	}
	old := docToUnified(out[1].Record)
	if !old.VerificationRequired || !hasRetrievalCaveat(old, retrievalCaveatHistoricalDocument) {
		t.Fatalf("historical doc missing trust caveat: %+v", old)
	}
}

func TestRankDocsLexicalAllowsMuchStrongerHistoricalMatch(t *testing.T) {
	index := docIndex{Records: []docRecord{
		{ID: "old", Text: "refresh semantic freshness contract", Historical: true},
		{ID: "current", Text: "refresh overview"},
	}}
	out := rankDocsLexical(index, "refresh semantic freshness", 2)
	if len(out) != 2 || out[0].Record.ID != "old" {
		t.Fatalf("trust discount became an absolute historical ban: %+v", out)
	}
	if result := docToUnified(out[0].Record); !result.VerificationRequired {
		t.Fatalf("historical winner lost verification caveat: %+v", result)
	}
}

func TestDocTrustAdjustedScorePenalizesNegativeHistoricalScore(t *testing.T) {
	current := docTrustAdjustedScore(-0.5, false)
	historical := docTrustAdjustedScore(-0.5, true)
	if historical >= current {
		t.Fatalf("historical negative score should be penalized: current=%v historical=%v", current, historical)
	}
}

func TestDocFileHistoricalUsesExplicitMarkers(t *testing.T) {
	if !docFileHistorical("seed/docs/plan.md", "# Plan\n<!-- entire-brain-status: historical -->\n") {
		t.Fatal("explicit historical marker was ignored")
	}
	if !docFileHistorical("seed/docs/archive/plan.md", "# Plan\n") {
		t.Fatal("archive directory was not treated as historical")
	}
	if docFileHistorical("seed/docs/current-plan.md", "# Current plan\n") {
		t.Fatal("an unmarked current plan was treated as historical")
	}
}

// A run of blank lines in a doc must not grow a chunk past maxBytes — the blank
// path is subject to the same size cap and flushes at the boundary.
func TestChunkLinesDocsBlankRunRespectsMaxBytes(t *testing.T) {
	in := "alpha line here\n" + strings.Repeat("\n", 200) + "beta line here\n"
	const max = 40
	for _, c := range chunkLines(in, max, false) {
		if len(c.Text) > max {
			t.Fatalf("chunk exceeded maxBytes (%d): %d bytes %q", max, len(c.Text), c.Text)
		}
	}
}

// loadDocRecordsFromSeed must keep leading indentation (indented code blocks /
// nested lists), trimming only the chunker's trailing newline.
func TestLoadDocRecordsPreservesLeadingIndentation(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, seedDirName)
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "# Title\n\n    indented code line\n    more code\n"
	if err := os.WriteFile(filepath.Join(seed, "x.md"), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, _, _, err := loadDocRecordsFromSeed(dir)
	if err != nil {
		t.Fatalf("loadDocRecordsFromSeed: %v", err)
	}
	var joined string
	for _, r := range recs {
		joined += r.Text
		if strings.HasSuffix(r.Text, "\n") {
			t.Fatalf("trailing newline not trimmed: %q", r.Text)
		}
	}
	if !strings.Contains(joined, "    indented code line") {
		t.Fatalf("leading indentation was stripped: %q", joined)
	}
}

func TestLoadDocRecordsFromSeedSkipsSymlinkedMarkdown(t *testing.T) {
	dir := t.TempDir()
	seedDocs := filepath.Join(dir, seedDirName, seedDocsDirName)
	if err := os.MkdirAll(seedDocs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDocs, "real.md"), []byte("# Real\nlocal docs only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outsidePath, []byte("# Outside\nsentinel secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(seedDocs, "leak.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	recs, files, warnings, err := loadDocRecordsFromSeed(dir)
	if err != nil {
		t.Fatalf("loadDocRecordsFromSeed: %v", err)
	}
	if files != 1 {
		t.Fatalf("files = %d, want only the real markdown file", files)
	}
	var joined string
	for _, rec := range recs {
		joined += rec.Text + "\n"
		if rec.Path == filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, "leak.md")) {
			t.Fatalf("symlinked markdown was indexed: %+v", rec)
		}
	}
	if strings.Contains(joined, "sentinel secret") {
		t.Fatalf("outside symlink content was indexed: %q", joined)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "symlink") {
		t.Fatalf("expected symlink warning, got %v", warnings)
	}
}
