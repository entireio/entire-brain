package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXlsxSharedStringExpansionBoundedWithinRow(t *testing.T) {
	shared := []string{strings.Repeat("word ", maxExtractOutputBytes/5)}
	raw := []byte(`<worksheet><sheetData><row>` + strings.Repeat(`<c t="s"><v>0</v></c>`, 3) + `</row></sheetData></worksheet>`)
	got := xlsxSheetText(raw, shared)
	if len(got) > maxExtractOutputBytes {
		t.Fatalf("one row expanded to %d bytes, limit %d", len(got), maxExtractOutputBytes)
	}
	if !strings.HasPrefix(got, "word word") {
		t.Fatal("lost readable prefix")
	}
}

func TestPDFCMapRangeAtUint32Limit(t *testing.T) {
	got := parsePDFToUnicode([]byte(`1 beginbfrange <FFFFFFFF> <FFFFFFFF> <0041> endbfrange`))
	if len(got) != 1 || got[0xffffffff] != "A" {
		t.Fatalf("unexpected map: %v", got)
	}
}

func TestPDFCMapExpansionBudget(t *testing.T) {
	// A short source range must not multiply a long destination without a cap.
	dest := strings.Repeat("0041", 1024)
	got := parsePDFToUnicode([]byte("1 beginbfrange <0000> <FFFF> <" + dest + "> endbfrange"))
	bytes := 0
	for _, s := range got {
		bytes += len(s)
	}
	if bytes > maxExtractOutputBytes {
		t.Fatalf("CMap expanded to %d bytes", bytes)
	}
}

func TestPDFShownStringExpansionBudget(t *testing.T) {
	font := &pdfFont{toUnicode: map[uint32]string{'A': strings.Repeat("word ", 1024)}}
	got := pdfDecodeShownString(strings.Repeat("A", 1024), font)
	if len(got) > maxExtractOutputBytes {
		t.Fatalf("shown string expanded to %d bytes", len(got))
	}
}

func TestPDFParserBoundsContainerNesting(t *testing.T) {
	p := &pdfParser{data: []byte(strings.Repeat("[", 4096) + "0" + strings.Repeat("]", 4096))}
	value := p.parseValue()
	depth := 0
	for {
		a, ok := value.([]pdfValue)
		if !ok || len(a) == 0 {
			break
		}
		depth++
		value = a[0]
	}
	if depth > 128 {
		t.Fatalf("parsed %d nested containers without a bound", depth)
	}
}

func TestPDFTextArrayExpansionBudget(t *testing.T) {
	font := &pdfFont{toUnicode: map[uint32]string{'A': strings.Repeat("word ", 1024)}}
	content := []byte("BT /F1 12 Tf [" + strings.Repeat("("+strings.Repeat("A", 512)+") ", 3) + "] TJ ET")
	got := pdfContentText(content, map[string]*pdfFont{"F1": font})
	if len(got) > maxExtractOutputBytes {
		t.Fatalf("text array expanded to %d bytes", len(got))
	}
}

func TestXlsxPreservesSparseColumns(t *testing.T) {
	raw := []byte(`<worksheet><sheetData><row><c r="A1" t="inlineStr"><is><t>Owner</t></is></c><c r="C1" t="inlineStr"><is><t>Status</t></is></c></row><row><c r="C2" t="inlineStr"><is><t>Ready</t></is></c></row></sheetData></worksheet>`)
	if got := xlsxSheetText(raw, nil); got != "Owner\t\tStatus\n\t\tReady\n" {
		t.Fatalf("columns shifted: %q", got)
	}
}

func TestPptxUsesPresentationSlideOrder(t *testing.T) {
	slide := func(s string) string { return `<sld><p><t>` + s + `</t></p></sld>` }
	data := zipFile(t, map[string]string{
		"ppt/presentation.xml":            `<presentation xmlns:r="relationships"><sldIdLst><sldId id="256" r:id="r2"/><sldId id="257" r:id="r1"/></sldIdLst></presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships><Relationship Id="r1" Target="slides/slide1.xml"/><Relationship Id="r2" Target="slides/slide2.xml"/></Relationships>`,
		"ppt/slides/slide1.xml":           slide("Second slide"), "ppt/slides/slide2.xml": slide("First slide"),
		"ppt/slides/slide3.xml": slide("Orphan slide"),
	})
	got, err := extractDocumentText("deck.pptx", data)
	if err != nil {
		t.Fatal(err)
	}
	if got != "First slide\n\nSecond slide" {
		t.Fatalf("wrong deck order: %q", got)
	}
}

func TestExtractedSeedPathsDoNotOverwriteMarkdown(t *testing.T) {
	repo, out := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"spec.pdf":       simplePDF(t, "BT /F1 12 Tf (PDF project knowledge.) Tj ET"),
		"spec.pdf.md":    []byte("Markdown project knowledge."),
		"spec.pdf.md.md": []byte("More markdown knowledge."),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(repo, "docs", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	scan, err := scanSeedRepository(context.Background(), &fakeCommandRunner{}, repo, "test", seedCommandOptions{maxFiles: 100, maxFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Docs) != 3 {
		t.Fatalf("got %d documents", len(scan.Docs))
	}
	if err := writeSeedArtifacts(out, &scan); err != nil {
		t.Fatal(err)
	}
	for _, doc := range scan.Docs {
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(doc.SeedPath)))
		if err != nil {
			t.Fatal(err)
		}
		want := string(files[filepath.Base(doc.Path)])
		if doc.Extracted {
			want = "PDF project knowledge."
		}
		if !strings.Contains(string(got), want) {
			t.Fatalf("%s was overwritten: %q", doc.Path, got)
		}
	}
}

func TestOfficeTableExtractionAllocationsStayLinear(t *testing.T) {
	raw := []byte(`<document><tbl>` + strings.Repeat(`<tr><tc><p><t>`+strings.Repeat("word ", 12)+`</t></p></tc></tr>`, 1000) + `</tbl></document>`)
	used := allocatedBy(t, func() {
		text, err := officeXMLText(raw)
		if err != nil || !strings.Contains(text, "word word") {
			t.Fatalf("extraction: %v", err)
		}
	})
	if used > 8<<20 {
		t.Fatalf("small table allocated %d bytes", used)
	}
}

func TestPDFResolvesInheritedPageFonts(t *testing.T) {
	objects := map[pdfObjectKey]*pdfObject{
		{1, 0}: {dict: map[string]pdfValue{"Resources": map[string]pdfValue{"Font": map[string]pdfValue{"F1": pdfRef{2, 0}}}}},
		{2, 0}: {dict: map[string]pdfValue{"Subtype": pdfName("Type0"), "Encoding": pdfName("Identity-H"), "ToUnicode": pdfRef{3, 0}}},
		{3, 0}: {dict: map[string]pdfValue{}, stream: []byte(`1 beginbfchar <0001> <0041> endbfchar`)},
	}
	fonts := pdfPageFonts(objects, map[string]pdfValue{"Parent": pdfRef{1, 0}}, newPDFDecodeBudget())
	got := pdfContentText([]byte(`BT /F1 12 Tf <0001> Tj ET`), fonts)
	if strings.TrimSpace(got) != "A" {
		t.Fatalf("inherited font not decoded: %q", got)
	}
}
