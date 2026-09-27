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

// A font is resolved once per page, so a /ToUnicode stream that is a
// decompression bomb would be inflated once per page — thousands of times
// inside a document whose content streams are properly bounded. Font streams
// have to spend from the same allowance.
func TestFontStreamsSpendFromTheDocumentBudget(t *testing.T) {
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
	b.addStream(t, "", "BT /F1 12 Tf (x) Tj ET")
	b.add("<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+Test /Encoding /Identity-H /ToUnicode 6 0 R >>")
	// A large, highly compressible ToUnicode stream.
	b.addStream(t, "", "begincmap\n"+strings.Repeat("% filler filler filler filler\n", 20000)+"endcmap\n")
	data := b.build()

	objects := scanPDFObjects(data, nil)
	pages := pdfPages(objects)
	if len(pages) != 1 {
		t.Fatalf("fixture has %d pages", len(pages))
	}

	budget := newPDFDecodeBudget()
	before := budget.remaining
	pdfPageFonts(objects, pages[0], budget)
	if budget.remaining == before {
		t.Fatal("resolving a font's ToUnicode stream spent nothing from the document budget")
	}

	// And an exhausted budget must stop font resolution decoding at all.
	spent := &pdfDecodeBudget{remaining: 0}
	spent.spend(1) // drive it negative
	fonts := pdfPageFonts(objects, pages[0], spent)
	for name, font := range fonts {
		if len(font.toUnicode) > 0 {
			t.Fatalf("font %s decoded its ToUnicode stream with the budget exhausted", name)
		}
	}
}

// A fixed budget per archive part is not a bound on the archive: it may hold
// maxExtractZipEntries parts, each able to expand to the full cap before the
// aggregate output check ever runs.
func TestOfficePartsDrawDownOneAllowance(t *testing.T) {
	// Slides whose XML is large and yields little text, so the output check
	// does not end the loop — the shape that defeats a per-part budget.
	filler := strings.Repeat("<a:p><a:r><a:t> </a:t></a:r></a:p>", 30000)
	parts := map[string]string{}
	for i := 1; i <= 40; i++ {
		parts[fmt.Sprintf("ppt/slides/slide%d.xml", i)] =
			`<?xml version="1.0"?><p:sld xmlns:a="a" xmlns:p="p"><p:cSld><p:spTree><p:sp><p:txBody>` +
				filler + `<a:p><a:r><a:t>Slide ` + fmt.Sprint(i) + ` text.</a:t></a:r></a:p>` +
				`</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
	}
	data := zipFile(t, parts)

	text, err := extractOfficeText(data, pptxTextParts)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// The allowance runs out partway, so later slides are not read. If every
	// part got a fresh budget, all forty would be.
	read := strings.Count(text, " text.")
	if read == 40 {
		t.Fatal("every part was read; the allowance is not shared across parts")
	}
	if read == 0 {
		t.Fatal("no part was read; the allowance is too small to make progress")
	}
}

// A document whose bytes cannot be read is as absent as one whose extraction
// fails, and must reach the file index and the warnings the same way.
func TestUnreadableDocumentBytesAreReportedLikeAFailedExtraction(t *testing.T) {
	repoDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repoDir, "docs", "spec.pdf")
	if err := os.WriteFile(path, simplePDF(t, "BT /F1 12 Tf (x) Tj ET"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := inspectSeedFile(repoDir, "docs/spec.pdf", seedCommandOptions{maxFileBytes: 1 << 20})
	if !entry.Included {
		t.Fatalf("setup: %q", entry.Reason)
	}
	// Unreadable, but still present — so this reaches safeReadFile rather than
	// tripping the path checks above it. A directory where a file is expected
	// is the portable way to do that; removing the file would exercise a
	// different branch, which is how the first version of this test passed
	// against a build that had not fixed this one.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	scan := seedScanResult{
		RepoDir:      repoDir,
		MaxFileBytes: 1 << 20,
		Files:        []seedFileIndexEntry{entry},
		Docs: []seedDocument{{
			Path:      "docs/spec.pdf",
			SeedPath:  extractedSeedDocPath(filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, "docs", "spec.pdf"))),
			Extracted: true,
		}},
	}
	if err := writeSeedArtifacts(outputDir, &scan); err != nil {
		t.Fatalf("writeSeedArtifacts: %v", err)
	}

	if len(scan.Warnings) == 0 {
		t.Fatal("an unreadable document produced no warning")
	}
	data, err := os.ReadFile(filepath.Join(outputDir, seedDirName, "file-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index []seedFileIndexEntry
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index) != 1 || index[0].Included {
		t.Fatalf("file-index.json still reports an unreadable document as included: %+v", index)
	}
}

// The scan fingerprint exists so a packet can tell whether its inputs changed.
// Extractable documents return early from inspectSeedFile to skip the NUL-byte
// check — every PDF and Office file fails it by construction — and that early
// return also skipped the content hash, so editing a PDF under docs/ left the
// fingerprint identical and the packet looked fresh when it was not.
func TestEditingAnExtractableDocumentChangesItsHash(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repoDir, "docs", "spec.pdf")

	write := func(text string) seedFileIndexEntry {
		if err := os.WriteFile(path, simplePDF(t, "BT /F1 12 Tf ("+text+") Tj ET"), 0o644); err != nil {
			t.Fatal(err)
		}
		return inspectSeedFile(repoDir, "docs/spec.pdf", seedCommandOptions{maxFileBytes: 1 << 20})
	}

	before := write("The retry budget is thirty seconds.")
	if !before.Included {
		t.Fatalf("the PDF was excluded as %q", before.Reason)
	}
	if before.Hash == "" {
		t.Fatal("an included document carries no content hash, so the fingerprint cannot move with it")
	}

	after := write("The retry budget is ninety seconds.")
	if after.Hash == before.Hash {
		t.Fatalf("editing the document left its hash unchanged (%s); the fingerprint would report it fresh", after.Hash)
	}

	// Rewriting identical content must not churn the hash either, or every
	// scan would look like a change.
	again := write("The retry budget is ninety seconds.")
	if again.Hash != after.Hash {
		t.Fatalf("identical content produced a different hash: %s vs %s", again.Hash, after.Hash)
	}
}

// The fallback scan and the ToUnicode merge both walk every stream. The merge
// runs first and decodes the CMap streams; the scan must not decode them again,
// because the decode budget is document-wide and this is the path taken by the
// degraded files where it is most likely to bind.
//
// The document below has no page tree, so the page path yields nothing and the
// fallback runs — with a CMap stream present for it to re-decode.
func TestPDFFallbackDoesNotDecodeCMapStreamsTwice(t *testing.T) {
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog >>") // no /Pages: forces the fallback
	b.addStream(t, "/Type /CMap", strings.Repeat("begincmap endcmap\n", 400))
	b.addStream(t, "", "BT /F1 12 Tf (The retry budget is thirty seconds.) Tj ET")
	pdf := b.build()

	full := &pdfDecodeBudget{remaining: 1 << 20}
	text, err := extractPDFTextWithin(pdf, full)
	if err != nil || !strings.Contains(text, "retry budget") {
		t.Skipf("this document does not reach the fallback: %q %v", text, err)
	}
	spent := (1 << 20) - full.remaining
	t.Logf("budget spent: %d bytes", spent)

	// The bound is absolute, derived from the fixture rather than from the
	// measurement: a budget computed off the observed spend scales with the
	// bug and can never fail. The CMap body is the dominant stream, so one
	// honest pass costs a little over its size and a second decode costs about
	// that again. Measured: 7,256 bytes with the skip, 14,456 without.
	cmapBytes := int64(400 * len("begincmap endcmap\n"))
	if ceiling := cmapBytes + cmapBytes/2; spent > ceiling {
		t.Fatalf("decoding spent %d bytes against a ceiling of %d; the CMap stream is being decoded twice", spent, ceiling)
	}
}

// Object streams decompress during the initial object scan, before any page is
// walked. Passing a nil budget there made that expansion unlimited -- each
// stream capped only per-stream, up to maxPDFObjects of them -- which is the
// decompression bomb the document-wide budget exists to stop, reachable before
// the first page is read.
func TestObjectStreamExpansionSpendsTheDocumentBudget(t *testing.T) {
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog >>")
	// A highly compressible object stream: small on disk, large decompressed.
	payload := strings.Repeat("0 0 ", 2000)
	b.addStream(t, "/Type /ObjStm /N 1 /First 4", payload)
	pdf := b.build()

	// A budget far below the decompressed size must stop the expansion rather
	// than let it run to completion.
	tiny := &pdfDecodeBudget{remaining: 64}
	objects := scanPDFObjects(pdf, tiny)
	if len(objects) == 0 {
		t.Fatal("the fixture produced no objects at all")
	}
	if !tiny.exhausted() {
		t.Fatalf("a %d-byte object stream expanded under a 64-byte budget without exhausting it (remaining %d)",
			len(payload), tiny.remaining)
	}
}

// Colors, BitsPerComponent and Columns come out of the document and their
// product decides make([]byte, rowLen). A file could ask for an allocation far
// larger than anything it actually contained.
func TestPredictorRefusesParametersThatWouldOverAllocate(t *testing.T) {
	data := []byte("short stream")
	for _, tc := range []struct {
		name  string
		parms map[string]pdfValue
	}{
		{"absurd column count", map[string]pdfValue{
			"Predictor": float64(12), "Columns": float64(1 << 40),
		}},
		{"more colour components than CMYK", map[string]pdfValue{
			"Predictor": float64(12), "Colors": float64(1 << 20), "Columns": float64(8),
		}},
		{"impossible sample size", map[string]pdfValue{
			"Predictor": float64(12), "BitsPerComponent": float64(1 << 20), "Columns": float64(8),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pdfApplyPredictor(data, tc.parms)
			// Refused means handed back untouched, never a giant buffer.
			if len(got) > len(data) {
				t.Fatalf("predictor produced %d bytes from %d: the parameters were trusted",
					len(got), len(data))
			}
		})
	}
}

// A legal predictor must still work, or the bounds above would have closed the
// feature rather than the hole.
func TestPredictorStillAppliesForLegalParameters(t *testing.T) {
	// Two PNG-Up rows (tag 2), one byte wide: 1, then +2 => 3.
	raw := []byte{2, 1, 2, 2}
	got := pdfApplyPredictor(raw, map[string]pdfValue{
		"Predictor": float64(12), "Colors": float64(1), "BitsPerComponent": float64(8), "Columns": float64(1),
	})
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("a legal PNG-Up predictor was not applied: %v", got)
	}
}
