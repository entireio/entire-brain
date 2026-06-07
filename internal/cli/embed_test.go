package cli

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"testing"

	entirebrain "github.com/ashtom/entire-brain"
)

func testEmbedder(t *testing.T) *staticEmbedder {
	t.Helper()
	e, err := loadStaticEmbedder(entirebrain.EmbedModel)
	if err != nil {
		t.Fatalf("loadStaticEmbedder: %v", err)
	}
	return e
}

// cosine aliases the production helper so the tests exercise the same code.
func cosine(a, b []float32) float64 { return cosineFloat32(a, b) }

// TestEmbedGoldenParity is the load-bearing correctness check: the pure-Go
// tokenizer + pooling must reproduce the reference Model2Vec embeddings (up to
// int8 quantization). A tokenizer bug would drop cosine far below the floor.
func TestEmbedGoldenParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/embed_golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden struct {
		Dim     int         `json:"dim"`
		Texts   []string    `json:"texts"`
		Vectors [][]float32 `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	e := testEmbedder(t)
	if e.Dim() != golden.Dim {
		t.Fatalf("dim mismatch: embedder %d golden %d", e.Dim(), golden.Dim)
	}
	for i, text := range golden.Texts {
		got := e.Embed(text)
		want := golden.Vectors[i]
		if text == "" {
			// empty input has no token signal: expect the zero vector.
			var norm float64
			for _, v := range got {
				norm += float64(v) * float64(v)
			}
			if norm > 1e-9 {
				t.Errorf("empty text should embed to zero vector, got norm %v", norm)
			}
			continue
		}
		if c := cosine(got, want); c < 0.995 {
			t.Errorf("text %q parity cosine %.5f < 0.995 (tokenizer/pooling mismatch)", text, c)
		}
	}
}

// TestEmbedSemanticOrdering sanity-checks that the model captures paraphrase —
// the property that lets it reach term-disjoint relevant facts. A query should
// be closer to its paraphrase than to an unrelated sentence.
func TestEmbedSemanticOrdering(t *testing.T) {
	e := testEmbedder(t)
	cases := []struct{ query, related, unrelated string }{
		{"use spaces for indentation", "the code is indented with space characters, not tabs", "the deployment pipeline runs nightly"},
		{"how is authentication handled", "tokens are validated before each request", "the cat sat on the mat"},
	}
	for _, c := range cases {
		q := e.Embed(c.query)
		rel := cosine(q, e.Embed(c.related))
		unrel := cosine(q, e.Embed(c.unrelated))
		if rel <= unrel {
			t.Errorf("query %q: related cosine %.3f should exceed unrelated %.3f", c.query, rel, unrel)
		}
	}
}

func TestWordPieceDecomposition(t *testing.T) {
	e := testEmbedder(t)
	// A rare/long compound should decompose into multiple subword pieces (more
	// ids than whole words) rather than collapsing to a single [UNK].
	ids := e.tokenize("supercalifragilistic")
	if len(ids) < 2 {
		t.Errorf("expected subword decomposition, got %d ids", len(ids))
	}
	for _, id := range ids {
		if id == e.unkID {
			t.Errorf("unexpected [UNK] in decomposition of a Latin word: %v", ids)
		}
	}
	// Punctuation is isolated: trailing "." becomes its own token, so
	// "tabs." has exactly one more id than "tabs".
	if got, base := len(e.tokenize("tabs.")), len(e.tokenize("tabs")); got != base+1 {
		t.Errorf("expected punctuation isolated as one extra token: %d vs base %d", got, base)
	}
}

func TestStripAccent(t *testing.T) {
	cases := map[rune]rune{'é': 'e', 'ñ': 'n', 'ü': 'u', 'č': 'c'}
	for in, want := range cases {
		got, ok := stripAccent(in)
		if !ok || got != want {
			t.Errorf("stripAccent(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
	}
	if _, ok := stripAccent('a'); ok {
		t.Errorf("stripAccent('a') should report not-an-accent")
	}
	if got, ok := stripAccent('́'); !ok || got != 0 {
		t.Errorf("combining mark should be dropped, got %q,%v", got, ok)
	}
}

func TestDefaultEmbedderLoads(t *testing.T) {
	if defaultEmbedder() == nil {
		t.Fatal("bundled embed model failed to load")
	}
}

// encodeTestModel builds a minimal valid embedmodel.bin byte layout (matching
// the format in scripts/convert_embedmodel.py) so the header-validation paths
// can be exercised without the 32 MB bundled asset.
func encodeTestModel(vocab []string, dim int, unkID uint32) []byte {
	var b bytes.Buffer
	b.WriteString(embedModelMagic)
	_ = binary.Write(&b, binary.LittleEndian, uint32(1)) // version
	_ = binary.Write(&b, binary.LittleEndian, uint16(len("test")))
	b.WriteString("test")
	_ = binary.Write(&b, binary.LittleEndian, uint32(dim))
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(vocab)))
	_ = binary.Write(&b, binary.LittleEndian, uint32(1)) // dtype int8
	_ = binary.Write(&b, binary.LittleEndian, uint32(0)) // flags
	_ = binary.Write(&b, binary.LittleEndian, unkID)
	_ = binary.Write(&b, binary.LittleEndian, uint16(len("##")))
	b.WriteString("##")
	for _, tok := range vocab {
		_ = binary.Write(&b, binary.LittleEndian, uint16(len(tok)))
		b.WriteString(tok)
	}
	for i := 0; i < dim; i++ {
		_ = binary.Write(&b, binary.LittleEndian, math.Float32bits(1.0)) // scales
	}
	b.Write(make([]byte, len(vocab)*dim)) // zero int8 matrix
	return b.Bytes()
}

func TestLoadStaticEmbedderRejectsBadUnkID(t *testing.T) {
	vocab := []string{"[UNK]", "hello", "world"}
	// A valid unkID loads cleanly.
	if _, err := loadStaticEmbedder(encodeTestModel(vocab, 4, 0)); err != nil {
		t.Fatalf("valid model should load: %v", err)
	}
	// An out-of-range unkID must fail the load rather than panic later in Embed
	// (out-of-bounds matrix slice) on an unknown token.
	if _, err := loadStaticEmbedder(encodeTestModel(vocab, 4, uint32(len(vocab)))); err == nil {
		t.Fatal("expected out-of-range unkID to be rejected at load")
	}
}

func TestLoadStaticEmbedderRejectsOversizedHeader(t *testing.T) {
	// Patch the dim field (offset 14: magic[4] + version[4] + nameLen[2] +
	// "test"[4]) to a value far larger than the file, simulating a corrupt
	// header. The preflight must reject it rather than attempt a giant
	// allocation before take() discovers the truncation.
	raw := encodeTestModel([]string{"[UNK]", "a"}, 4, 0)
	binary.LittleEndian.PutUint32(raw[14:], 1<<30)
	if _, err := loadStaticEmbedder(raw); err == nil {
		t.Fatal("expected oversized header to be rejected before allocation")
	}
}
