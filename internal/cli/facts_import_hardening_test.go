package cli

import (
	"strings"
	"testing"
	"time"
)

// Imported text is UNTRUSTED CONTENT, not merely untrusted size.
//
// The import path bounded how much of the file it would read, but stored
// m.Memory verbatim. printFactLine and the compact brief print fact text
// unescaped, so ANSI escapes, bidi overrides and zero-width runes from another
// tool's export reached the terminal and the agent's prompt.
//
// sanitizeDistilledFactText already guarded agent-produced text. Text from a
// third-party tool has strictly weaker provenance, so it cannot be held to a
// weaker standard.
func TestImportedMemoryTextIsSanitised(t *testing.T) {
	t.Parallel()

	raw := "deploy on \u001b[31mFriday\u001b[0m ‮neven‬​"
	payload := `[{"id":"m1","memory":` + jsonQuoteForTest(raw) + `}]`

	memories, skipped, err := parseMem0([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if skipped != 0 || len(memories) != 1 {
		t.Fatalf("want 1 memory, got %d (skipped %d)", len(memories), skipped)
	}
	got := memories[0].Text
	for name, bad := range map[string]string{
		"ANSI escape":   "\u001b",
		"bidi override": "‮",
		"zero width":    "​",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("%s survived import into fact text: %q", name, got)
		}
	}
	if !strings.Contains(got, "deploy on") {
		t.Fatalf("sanitising destroyed the legible text: %q", got)
	}
	// Changing a fact's text on the way in must be visible, not silent.
	joined := strings.Join(memories[0].Unsupported, " | ")
	if !strings.Contains(joined, "stripped on import") {
		t.Errorf("the text was altered but the report does not say so; Unsupported = %v", memories[0].Unsupported)
	}
}

// Clean text must pass through untouched and raise no note, or the report
// becomes noise everyone learns to ignore.
func TestCleanImportedTextIsUnchangedAndUnflagged(t *testing.T) {
	t.Parallel()

	memories, _, err := parseMem0([]byte(`[{"id":"m1","memory":"retries are capped at three"}]`))
	if err != nil || len(memories) != 1 {
		t.Fatalf("parse: %v (n=%d)", err, len(memories))
	}
	if memories[0].Text != "retries are capped at three" {
		t.Fatalf("clean text was altered: %q", memories[0].Text)
	}
	for _, u := range memories[0].Unsupported {
		if strings.Contains(u, "stripped on import") {
			t.Errorf("clean text raised a stripping note: %v", memories[0].Unsupported)
		}
	}
}

// `facts add --path` refuses a path outside the taxonomy because it would
// "mint immediately-orphaned facts the rest of the system treats as invalid".
// --path-prefix had only a shape check, so it could land a whole import under
// a category GC reports as orphaned while printing "imported N of N".
func TestImportPrefixMustBeUnderAKnownTaxonomyCategory(t *testing.T) {
	t.Parallel()

	f := newVerifyFixture(t)
	now := time.Now().UTC()

	if err := validateImportPrefixAgainstTaxonomy(f.brainDir, "zzznotacategory.inbox", now); err == nil {
		t.Error("an unknown top-level category must be refused, as it is for `facts add --path`")
	}
	// The default (empty) prefix is valid by construction and must not error.
	if err := validateImportPrefixAgainstTaxonomy(f.brainDir, "", now); err != nil {
		t.Errorf("the default prefix must be accepted: %v", err)
	}
	// A known category must pass.
	if err := validateImportPrefixAgainstTaxonomy(f.brainDir, "architecture.imported", now); err != nil {
		t.Errorf("a known category must be accepted: %v", err)
	}
}

func jsonQuoteForTest(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 || r > 0x7e {
				b.WriteString(`\u`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[(r>>12)&0xf])
				b.WriteByte(hex[(r>>8)&0xf])
				b.WriteByte(hex[(r>>4)&0xf])
				b.WriteByte(hex[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// A real export carrying one bad field was reported as the wrong KIND of file,
// and the decoder's own message — which already named the field — was thrown
// away. `{"results":[{"id":1}]}` is unmistakably a mem0 export.
func TestTypeErrorNamesTheFieldInsteadOfDenyingTheFormat(t *testing.T) {
	t.Parallel()

	_, _, err := parseMem0([]byte(`{"results":[{"id":1,"memory":"x"}]}`))
	if err == nil {
		t.Fatal("a numeric id must be an error")
	}
	if strings.Contains(err.Error(), "not a mem0 export") {
		t.Errorf("a real export with one bad field was reported as the wrong format: %v", err)
	}
	if !strings.Contains(err.Error(), "unexpected type") {
		t.Errorf("the error must say a type was wrong: %v", err)
	}
	// The decoder names the field; discarding that leaves the user nothing to
	// look for.
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("the error must name the offending field: %v", err)
	}
}

// A file that is genuinely not an export must still say so, or the fix above
// would turn every shape problem into a type problem.
func TestNonExportStillReportsTheFormat(t *testing.T) {
	t.Parallel()

	_, _, err := parseMem0([]byte(`{"nope":true}`))
	if err == nil {
		t.Fatal("an object with no results array must be an error")
	}
	if !strings.Contains(err.Error(), "not a mem0 export") {
		t.Errorf("a non-export must be reported as such: %v", err)
	}
}

// The origin field is SELF-DECLARED. factmerge/identity.go is explicit that
// records are not authenticated by transport, and a fact can reach this store
// from a shared fact-set head or a hand-written file as easily as from
// `facts import`.
//
// verify skipped its anchor checks for anything claiming origin "imported",
// on the premise that an imported fact's evidence was never here. That premise
// fails when the anchor itself names repository evidence: `facts import`
// writes one anchor carrying only SessionID "<tool>:<foreign id>", never a
// commit. So a record claiming imported WITH a commit anchor was waved through
// as benign.
func TestImportedOriginWithRepositoryEvidenceIsNotWavedThrough(t *testing.T) {
	t.Parallel()

	legit := factRecord{
		Origin:     factOriginImported,
		Provenance: []factAnchor{{SessionID: "mem0:abc123"}},
	}
	if importedAnchorClaimsThisRepo(legit) {
		t.Error("a genuine import carries only its source tool and foreign id; it must not be flagged")
	}

	for name, anchor := range map[string]factAnchor{
		"commit":     {SessionID: "mem0:abc", Commit: "deadbeef"},
		"checkpoint": {SessionID: "mem0:abc", CheckpointID: "cp1"},
		"transcript": {SessionID: "mem0:abc", Transcript: "sessions/main/s1.jsonl"},
		"verified":   {SessionID: "mem0:abc", Verified: true},
	} {
		t.Run(name, func(t *testing.T) {
			fact := factRecord{Origin: factOriginImported, Provenance: []factAnchor{anchor}}
			if !importedAnchorClaimsThisRepo(fact) {
				t.Errorf("an imported fact naming %s evidence must be flagged; the origin and anchor disagree", name)
			}
		})
	}
}
