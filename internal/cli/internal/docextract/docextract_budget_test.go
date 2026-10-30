package docextract

import (
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

// A per-stream cap is not a bound on a document. Thousands of pages, each
// inflating to the per-stream cap and producing almost no text, advance nothing
// that ends the loop — so a small file can ask for an unbounded amount of
// decompression work.
func TestPDFDecompressionIsBoundedAcrossTheWholeDocument(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	// A per-stream cap is not a bound on a document: thousands of pages, each
	// inflating to that cap and producing almost no text, advance nothing that
	// ends the loop. The budget is what ends it, and this proves the loop
	// actually consults it by giving it one too small to finish the document.
	const pages = 20
	// Each page carries a distinct sentence and a lot of drawing operators, so
	// pages that were read are identifiable and decoded bytes far exceed text.
	filler := strings.Repeat("1 0 0 1 0 0 cm ", 4000)
	b := &clitest.PDFBuilder{}
	b.Add("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, 0, pages)
	for i := 0; i < pages; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+i*2))
	}
	b.Add(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))
	for i := 0; i < pages; i++ {
		b.Add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			3+pages*2, 4+i*2))
		b.AddStream(t, "", fmt.Sprintf("q %s Q BT /F1 12 Tf (Sentence number %d appears here.) Tj ET", filler, i))
	}
	b.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	data := b.Build()

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
	t.Parallel()
	b := &clitest.PDFBuilder{}
	b.Add("<< /Type /Catalog /Pages 2 0 R >>")
	b.Add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.Add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
	b.AddStream(t, "", "BT /F1 12 Tf (x) Tj ET")
	b.Add("<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+Test /Encoding /Identity-H /ToUnicode 6 0 R >>")
	// A large, highly compressible ToUnicode stream.
	b.AddStream(t, "", "begincmap\n"+strings.Repeat("% filler filler filler filler\n", 20000)+"endcmap\n")
	data := b.Build()

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
	t.Parallel()
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
	data := clitest.ZipFile(t, parts)

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

// The fallback scan and the ToUnicode merge both walk every stream. The merge
// runs first and decodes the CMap streams; the scan must not decode them again,
// because the decode budget is document-wide and this is the path taken by the
// degraded files where it is most likely to bind.
//
// The document below has no page tree, so the page path yields nothing and the
// fallback runs — with a CMap stream present for it to re-decode.
func TestPDFFallbackDoesNotDecodeCMapStreamsTwice(t *testing.T) {
	t.Parallel()
	b := &clitest.PDFBuilder{}
	b.Add("<< /Type /Catalog >>") // no /Pages: forces the fallback
	b.AddStream(t, "/Type /CMap", strings.Repeat("begincmap endcmap\n", 400))
	b.AddStream(t, "", "BT /F1 12 Tf (The retry budget is thirty seconds.) Tj ET")
	pdf := b.Build()

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
	t.Parallel()
	b := &clitest.PDFBuilder{}
	b.Add("<< /Type /Catalog >>")
	// A highly compressible object stream: small on disk, large decompressed.
	payload := strings.Repeat("0 0 ", 2000)
	b.AddStream(t, "/Type /ObjStm /N 1 /First 4", payload)
	pdf := b.Build()

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
	t.Parallel()
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
	t.Parallel()
	// Two PNG-Up rows (tag 2), one byte wide: 1, then +2 => 3.
	raw := []byte{2, 1, 2, 2}
	got := pdfApplyPredictor(raw, map[string]pdfValue{
		"Predictor": float64(12), "Colors": float64(1), "BitsPerComponent": float64(8), "Columns": float64(1),
	})
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("a legal PNG-Up predictor was not applied: %v", got)
	}
}
