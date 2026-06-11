package cli

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"unicode"

	entirebrain "github.com/ashtom/entire-brain"
)

// Embedder turns text into a unit-length vector for semantic recall. It is the
// Phase D abstraction boundary: the bundled backend is a pure-Go Model2Vec
// static model (staticEmbedder), but a transformer bi-encoder (ONNX) or a
// provider-shelled embedder can drop in behind this interface without touching
// the ranking path, once the eval harness shows the static model leaves recall
// headroom. Embed returns a slice of length Dim(); input that tokenizes to
// nothing (empty/whitespace-only) returns the zero vector, which callers treat
// as "no signal". Unknown words map to [UNK] and so still yield a non-zero
// vector.
type Embedder interface {
	Embed(text string) []float32
	Dim() int
	// ID identifies the model + revision, used to invalidate cached fact
	// vectors when the backend changes.
	ID() string
}

// staticEmbedder is a Model2Vec static embedding table: tokenize (BERT
// WordPiece) -> gather per-token vectors -> mean-pool -> L2-normalize. The
// matrix is int8 per-column quantized; rows are dequantized on gather, which
// keeps only ~the int8 table resident and is cheap since each text touches only
// a handful of tokens.
type staticEmbedder struct {
	id      string
	dim     int
	vocab   map[string]int32
	matrix  []int8    // vocab*dim, row-major
	scales  []float32 // len dim, per-column dequant scale
	unkID   int32
	cont    string // WordPiece continuation prefix, e.g. "##"
	lower   bool
	strip   bool
	chinese bool
}

const embedModelMagic = "EBM1"

// maxWordPieceChars matches BERT's max_input_chars_per_word: a token longer
// than this is emitted as [UNK] rather than greedily decomposed.
const maxWordPieceChars = 100

func loadStaticEmbedder(raw []byte) (*staticEmbedder, error) {
	r := &byteReader{b: raw}
	if string(r.take(4)) != embedModelMagic {
		return nil, errors.New("embed model: bad magic")
	}
	if v := r.u32(); v != 1 {
		return nil, fmt.Errorf("embed model: unsupported version %d", v)
	}
	id := string(r.take(int(r.u16())))
	dim := int(r.u32())
	vocabSize := int(r.u32())
	if dtype := r.u32(); dtype != 1 {
		return nil, fmt.Errorf("embed model: unsupported dtype %d", dtype)
	}
	flags := r.u32()
	unkID := int32(r.u32())
	cont := string(r.take(int(r.u16())))
	if r.err != nil {
		return nil, fmt.Errorf("embed model header: %w", r.err)
	}
	if dim <= 0 || vocabSize <= 0 {
		return nil, errors.New("embed model: empty dim/vocab")
	}
	if unkID < 0 || int(unkID) >= vocabSize {
		return nil, fmt.Errorf("embed model: unkID %d out of range [0,%d)", unkID, vocabSize)
	}
	// Preflight the header sizes against the actual file length before any
	// allocation: a corrupt/malformed header could otherwise request a giant
	// vocab map / scales / matrix (or overflow vocabSize*dim) before take()
	// discovers the truncation, defeating the graceful degrade-to-lexical path.
	// The file is an upper bound on every section, and dividing avoids computing
	// the (potentially overflowing) products: scales need dim*4 bytes, the vocab
	// needs ≥vocabSize*2 (a uint16 length per token), the matrix vocabSize*dim.
	if n := len(raw); dim > n/4 || vocabSize > n/2 || vocabSize > n/dim {
		return nil, fmt.Errorf("embed model: header sizes (dim=%d vocab=%d) exceed file length %d", dim, vocabSize, n)
	}
	vocab := make(map[string]int32, vocabSize)
	for i := 0; i < vocabSize; i++ {
		tok := string(r.take(int(r.u16())))
		if r.err != nil {
			return nil, fmt.Errorf("embed model vocab[%d]: %w", i, r.err)
		}
		vocab[tok] = int32(i)
	}
	scales := make([]float32, dim)
	for i := range scales {
		scales[i] = math.Float32frombits(r.u32())
	}
	matrix := make([]int8, vocabSize*dim)
	mb := r.take(len(matrix))
	if r.err != nil {
		return nil, fmt.Errorf("embed model matrix: %w", r.err)
	}
	for i, b := range mb {
		matrix[i] = int8(b)
	}
	return &staticEmbedder{
		id: id, dim: dim, vocab: vocab, matrix: matrix, scales: scales,
		unkID: unkID, cont: cont,
		lower:   flags&1 != 0,
		strip:   flags&2 != 0,
		chinese: flags&4 != 0,
	}, nil
}

func (e *staticEmbedder) Dim() int   { return e.dim }
func (e *staticEmbedder) ID() string { return e.id }

func (e *staticEmbedder) Embed(text string) []float32 {
	out := make([]float32, e.dim)
	ids := e.tokenize(text)
	if len(ids) == 0 {
		return out
	}
	for _, id := range ids {
		base := int(id) * e.dim
		row := e.matrix[base : base+e.dim]
		for d := 0; d < e.dim; d++ {
			out[d] += float32(row[d]) * e.scales[d]
		}
	}
	inv := 1.0 / float32(len(ids))
	var norm float64
	for d := range out {
		out[d] *= inv
		norm += float64(out[d]) * float64(out[d])
	}
	if norm > 0 {
		s := float32(1.0 / math.Sqrt(norm))
		for d := range out {
			out[d] *= s
		}
	}
	return out
}

// tokenize replicates BERT BertNormalizer + BertPreTokenizer + WordPiece so the
// gathered token ids match the ids the model was distilled against (validated
// by the golden-parity test).
func (e *staticEmbedder) tokenize(text string) []int32 {
	var ids []int32
	for _, word := range e.preTokenize(e.normalize(text)) {
		ids = e.wordPiece(word, ids)
	}
	return ids
}

// normalize applies clean_text, handle_chinese_chars, lowercase, and accent
// stripping. Accent stripping folds Latin precomposed letters to their base
// (covering the realistic English-plus-names corpus) rather than pulling in a
// full Unicode NFD dependency.
func (e *staticEmbedder) normalize(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if r == 0 || r == 0xFFFD || (unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r') {
			continue // clean_text: drop nulls and control chars
		}
		if unicode.IsSpace(r) {
			b.WriteByte(' ')
			continue
		}
		if e.chinese && isCJK(r) {
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
			continue
		}
		if e.lower {
			r = unicode.ToLower(r)
		}
		if e.strip {
			if folded, ok := stripAccent(r); ok {
				if folded != 0 {
					b.WriteRune(folded)
				}
				continue
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// preTokenize splits on whitespace and isolates punctuation into its own
// tokens, matching BertPreTokenizer.
func (e *staticEmbedder) preTokenize(text string) []string {
	var out []string
	for _, field := range strings.Fields(text) {
		var cur strings.Builder
		for _, r := range field {
			if isBertPunct(r) {
				if cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
				}
				out = append(out, string(r))
				continue
			}
			cur.WriteRune(r)
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
	}
	return out
}

// wordPiece greedily decomposes one word into the longest matching vocab
// pieces (continuation pieces carry the cont prefix), appending their ids. A
// word that cannot be fully covered contributes a single [UNK].
func (e *staticEmbedder) wordPiece(word string, ids []int32) []int32 {
	runes := []rune(word)
	if len(runes) == 0 {
		return ids
	}
	if len(runes) > maxWordPieceChars {
		return append(ids, e.unkID)
	}
	start := 0
	var pieces []int32
	for start < len(runes) {
		end := len(runes)
		found := int32(-1)
		for start < end {
			sub := string(runes[start:end])
			if start > 0 {
				sub = e.cont + sub
			}
			if id, ok := e.vocab[sub]; ok {
				found = id
				break
			}
			end--
		}
		if found < 0 {
			return append(ids, e.unkID) // whole word is [UNK]
		}
		pieces = append(pieces, found)
		start = end
	}
	return append(ids, pieces...)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) || (r >= 0xF900 && r <= 0xFAFF)
}

// isBertPunct matches BERT's punctuation set: the ASCII punctuation ranges plus
// any Unicode punctuation category.
func isBertPunct(r rune) bool {
	if (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~') {
		return true
	}
	return unicode.IsPunct(r)
}

// defaultEmbedder lazily loads the bundled static model once. A load failure
// (corrupt/absent asset) yields nil so semantic recall degrades to the lexical
// path rather than failing — embeddings are optional by design.
var (
	defaultEmbedderOnce sync.Once
	defaultEmbedderInst Embedder
)

// configuredEmbedder resolves the process embedder from ENTIRE_BRAIN_EMBEDDER,
// returning the embedder plus an optional human warning when an opt-in could not
// be honored. ENTIRE_BRAIN_EMBEDDER=ollama swaps the bundled Model2Vec static
// model for EmbeddingGemma served over a local Ollama (or node-llama-cpp) HTTP
// endpoint. When that opt-in is selected but the server does not return an
// embedding (unreachable, wrong model, or an error response), it falls
// back to the bundled Model2Vec model — keeping a single consistent vector space
// (and cache namespace) — rather than silently degrading the semantic arm to
// lexical-only, and reports the fallback so the user knows their opt-in did not
// take effect.
func configuredEmbedder() (Embedder, string) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_EMBEDDER")), "ollama") {
		o := newOllamaEmbedder()
		if o == nil {
			// The loopback guard rejected ENTIRE_BRAIN_EMBED_URL (non-loopback
			// or unparseable). The exact scenario the guard hardens must
			// degrade to the bundled model, not panic.
			base := "ENTIRE_BRAIN_EMBEDDER=ollama set but ENTIRE_BRAIN_EMBED_URL is not a loopback URL (rejected to keep embeddings on-box)"
			if e, err := loadStaticEmbedder(entirebrain.EmbedModel); err == nil {
				return e, base + "; falling back to the bundled Model2Vec embedder"
			}
			return nil, base + " and the bundled Model2Vec fallback could not be loaded; semantic retrieval is unavailable (lexical only)"
		}
		if o.reachable() {
			return o, ""
		}
		base := fmt.Sprintf("ENTIRE_BRAIN_EMBEDDER=ollama set but the embed server at %s did not return an embedding (unreachable, wrong model, or an error response)", o.url)
		if e, err := loadStaticEmbedder(entirebrain.EmbedModel); err == nil {
			return e, base + "; falling back to the bundled Model2Vec embedder"
		}
		// The bundled fallback also failed to load — there is no semantic arm.
		return nil, base + " and the bundled Model2Vec fallback could not be loaded; semantic retrieval is unavailable (lexical only)"
	}
	if e, err := loadStaticEmbedder(entirebrain.EmbedModel); err == nil {
		return e, ""
	}
	// The bundled asset is the default embedder; if it cannot be loaded (e.g. a
	// corrupt build) surface it rather than silently degrading to lexical-only.
	return nil, "the bundled Model2Vec embedder could not be loaded; semantic retrieval is unavailable (lexical only)"
}

func defaultEmbedder() Embedder {
	defaultEmbedderOnce.Do(func() {
		e, warn := configuredEmbedder()
		if warn != "" {
			fmt.Fprintln(os.Stderr, "entire-brain: "+warn)
		}
		defaultEmbedderInst = e
	})
	return defaultEmbedderInst
}

// byteReader is a tiny sticky-error reader for the fixed model layout.
type byteReader struct {
	b   []byte
	off int
	err error
}

func (r *byteReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || r.off+n > len(r.b) {
		r.err = errors.New("unexpected end of data")
		return nil
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}

func (r *byteReader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *byteReader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}
