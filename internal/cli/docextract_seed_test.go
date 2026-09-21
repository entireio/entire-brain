package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The extractor tests prove a document can be read. These prove it actually
// gets read — that a PDF sitting in docs/ survives selection, conversion and
// indexing and comes back out of a search. A parser nothing calls is worth
// nothing, and every step between the two has its own way of dropping a file:
// the extension denylist, the NUL-byte check that every binary container fails,
// and a doc indexer that only walks markdown.

func TestExtractableDocumentsAreSelectedFromDocs(t *testing.T) {
	for path, want := range map[string]bool{
		"docs/spec.pdf":            true,
		"docs/design.docx":         true,
		"docs/requirements.xlsx":   true,
		"docs/review.pptx":         true,
		"docs/nested/deep/one.pdf": true,
		"docs/notes.md":            true,

		// Not under docs/. A repository is full of binary files that are data
		// rather than documents, and hoovering them in would bury the ones
		// somebody meant to be read.
		"testdata/fixture.pdf":     false,
		"vendor/lib/manual.pdf":    false,
		"spec.pdf":                 false,
		"internal/cli/golden.xlsx": false,

		// A different format entirely, not a variant of one we read.
		"docs/legacy.doc": false,
		"docs/logo.png":   false,
	} {
		if got := isHighSignalDoc(path); got != want {
			t.Fatalf("isHighSignalDoc(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDocumentsOutsideDocsDoNotBypassTheBinaryChecks(t *testing.T) {
	// The bypass in inspectSeedFile is what lets a PDF through two gates that
	// exist to keep binaries out. If it were not scoped, every .pdf, .xlsx and
	// .docx anywhere in the tree — test fixtures, vendored manuals, sample data
	// — would be read and indexed as project documentation.
	repoDir := t.TempDir()
	for _, dir := range []string{"testdata", "vendor"} {
		if err := os.MkdirAll(filepath.Join(repoDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pdf := simplePDF(t, "BT /F1 12 Tf (Fixture data, not documentation.) Tj ET")
	for _, rel := range []string{"testdata/fixture.pdf", "vendor/manual.pdf"} {
		if err := os.WriteFile(filepath.Join(repoDir, filepath.FromSlash(rel)), pdf, 0o644); err != nil {
			t.Fatal(err)
		}
		entry := inspectSeedFile(repoDir, rel, seedCommandOptions{maxFileBytes: 1 << 20})
		if entry.Included {
			t.Fatalf("%s was included as a document (reason %q)", rel, entry.Reason)
		}
	}
}

func TestExtractableDocumentSurvivesTheBinaryChecks(t *testing.T) {
	// Two separate gates reject binary files, and a PDF fails both: .pdf is on
	// the extension denylist, and every PDF and zip contains NUL bytes. Either
	// one silently drops the document before extraction is ever reached, and
	// the symptom is a refresh that succeeds with the file missing.
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	pdf := simplePDF(t, "BT /F1 12 Tf (The retry budget is thirty seconds.) Tj ET")
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "spec.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}

	entry := inspectSeedFile(repoDir, "docs/spec.pdf", seedCommandOptions{maxFileBytes: 1 << 20})
	if !entry.Included {
		t.Fatalf("a readable PDF was excluded as %q", entry.Reason)
	}
	if entry.Category != "docs" {
		t.Fatalf("category = %q, want docs", entry.Category)
	}
}

// seedWithDocument runs the scan and write steps against a repository holding
// one document, and returns what landed in the brain.
func seedWithDocument(t *testing.T, name string, content []byte) (*seedScanResult, string) {
	t.Helper()
	repoDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "docs", name), content, 0o644); err != nil {
		t.Fatal(err)
	}
	entry := inspectSeedFile(repoDir, "docs/"+name, seedCommandOptions{maxFileBytes: 1 << 20})
	if !entry.Included {
		t.Fatalf("%s was excluded as %q", name, entry.Reason)
	}
	seedPath := filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, "docs", name))
	extracted := isExtractableSeedDocument("docs/" + name)
	if extracted {
		seedPath = extractedSeedDocPath(seedPath)
	}
	scan := &seedScanResult{
		RepoDir:      repoDir,
		MaxFileBytes: 1 << 20,
		Docs: []seedDocument{{
			Path:      "docs/" + name,
			SeedPath:  seedPath,
			Bytes:     entry.Bytes,
			Extracted: extracted,
		}},
	}
	if err := os.MkdirAll(filepath.Join(outputDir, seedDirName, seedDocsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSeedDocs(outputDir, scan); err != nil {
		t.Fatalf("writeSeedDocs: %v", err)
	}
	return scan, outputDir
}

func TestSeedWritesExtractedTextAsMarkdown(t *testing.T) {
	pdf := simplePDF(t, "BT /F1 12 Tf (The retry budget is thirty seconds.) Tj ET")
	scan, outputDir := seedWithDocument(t, "spec.pdf", pdf)

	// The .md suffix is what makes the text reachable at all: the doc indexer
	// walks the seed for markdown, so text written as spec.pdf would be parsed
	// by nothing and retrieved by nobody.
	written := filepath.Join(outputDir, filepath.FromSlash(scan.Docs[0].SeedPath))
	if !strings.HasSuffix(written, ".md") {
		t.Fatalf("extracted text was written as %q, which the doc indexer will not read", scan.Docs[0].SeedPath)
	}
	data, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("read extracted doc: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "The retry budget is thirty seconds.") {
		t.Fatalf("the document's text is missing: %q", body)
	}
	// A reader of the brain directory must be able to tell this file is derived
	// and where it came from, or they will edit it and lose the edit.
	if !strings.Contains(body, "docs/spec.pdf") {
		t.Fatalf("the extracted file does not name its source: %q", body)
	}
	if !strings.Contains(body, "derived") {
		t.Fatalf("the extracted file does not say it is derived: %q", body)
	}
}

func TestSeedIndexesExtractedTextForRetrieval(t *testing.T) {
	// The whole point: text out of a PDF has to become retrievable chunks, the
	// same as text out of a markdown file.
	pdf := simplePDF(t, "BT /F1 12 Tf (Compaction runs only when the segment count exceeds eight.) Tj ET")
	_, outputDir := seedWithDocument(t, "storage.pdf", pdf)

	records, files, warnings, err := loadDocRecordsFromSeed(outputDir)
	if err != nil {
		t.Fatalf("build doc index: %v", err)
	}
	if files == 0 {
		t.Fatalf("the doc indexer read no files (warnings: %v)", warnings)
	}
	found := false
	for _, record := range records {
		if strings.Contains(record.Text, "Compaction runs only when the segment count exceeds eight.") {
			found = true
			if !strings.Contains(record.Path, "storage.pdf") {
				t.Fatalf("the chunk does not cite the source document: %q", record.Path)
			}
		}
	}
	if !found {
		t.Fatalf("the PDF's text was not indexed; %d record(s) from %d file(s)", len(records), files)
	}
}

func TestSeedReadsTheWholeContainerNotAPrefix(t *testing.T) {
	// A PDF's cross-references and a zip's central directory both live at the
	// END of the file, so the prefix read used for markdown hands the extractor
	// something it cannot parse. A small fixture hides this — it fits inside any
	// prefix — so this one puts its evidence on the last page of a long document.
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	const pages = 40
	kids := make([]string, 0, pages)
	for i := 0; i < pages; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+i*2))
	}
	b.add(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	for i := 0; i < pages; i++ {
		content := fmt.Sprintf("Page %d of routine filler text that exists only to make this document long. ", i)
		if i == pages-1 {
			content = "Compaction thresholds are documented on the final page."
		}
		b.add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			3+pages*2, 4+i*2))
		b.addStream(t, "", "BT /F1 12 Tf ("+content+") Tj ET")
	}
	b.add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	data := b.build()
	if len(data) < 4096 {
		t.Fatalf("the fixture is too small to test a prefix read (%d bytes)", len(data))
	}

	scan, outputDir := seedWithDocument(t, "long.pdf", data)
	written, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(scan.Docs[0].SeedPath)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(written), "Compaction thresholds are documented on the final page.") {
		t.Fatalf("text from the last page is missing; only a prefix of the container was read")
	}
}

func TestSeedReportsADocumentItCouldNotRead(t *testing.T) {
	// A scan must not vanish. The refresh succeeds either way, so silence here
	// means somebody puts a PDF in docs/, sees no error, and never learns that
	// nothing was indexed.
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << >> /Contents 4 0 R >>")
	b.addStream(t, "", "q 600 0 0 800 0 0 cm /Im0 Do Q")

	scan, outputDir := seedWithDocument(t, "scan.pdf", b.build())

	if len(scan.Warnings) == 0 {
		t.Fatal("an unreadable document produced no warning")
	}
	if !strings.Contains(strings.Join(scan.Warnings, " "), "scan.pdf") {
		t.Fatalf("the warning does not name the document: %v", scan.Warnings)
	}
	if !strings.Contains(scan.Docs[0].Reason, "not indexed") {
		t.Fatalf("the file index does not record why: %q", scan.Docs[0].Reason)
	}
	// And nothing must be written, because an empty markdown file in the seed
	// would be indexed as a document that exists and says nothing.
	if scan.Docs[0].SeedPath != "" {
		t.Fatalf("a seed path was kept for a document that was never written: %q", scan.Docs[0].SeedPath)
	}
	entries, err := os.ReadDir(filepath.Join(outputDir, seedDirName, seedDocsDirName))
	if err == nil && len(entries) > 0 {
		t.Fatalf("an unreadable document still wrote %d file(s) into the seed", len(entries))
	}
}

func TestSeedStillCopiesMarkdownUnchanged(t *testing.T) {
	// The extraction path must not disturb the one that already worked.
	scan, outputDir := seedWithDocument(t, "notes.md", []byte("# Notes\n\nOriginal markdown, byte for byte.\n"))
	if scan.Docs[0].Extracted {
		t.Fatal("markdown was routed through the extractor")
	}
	data, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(scan.Docs[0].SeedPath)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "# Notes\n\nOriginal markdown, byte for byte.\n" {
		t.Fatalf("markdown was rewritten: %q", data)
	}
	if strings.HasSuffix(scan.Docs[0].SeedPath, ".md.md") {
		t.Fatalf("markdown gained a second extension: %q", scan.Docs[0].SeedPath)
	}
}

// The corrections writeSeedDocs makes when extraction fails have to survive the
// trip back to the caller. writeSeedArtifacts took the scan BY VALUE, so every
// one of them — the cleared seed path, the reason, the warning — was written to
// a copy and thrown away. The durable manifest then recorded an unreadable
// document as extracted, with a path that was never written.
//
// This goes through writeSeedArtifacts on purpose. The earlier test called
// writeSeedDocs directly and passed the whole time the bug was live.
func TestSeedArtifactsCarryBackWhatExtractionDiscovered(t *testing.T) {
	repoDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A PDF with no text layer: parses, contains nothing.
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << >> /Contents 4 0 R >>")
	b.addStream(t, "", "q 600 0 0 800 0 0 cm /Im0 Do Q")
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "scan.pdf"), b.build(), 0o644); err != nil {
		t.Fatal(err)
	}

	entry := inspectSeedFile(repoDir, "docs/scan.pdf", seedCommandOptions{maxFileBytes: 1 << 20})
	scan := seedScanResult{
		RepoDir:      repoDir,
		MaxFileBytes: 1 << 20,
		Files:        []seedFileIndexEntry{entry},
		Docs: []seedDocument{{
			Path:      "docs/scan.pdf",
			SeedPath:  extractedSeedDocPath(filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, "docs", "scan.pdf"))),
			Extracted: true,
		}},
	}
	if err := writeSeedArtifacts(outputDir, &scan); err != nil {
		t.Fatalf("writeSeedArtifacts: %v", err)
	}

	if scan.Docs[0].Extracted {
		t.Fatal("the caller's scan still records an unreadable document as extracted")
	}
	if scan.Docs[0].SeedPath != "" {
		t.Fatalf("the caller's scan still carries a seed path that was never written: %q", scan.Docs[0].SeedPath)
	}
	if len(scan.Warnings) == 0 {
		t.Fatal("the caller's scan carries no warning; `document not indexed` never reaches anybody")
	}
	if !strings.Contains(strings.Join(scan.Warnings, " "), "scan.pdf") {
		t.Fatalf("the warning does not name the document: %v", scan.Warnings)
	}
}

// file-index.json is the documented place to see why a file was included or
// skipped. inspectSeedFile marks an extractable document included before
// extraction is attempted — it cannot know whether a PDF has a text layer
// without opening it — so the index must be written after extraction, with the
// answer extraction found.
func TestFileIndexReportsADocumentThatCouldNotBeRead(t *testing.T) {
	repoDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << >> /Contents 4 0 R >>")
	b.addStream(t, "", "q 600 0 0 800 0 0 cm /Im0 Do Q")
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "scan.pdf"), b.build(), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := inspectSeedFile(repoDir, "docs/scan.pdf", seedCommandOptions{maxFileBytes: 1 << 20})
	if !entry.Included {
		t.Fatalf("setup: the document was not selected (%q)", entry.Reason)
	}
	scan := seedScanResult{
		RepoDir:      repoDir,
		MaxFileBytes: 1 << 20,
		Files:        []seedFileIndexEntry{entry},
		Docs: []seedDocument{{
			Path:      "docs/scan.pdf",
			SeedPath:  extractedSeedDocPath(filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, "docs", "scan.pdf"))),
			Extracted: true,
		}},
	}
	if err := writeSeedArtifacts(outputDir, &scan); err != nil {
		t.Fatalf("writeSeedArtifacts: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outputDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatalf("read file index: %v", err)
	}
	var index []seedFileIndexEntry
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("decode file index: %v", err)
	}
	if len(index) != 1 {
		t.Fatalf("file index has %d entries", len(index))
	}
	if index[0].Included {
		t.Fatal("file-index.json reports an unreadable document as successfully included")
	}
	if !strings.Contains(index[0].Reason, "not indexed") {
		t.Fatalf("file-index.json does not say why: %q", index[0].Reason)
	}
}

// A per-stream cap is not a bound on a document. Thousands of pages, each
// inflating to the per-stream cap and producing almost no text, advance nothing
// that ends the loop — so a small file can ask for an unbounded amount of
// decompression work.
func TestPDFDecompressionIsBoundedAcrossTheWholeDocument(t *testing.T) {
	budget := newPDFDecodeBudget()
	if !budget.spend(maxPDFTotalDecodedBytes / 2) {
		t.Fatal("half the budget was refused")
	}
	if budget.exhausted() {
		t.Fatal("the budget reported exhaustion with half of it left")
	}
	if budget.spend(maxPDFTotalDecodedBytes) {
		t.Fatal("a spend past the document budget was allowed")
	}
	if !budget.exhausted() {
		t.Fatal("the budget did not record exhaustion")
	}
	// A nil budget is unlimited, so callers with no document context are
	// unaffected rather than silently capped at zero.
	var none *pdfDecodeBudget
	if !none.spend(1 << 30) {
		t.Fatal("a nil budget refused a spend")
	}
	if none.exhausted() {
		t.Fatal("a nil budget reported exhaustion")
	}
}

func TestPDFStopsReadingPagesOnceTheBudgetIsGone(t *testing.T) {
	// A per-stream cap is not a bound on a document: thousands of pages, each
	// inflating to that cap and producing almost no text, advance nothing that
	// ends the loop. The budget is what ends it, and this proves the loop
	// actually consults it by giving it one too small to finish the document.
	const pages = 20
	// Each page carries a distinct sentence and a lot of drawing operators, so
	// pages that were read are identifiable and decoded bytes far exceed text.
	filler := strings.Repeat("1 0 0 1 0 0 cm ", 4000)
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, 0, pages)
	for i := 0; i < pages; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+i*2))
	}
	b.add(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	for i := 0; i < pages; i++ {
		b.add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			3+pages*2, 4+i*2))
		b.addStream(t, "", fmt.Sprintf("q %s Q BT /F1 12 Tf (Sentence number %d appears here.) Tj ET", filler, i))
	}
	b.add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	data := b.build()

	full, err := extractPDFTextWithin(data, newPDFDecodeBudget())
	if err != nil {
		t.Fatalf("unbounded extraction: %v", err)
	}
	fullPages := strings.Count(full, "appears here.")
	if fullPages != pages {
		t.Fatalf("the fixture is wrong: %d of %d pages read with a full budget", fullPages, pages)
	}

	// A budget that runs out partway through the document.
	small := &pdfDecodeBudget{remaining: int64(len(filler))}
	limited, err := extractPDFTextWithin(data, small)
	if err != nil {
		t.Fatalf("bounded extraction: %v", err)
	}
	limitedPages := strings.Count(limited, "appears here.")
	if limitedPages >= fullPages {
		t.Fatalf("the budget did not stop the page loop: %d of %d pages still read", limitedPages, fullPages)
	}
	if !small.exhausted() {
		t.Fatal("the budget was never spent; decompression is not being accounted for")
	}
}
