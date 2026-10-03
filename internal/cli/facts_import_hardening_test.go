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
