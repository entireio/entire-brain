package cli

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// PDF text extraction, standard library only.
//
// A PDF does not contain text. It contains instructions for placing glyphs, and
// the mapping from the bytes in those instructions back to characters lives in
// the font. For a document using a standard encoding the bytes happen to be
// Latin-1 and naive extraction works; for one using an embedded subset font
// with Identity-H encoding — which is what every modern producer emits — the
// bytes are glyph indices and naive extraction yields fluent-looking rubbish.
// That is the failure this file is mostly written to avoid: it resolves each
// font's /ToUnicode CMap and decodes through it, and where it cannot, the
// caller's prose gate rejects the result rather than indexing it.
//
// Three things are deliberately not attempted. Encrypted PDFs are reported, not
// cracked. Scanned PDFs have no text layer at all and are reported as needing
// OCR, which a dependency-free binary cannot do. And glyph positions are used
// only to decide where lines break — reconstructing multi-column layout from
// coordinates is a research problem, and guessing wrong silently interleaves two
// columns into nonsense.

// pdfObjectKey identifies an indirect object.
type pdfObjectKey struct{ number, generation int }

type pdfObject struct {
	dict   map[string]pdfValue
	stream []byte // raw, still encoded
}

// pdfValue is a parsed PDF object: one of nil, bool, float64, string (literal),
// pdfName, pdfRef, []pdfValue, or map[string]pdfValue.
type pdfValue any

type pdfName string

type pdfRef struct{ number, generation int }

const (
	maxPDFObjects      = 200000
	maxPDFStreamBytes  = 64 << 20
	maxPDFContentBytes = 32 << 20

	// maxPDFTotalDecodedBytes bounds decompression across the WHOLE document,
	// not just per stream.
	//
	// The per-stream cap alone is not a bound: a page tree may hold thousands
	// of pages, each page's content stream may inflate to the per-stream cap,
	// and a page that decodes to 32 MiB of drawing operators and almost no text
	// does not advance the output-size check that ends the loop. A small file
	// can therefore ask for hundreds of gigabytes of inflation while producing
	// nothing. Documents are untrusted input, and the work has to be bounded by
	// what was done rather than by what came out of it.
	maxPDFTotalDecodedBytes = 256 << 20
)

// extractPDFText is the entry point. It returns the document's text in reading
// order per page, or an error naming why it could not.
func extractPDFText(data []byte) (string, error) {
	return extractPDFTextWithin(data, newPDFDecodeBudget())
}

// extractPDFTextWithin is extractPDFText with the document-wide decompression
// allowance supplied, so a test can set one small enough to be reached.
func extractPDFTextWithin(data []byte, budget *pdfDecodeBudget) (string, error) {
	if !bytes.HasPrefix(bytes.TrimLeft(data[:min(len(data), 1024)], "\x00 \t\r\n"), []byte("%PDF-")) {
		return "", fmt.Errorf("not a PDF (no %%PDF- header)")
	}
	objects := scanPDFObjects(data, budget)
	if len(objects) == 0 {
		return "", fmt.Errorf("no PDF objects could be read; the file may be damaged")
	}
	if pdfIsEncrypted(data, objects) {
		return "", fmt.Errorf("the PDF is encrypted; decrypt it first (this build does not carry a cipher for it)")
	}

	pages := pdfPages(objects)
	var out strings.Builder
	readAnyContent := false
	for _, page := range pages {
		if budget.exhausted() {
			// A short-circuit, not the guard. The guard is in the decoder,
			// which refuses once the budget is gone, so a document with 20,000
			// pages would already produce no further text — this just stops
			// walking them. What was read before the budget ran out is still
			// the document's text as far as it goes.
			break
		}
		fonts := pdfPageFonts(objects, page, budget)
		content := pdfPageContent(objects, page, budget)
		if len(content) == 0 {
			continue
		}
		readAnyContent = true
		text := pdfContentText(content, fonts)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(text)
		if out.Len() > maxExtractOutputBytes {
			break
		}
	}
	if strings.TrimSpace(out.String()) != "" {
		return out.String(), nil
	}
	// A page tree that WAS readable and produced no text is a real answer — a
	// scan, or a font this build cannot decode — and the fallback below must
	// not overturn it. Running it anyway re-reads the same streams without the
	// font information that made the refusal correct, and hands back the glyph
	// bytes the page path deliberately withheld. That is the mojibake this
	// whole file exists to avoid, arriving through the back door.
	if readAnyContent {
		return "", nil
	}

	// Fallback for a file whose page tree could not be walked — a damaged xref,
	// a linearized file we mis-scanned, a producer doing something unusual.
	// Every content-looking stream is read with the union of all ToUnicode maps
	// in the document. It loses page order and can mix two fonts' encodings, so
	// it is a last resort rather than the main path; the prose gate decides
	// whether what comes back is usable.
	merged, consumed := pdfAllToUnicode(objects, budget)
	for _, key := range pdfSortedKeys(objects) {
		object := objects[key]
		if len(object.stream) == 0 || consumed[key] {
			continue
		}
		content, err := pdfDecodeStreamWithin(objects, object, budget)
		if err != nil || !bytes.Contains(content, []byte("BT")) {
			continue
		}
		text := pdfContentText(content, map[string]*pdfFont{"": {toUnicode: merged, twoByte: len(merged) > 0}})
		if strings.TrimSpace(text) == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		out.WriteString(text)
		if out.Len() > maxExtractOutputBytes {
			break
		}
	}
	return out.String(), nil
}

// --- object scanning ---------------------------------------------------------

// scanPDFObjects finds every `N G obj ... endobj` in the file.
//
// This scans rather than following the cross-reference table, on purpose: a
// damaged or unusual xref is the single most common reason a PDF reader gives up
// on a file that is otherwise perfectly readable, and the objects themselves are
// self-describing. Objects inside object streams (/Type /ObjStm) are expanded
// afterwards, because in a modern PDF the page tree usually lives there.
func scanPDFObjects(data []byte, budget *pdfDecodeBudget) map[pdfObjectKey]*pdfObject {
	objects := make(map[pdfObjectKey]*pdfObject)
	for offset := 0; offset < len(data) && len(objects) < maxPDFObjects; {
		index := bytes.Index(data[offset:], []byte("obj"))
		if index < 0 {
			break
		}
		start := offset + index
		number, generation, ok := pdfObjectHeaderBefore(data, start)
		offset = start + 3
		if !ok {
			continue
		}
		body := data[offset:]
		parser := &pdfParser{data: body}
		value := parser.parseValue()
		object := &pdfObject{}
		if dict, isDict := value.(map[string]pdfValue); isDict {
			object.dict = dict
		}
		parser.skipSpace()
		if parser.hasPrefix("stream") {
			parser.pos += len("stream")
			// The spec allows CRLF or LF after the keyword, never CR alone.
			if parser.pos < len(body) && body[parser.pos] == '\r' {
				parser.pos++
			}
			if parser.pos < len(body) && body[parser.pos] == '\n' {
				parser.pos++
			}
			object.stream = pdfStreamBytes(objects, object, body[parser.pos:])
			offset += parser.pos + len(object.stream)
		} else {
			offset += parser.pos
		}
		objects[pdfObjectKey{number, generation}] = object
	}
	expandPDFObjectStreams(objects, budget)
	return objects
}

// pdfStreamBytes takes /Length when it is a literal, and otherwise finds
// `endstream`. A /Length given as an indirect reference cannot be resolved while
// the object map is still being built, which is exactly when many producers use
// one — so the search is the normal path, not a fallback.
func pdfStreamBytes(objects map[pdfObjectKey]*pdfObject, object *pdfObject, rest []byte) []byte {
	if length, ok := pdfInt(object.dict["Length"]); ok && length >= 0 && length <= len(rest) {
		// Trust it only if `endstream` really follows; a stale /Length after an
		// incremental update would otherwise truncate the stream mid-way.
		tail := rest[length:]
		if index := bytes.Index(tail, []byte("endstream")); index >= 0 && index < 4 {
			return rest[:length]
		}
	}
	index := bytes.Index(rest, []byte("endstream"))
	if index < 0 {
		if len(rest) > maxPDFStreamBytes {
			return rest[:maxPDFStreamBytes]
		}
		return rest
	}
	end := index
	// Back off the EOL the writer put before the keyword.
	for end > 0 && (rest[end-1] == '\n' || rest[end-1] == '\r') {
		end--
	}
	if end > maxPDFStreamBytes {
		end = maxPDFStreamBytes
	}
	return rest[:end]
}

// pdfObjectHeaderBefore reads the `N G ` that must precede an `obj` keyword.
// Without this check the word "obj" inside a stream would be mistaken for the
// start of an object.
func pdfObjectHeaderBefore(data []byte, objIndex int) (number, generation int, ok bool) {
	i := objIndex - 1
	skipSpace := func() {
		for i >= 0 && (data[i] == ' ' || data[i] == '\r' || data[i] == '\n' || data[i] == '\t') {
			i--
		}
	}
	readInt := func() (int, bool) {
		end := i
		for i >= 0 && data[i] >= '0' && data[i] <= '9' {
			i--
		}
		if end == i {
			return 0, false
		}
		n, err := strconv.Atoi(string(data[i+1 : end+1]))
		return n, err == nil
	}
	skipSpace()
	if i == objIndex-1 {
		return 0, 0, false // "obj" must be preceded by whitespace
	}
	generation, ok = readInt()
	if !ok {
		return 0, 0, false
	}
	skipSpace()
	number, ok = readInt()
	if !ok {
		return 0, 0, false
	}
	return number, generation, true
}

// expandPDFObjectStreams parses /Type /ObjStm containers. In a PDF 1.5+ file the
// page tree and font dictionaries usually live inside one, so a reader that
// ignores them sees a file with streams and no structure.
// Object streams decompress before any page is walked, so they have to spend
// the same document-wide budget as everything else. Passing nil here made ObjStm
// expansion unlimited — each stream capped only at maxPDFContentBytes, up to
// maxPDFObjects of them — which is the decompression bomb the budget exists to
// stop, reachable before the first page is read.
func expandPDFObjectStreams(objects map[pdfObjectKey]*pdfObject, budget *pdfDecodeBudget) {
	for _, key := range pdfSortedKeys(objects) {
		object := objects[key]
		if name, _ := object.dict["Type"].(pdfName); name != "ObjStm" {
			continue
		}
		data, err := pdfDecodeStreamWithin(objects, object, budget)
		if err != nil {
			continue
		}
		count, _ := pdfInt(object.dict["N"])
		first, _ := pdfInt(object.dict["First"])
		if count <= 0 || first <= 0 || first > len(data) {
			continue
		}
		header := &pdfParser{data: data[:first]}
		type entry struct{ number, offset int }
		entries := make([]entry, 0, count)
		for i := 0; i < count; i++ {
			number, ok1 := pdfInt(header.parseValue())
			offset, ok2 := pdfInt(header.parseValue())
			if !ok1 || !ok2 {
				break
			}
			entries = append(entries, entry{number, offset})
		}
		for _, e := range entries {
			at := first + e.offset
			if at < 0 || at >= len(data) {
				continue
			}
			inner := &pdfParser{data: data[at:]}
			value := inner.parseValue()
			dict, isDict := value.(map[string]pdfValue)
			if !isDict {
				continue
			}
			objectKey := pdfObjectKey{e.number, 0}
			// A directly-stored object wins over one from a container: it is
			// what an incremental update would have written.
			if _, exists := objects[objectKey]; !exists {
				objects[objectKey] = &pdfObject{dict: dict}
			}
		}
	}
}

func pdfSortedKeys(objects map[pdfObjectKey]*pdfObject) []pdfObjectKey {
	keys := make([]pdfObjectKey, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	// Object number order, so output is deterministic across runs — a map walk
	// would reorder the fallback path's text on every invocation.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && (keys[j].number < keys[j-1].number ||
			(keys[j].number == keys[j-1].number && keys[j].generation < keys[j-1].generation)); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func pdfResolve(objects map[pdfObjectKey]*pdfObject, value pdfValue) pdfValue {
	for i := 0; i < 32; i++ { // bounded: a malformed file can make a reference cycle
		ref, isRef := value.(pdfRef)
		if !isRef {
			return value
		}
		object, ok := objects[pdfObjectKey{ref.number, ref.generation}]
		if !ok || object.dict == nil {
			return nil
		}
		value = object.dict
	}
	return nil
}

func pdfIsEncrypted(data []byte, objects map[pdfObjectKey]*pdfObject) bool {
	// The trailer names /Encrypt. Checking the raw tail as well as the parsed
	// objects catches files whose trailer we did not turn into an object.
	tail := data
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	if bytes.Contains(tail, []byte("/Encrypt")) {
		return true
	}
	for _, object := range objects {
		if _, ok := object.dict["Encrypt"]; ok {
			return true
		}
	}
	return false
}

// --- filters -----------------------------------------------------------------

// pdfDecodeBudget is the document-wide decompression allowance, shared by every
// stream decoded while reading one file.
type pdfDecodeBudget struct{ remaining int64 }

func newPDFDecodeBudget() *pdfDecodeBudget {
	return &pdfDecodeBudget{remaining: maxPDFTotalDecodedBytes}
}

// spend reports whether n bytes of decoded output are still affordable. A nil
// budget is unlimited, so callers that legitimately have no document context
// (tests, one-off decodes) are unaffected.
func (b *pdfDecodeBudget) spend(n int) bool {
	if b == nil {
		return true
	}
	b.remaining -= int64(n)
	return b.remaining >= 0
}

func (b *pdfDecodeBudget) exhausted() bool {
	return b != nil && b.remaining < 0
}

func pdfDecodeStream(objects map[pdfObjectKey]*pdfObject, object *pdfObject) ([]byte, error) {
	return pdfDecodeStreamWithin(objects, object, nil)
}

func pdfDecodeStreamWithin(objects map[pdfObjectKey]*pdfObject, object *pdfObject, budget *pdfDecodeBudget) ([]byte, error) {
	// The accounting that actually bounds the work is the spend at the end of
	// this function; refusing up front only avoids inflating one more stream
	// whose output would be rejected anyway.
	if budget.exhausted() {
		return nil, fmt.Errorf("document decompression budget exhausted")
	}
	data := object.stream
	if len(data) == 0 {
		return nil, fmt.Errorf("empty stream")
	}
	filters := pdfFilterNames(objects, object.dict["Filter"])
	for _, filter := range filters {
		switch filter {
		case "FlateDecode", "Fl":
			decoded, err := pdfInflate(data)
			if err != nil {
				return nil, err
			}
			data = decoded
		case "ASCIIHexDecode", "AHx":
			data = pdfASCIIHexDecode(data)
		case "ASCII85Decode", "A85":
			decoded, err := pdfASCII85Decode(data)
			if err != nil {
				return nil, err
			}
			data = decoded
		case "DCTDecode", "JPXDecode", "CCITTFaxDecode", "JBIG2Decode":
			// Images. There is no text in them by definition, and this is the
			// signature of a scanned page.
			return nil, fmt.Errorf("image stream (%s)", filter)
		default:
			return nil, fmt.Errorf("unsupported filter %s", filter)
		}
		if len(data) > maxPDFContentBytes {
			return nil, fmt.Errorf("stream expands past the %d-byte limit", maxPDFContentBytes)
		}
	}
	if parms := pdfResolve(objects, object.dict["DecodeParms"]); parms != nil {
		if dict, ok := parms.(map[string]pdfValue); ok {
			data = pdfApplyPredictor(data, dict)
		}
	}
	if !budget.spend(len(data)) {
		return nil, fmt.Errorf("document decompression budget exhausted")
	}
	return data, nil
}

func pdfFilterNames(objects map[pdfObjectKey]*pdfObject, value pdfValue) []string {
	switch v := pdfResolve(objects, value).(type) {
	case pdfName:
		return []string{string(v)}
	case []pdfValue:
		var names []string
		for _, item := range v {
			if name, ok := pdfResolve(objects, item).(pdfName); ok {
				names = append(names, string(name))
			}
		}
		return names
	}
	return nil
}

// pdfInflate reads zlib, and falls back to raw deflate. Producers that omit the
// two-byte zlib header are not rare, and treating those files as corrupt would
// lose documents that every viewer opens.
func pdfInflate(data []byte) ([]byte, error) {
	read := func(r io.Reader) ([]byte, error) {
		out, err := io.ReadAll(io.LimitReader(r, maxPDFContentBytes+1))
		if len(out) > maxPDFContentBytes {
			return nil, fmt.Errorf("stream expands past the %d-byte limit", maxPDFContentBytes)
		}
		// A truncated stream still decodes usefully up to the truncation, and
		// partial text beats no text.
		if err != nil && len(out) == 0 {
			return nil, err
		}
		return out, nil
	}
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		defer zr.Close()
		if out, err := read(zr); err == nil && len(out) > 0 {
			return out, nil
		}
	}
	fr := flate.NewReader(bytes.NewReader(data))
	defer fr.Close()
	out, err := read(fr)
	if err != nil {
		return nil, fmt.Errorf("stream is not valid Flate data")
	}
	return out, nil
}

func pdfASCIIHexDecode(data []byte) []byte {
	var out []byte
	var hi byte
	var have bool
	for _, c := range data {
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		case c == '>':
			if have {
				out = append(out, hi<<4)
			}
			return out
		default:
			continue
		}
		if have {
			out = append(out, hi<<4|v)
			have = false
		} else {
			hi = v
			have = true
		}
	}
	if have {
		out = append(out, hi<<4)
	}
	return out
}

func pdfASCII85Decode(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(bytes.TrimSpace(data), []byte("<~"))
	if index := bytes.Index(data, []byte("~>")); index >= 0 {
		data = data[:index]
	}
	var out []byte
	var group [5]byte
	n := 0
	for _, c := range data {
		switch {
		case c == 'z' && n == 0:
			out = append(out, 0, 0, 0, 0)
			continue
		case c < '!' || c > 'u':
			continue
		}
		group[n] = c - '!'
		n++
		if n == 5 {
			value := uint32(0)
			for _, g := range group {
				value = value*85 + uint32(g)
			}
			out = append(out, byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
			n = 0
		}
	}
	if n > 0 {
		for i := n; i < 5; i++ {
			group[i] = 84
		}
		value := uint32(0)
		for _, g := range group {
			value = value*85 + uint32(g)
		}
		full := []byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
		out = append(out, full[:n-1]...)
	}
	return out, nil
}

// pdfApplyPredictor undoes PNG row prediction. Streams written with /Predictor
// 12 decode to correct-looking bytes that are actually row deltas, so skipping
// this step produces plausible garbage rather than an obvious failure.
func pdfApplyPredictor(data []byte, parms map[string]pdfValue) []byte {
	predictor, _ := pdfInt(parms["Predictor"])
	if predictor < 10 {
		return data
	}
	colors, ok := pdfInt(parms["Colors"])
	if !ok || colors <= 0 {
		colors = 1
	}
	bpc, ok := pdfInt(parms["BitsPerComponent"])
	if !ok || bpc <= 0 {
		bpc = 8
	}
	columns, ok := pdfInt(parms["Columns"])
	if !ok || columns <= 0 {
		columns = 1
	}
	bpp := max(1, colors*bpc/8)
	rowLen := (columns*colors*bpc + 7) / 8
	if rowLen <= 0 {
		return data
	}
	var out []byte
	previous := make([]byte, rowLen)
	for offset := 0; offset+1 <= len(data); offset += rowLen + 1 {
		tag := data[offset]
		end := min(offset+1+rowLen, len(data))
		row := make([]byte, end-offset-1)
		copy(row, data[offset+1:end])
		for i := range row {
			var left, up, upLeft byte
			if i >= bpp {
				left = row[i-bpp]
				upLeft = previous[i-bpp]
			}
			if i < len(previous) {
				up = previous[i]
			}
			switch tag {
			case 1:
				row[i] += left
			case 2:
				row[i] += up
			case 3:
				row[i] += byte((int(left) + int(up)) / 2)
			case 4:
				row[i] += pdfPaeth(left, up, upLeft)
			}
		}
		out = append(out, row...)
		copy(previous, row)
	}
	return out
}

func pdfPaeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// --- pages and fonts ---------------------------------------------------------

// pdfPages returns the page dictionaries in document order, walking the page
// tree from the catalog and falling back to object order when the tree is
// unusable.
func pdfPages(objects map[pdfObjectKey]*pdfObject) []map[string]pdfValue {
	var pages []map[string]pdfValue
	var walk func(node pdfValue, depth int)
	seen := map[pdfObjectKey]bool{}
	walk = func(node pdfValue, depth int) {
		if depth > 64 || len(pages) > 20000 {
			return
		}
		if ref, isRef := node.(pdfRef); isRef {
			key := pdfObjectKey{ref.number, ref.generation}
			if seen[key] { // a /Kids cycle in a malformed file must not hang the reader
				return
			}
			seen[key] = true
		}
		dict, ok := pdfResolve(objects, node).(map[string]pdfValue)
		if !ok {
			return
		}
		switch name, _ := dict["Type"].(pdfName); name {
		case "Page":
			pages = append(pages, dict)
			return
		case "Pages", "":
			kids, _ := pdfResolve(objects, dict["Kids"]).([]pdfValue)
			for _, kid := range kids {
				walk(kid, depth+1)
			}
		}
	}
	for _, key := range pdfSortedKeys(objects) {
		dict := objects[key].dict
		if name, _ := dict["Type"].(pdfName); name == "Catalog" {
			walk(dict["Pages"], 0)
		}
	}
	if len(pages) > 0 {
		return pages
	}
	for _, key := range pdfSortedKeys(objects) {
		dict := objects[key].dict
		if name, _ := dict["Type"].(pdfName); name == "Page" {
			pages = append(pages, dict)
		}
	}
	return pages
}

// pdfPageContent concatenates a page's content streams. /Contents may be one
// stream or an array of them, and the array case is not exotic — producers split
// a page across streams routinely, and reading only the first loses most of it.
func pdfPageContent(objects map[pdfObjectKey]*pdfObject, page map[string]pdfValue, budget *pdfDecodeBudget) []byte {
	var refs []pdfValue
	switch v := page["Contents"].(type) {
	case pdfRef:
		refs = []pdfValue{v}
	case []pdfValue:
		refs = v
	default:
		return nil
	}
	var out []byte
	for _, item := range refs {
		ref, isRef := item.(pdfRef)
		if !isRef {
			continue
		}
		object, ok := objects[pdfObjectKey{ref.number, ref.generation}]
		if !ok || len(object.stream) == 0 {
			continue
		}
		decoded, err := pdfDecodeStreamWithin(objects, object, budget)
		if err != nil {
			continue
		}
		out = append(out, decoded...)
		out = append(out, '\n')
		if len(out) > maxPDFContentBytes {
			break
		}
	}
	return out
}

// pdfFont is what a content stream needs to turn bytes into characters.
type pdfFont struct {
	toUnicode map[uint32]string
	twoByte   bool // Identity-H and friends address glyphs with 2-byte codes
}

// pdfPageFonts resolves /Resources /Font into a map keyed by the name the
// content stream uses with Tf.
func pdfPageFonts(objects map[pdfObjectKey]*pdfObject, page map[string]pdfValue, budget *pdfDecodeBudget) map[string]*pdfFont {
	fonts := map[string]*pdfFont{}
	resources, _ := pdfResolve(objects, page["Resources"]).(map[string]pdfValue)
	if resources == nil {
		return fonts
	}
	fontDict, _ := pdfResolve(objects, resources["Font"]).(map[string]pdfValue)
	for name, value := range fontDict {
		dict, ok := pdfResolve(objects, value).(map[string]pdfValue)
		if !ok {
			continue
		}
		font := &pdfFont{}
		if encoding, ok := dict["Encoding"].(pdfName); ok {
			font.twoByte = strings.HasPrefix(string(encoding), "Identity")
		}
		if subtype, _ := dict["Subtype"].(pdfName); subtype == "Type0" {
			// A composite font is multi-byte even when /Encoding is a stream
			// rather than a name.
			font.twoByte = true
		}
		if ref, isRef := dict["ToUnicode"].(pdfRef); isRef {
			if object, ok := objects[pdfObjectKey{ref.number, ref.generation}]; ok {
				// Through the budget: a font is resolved once per page, so a
				// ToUnicode stream that is a decompression bomb would otherwise
				// be inflated thousands of times inside a document whose
				// content streams are properly bounded.
				if data, err := pdfDecodeStreamWithin(objects, object, budget); err == nil {
					font.toUnicode = parsePDFToUnicode(data)
				}
			}
		}
		fonts[name] = font
	}
	return fonts
}

// pdfAllToUnicode merges every ToUnicode map in the document, for the fallback
// path where fonts could not be attributed to a page. Conflicting codes resolve
// to whichever was seen first, which is a real limitation of that path.
// The second return names the streams this consumed. They are CMaps, so they
// cannot hold page content, and the fallback scan below must not decode them a
// second time: the decode budget is document-wide, and spending it twice on the
// same bytes is spent where it matters most — the degraded file that needed the
// fallback in the first place.
func pdfAllToUnicode(objects map[pdfObjectKey]*pdfObject, budget *pdfDecodeBudget) (map[uint32]string, map[pdfObjectKey]bool) {
	merged := map[uint32]string{}
	consumed := map[pdfObjectKey]bool{}
	for _, key := range pdfSortedKeys(objects) {
		object := objects[key]
		if len(object.stream) == 0 {
			continue
		}
		if _, isFont := object.dict["ToUnicode"]; !isFont {
			// Also accept the CMap stream itself, which names its own type.
			if name, _ := object.dict["Type"].(pdfName); name != "CMap" {
				if !bytes.Contains(object.stream[:min(len(object.stream), 64)], []byte("CMap")) {
					continue
				}
			}
		}
		data, err := pdfDecodeStreamWithin(objects, object, budget)
		if err != nil {
			continue
		}
		consumed[key] = true
		for code, text := range parsePDFToUnicode(data) {
			if _, exists := merged[code]; !exists {
				merged[code] = text
			}
		}
	}
	return merged, consumed
}

// parsePDFToUnicode reads the bfchar/bfrange sections of a ToUnicode CMap.
// This is the piece that makes modern PDFs readable at all: without it a
// subset-embedded font's glyph indices are extracted as if they were characters.
func parsePDFToUnicode(data []byte) map[uint32]string {
	out := map[uint32]string{}
	text := string(data)

	for _, section := range pdfCMapSections(text, "beginbfchar", "endbfchar") {
		parser := &pdfParser{data: []byte(section)}
		for {
			parser.skipSpace()
			if parser.pos >= len(parser.data) {
				break
			}
			src, ok1 := parser.parseValue().(string)
			dst, ok2 := parser.parseValue().(string)
			if !ok1 || !ok2 {
				break
			}
			out[pdfCodeOf(src)] = pdfUTF16BEToString(dst)
		}
	}

	for _, section := range pdfCMapSections(text, "beginbfrange", "endbfrange") {
		parser := &pdfParser{data: []byte(section)}
		for {
			parser.skipSpace()
			if parser.pos >= len(parser.data) {
				break
			}
			lowValue := parser.parseValue()
			highValue := parser.parseValue()
			low, ok1 := lowValue.(string)
			high, ok2 := highValue.(string)
			if !ok1 || !ok2 {
				break
			}
			parser.skipSpace()
			lowCode, highCode := pdfCodeOf(low), pdfCodeOf(high)
			// A range covering the whole code space is a corrupt CMap; refuse
			// it rather than allocating four billion entries.
			if highCode < lowCode || highCode-lowCode > 65535 {
				break
			}
			if parser.pos < len(parser.data) && parser.data[parser.pos] == '[' {
				items, _ := parser.parseValue().([]pdfValue)
				for i, item := range items {
					if s, ok := item.(string); ok {
						out[lowCode+uint32(i)] = pdfUTF16BEToString(s)
					}
				}
				continue
			}
			dst, ok := parser.parseValue().(string)
			if !ok {
				break
			}
			base := pdfUTF16BEToString(dst)
			runes := []rune(base)
			for code := lowCode; code <= highCode; code++ {
				if len(runes) == 0 {
					break
				}
				// Only the last character advances across a range, per the spec.
				next := make([]rune, len(runes))
				copy(next, runes)
				next[len(next)-1] += rune(code - lowCode)
				out[code] = string(next)
			}
		}
	}
	return out
}

func pdfCMapSections(text, open, close string) []string {
	var sections []string
	for {
		start := strings.Index(text, open)
		if start < 0 {
			return sections
		}
		text = text[start+len(open):]
		end := strings.Index(text, close)
		if end < 0 {
			sections = append(sections, text)
			return sections
		}
		sections = append(sections, text[:end])
		text = text[end+len(close):]
	}
}

// pdfCodeOf turns the bytes of a CMap source code into an integer. Codes are
// written as hex strings and are one or two bytes wide.
func pdfCodeOf(s string) uint32 {
	var code uint32
	for i := 0; i < len(s) && i < 4; i++ {
		code = code<<8 | uint32(s[i])
	}
	return code
}

func pdfUTF16BEToString(s string) string {
	if len(s)%2 != 0 {
		return s
	}
	units := make([]uint16, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		units = append(units, uint16(s[i])<<8|uint16(s[i+1]))
	}
	decoded := string(utf16.Decode(units))
	if !utf8.ValidString(decoded) {
		return s
	}
	return decoded
}

// --- content streams ---------------------------------------------------------

// pdfContentText walks a decoded content stream and emits the text it draws.
//
// Only the operators that show text or move the cursor matter here. Position is
// used solely to decide where a line ends: a Td/TD/T*/Tm that moves down starts
// a new line, and a large negative kern inside a TJ array is a word space. That
// is enough for prose and deliberately not enough to reconstruct columns.
func pdfContentText(content []byte, fonts map[string]*pdfFont) string {
	parser := &pdfParser{data: content}
	var (
		out      strings.Builder
		operands []pdfValue
		font     *pdfFont
		lastY    float64
		haveY    bool
	)
	newline := func() {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
	}
	show := func(value pdfValue) {
		s, ok := value.(string)
		if !ok {
			return
		}
		out.WriteString(pdfDecodeShownString(s, font))
	}
	for out.Len() < maxExtractOutputBytes {
		parser.skipSpace()
		if parser.pos >= len(parser.data) {
			break
		}
		if operator, ok := parser.parseOperator(); ok {
			switch operator {
			case "Tf":
				font = nil
				if len(operands) >= 2 {
					if name, ok := operands[len(operands)-2].(pdfName); ok {
						font = fonts[string(name)]
						if font == nil {
							font = fonts[""] // the fallback path's merged map
						}
					}
				}
			case "Tj":
				if len(operands) >= 1 {
					show(operands[len(operands)-1])
				}
			case "'", "\"":
				newline()
				if len(operands) >= 1 {
					show(operands[len(operands)-1])
				}
			case "TJ":
				if len(operands) >= 1 {
					items, _ := operands[len(operands)-1].([]pdfValue)
					for _, item := range items {
						switch v := item.(type) {
						case string:
							out.WriteString(pdfDecodeShownString(v, font))
						case float64:
							// Kerning is in thousandths of an em, negative to
							// move right. Anything past a fifth of an em is a
							// space the producer chose not to encode as one.
							if v < -200 {
								out.WriteByte(' ')
							}
						}
					}
				}
			case "Td", "TD":
				if len(operands) >= 2 {
					if dy, ok := pdfFloat(operands[len(operands)-1]); ok && dy != 0 {
						newline()
					}
				}
			case "Tm":
				if len(operands) >= 6 {
					if y, ok := pdfFloat(operands[len(operands)-1]); ok {
						if haveY && y != lastY {
							newline()
						}
						lastY, haveY = y, true
					}
				}
			case "T*", "ET":
				newline()
			}
			operands = operands[:0]
			continue
		}
		value := parser.parseValue()
		if value == nil && parser.stuck {
			parser.pos++ // never spin on a byte we cannot classify
			parser.stuck = false
			continue
		}
		operands = append(operands, value)
		if len(operands) > 64 {
			// Malformed content can pile up operands forever; a real operator
			// never takes anywhere near this many.
			operands = operands[:0]
		}
	}
	return out.String()
}

// pdfDecodeShownString turns the bytes of a shown string into characters,
// through the font's ToUnicode map when there is one.
func pdfDecodeShownString(s string, font *pdfFont) string {
	if font == nil || len(font.toUnicode) == 0 {
		if font != nil && font.twoByte {
			// A composite font with no usable ToUnicode: the bytes are glyph
			// indices and there is nothing to map them with. Emitting them
			// would be mojibake, so emit nothing and let the caller's prose
			// gate see an empty or partial document rather than a corrupt one.
			return ""
		}
		return s
	}
	var b strings.Builder
	width := 1
	if font.twoByte {
		width = 2
	}
	for i := 0; i+width <= len(s); i += width {
		var code uint32
		for j := 0; j < width; j++ {
			code = code<<8 | uint32(s[i+j])
		}
		if text, ok := font.toUnicode[code]; ok {
			b.WriteString(text)
			continue
		}
		// An unmapped code in a font that HAS a ToUnicode map is a glyph the
		// producer could not name — typically a ligature it left out of its own
		// CMap. The raw byte is a glyph index, not a character, so writing it
		// emits something that is certainly wrong ("Cover Le(er"), and runs of
		// them can trip the prose gate and lose an otherwise-good document.
		// Dropping costs a character and keeps the rest of the word readable.
	}
	return b.String()
}

// --- a small PDF object parser ----------------------------------------------

type pdfParser struct {
	data  []byte
	pos   int
	stuck bool
}

func (p *pdfParser) skipSpace() {
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '%' { // comment to end of line
			for p.pos < len(p.data) && p.data[p.pos] != '\n' && p.data[p.pos] != '\r' {
				p.pos++
			}
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0 {
			p.pos++
			continue
		}
		return
	}
}

func (p *pdfParser) hasPrefix(s string) bool {
	return p.pos+len(s) <= len(p.data) && string(p.data[p.pos:p.pos+len(s)]) == s
}

func pdfIsDelimiter(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func pdfIsSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == 0
}

// parseOperator reads a content-stream operator: a bare keyword that is not a
// number, a literal, or one of the object keywords.
func (p *pdfParser) parseOperator() (string, bool) {
	p.skipSpace()
	start := p.pos
	if start >= len(p.data) {
		return "", false
	}
	if c := p.data[start]; c == '\'' || c == '"' {
		p.pos++
		return string(c), true
	}
	for p.pos < len(p.data) && !pdfIsSpace(p.data[p.pos]) && !pdfIsDelimiter(p.data[p.pos]) {
		p.pos++
	}
	token := string(p.data[start:p.pos])
	if token == "" {
		p.pos = start
		return "", false
	}
	switch token {
	case "true", "false", "null", "obj", "endobj", "stream", "endstream", "R", "xref", "trailer", "startxref":
		p.pos = start
		return "", false
	}
	if c := token[0]; c == '+' || c == '-' || c == '.' || (c >= '0' && c <= '9') {
		p.pos = start
		return "", false
	}
	return token, true
}

func (p *pdfParser) parseValue() pdfValue {
	p.skipSpace()
	if p.pos >= len(p.data) {
		return nil
	}
	switch c := p.data[p.pos]; {
	case c == '<' && p.hasPrefix("<<"):
		return p.parseDict()
	case c == '<':
		return p.parseHexString()
	case c == '(':
		return p.parseLiteralString()
	case c == '[':
		return p.parseArray()
	case c == '/':
		return p.parseName()
	case c == ']' || c == '>' || c == '}' || c == ')':
		p.stuck = true
		return nil
	default:
		return p.parseKeywordOrNumber()
	}
}

func (p *pdfParser) parseDict() pdfValue {
	p.pos += 2
	dict := map[string]pdfValue{}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) {
			return dict
		}
		if p.hasPrefix(">>") {
			p.pos += 2
			return dict
		}
		if p.data[p.pos] != '/' {
			// Not a key: a malformed dictionary. Step past the offending byte
			// rather than looping on it.
			p.pos++
			continue
		}
		key, ok := p.parseName().(pdfName)
		if !ok {
			return dict
		}
		dict[string(key)] = p.parseValue()
	}
}

func (p *pdfParser) parseArray() pdfValue {
	p.pos++
	var items []pdfValue
	for {
		p.skipSpace()
		if p.pos >= len(p.data) {
			return items
		}
		if p.data[p.pos] == ']' {
			p.pos++
			return items
		}
		before := p.pos
		value := p.parseValue()
		if p.pos == before {
			p.pos++ // guarantee progress
			continue
		}
		if p.stuck {
			p.stuck = false
			continue
		}
		items = append(items, value)
		if len(items) > 65536 {
			return items
		}
	}
}

func (p *pdfParser) parseName() pdfValue {
	p.pos++ // '/'
	var b strings.Builder
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if pdfIsSpace(c) || pdfIsDelimiter(c) {
			break
		}
		if c == '#' && p.pos+2 < len(p.data) {
			if v, err := strconv.ParseUint(string(p.data[p.pos+1:p.pos+3]), 16, 8); err == nil {
				b.WriteByte(byte(v))
				p.pos += 3
				continue
			}
		}
		b.WriteByte(c)
		p.pos++
	}
	return pdfName(b.String())
}

func (p *pdfParser) parseLiteralString() pdfValue {
	p.pos++ // '('
	var b strings.Builder
	depth := 1
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		p.pos++
		switch c {
		case '\\':
			if p.pos >= len(p.data) {
				return b.String()
			}
			e := p.data[p.pos]
			p.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '\n':
				// A backslash-newline is a line continuation inside the source
				// and contributes nothing to the string.
			case '\r':
				if p.pos < len(p.data) && p.data[p.pos] == '\n' {
					p.pos++
				}
			default:
				if e >= '0' && e <= '7' {
					value := int(e - '0')
					for k := 0; k < 2 && p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '7'; k++ {
						value = value*8 + int(p.data[p.pos]-'0')
						p.pos++
					}
					b.WriteByte(byte(value))
					continue
				}
				b.WriteByte(e)
			}
		case '(':
			depth++
			b.WriteByte(c)
		case ')':
			depth--
			if depth == 0 {
				return b.String()
			}
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (p *pdfParser) parseHexString() pdfValue {
	p.pos++ // '<'
	start := p.pos
	for p.pos < len(p.data) && p.data[p.pos] != '>' {
		p.pos++
	}
	raw := p.data[start:p.pos]
	if p.pos < len(p.data) {
		p.pos++
	}
	return string(pdfASCIIHexDecode(append(raw, '>')))
}

// parseKeywordOrNumber handles numbers, booleans, null, and the `N G R`
// reference form — which can only be recognised by looking ahead past two
// integers for the R.
func (p *pdfParser) parseKeywordOrNumber() pdfValue {
	start := p.pos
	for p.pos < len(p.data) && !pdfIsSpace(p.data[p.pos]) && !pdfIsDelimiter(p.data[p.pos]) {
		p.pos++
	}
	token := string(p.data[start:p.pos])
	switch token {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	case "":
		p.stuck = true
		return nil
	}
	if number, err := strconv.Atoi(token); err == nil {
		save := p.pos
		p.skipSpace()
		genStart := p.pos
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
		if p.pos > genStart {
			generation, _ := strconv.Atoi(string(p.data[genStart:p.pos]))
			p.skipSpace()
			if p.pos < len(p.data) && p.data[p.pos] == 'R' &&
				(p.pos+1 >= len(p.data) || pdfIsSpace(p.data[p.pos+1]) || pdfIsDelimiter(p.data[p.pos+1])) {
				p.pos++
				return pdfRef{number, generation}
			}
		}
		p.pos = save
		return float64(number)
	}
	if f, err := strconv.ParseFloat(token, 64); err == nil {
		return f
	}
	return pdfName(token) // an unrecognised bare keyword; harmless as an operand
}

func pdfInt(value pdfValue) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

func pdfFloat(value pdfValue) (float64, bool) {
	if f, ok := value.(float64); ok {
		return f, true
	}
	return 0, false
}
