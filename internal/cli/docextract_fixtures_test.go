package cli

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"
)

// Fixtures are built here rather than checked in as binaries, so a reader can
// see exactly what shape of document each test claims to cover — and so a
// fixture cannot quietly stop matching what the test says it is.

func zipFile(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	buf := &bytes.Buffer{}
	w := zip.NewWriter(buf)
	// Deterministic order so a failure is reproducible.
	for _, name := range sortedKeys(parts) {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := f.Write([]byte(parts[name])); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// docxFixture builds a WordprocessingML document. The namespace prefix is `w:`
// here and deliberately varied in one test, because a reader that matches on the
// prefix rather than the local name works on Word and silently returns nothing
// on other producers.
func docxFixture(t *testing.T, bodyXML string) []byte {
	t.Helper()
	return zipFile(t, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml": `<?xml version="1.0"?>` +
			`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
			`<w:body>` + bodyXML + `</w:body></w:document>`,
	})
}

func docxParagraph(runs ...string) string {
	var b strings.Builder
	b.WriteString("<w:p>")
	for _, run := range runs {
		fmt.Fprintf(&b, "<w:r><w:t>%s</w:t></w:r>", run)
	}
	b.WriteString("</w:p>")
	return b.String()
}

// --- PDF construction --------------------------------------------------------

// pdfBuilder assembles a syntactically real PDF: numbered indirect objects, a
// catalog, a page tree and a cross-reference table. Tests that hand-wrote a
// fragment would prove only that the parser reads that fragment.
type pdfBuilder struct {
	objects []string
}

func (b *pdfBuilder) add(body string) int {
	b.objects = append(b.objects, body)
	return len(b.objects)
}

// addStream adds a Flate-compressed stream object, which is how every real
// producer writes content.
func (b *pdfBuilder) addStream(t *testing.T, dictExtra, content string) int {
	t.Helper()
	buf := &bytes.Buffer{}
	zw := zlib.NewWriter(buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	compressed := buf.String()
	return b.add(fmt.Sprintf("<< /Length %d /Filter /FlateDecode %s >>\nstream\n%s\nendstream",
		len(compressed), dictExtra, compressed))
}

func (b *pdfBuilder) build() []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(b.objects)+1)
	for i, body := range b.objects {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(b.objects)+1)
	for i := 1; i <= len(b.objects); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(b.objects)+1, xref)
	return out.Bytes()
}

// simplePDF is a one-page document in a font with no ToUnicode map, where the
// string bytes are the characters.
func simplePDF(t *testing.T, content string) []byte {
	t.Helper()
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")                                                      // 1
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")                                              // 2
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>") // 3
	b.addStream(t, "", content)                                                                     // 4
	b.add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")                                 // 5
	return b.build()
}

// identityHPDF is the modern case: a composite font addressed with two-byte
// glyph codes, readable only through its /ToUnicode CMap. `codes` are the raw
// two-byte codes drawn; `cmap` is the CMap body mapping them back.
func identityHPDF(t *testing.T, codes string, cmapBody string) []byte {
	t.Helper()
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
	b.addStream(t, "", "BT /F1 12 Tf ("+codes+") Tj ET")
	b.add("<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+Test /Encoding /Identity-H /ToUnicode 6 0 R >>")
	b.addStream(t, "", `/CIDInit /ProcSet findresource begin
begincmap
/CMapName /Adobe-Identity-UCS def
/CMapType 2 def
1 begincodespacerange
<0000><FFFF>
endcodespacerange
`+cmapBody+`
endcmap
end`)
	return b.build()
}
