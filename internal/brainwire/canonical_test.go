package brainwire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// goldenCanonicalArtifact is the byte fixture for representativeArtifact (see
// contract_test.go) in canonical form. It is written out by hand, not copied
// from the encoder, so it independently pins every rule:
//
//   - top-level keys sorted facts < manifest < overlays < snapshots (NOT the
//     struct order manifest, snapshots, overlays, facts);
//   - manifest keys sorted brain_schema_version first, repo_key last;
//   - content keys sorted digest, media_type, path, size;
//   - generated_at UTC, whole seconds, literal Z;
//   - sizes as bare decimal integers;
//   - no whitespace anywhere.
const goldenCanonicalArtifact = `{"facts":[{"branch":"main","content":{"digest":"sha256:3333333333333333333333333333333333333333333333333333333333333333","media_type":"application/x-ndjson","path":"facts/main/facts.ndjson","size":4096}}],` +
	`"manifest":{"brain_schema_version":"1.0","default_branch":"main","generated_at":"2026-06-01T12:00:00Z","provider":"entire-graph","provider_schema_version":"1.1","provider_version":"0.1.0","repo_key":"gh/org/repo"},` +
	`"overlays":[{"base_commit":"abc123","branch":"feature-x","content":{"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222","media_type":"application/json","path":"semantic/overlays/abc123..def456.json","size":512},"head_commit":"def456"}],` +
	`"snapshots":[{"commit":"abc123","content":{"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","media_type":"application/json","path":"semantic/snapshots/abc123/snapshot.json","size":2048},"tree":"tree789"}]}`

func mustCanonicalMarshal(t *testing.T, a BrainArtifact) string {
	t.Helper()
	b, err := CanonicalMarshal(a)
	if err != nil {
		t.Fatalf("CanonicalMarshal: %v", err)
	}
	return string(b)
}

func mustCanonicalize(t *testing.T, raw string) string {
	t.Helper()
	b, err := Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("Canonicalize(%s): %v", raw, err)
	}
	return string(b)
}

// TestCanonicalGolden pins the exact bytes of a full artifact.
func TestCanonicalGolden(t *testing.T) {
	got := mustCanonicalMarshal(t, *representativeArtifact(t))
	if got != goldenCanonicalArtifact {
		t.Fatalf("canonical bytes drifted:\n got=%s\nwant=%s", got, goldenCanonicalArtifact)
	}
}

// TestCanonicalKeyOrderIsLexicographicNotDeclaration proves rule 2: the encoder
// does not follow Go struct declaration order.
func TestCanonicalKeyOrderIsLexicographicNotDeclaration(t *testing.T) {
	got := mustCanonicalMarshal(t, *representativeArtifact(t))

	if !strings.HasPrefix(got, `{"facts":`) {
		t.Fatalf("top-level keys not lexicographic (want facts first): %s", got)
	}
	if !strings.Contains(got, `"manifest":{"brain_schema_version":`) {
		t.Fatalf("manifest keys not lexicographic (want brain_schema_version first): %s", got)
	}
	if !strings.Contains(got, `"content":{"digest":"sha256:1111`) {
		t.Fatalf("content keys not lexicographic (want digest first): %s", got)
	}
	// Declaration order would have put manifest first and size before
	// media_type; make that explicit.
	if strings.HasPrefix(got, `{"manifest":`) {
		t.Fatalf("encoder used Go declaration order: %s", got)
	}
	if strings.Contains(got, `"size":2048,"media_type"`) {
		t.Fatalf("content used declaration order: %s", got)
	}
}

// TestCanonicalIdempotent proves Canonical(Canonical(x)) == Canonical(x) for
// canonical, Go-default and deliberately non-canonical inputs.
func TestCanonicalIdempotent(t *testing.T) {
	art := representativeArtifact(t)
	goDefault, err := json.Marshal(art)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	cases := []struct {
		name string
		raw  string
	}{
		{name: "already canonical", raw: goldenCanonicalArtifact},
		{name: "go encoding/json default", raw: string(goDefault)},
		{
			name: "non-canonical: reordered, padded, explicit defaults",
			raw: `{
				"snapshots": [{"tree": "", "content": {"size": 0, "digest": "sha256:aaaa"}, "commit": "c1"}],
				"manifest": {"generated_at": "2026-06-01T12:00:00.500Z", "repo_key": "gh/org/repo", "brain_schema_version": "1.0"}
			}`,
		},
		{name: "minimal", raw: `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			once := mustCanonicalize(t, tc.raw)
			twice := mustCanonicalize(t, once)
			if once != twice {
				t.Fatalf("not idempotent:\n once=%s\ntwice=%s", once, twice)
			}
		})
	}
}

// TestCanonicalOrderIndependence proves rule 2 end-to-end: the same logical
// artifact written with different field orders canonicalizes to the same bytes.
func TestCanonicalOrderIndependence(t *testing.T) {
	a := `{"manifest":{"repo_key":"gh/org/repo","generated_at":"2026-06-01T12:00:00Z","brain_schema_version":"1.0","provider":"entire-graph"},
	       "facts":[{"branch":"main","content":{"digest":"sha256:3333","size":9}}]}`
	b := `{"facts":[{"content":{"size":9,"digest":"sha256:3333"},"branch":"main"}],
	       "manifest":{"provider":"entire-graph","brain_schema_version":"1.0","generated_at":"2026-06-01T12:00:00Z","repo_key":"gh/org/repo"}}`

	if got, want := mustCanonicalize(t, b), mustCanonicalize(t, a); got != want {
		t.Fatalf("field order changed the bytes:\n a=%s\n b=%s", want, got)
	}
}

// TestCanonicalOmitsDefaults proves rule 3: every optional field's omitted
// default is normalized away, and null/empty-array are treated as absent, while
// required fields survive at their zero value.
func TestCanonicalOmitsDefaults(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		want     string
		wantNots []string
	}{
		{
			name: "explicit size 0 and empty tree are dropped",
			raw:  `{"manifest":{"repo_key":"r","generated_at":"2026-01-01T00:00:00Z","brain_schema_version":"1.0"},"snapshots":[{"commit":"c1","tree":"","content":{"digest":"sha256:aa","size":0,"media_type":"","path":""}}]}`,
			want: `{"manifest":{"brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z","repo_key":"r"},"snapshots":[{"commit":"c1","content":{"digest":"sha256:aa"}}]}`,
		},
		{
			name: "null optionals are dropped",
			raw:  `{"manifest":{"repo_key":"r","default_branch":null,"generated_at":"2026-01-01T00:00:00Z","brain_schema_version":"1.0","provider":null},"snapshots":[{"commit":"c1","tree":null,"content":{"digest":"sha256:aa","size":null}}]}`,
			want: `{"manifest":{"brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z","repo_key":"r"},"snapshots":[{"commit":"c1","content":{"digest":"sha256:aa"}}]}`,
		},
		{
			name: "empty and null reference slices are dropped",
			raw:  `{"manifest":{"repo_key":"r","generated_at":"2026-01-01T00:00:00Z","brain_schema_version":"1.0"},"snapshots":[],"overlays":null,"facts":[]}`,
			want: `{"manifest":{"brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z","repo_key":"r"}}`,
		},
		{
			name: "required fields are zero-filled, never omitted",
			raw:  `{}`,
			want: `{"manifest":{"brain_schema_version":"","generated_at":"0001-01-01T00:00:00Z","repo_key":""}}`,
		},
		{
			name: "required empty strings survive on references",
			raw:  `{"manifest":{"repo_key":"r","generated_at":"2026-01-01T00:00:00Z","brain_schema_version":"1.0"},"overlays":[{"base_commit":"","head_commit":"","content":{}}]}`,
			want: `{"manifest":{"brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z","repo_key":"r"},"overlays":[{"base_commit":"","content":{"digest":""},"head_commit":""}]}`,
		},
		{
			name:     "explicit negative zero size is dropped",
			raw:      `{"manifest":{"repo_key":"r","generated_at":"2026-01-01T00:00:00Z","brain_schema_version":"1.0"},"facts":[{"branch":"main","content":{"digest":"sha256:aa","size":-0}}]}`,
			wantNots: []string{`"size"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustCanonicalize(t, tc.raw)
			if tc.want != "" && got != tc.want {
				t.Fatalf("canonical mismatch:\n got=%s\nwant=%s", got, tc.want)
			}
			for _, not := range tc.wantNots {
				if strings.Contains(got, not) {
					t.Fatalf("canonical output must not contain %q: %s", not, got)
				}
			}
		})
	}
}

// TestCanonicalDefaultsMatchGoOmitempty cross-checks the field table against Go:
// for each optional field set to its omitted default, the canonical form equals
// the canonical form of an artifact where the field was never set.
func TestCanonicalDefaultsMatchGoOmitempty(t *testing.T) {
	base := func() BrainArtifact {
		ts, _ := time.Parse(time.RFC3339, "2026-06-01T12:00:00Z")
		return *NewBrainArtifact("gh/org/repo", "", ts)
	}

	unset := base()
	unset.Snapshots = nil
	unset.Overlays = []OverlayRef{}
	unset.Facts = nil
	unset.AddSnapshot(SnapshotRef{Commit: "c1", Content: ContentRef{Digest: "sha256:aa"}})

	explicit := base()
	explicit.Manifest.DefaultBranch = ""
	explicit.Manifest.Provider = ""
	explicit.Overlays = []OverlayRef{}
	explicit.AddSnapshot(SnapshotRef{
		Commit:  "c1",
		Tree:    "",
		Content: ContentRef{Digest: "sha256:aa", Size: 0, MediaType: "", Path: ""},
	})

	if got, want := mustCanonicalMarshal(t, explicit), mustCanonicalMarshal(t, unset); got != want {
		t.Fatalf("explicit defaults differ from unset:\n got=%s\nwant=%s", got, want)
	}
}

// TestCanonicalStringEscaping proves rules 1 and 6.
func TestCanonicalStringEscaping(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "html chars stay raw", in: `a<b>c&d`, want: `a<b>c&d`},
		{name: "quote and backslash escaped", in: `he said "hi" \ ok`, want: `he said \"hi\" \\ ok`},
		{name: "short-form controls", in: "a\b\f\n\r\tb", want: `a\b\f\n\r\tb`},
		{name: "other C0 as lowercase u00xx", in: "a\x00\x01\x1fb", want: `a\u0000\u0001\u001fb`},
		{name: "non-ascii stays raw utf8", in: "héllo→🧠", want: "héllo→🧠"},
		{name: "line separators stay raw", in: "a b c", want: "a b c"},
		{name: "del stays raw", in: "a\x7fb", want: "a\x7fb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			art := BrainArtifact{Manifest: BrainManifest{RepoKey: tc.in}}
			got := mustCanonicalMarshal(t, art)
			wantFragment := `"repo_key":"` + tc.want + `"`
			if !strings.Contains(got, wantFragment) {
				t.Fatalf("escaping mismatch:\n got=%s\nwant fragment=%s", got, wantFragment)
			}
			// Go's default encoder would have emitted < etc.
			if strings.Contains(got, `\u003`) {
				t.Fatalf("HTML escaping leaked into canonical output: %s", got)
			}
		})
	}
}

// TestCanonicalTime proves rule 4: UTC, truncated to whole seconds, literal Z.
func TestCanonicalTime(t *testing.T) {
	const want = "2026-06-01T12:00:00Z"

	cases := []struct {
		name string
		in   string
	}{
		{name: "already canonical", in: "2026-06-01T12:00:00Z"},
		{name: "nanoseconds truncate", in: "2026-06-01T12:00:00.123456789Z"},
		{name: "milliseconds truncate", in: "2026-06-01T12:00:00.500Z"},
		{name: "plus zero offset becomes Z", in: "2026-06-01T12:00:00+00:00"},
		{name: "minus zero offset becomes Z", in: "2026-06-01T12:00:00-00:00"},
		{name: "non-zero offset converts to UTC", in: "2026-06-01T17:30:00.5+05:30"},
		{name: "negative offset converts to UTC", in: "2026-06-01T07:00:00-05:00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"` + tc.in + `"}}`
			got := mustCanonicalize(t, raw)
			if !strings.Contains(got, `"generated_at":"`+want+`"`) {
				t.Fatalf("generated_at not normalized to %s: %s", want, got)
			}
			if strings.Contains(got, "+00:00") {
				t.Fatalf("canonical time must use literal Z: %s", got)
			}
		})
	}

	// The typed path agrees with the raw path, including sub-second input.
	sub := time.Date(2026, 6, 1, 12, 0, 0, 123456789, time.UTC)
	whole := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	offset := time.Date(2026, 6, 1, 17, 30, 0, 500000000, time.FixedZone("IST", 5*3600+1800))
	a := mustCanonicalMarshal(t, *NewBrainArtifact("r", "", sub))
	b := mustCanonicalMarshal(t, *NewBrainArtifact("r", "", whole))
	c := mustCanonicalMarshal(t, *NewBrainArtifact("r", "", offset))
	if a != b || b != c {
		t.Fatalf("time normalization disagrees:\n sub=%s\nwhole=%s\noffset=%s", a, b, c)
	}
	if !strings.Contains(b, `"generated_at":"2026-06-01T12:00:00Z"`) {
		t.Fatalf("unexpected canonical time: %s", b)
	}
}

// TestCanonicalNumbers proves rule 5.
func TestCanonicalNumbers(t *testing.T) {
	sized := func(lit string) string {
		return `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"},` +
			`"facts":[{"branch":"main","content":{"digest":"sha256:aa","size":` + lit + `}}]}`
	}

	cases := []struct {
		name    string
		lit     string
		want    string // expected "size":<want> fragment; "" means field omitted
		wantErr bool
	}{
		{name: "plain integer", lit: "4096", want: "4096"},
		{name: "zero omitted", lit: "0", want: ""},
		{name: "negative zero normalized then omitted", lit: "-0", want: ""},
		{name: "negative integer", lit: "-12", want: "-12"},
		// ±2^53 is the largest magnitude every IEEE-754 double-based parser
		// represents exactly; beyond it a conforming JCS verifier re-serializes
		// a different value and the signature silently fails cross-language.
		{name: "max interoperable integer", lit: "9007199254740992", want: "9007199254740992"},
		{name: "min interoperable integer", lit: "-9007199254740992", want: "-9007199254740992"},
		{name: "beyond 2^53 rejected", lit: "9007199254740993", wantErr: true},
		{name: "max int64 rejected", lit: "9223372036854775807", wantErr: true},
		{name: "exponent rejected", lit: "1e2", wantErr: true},
		{name: "fraction rejected", lit: "1.0", wantErr: true},
		{name: "overflow rejected", lit: "9223372036854775808", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonicalize([]byte(sized(tc.lit)))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Canonicalize(size=%s) = %s, want error", tc.lit, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Canonicalize(size=%s): %v", tc.lit, err)
			}
			if tc.want == "" {
				if strings.Contains(string(got), `"size"`) {
					t.Fatalf("size %s should be omitted: %s", tc.lit, got)
				}
				return
			}
			if !strings.Contains(string(got), `"size":`+tc.want) {
				t.Fatalf("size %s not normalized to %s: %s", tc.lit, tc.want, got)
			}
		})
	}

	// Leading zeros and a leading + are already rejected by JSON syntax; assert
	// the canonicalizer surfaces that as an error rather than accepting them.
	for _, lit := range []string{"01", "+1"} {
		if _, err := Canonicalize([]byte(sized(lit))); err == nil {
			t.Fatalf("Canonicalize(size=%s) succeeded, want error", lit)
		}
	}
}

// TestCanonicalUnknownFieldsPreserved proves a newer-minor artifact keeps its
// additive fields (canonically re-encoded) so its bytes remain verifiable.
func TestCanonicalUnknownFieldsPreserved(t *testing.T) {
	raw := `{"manifest":{"repo_key":"r","brain_schema_version":"1.4","generated_at":"2026-01-01T00:00:00Z","future":{"z":1,"a":"<b>"}},"future_top":[1,2]}`
	got := mustCanonicalize(t, raw)
	for _, frag := range []string{
		`"future":{"a":"<b>","z":1}`,
		`"future_top":[1,2]`,
		`"brain_schema_version":"1.4"`,
	} {
		if !strings.Contains(got, frag) {
			t.Fatalf("missing %s in canonical output: %s", frag, got)
		}
	}
	if mustCanonicalize(t, got) != got {
		t.Fatalf("unknown-field canonicalization is not idempotent: %s", got)
	}
}

// TestCanonicalUnmarshal proves the decode helper normalizes the value it
// produces, so re-marshaling it is byte-stable.
func TestCanonicalUnmarshal(t *testing.T) {
	raw := `{"snapshots":[{"content":{"size":2048,"digest":"sha256:aa","media_type":"application/json"},"commit":"c1","tree":""}],
	         "manifest":{"generated_at":"2026-06-01T12:00:00.999+02:00","repo_key":"gh/org/repo","brain_schema_version":"1.0","default_branch":"main"}}`

	var got BrainArtifact
	if err := CanonicalUnmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("CanonicalUnmarshal: %v", err)
	}
	if got.Manifest.RepoKey != "gh/org/repo" || got.Manifest.DefaultBranch != "main" {
		t.Fatalf("manifest not decoded: %#v", got.Manifest)
	}
	wantTime := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if !got.Manifest.GeneratedAt.Equal(wantTime) {
		t.Fatalf("generated_at = %s, want %s", got.Manifest.GeneratedAt, wantTime)
	}
	if len(got.Snapshots) != 1 || got.Snapshots[0].Tree != "" || got.Snapshots[0].Content.Size != 2048 {
		t.Fatalf("snapshots not decoded: %#v", got.Snapshots)
	}

	remarshaled := mustCanonicalMarshal(t, got)
	if want := mustCanonicalize(t, raw); remarshaled != want {
		t.Fatalf("re-marshal not byte-stable:\n got=%s\nwant=%s", remarshaled, want)
	}

	if err := CanonicalUnmarshal([]byte(raw), nil); err == nil {
		t.Fatal("CanonicalUnmarshal(nil) succeeded, want error")
	}
}

// TestCanonicalErrors covers the inputs that cannot be canonicalized without
// changing meaning.
func TestCanonicalErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "malformed json", raw: `{"manifest":`},
		{name: "trailing data", raw: `{} {}`},
		{name: "non-object artifact", raw: `[]`},
		{name: "manifest wrong type", raw: `{"manifest":"nope"}`},
		{name: "repo_key wrong type", raw: `{"manifest":{"repo_key":7}}`},
		{name: "size wrong type", raw: `{"facts":[{"branch":"m","content":{"digest":"d","size":"10"}}]}`},
		{name: "snapshots wrong type", raw: `{"snapshots":{"commit":"c1"}}`},
		{name: "snapshot element not an object", raw: `{"snapshots":["c1"]}`},
		{name: "snapshot element null", raw: `{"snapshots":[null]}`},
		{name: "generated_at not rfc3339", raw: `{"manifest":{"generated_at":"June 1 2026"}}`},
		{name: "generated_at wrong type", raw: `{"manifest":{"generated_at":12345}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Canonicalize([]byte(tc.raw)); err == nil {
				t.Fatalf("Canonicalize(%s) = %s, want error", tc.raw, got)
			}
		})
	}
}

// TestCanonicalFieldTableMatchesStructTags is the drift guard: the normative
// field table in canonical.go must name exactly the JSON tags declared in
// contract.go, with `omitempty` present exactly where the table says a default
// is omitted. Tags are the contract, so a mismatch is a bug in the table, never
// a reason to change a tag.
func TestCanonicalFieldTableMatchesStructTags(t *testing.T) {
	cases := []struct {
		spec *canonicalType
		typ  reflect.Type
	}{
		{canonicalArtifact, reflect.TypeOf(BrainArtifact{})},
		{canonicalManifest, reflect.TypeOf(BrainManifest{})},
		{canonicalContentRef, reflect.TypeOf(ContentRef{})},
		{canonicalSnapshotRef, reflect.TypeOf(SnapshotRef{})},
		{canonicalOverlayRef, reflect.TypeOf(OverlayRef{})},
		{canonicalFactsRef, reflect.TypeOf(FactsRef{})},
	}

	for _, tc := range cases {
		t.Run(tc.spec.Name, func(t *testing.T) {
			if tc.spec.Name != tc.typ.Name() {
				t.Fatalf("field table name %q does not match Go type %q", tc.spec.Name, tc.typ.Name())
			}
			if got, want := len(tc.spec.Fields), tc.typ.NumField(); got != want {
				t.Fatalf("field table has %d fields, struct has %d", got, want)
			}
			for i := 0; i < tc.typ.NumField(); i++ {
				sf := tc.typ.Field(i)
				parts := strings.Split(sf.Tag.Get("json"), ",")
				name := parts[0]
				hasOmitempty := false
				for _, p := range parts[1:] {
					if p == "omitempty" {
						hasOmitempty = true
					}
				}

				var spec *canonicalField
				for j := range tc.spec.Fields {
					if tc.spec.Fields[j].Key == name {
						spec = &tc.spec.Fields[j]
						break
					}
				}
				if spec == nil {
					t.Fatalf("struct field %s has JSON tag %q with no entry in the canonical field table", sf.Name, name)
				}
				if got := spec.Omitted != omitNever; got != hasOmitempty {
					t.Fatalf("%s.%s: table says omitted=%v, struct tag omitempty=%v", tc.spec.Name, name, got, hasOmitempty)
				}
			}
		})
	}
}

// TestCanonicalKeySortIsUTF16 pins the RFC 8785 ordering rule for the unknown
// keys a future minor could introduce: sorting is by UTF-16 code unit, so a
// non-BMP key sorts BEFORE a high-BMP one even though its UTF-8 bytes are
// larger.
func TestCanonicalKeySortIsUTF16(t *testing.T) {
	// U+1F9E0 (surrogate pair 0xD83E,0xDDE0) < U+FB00 in UTF-16 order, but its
	// UTF-8 bytes (0xF0...) sort after U+FB00's (0xEF...).
	raw := "{\"ﬀ\":1,\"\U0001F9E0\":2}"
	got := mustCanonicalize(t, raw)
	iAstral := strings.Index(got, "\U0001F9E0")
	iBMP := strings.Index(got, "ﬀ")
	if iAstral < 0 || iBMP < 0 {
		t.Fatalf("keys missing from output: %s", got)
	}
	if iAstral > iBMP {
		t.Fatalf("keys sorted by UTF-8 bytes, not UTF-16 code units: %s", got)
	}
}

// TestCanonicalizeRejectsDuplicateKeys proves canonical form refuses duplicated
// object members instead of inheriting Go's last-wins decode: a signature over
// re-canonicalized bytes must never validate transported bytes whose
// first-occurrence values a first-wins parser would read instead.
func TestCanonicalizeRejectsDuplicateKeys(t *testing.T) {
	base := `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"}}`
	if _, err := Canonicalize([]byte(base)); err != nil {
		t.Fatalf("baseline artifact must canonicalize: %v", err)
	}
	for name, doc := range map[string]string{
		"top level": `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"},` +
			`"manifest":{"repo_key":"evil","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"}}`,
		"nested known object": `{"manifest":{"repo_key":"evil","repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"}}`,
		"unknown object": `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"},` +
			`"future":{"k":1,"k":2}}`,
		"object inside array": `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"},` +
			`"facts":[{"branch":"main","branch":"other","content":{"digest":"sha256:aa","size":1}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Canonicalize([]byte(doc)); err == nil {
				t.Fatalf("duplicate member accepted: %s", doc)
			} else if !strings.Contains(err.Error(), "duplicate object member") {
				t.Fatalf("wrong error for duplicate member: %v", err)
			}
		})
	}
}

// TestCanonicalTimeAcceptsLowercaseSeparators proves the RFC 3339 forms Go's
// time.Parse rejects but section 5.6 permits — lowercase 't' and 'z' — verify.
func TestCanonicalTimeAcceptsLowercaseSeparators(t *testing.T) {
	doc := `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-06-01t12:00:00z"}}`
	got, err := Canonicalize([]byte(doc))
	if err != nil {
		t.Fatalf("lowercase RFC 3339 separators must canonicalize: %v", err)
	}
	if !strings.Contains(string(got), `"generated_at":"2026-06-01T12:00:00Z"`) {
		t.Fatalf("lowercase separators not normalized: %s", got)
	}
}

// TestCanonicalizeBoundsNestingDepth proves attacker-controlled nesting is an
// error, not a runtime stack overflow: Canonicalize is an entry point for
// untrusted bytes and must never kill the verifying process.
func TestCanonicalizeBoundsNestingDepth(t *testing.T) {
	deep := strings.Repeat("[", 100000) + strings.Repeat("]", 100000)
	doc := `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"},"future":` + deep + `}`
	if _, err := Canonicalize([]byte(doc)); err == nil {
		t.Fatal("pathological nesting must be rejected")
	} else if !strings.Contains(err.Error(), "nesting deeper") {
		t.Fatalf("wrong error for pathological nesting: %v", err)
	}
	// A handful of levels — beyond any real artifact but under the bound — is fine.
	ok := `{"manifest":{"repo_key":"r","brain_schema_version":"1.0","generated_at":"2026-01-01T00:00:00Z"},"future":` +
		strings.Repeat("[", 40) + "1" + strings.Repeat("]", 40) + `}`
	if _, err := Canonicalize([]byte(ok)); err != nil {
		t.Fatalf("legitimate nesting rejected: %v", err)
	}
}
