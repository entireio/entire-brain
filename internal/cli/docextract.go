package cli

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Reading the documents a repository keeps that are not code and not markdown.
//
// A brain indexed .md and nothing else, so the design doc somebody wrote in
// Word, the spec that arrived as a PDF and the requirements matrix living in a
// spreadsheet were invisible to it — even sitting in `docs/` beside the files it
// did read. "We run locally" never explained the absence: these are container
// formats with deterministic parsers, they cost no tokens, and Graphify parses
// the same set on-device.
//
// Everything here is standard library. Office files are ZIP archives of XML,
// which `archive/zip` and `encoding/xml` read completely; PDF text extraction
// lives in docextract_pdf.go and uses `compress/zlib`. That matters because the
// install instructions promise no Go toolchain and no C compiler, and a
// document reader that needed libxml or tesseract would quietly make those
// instructions false.
//
// The hard part is not parsing. It is refusing to produce plausible garbage: a
// scanned PDF has no text layer, an encrypted one cannot be read, and a badly
// encoded one yields mojibake that would poison retrieval with text nobody can
// search for and no reader can recognise as broken. Every path here either
// returns text that passed extractedTextLooksLikeProse or returns an error
// naming what went wrong.

// Limits. Documents are untrusted input — a 2KB zip can inflate to gigabytes,
// and a malformed PDF can describe an enormous stream — so extraction is bounded
// before it starts rather than after it hurts.
const (
	maxExtractDocumentBytes = 32 << 20 // 32 MiB of source file
	maxExtractOutputBytes   = 4 << 20  // 4 MiB of extracted text per document
	maxExtractZipEntries    = 2048     // an Office file with more parts than this is not a document
)

// documentFormat is what the extension claims the file is.
type documentFormat string

const (
	formatPDF     documentFormat = "pdf"
	formatDocx    documentFormat = "docx"
	formatXlsx    documentFormat = "xlsx"
	formatPptx    documentFormat = "pptx"
	formatUnknown documentFormat = ""
)

// documentFormatFor classifies by extension. Content sniffing would be more
// robust, but a file named .pdf that is secretly a zip is a file we should
// refuse rather than helpfully reinterpret.
func documentFormatFor(path string) documentFormat {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return formatPDF
	case ".docx":
		return formatDocx
	case ".xlsx":
		return formatXlsx
	case ".pptx":
		return formatPptx
	default:
		return formatUnknown
	}
}

// extractableDocumentExtensions is what this build can read, for help text and
// for the selection rule. Legacy .doc/.xls/.ppt are deliberately absent: they
// are OLE compound files, an entirely different and much larger problem, and
// claiming them here would mean failing on them at ingest time instead.
func extractableDocumentExtensions() []string {
	return []string{".pdf", ".docx", ".xlsx", ".pptx"}
}

// errNoTextLayer is the honest answer for a document that parsed correctly and
// simply contains no text — overwhelmingly a scanned or image-only PDF. It is
// separated from a parse failure because the remedy is different: this one needs
// OCR, which is out of scope for a dependency-free binary, and saying so beats
// indexing an empty document that looks like a successful ingest.
type documentExtractionError struct {
	Path   string
	Format documentFormat
	Reason string
	Remedy string
}

func (e *documentExtractionError) Error() string {
	message := fmt.Sprintf("%s: %s", e.Path, e.Reason)
	if e.Remedy != "" {
		message += " (" + e.Remedy + ")"
	}
	return message
}

// extractDocumentText reads one document and returns its text.
//
// data is the file's bytes; the caller has already applied its own size limit,
// and this applies its own so a direct caller cannot skip it.
func extractDocumentText(path string, data []byte) (string, error) {
	format := documentFormatFor(path)
	if format == formatUnknown {
		return "", &documentExtractionError{
			Path:   path,
			Reason: "not a document format this build reads",
			Remedy: "supported: " + strings.Join(extractableDocumentExtensions(), " "),
		}
	}
	if len(data) == 0 {
		return "", &documentExtractionError{Path: path, Format: format, Reason: "the file is empty"}
	}
	if len(data) > maxExtractDocumentBytes {
		return "", &documentExtractionError{
			Path: path, Format: format,
			Reason: fmt.Sprintf("the file is %d bytes, over the %d-byte extraction limit", len(data), maxExtractDocumentBytes),
		}
	}

	var (
		text string
		err  error
	)
	switch format {
	case formatPDF:
		text, err = extractPDFText(data)
	case formatDocx:
		text, err = extractOfficeText(data, docxTextParts)
	case formatXlsx:
		text, err = extractXlsxText(data)
	case formatPptx:
		text, err = extractOfficeText(data, pptxTextParts)
	}
	if err != nil {
		if _, ok := err.(*documentExtractionError); ok {
			return "", err
		}
		return "", &documentExtractionError{Path: path, Format: format, Reason: err.Error()}
	}

	text = normalizeExtractedText(text)
	if strings.TrimSpace(text) == "" {
		reason, remedy := "no text was found in the file", ""
		if format == formatPDF {
			reason = "the PDF has no text layer"
			remedy = "it is probably a scan; OCR is out of scope for this build"
		}
		return "", &documentExtractionError{Path: path, Format: format, Reason: reason, Remedy: remedy}
	}
	// The last gate, and the one that matters most. An extractor that emits
	// mojibake is worse than one that fails: the failure is visible and the
	// mojibake silently fills the index with text no query can match.
	if !extractedTextLooksLikeProse(text) {
		return "", &documentExtractionError{
			Path: path, Format: format,
			Reason: "the extracted text is not readable",
			Remedy: "the document probably uses an embedded font encoding this build cannot map; convert it to PDF/A or paste the text into a markdown file",
		}
	}
	return text, nil
}

// normalizeExtractedText makes extractor output safe to store and chunk: real
// newlines, no control characters, no runaway blank space, and bounded size.
func normalizeExtractedText(text string) string {
	if !utf8.ValidString(text) {
		// Latin-1 is the right fallback rather than dropping bytes: Office and
		// PDF simple encodings are Latin-1-shaped, so this recovers accented
		// characters instead of turning them into replacement runes.
		var b strings.Builder
		for _, c := range []byte(text) {
			b.WriteRune(rune(c))
		}
		text = b.String()
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == utf8.RuneError:
			// drop
		case unicode.IsControl(r):
			// drop: control bytes in a chunk corrupt terminal output and the
			// stored JSON alike
		default:
			b.WriteRune(r)
		}
	}
	text = b.String()

	lines := strings.Split(text, "\n")
	trimmed := make([]string, 0, len(lines))
	blanks := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			blanks++
			// A converted document is mostly layout whitespace. More than one
			// blank line in a row carries no information and costs chunk budget.
			if blanks > 1 {
				continue
			}
			trimmed = append(trimmed, "")
			continue
		}
		blanks = 0
		trimmed = append(trimmed, line)
	}
	text = strings.TrimSpace(strings.Join(trimmed, "\n"))

	if len(text) > maxExtractOutputBytes {
		text = text[:maxExtractOutputBytes]
		// Cut back to a rune boundary so truncation cannot produce invalid UTF-8.
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "\n\n[truncated]"
	}
	return text
}

// extractedTextLooksLikeProse is the quality gate. It is a heuristic and is
// described as one; its job is to catch the specific failure that matters —
// a PDF whose font encoding we could not map, which yields long runs of
// punctuation and accented bytes that pass every structural check and are
// nonetheless unreadable.
func extractedTextLooksLikeProse(text string) bool {
	var letters, digits, spaces, other int
	for _, r := range text {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r):
			digits++
		case unicode.IsSpace(r):
			spaces++
		default:
			other++
		}
	}
	total := letters + digits + spaces + other
	if total == 0 {
		return false
	}
	// A spreadsheet of numbers is legitimate and has few letters, so digits and
	// spaces count toward "readable" too. What disqualifies a document is a
	// dominant share of symbols.
	readable := letters + digits + spaces
	if float64(readable)/float64(total) < 0.70 {
		return false
	}
	// Mojibake tends to be letter-dense with no word boundaries at all, which
	// the ratio above happily accepts. Require that a document of any length
	// actually contains spaces.
	if total > 200 && spaces == 0 {
		return false
	}
	return true
}

// --- Office: ZIP of XML ------------------------------------------------------

// officePartSelector decides which parts of an Office package hold text, and in
// what order. Returning the order explicitly matters for pptx, where slide10
// sorts before slide2 lexically and a reader would get the deck shuffled.
type officePartSelector func(names []string) []string

func docxTextParts(names []string) []string {
	var parts []string
	for _, name := range names {
		// The body, then footnotes/endnotes. Headers and footers are excluded:
		// they repeat on every page and would dominate a chunked index with
		// running titles.
		if name == "word/document.xml" ||
			strings.HasPrefix(name, "word/footnotes") ||
			strings.HasPrefix(name, "word/endnotes") {
			parts = append(parts, name)
		}
	}
	sort.Slice(parts, func(i, j int) bool {
		// document.xml first, then the notes.
		if (parts[i] == "word/document.xml") != (parts[j] == "word/document.xml") {
			return parts[i] == "word/document.xml"
		}
		return parts[i] < parts[j]
	})
	return parts
}

func pptxTextParts(names []string) []string {
	var slides, notes []string
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml"):
			slides = append(slides, name)
		case strings.HasPrefix(name, "ppt/notesSlides/notesSlide") && strings.HasSuffix(name, ".xml"):
			notes = append(notes, name)
		}
	}
	sortOfficeNumbered(slides)
	sortOfficeNumbered(notes)
	return append(slides, notes...)
}

// sortOfficeNumbered orders slide2 before slide10. Lexical order would not, and
// a deck delivered out of order is a deck whose argument no longer follows.
func sortOfficeNumbered(names []string) {
	sort.Slice(names, func(i, j int) bool {
		ni, oki := officePartNumber(names[i])
		nj, okj := officePartNumber(names[j])
		if oki && okj && ni != nj {
			return ni < nj
		}
		return names[i] < names[j]
	})
}

func officePartNumber(name string) (int, bool) {
	base := strings.TrimSuffix(filepath.Base(name), ".xml")
	digits := strings.TrimLeftFunc(base, func(r rune) bool { return !unicode.IsDigit(r) })
	if digits == "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

func openOfficeZip(data []byte) (*zip.Reader, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a readable Office file: %w", err)
	}
	if len(reader.File) > maxExtractZipEntries {
		return nil, fmt.Errorf("the archive has %d entries, over the %d-entry limit", len(reader.File), maxExtractZipEntries)
	}
	return reader, nil
}

func officeZipNames(reader *zip.Reader) []string {
	names := make([]string, 0, len(reader.File))
	for _, f := range reader.File {
		names = append(names, f.Name)
	}
	return names
}

// readZipPart decompresses one entry under a hard output cap, so a zip bomb
// costs a bounded read rather than the machine's memory.
// The second return is how many bytes were decompressed, reported even when the
// part is refused: discovering that a part is over the cap costs the whole
// remaining allowance, and a caller that charges only on success lets a package
// of over-cap parts pay nothing and repeat for every entry it holds.
func readZipPart(reader *zip.Reader, name string, budget int) ([]byte, int, error) {
	for _, f := range reader.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, err
		}
		defer rc.Close()
		// One byte past the budget, so hitting the cap is distinguishable from
		// a part that happens to be exactly that size.
		data, err := io.ReadAll(io.LimitReader(rc, int64(budget)+1))
		if err != nil {
			return nil, len(data), err
		}
		if len(data) > budget {
			return nil, len(data), fmt.Errorf("%s expands past the %d-byte limit", name, budget)
		}
		return data, len(data), nil
	}
	return nil, 0, fmt.Errorf("%s is missing", name)
}

// extractOfficeText pulls the text out of the selected parts of a WordprocessingML
// or PresentationML package.
//
// Both mark runs of text with a leaf element (w:t, a:t) and paragraphs with a
// container (w:p, a:p). Matching on the LOCAL name rather than the namespaced
// one is deliberate: the same document is written with different prefixes by
// different producers, and a prefix-sensitive reader works on Word's output and
// silently returns nothing on LibreOffice's.
func extractOfficeText(data []byte, selectParts officePartSelector) (string, error) {
	reader, err := openOfficeZip(data)
	if err != nil {
		return "", err
	}
	parts := selectParts(officeZipNames(reader))
	if len(parts) == 0 {
		return "", fmt.Errorf("the archive contains no document body")
	}
	var out strings.Builder
	// One allowance for the whole package, drawn down per part. A fixed budget
	// per part is not a bound: an archive may hold maxExtractZipEntries parts,
	// and each could expand to the full cap before the aggregate output check
	// below ever runs — the same unbounded-aggregate problem the PDF reader
	// has a document budget for.
	budget := maxExtractOutputBytes
	for _, part := range parts {
		if budget <= 0 {
			break
		}
		raw, read, err := readZipPart(reader, part, budget)
		// Charged where the work happened, not where it succeeded. Deducting
		// only on a part that yields text let a package whose parts each
		// decompress to the cap and contain none spend the allowance over and
		// over -- and an over-cap part costs the whole remaining allowance to
		// discover, so it is charged too.
		budget -= read
		if err != nil {
			// One unreadable note or slide must not lose the whole document.
			continue
		}
		text, err := officeXMLText(raw)
		if err != nil {
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(text)
		if out.Len() >= maxExtractOutputBytes {
			break
		}
	}
	return out.String(), nil
}

// officeXMLText walks one XML part and joins its text runs, breaking lines
// where the format says a paragraph, row, tab or line break occurs.
func officeXMLText(raw []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	// Office XML is well-formed UTF-8; refusing anything else is safer than
	// guessing a charset from an untrusted document.
	decoder.Strict = false

	var (
		out       strings.Builder
		inText    bool
		textBuf   strings.Builder
		cellDepth int
	)
	flushParagraph := func() {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n")
		}
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Return what was read rather than nothing: a document truncated
			// mid-part is still worth more than an empty one.
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t": // w:t (Word), a:t (PowerPoint), t within si (Excel)
				inText = true
				textBuf.Reset()
			case "tc":
				cellDepth++
			case "tab":
				out.WriteString("\t")
			case "br", "cr":
				out.WriteString("\n")
			}
		case xml.CharData:
			if inText {
				textBuf.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				if inText {
					out.WriteString(textBuf.String())
					inText = false
				}
			case "p":
				// Every table cell wraps its text in a paragraph, so breaking
				// the line here would put each cell of a row on its own line
				// and destroy the row — which is most of what a table means.
				// Inside a cell a paragraph is a soft break; outside one it is
				// the real thing.
				if cellDepth > 0 {
					if out.Len() > 0 && !strings.HasSuffix(out.String(), " ") && !strings.HasSuffix(out.String(), "\t") {
						out.WriteString(" ")
					}
				} else {
					flushParagraph()
				}
			case "tc":
				if cellDepth > 0 {
					cellDepth--
				}
				// Separate columns so a row does not run together into one
				// unreadable word, dropping the soft break the cell's last
				// paragraph left behind.
				trimmed := strings.TrimRight(out.String(), " ")
				out.Reset()
				out.WriteString(trimmed)
				out.WriteString("\t")
			case "tr":
				// Drop the tab the last cell left, then end the row.
				trimmed := strings.TrimRight(out.String(), " \t")
				out.Reset()
				out.WriteString(trimmed)
				flushParagraph()
			}
		}
		if out.Len() > maxExtractOutputBytes {
			break
		}
	}
	return out.String(), nil
}

// --- Excel -------------------------------------------------------------------

// extractXlsxText reads a workbook sheet by sheet.
//
// Excel stores most cell text once in a shared-strings table and refers to it by
// index from the sheets, so a reader that only walks the sheets finds numbers
// and nothing else. The sheet order and names come from the workbook part; a
// spreadsheet whose sheets arrive unlabelled and shuffled is much harder to read
// than one that says "Sheet: Requirements" above each block.
func extractXlsxText(data []byte) (string, error) {
	reader, err := openOfficeZip(data)
	if err != nil {
		return "", err
	}
	shared := xlsxSharedStrings(reader)

	type sheet struct {
		name string
		part string
	}
	var sheets []sheet
	for _, name := range xlsxSheetNames(reader) {
		sheets = append(sheets, sheet{name: name.label, part: name.part})
	}
	if len(sheets) == 0 {
		// No workbook part, or it did not resolve: fall back to every sheet
		// part in numeric order so a nonstandard producer still yields text.
		var parts []string
		for _, n := range officeZipNames(reader) {
			if strings.HasPrefix(n, "xl/worksheets/sheet") && strings.HasSuffix(n, ".xml") {
				parts = append(parts, n)
			}
		}
		sortOfficeNumbered(parts)
		for _, p := range parts {
			sheets = append(sheets, sheet{name: strings.TrimSuffix(filepath.Base(p), ".xml"), part: p})
		}
	}
	if len(sheets) == 0 {
		return "", fmt.Errorf("the workbook contains no sheets")
	}

	var out strings.Builder
	// One allowance across every sheet, for the same reason as the other Office
	// formats: a workbook may declare thousands of sheets.
	budget := maxExtractOutputBytes
	for _, s := range sheets {
		if budget <= 0 {
			break
		}
		raw, read, err := readZipPart(reader, s.part, budget)
		// An over-cap sheet costs the whole remaining allowance to discover, so
		// it is charged like a successful read rather than retried free of
		// charge for every sheet the workbook declares.
		budget -= read
		if err != nil {
			continue
		}
		body := xlsxSheetText(raw, shared)
		if strings.TrimSpace(body) == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		fmt.Fprintf(&out, "## %s\n", s.name)
		out.WriteString(body)
		if out.Len() >= maxExtractOutputBytes {
			break
		}
	}
	return out.String(), nil
}

func xlsxSharedStrings(reader *zip.Reader) []string {
	raw, _, err := readZipPart(reader, "xl/sharedStrings.xml", maxExtractOutputBytes)
	if err != nil {
		return nil // a workbook of pure numbers has no shared strings, which is fine
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = false
	var (
		strs    []string
		current strings.Builder
		inItem  bool
		inText  bool
	)
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inItem = true
				current.Reset()
			case "t":
				inText = true
			}
		case xml.CharData:
			// A shared string can be split across several runs (<si><r><t>..),
			// so append rather than replace or the formatting would eat words.
			if inItem && inText {
				current.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "si":
				strs = append(strs, current.String())
				inItem = false
			}
		}
	}
	return strs
}

type xlsxSheetRef struct {
	label string
	part  string
}

// xlsxSheetNames resolves each sheet's display name to its part path through the
// workbook relationships, so blocks come out labelled the way the author named
// them rather than as sheet1/sheet2.
func xlsxSheetNames(reader *zip.Reader) []xlsxSheetRef {
	rels := map[string]string{} // rId -> part path
	if raw, _, err := readZipPart(reader, "xl/_rels/workbook.xml.rels", maxExtractOutputBytes); err == nil {
		decoder := xml.NewDecoder(bytes.NewReader(raw))
		decoder.Strict = false
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			start, ok := token.(xml.StartElement)
			if !ok || start.Name.Local != "Relationship" {
				continue
			}
			var id, target string
			for _, attr := range start.Attr {
				switch attr.Name.Local {
				case "Id":
					id = attr.Value
				case "Target":
					target = attr.Value
				}
			}
			if id == "" || target == "" {
				continue
			}
			target = strings.TrimPrefix(target, "/xl/")
			target = strings.TrimPrefix(target, "/")
			if !strings.HasPrefix(target, "xl/") {
				target = "xl/" + target
			}
			rels[id] = target
		}
	}

	raw, _, err := readZipPart(reader, "xl/workbook.xml", maxExtractOutputBytes)
	if err != nil {
		return nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = false
	var sheets []xlsxSheetRef
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "sheet" {
			continue
		}
		var name, rid string
		for _, attr := range start.Attr {
			switch attr.Name.Local {
			case "name":
				name = attr.Value
			case "id": // r:id
				rid = attr.Value
			}
		}
		part := rels[rid]
		if part == "" {
			continue
		}
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(part), ".xml")
		}
		sheets = append(sheets, xlsxSheetRef{label: name, part: part})
	}
	return sheets
}

// xlsxSheetText renders one sheet as tab-separated rows, resolving shared-string
// cells through the table.
func xlsxSheetText(raw []byte, shared []string) string {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = false
	var (
		out      strings.Builder
		row      []string
		cellType string
		inValue  bool
		inInline bool
		value    strings.Builder
	)
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := token.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				cellType = ""
				for _, attr := range t.Attr {
					if attr.Name.Local == "t" {
						cellType = attr.Value
					}
				}
				value.Reset()
			case "v":
				inValue = true
			case "is": // inline string, used when shared strings are disabled
				inInline = true
			case "t":
				if inInline {
					inValue = true
				}
			}
		case xml.CharData:
			if inValue {
				value.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v", "t":
				inValue = false
			case "is":
				inInline = false
			case "c":
				row = append(row, xlsxCellText(cellType, value.String(), shared))
			case "row":
				line := strings.TrimRight(strings.Join(row, "\t"), "\t")
				if strings.TrimSpace(line) != "" {
					out.WriteString(line)
					out.WriteString("\n")
				}
			}
		}
		if out.Len() > maxExtractOutputBytes {
			break
		}
	}
	return out.String()
}

func xlsxCellText(cellType, value string, shared []string) string {
	if cellType == "s" {
		index, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || index < 0 || index >= len(shared) {
			// A dangling index is a corrupt workbook; an empty cell is a more
			// honest rendering than the raw index, which would read as a number.
			return ""
		}
		return shared[index]
	}
	return value
}
