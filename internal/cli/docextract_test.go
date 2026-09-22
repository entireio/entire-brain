package cli

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// What these tests are for.
//
// An extractor has two ways to fail and only one of them is visible. It can
// refuse a document it should have read, which somebody notices; or it can
// return text that looks fine and is wrong, which nobody notices until a search
// quietly stops finding things. Most of what follows is about the second kind:
// the Identity-H case where bytes are glyph indices, the shared-strings case
// where a spreadsheet reads as numbers with no labels, the scanned PDF that
// would otherwise be indexed as an empty document and counted as a success.

// --- Word --------------------------------------------------------------------

func TestDocxReadsParagraphsAndRuns(t *testing.T) {
	// Word splits a sentence across runs whenever formatting changes, so a
	// reader that takes one run per paragraph loses most of the words in any
	// document that has bold in it.
	data := docxFixture(t,
		docxParagraph("The retry policy ", "backs off ", "exponentially.")+
			docxParagraph("Second paragraph."))

	text, err := extractDocumentText("spec.docx", data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "The retry policy backs off exponentially.") {
		t.Fatalf("runs were not joined into a sentence: %q", text)
	}
	if !strings.Contains(text, "Second paragraph.") {
		t.Fatalf("the second paragraph is missing: %q", text)
	}
	// Paragraphs are separate lines, or every document arrives as one wall of
	// text and chunking has nothing to cut on.
	if !strings.Contains(text, "exponentially.\nSecond") {
		t.Fatalf("paragraphs were not separated: %q", text)
	}
}

func TestDocxDoesNotDependOnTheNamespacePrefix(t *testing.T) {
	// The same document written by a different producer uses a different
	// prefix. Matching on the prefix passes against Word's output and returns
	// nothing for everyone else — a failure that a Word-only fixture hides.
	data := zipFile(t, map[string]string{
		"word/document.xml": `<?xml version="1.0"?>` +
			`<document xmlns="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
			`<body><p><r><t>Prefix-free producers exist.</t></r></p></body></document>`,
	})
	text, err := extractDocumentText("other.docx", data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Prefix-free producers exist.") {
		t.Fatalf("a differently-namespaced document read as %q", text)
	}
}

func TestDocxSeparatesTableCells(t *testing.T) {
	// Without a separator a row's cells run together into one unreadable word,
	// and a table of terms becomes unsearchable.
	row := func(a, b string) string {
		return `<w:tr>` +
			`<w:tc><w:p><w:r><w:t>` + a + `</w:t></w:r></w:p></w:tc>` +
			`<w:tc><w:p><w:r><w:t>` + b + `</w:t></w:r></w:p></w:tc>` +
			`</w:tr>`
	}
	body := `<w:tbl>` + row("Timeout", "30s") + row("Retries", "3") + `</w:tbl>`
	text, err := extractDocumentText("table.docx", docxFixture(t, body))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if strings.Contains(text, "Timeout30s") {
		t.Fatalf("cells ran together: %q", text)
	}
	// Every cell wraps its text in a paragraph, so the naive reading puts each
	// cell on its own line and the row — which is the entire meaning of a table
	// row — is gone. A label separated from its value is not a table.
	if text != "Timeout\t30s\nRetries\t3" {
		t.Fatalf("rows did not survive as rows: %q", text)
	}
}

// --- Excel -------------------------------------------------------------------

func xlsxFixture(t *testing.T) []byte {
	t.Helper()
	return zipFile(t, map[string]string{
		"xl/workbook.xml": `<?xml version="1.0"?><workbook xmlns:r="x"><sheets>` +
			`<sheet name="Requirements" sheetId="1" r:id="rId1"/>` +
			`</sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?><Relationships>` +
			`<Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml": `<?xml version="1.0"?><sst count="3">` +
			`<si><t>Latency budget</t></si>` +
			`<si><t>Owner</t></si>` +
			`<si><r><t>Platform </t></r><r><t>team</t></r></si>` +
			`</sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0"?><worksheet><sheetData>` +
			`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
			`<row r="2"><c r="A2"><v>200</v></c><c r="B2" t="s"><v>2</v></c></row>` +
			`</sheetData></worksheet>`,
	})
}

func TestXlsxResolvesSharedStrings(t *testing.T) {
	// Excel stores text once and refers to it by index, so a reader that only
	// walks the sheets gets a grid of numbers. This is the difference between
	// a searchable spreadsheet and an unsearchable one.
	text, err := extractDocumentText("reqs.xlsx", xlsxFixture(t))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, want := range []string{"Latency budget", "Owner", "200"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q is missing from %q", want, text)
		}
	}
	// A shared string split across formatting runs must come back whole.
	if !strings.Contains(text, "Platform team") {
		t.Fatalf("a multi-run shared string was truncated: %q", text)
	}
	// The index must not leak through as though it were the value.
	if strings.Contains(text, "Latency budget\t1") {
		t.Fatalf("a shared-string index was rendered as a number: %q", text)
	}
}

func TestXlsxLabelsSheetsWithTheirNames(t *testing.T) {
	text, err := extractDocumentText("reqs.xlsx", xlsxFixture(t))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// "Requirements" is the author's own name for the block and is often the
	// most useful search term in the whole file; sheet1 is not.
	if !strings.Contains(text, "Requirements") {
		t.Fatalf("the sheet name is missing: %q", text)
	}
}

func TestXlsxKeepsRowsOnSeparateLines(t *testing.T) {
	text, err := extractDocumentText("reqs.xlsx", xlsxFixture(t))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Latency budget\tOwner") {
		t.Fatalf("columns are not tab-separated: %q", text)
	}
	if strings.Count(text, "\n") < 2 {
		t.Fatalf("rows were joined onto one line: %q", text)
	}
}

// --- PowerPoint --------------------------------------------------------------

func TestPptxReadsSlidesInDeckOrder(t *testing.T) {
	slide := func(text string) string {
		return `<?xml version="1.0"?><p:sld xmlns:a="a" xmlns:p="p"><p:cSld><p:spTree>` +
			`<p:sp><p:txBody><a:p><a:r><a:t>` + text + `</a:t></a:r></a:p></p:txBody></p:sp>` +
			`</p:spTree></p:cSld></p:sld>`
	}
	// slide10 sorts before slide2 lexically. A deck delivered in that order is
	// a deck whose argument no longer follows, which is the whole value of a
	// deck.
	data := zipFile(t, map[string]string{
		"ppt/slides/slide1.xml":  slide("First the problem"),
		"ppt/slides/slide2.xml":  slide("Then the approach"),
		"ppt/slides/slide10.xml": slide("Finally the cost"),
	})
	text, err := extractDocumentText("deck.pptx", data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	first := strings.Index(text, "First the problem")
	second := strings.Index(text, "Then the approach")
	tenth := strings.Index(text, "Finally the cost")
	if first < 0 || second < 0 || tenth < 0 {
		t.Fatalf("a slide is missing: %q", text)
	}
	if !(first < second && second < tenth) {
		t.Fatalf("slides came out in the wrong order (slide10 before slide2): %q", text)
	}
}

// --- PDF ---------------------------------------------------------------------

func TestPDFReadsASimpleFont(t *testing.T) {
	data := simplePDF(t, "BT /F1 12 Tf (Refs are written through a compare-and-swap.) Tj ET")
	text, err := extractDocumentText("spec.pdf", data)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Refs are written through a compare-and-swap.") {
		t.Fatalf("got %q", text)
	}
}

func TestPDFDecodesIdentityHThroughToUnicode(t *testing.T) {
	// This is the case that decides whether the extractor is useful at all.
	// Every modern producer embeds subset fonts addressed by glyph index; the
	// bytes below are indices 1..5, and only the CMap says they spell "HELLO".
	codes := "\\000\\001\\000\\002\\000\\003\\000\\003\\000\\004"
	cmap := `5 beginbfchar
<0001><0048>
<0002><0045>
<0003><004C>
<0004><004F>
endbfchar`
	text, err := extractDocumentText("modern.pdf", identityHPDF(t, codes, cmap))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "HELLO") {
		t.Fatalf("glyph indices were not mapped through /ToUnicode: %q", text)
	}
	// The raw indices must not appear. If they do, the extractor is emitting
	// glyph numbers as characters, which is the mojibake failure.
	if strings.ContainsRune(text, '\x01') || strings.ContainsRune(text, '\x02') {
		t.Fatalf("raw glyph codes leaked into the text: %q", text)
	}
}

func TestPDFDecodesToUnicodeRanges(t *testing.T) {
	// bfrange is the compact form producers use for contiguous glyphs, and it
	// advances the last character across the range rather than repeating it.
	codes := "\\000\\001\\000\\002\\000\\003"
	cmap := `1 beginbfrange
<0001><0003><0061>
endbfrange`
	text, err := extractDocumentText("range.pdf", identityHPDF(t, codes, cmap))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "abc") {
		t.Fatalf("a bfrange did not advance across the range: %q", text)
	}
}

func TestPDFRefusesRatherThanEmitGlyphIndices(t *testing.T) {
	// A composite font with no usable ToUnicode cannot be decoded. Emitting the
	// bytes would produce fluent-looking rubbish that passes into the index and
	// can never be searched for; the honest outcome is a named failure.
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
	// Printable glyph indices on purpose. Indices that happen to be control
	// bytes would be stripped by normalization anyway, so a test using those
	// would pass even with this guard deleted — it has to be the case where
	// emitting the bytes produces convincing-looking letters.
	b.addStream(t, "", "BT /F1 12 Tf (Hello there, this reads like English) Tj ET")
	b.add("<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+NoMap /Encoding /Identity-H >>")

	text, err := extractDocumentText("nomap.pdf", b.build())
	if err == nil {
		t.Fatalf("an unmappable composite font was reported as successfully extracted: %q", text)
	}
	if strings.Contains(err.Error(), "Hello") {
		t.Fatalf("glyph bytes leaked into the output: %v", err)
	}
	if !strings.Contains(err.Error(), "no text layer") && !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("the error should say the text could not be read: %v", err)
	}
}

func TestPDFTreatsLargeKernsAsWordSpaces(t *testing.T) {
	// Real producers encode word spaces as negative kerns inside a TJ array
	// rather than as space characters. Verified against a real document whose
	// kerns run -200 to -500; without this the whole page is one long word.
	content := `BT /F1 12 Tf [(Refs)-320(are)-310(written)-300(through)-290(a)-280(swap)] TJ ET`
	text, err := extractDocumentText("kerned.pdf", simplePDF(t, content))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Refs are written through a swap") {
		t.Fatalf("kern-encoded word spaces were not restored: %q", text)
	}
}

func TestPDFDoesNotInsertSpacesForOrdinaryKerning(t *testing.T) {
	// The mirror of the test above. Small kerns are letter-fitting inside a
	// word; turning those into spaces shatters every word in the document.
	content := `BT /F1 12 Tf [(Ex)-12(po)-9(nen)-15(tial)] TJ ET`
	text, err := extractDocumentText("tight.pdf", simplePDF(t, content))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Exponential") {
		t.Fatalf("ordinary kerning was turned into spaces: %q", text)
	}
}

func TestPDFReadsEveryPageAndEveryContentStream(t *testing.T) {
	// /Contents is routinely an array; reading only the first stream loses most
	// of a page. And a reader that stops after page one loses the document.
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R 6 0 R] /Count 2 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 7 0 R >> >> /Contents [4 0 R 5 0 R] >>")
	b.addStream(t, "", "BT /F1 12 Tf (First stream. ) Tj ET")
	b.addStream(t, "", "BT /F1 12 Tf (Second stream.) Tj ET")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 7 0 R >> >> /Contents 8 0 R >>")
	b.add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	b.addStream(t, "", "BT /F1 12 Tf (Page two.) Tj ET")

	text, err := extractDocumentText("multi.pdf", b.build())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, want := range []string{"First stream.", "Second stream.", "Page two."} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q is missing from %q", want, text)
		}
	}
}

func TestPDFWithNoTextLayerSaysSo(t *testing.T) {
	// A scan is the single most common document somebody will try to ingest and
	// the single most misleading thing to handle silently: it parses perfectly
	// and contains nothing. Verified against three real scans on disk, all of
	// which declare zero fonts.
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>")
	b.addStream(t, "", "q 600 0 0 800 0 0 cm /Im0 Do Q")
	b.add("<< /Type /XObject /Subtype /Image /Width 600 /Height 800 /Filter /DCTDecode /Length 4 >>\nstream\n\xff\xd8\xff\xd9\nendstream")

	_, err := extractDocumentText("scan.pdf", b.build())
	if err == nil {
		t.Fatal("a scanned PDF was reported as successfully extracted")
	}
	if !strings.Contains(err.Error(), "no text layer") {
		t.Fatalf("the error should name the missing text layer: %v", err)
	}
	// And it must point at the remedy, because "no text layer" alone reads like
	// a bug in the tool rather than a property of the file.
	if !strings.Contains(err.Error(), "OCR") {
		t.Fatalf("the error should mention OCR as the remedy: %v", err)
	}
}

func TestEncryptedPDFIsReportedNotGuessedAt(t *testing.T) {
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [] /Count 0 >>")
	data := b.build()
	data = bytes.Replace(data, []byte("/Root 1 0 R"), []byte("/Root 1 0 R /Encrypt 3 0 R"), 1)

	_, err := extractDocumentText("locked.pdf", data)
	if err == nil {
		t.Fatal("an encrypted PDF was reported as successfully extracted")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("the error should say the file is encrypted: %v", err)
	}
}

// --- the quality gate --------------------------------------------------------

func TestExtractionRejectsUnreadableOutput(t *testing.T) {
	// The gate exists because a broken font encoding produces text that passes
	// every structural check and is nonetheless unsearchable. Indexing it is
	// worse than failing, because the failure is invisible.
	if extractedTextLooksLikeProse(strings.Repeat("@#$%^&*(){}", 60)) {
		t.Fatal("a wall of symbols was accepted as readable text")
	}
	if extractedTextLooksLikeProse(strings.Repeat("abcdefgh", 40)) {
		t.Fatal("a long run with no word boundaries at all was accepted")
	}
	// And it must not reject the legitimate documents it sits in front of.
	for name, sample := range map[string]string{
		"prose":       "The retry policy backs off exponentially, capped at thirty seconds.",
		"spreadsheet": "Latency\t200\t300\nThroughput\t1500\t1800\n",
		"code-ish":    "func retry(ctx context.Context) error { return backoff(ctx) }",
		"accented":    "La révision précédente utilisait une stratégie différente.",
	} {
		if !extractedTextLooksLikeProse(sample) {
			t.Fatalf("%s was rejected as unreadable: %q", name, sample)
		}
	}
}

// The predicate above is unit-tested; this drives a whole document through the
// real entry point, because a gate that exists and is never called is the same
// as no gate. The content is a page of symbols — the shape a mis-decoded font
// produces — and it must not reach the caller as text.
func TestExtractionAppliesTheProseGateToWholeDocuments(t *testing.T) {
	garbage := strings.Repeat(`\(\)#$%^&*{}[]<>|~`+"`", 80)
	data := simplePDF(t, "BT /F1 12 Tf ("+garbage+") Tj ET")

	text, err := extractDocumentText("mojibake.pdf", data)
	if err == nil {
		t.Fatalf("an unreadable document was accepted: %q", text)
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("the error should say the text is unreadable: %v", err)
	}
	// And it must suggest something to do about it.
	if !strings.Contains(err.Error(), "encoding") {
		t.Fatalf("the error should explain the likely cause: %v", err)
	}
}

func TestExtractionRejectsUnsupportedFormats(t *testing.T) {
	// Legacy .doc is an OLE compound file, an entirely different format. Saying
	// so beats failing to parse it as a zip and reporting a corrupt archive.
	_, err := extractDocumentText("old.doc", []byte("\xd0\xcf\x11\xe0anything"))
	if err == nil {
		t.Fatal(".doc was accepted")
	}
	if !strings.Contains(err.Error(), ".docx") {
		t.Fatalf("the error should list what is supported: %v", err)
	}
}

func TestExtractionNormalizesControlCharactersAway(t *testing.T) {
	// Control bytes from a document corrupt terminal output and the stored JSON
	// alike, and nothing downstream expects them.
	text := normalizeExtractedText("before\x00\x07\x1baftertext\r\nnext")
	if strings.ContainsAny(text, "\x00\x07\x1b") {
		t.Fatalf("control characters survived: %q", text)
	}
	if !strings.Contains(text, "next") || !strings.Contains(text, "before") {
		t.Fatalf("normalization dropped real content: %q", text)
	}
}

func TestExtractionCollapsesRunsOfBlankLines(t *testing.T) {
	// A converted document is mostly layout whitespace, and a chunk of blank
	// lines is chunk budget spent on nothing.
	text := normalizeExtractedText("one\n\n\n\n\n\ntwo")
	if strings.Contains(text, "\n\n\n") {
		t.Fatalf("blank lines were not collapsed: %q", text)
	}
	// One blank line survives: it is the paragraph break.
	if !strings.Contains(text, "one\n\ntwo") {
		t.Fatalf("the paragraph break was lost: %q", text)
	}
}

// --- limits ------------------------------------------------------------------

func TestZipWithTooManyEntriesIsRefused(t *testing.T) {
	// A document is not an archive of thousands of parts. Refusing up front
	// bounds the work before it starts rather than after it hurts.
	parts := map[string]string{"word/document.xml": "<document><body><p><r><t>x</t></r></p></body></document>"}
	for i := 0; i < maxExtractZipEntries+10; i++ {
		parts[fmt.Sprintf("junk/%d.bin", i)] = "x"
	}
	_, err := extractDocumentText("many.docx", zipFile(t, parts))
	if err == nil {
		t.Fatal("an archive past the entry limit was accepted")
	}
	if !strings.Contains(err.Error(), "entries") {
		t.Fatalf("the error should name the entry limit: %v", err)
	}
}

func TestHighlyCompressedPartIsRefusedRatherThanExpanded(t *testing.T) {
	// The zip-bomb shape: a small archive whose part expands enormously. The
	// cap is enforced while reading, so the cost is bounded rather than paid
	// and then regretted.
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)
	f, err := w.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	// Valid XML carrying real text. A fixture of raw bytes would yield no text
	// either way, so the test would pass with the cap deleted; this one only
	// passes if the read is actually refused.
	body := `<?xml version="1.0"?><document><body>` +
		strings.Repeat(`<p><r><t>The retry policy backs off exponentially. </t></r></p>`, maxExtractOutputBytes/40) +
		`</body></document>`
	if _, err := f.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if len(buf.Bytes()) > 200<<10 {
		t.Fatalf("the fixture is not actually compressed (%d bytes)", len(buf.Bytes()))
	}
	_, err = extractDocumentText("bomb.docx", buf.Bytes())
	if err == nil {
		t.Fatal("a part expanding past the output limit was accepted")
	}
}

func TestOversizedFileIsRefusedBeforeParsing(t *testing.T) {
	_, err := extractDocumentText("huge.pdf", make([]byte, maxExtractDocumentBytes+1))
	if err == nil {
		t.Fatal("a file past the size limit was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("the error should name the limit: %v", err)
	}
}

func TestMalformedInputTerminates(t *testing.T) {
	// A damaged or hostile document must not hang the ingest. These are the
	// shapes that make a hand-written parser spin: unterminated containers,
	// truncated streams, and dictionaries that never close.
	for name, data := range map[string][]byte{
		"unterminated array":  []byte("%PDF-1.7\n1 0 obj\n[[[[[[[[[[\nendobj\n"),
		"unterminated dict":   []byte("%PDF-1.7\n1 0 obj\n<< /A << /B << /C\nendobj\n"),
		"unterminated string": []byte("%PDF-1.7\n1 0 obj\n<< /Length 5 >>\nstream\n(((((\n"),
		"stream, no end":      []byte("%PDF-1.7\n1 0 obj\n<< >>\nstream\nBT /F1 12 Tf (x) Tj ET\n"),
		"obj with no header":  []byte("%PDF-1.7\nobj obj obj obj\n"),
		"nul bytes":           append([]byte("%PDF-1.7\n"), make([]byte, 4096)...),
		"header only":         []byte("%PDF-1.7\n"),
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panicked: %v", name, r)
				}
			}()
			_, _ = extractDocumentText("bad.pdf", data)
		}()
		<-done
	}
}

// The per-package allowance is drawn down per part. Deducting it only on a part
// that yielded text meant a package whose parts each decompress to the cap and
// contain none never spent it — so an archive of up to maxExtractZipEntries
// such parts could force that many near-full decompressions, which is exactly
// the aggregate this budget exists to bound.
//
// Asserted on what the reader returns rather than on how long it took. Parts
// are read in a fixed order (document.xml, then endnotes, then footnotes), so a
// run of textless endnotes that spends the allowance must stop the walk before
// the footnote at the end — and the sentence in it must not come back.
func TestTextlessOfficePartsSpendTheBudget(t *testing.T) {
	filler := "<w:document><w:body>" + strings.Repeat("<w:p><w:r></w:r></w:p>", 30000) + "</w:body></w:document>"
	parts := map[string]string{
		"word/document.xml": "<w:document><w:body><w:p><w:r><w:t>Opening line.</w:t></w:r></w:p></w:body></w:document>",
		"word/footnotes99.xml": "<w:document><w:body><w:p><w:r><w:t>SENTINEL past the allowance.</w:t></w:r>" +
			"</w:p></w:body></w:document>",
	}
	for i := 0; i < 10; i++ {
		parts[fmt.Sprintf("word/endnotes%02d.xml", i)] = filler
	}

	text, err := extractDocumentText("big.docx", zipFile(t, parts))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !strings.Contains(text, "Opening line.") {
		t.Fatalf("the body was lost entirely: %q", text)
	}
	if strings.Contains(text, "SENTINEL") {
		t.Fatal("textless parts spent none of the allowance, so the walk ran past it")
	}
}
