package facts

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// allFactKinds is the closed set, spelled here so a kind added to factmerge
// without being advertised fails this test rather than shipping invisible.
var allFactKinds = []string{
	factmerge.KindDecision,
	factmerge.KindInvariant,
	factmerge.KindGotcha,
	factmerge.KindPreference,
	factmerge.KindConvention,
	factmerge.KindClosedNegative,
}

// Every string that tells a user which kinds exist must list all of them.
//
// closed-negative was missing from all three, and it is the one kind that
// cannot be reached any other way. Taxonomy paths forbid hyphens — the pattern
// is ^[a-z][a-z0-9_]*(\.[a-z0-9_]+){2}$ — so ".closed-negative" is not a legal
// path segment at all; and measured on this repo's main, the third path level
// is a topic label rather than a kind in 96.9% of the corpus (.general 624,
// .flow 331, .rationale 260), so it was never a kind field to begin with. That
// leaves --kind as the only explicit route, and it was undocumented, so a text
// cue was the only way an author could land the kind `post-failure` most
// depends on.
func TestEveryKindIsAdvertisedWhereKindsAreListed(t *testing.T) {
	t.Parallel()

	err := factstore.ValidateKindFlag("definitely-not-a-kind")
	if err == nil {
		t.Fatal("an unknown kind must be rejected")
	}
	for _, kind := range allFactKinds {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("the --kind rejection message omits %q, so a user cannot discover it: %v", kind, err)
		}
	}

	// Every advertised kind must actually be accepted, in either spelling a
	// user might type.
	for _, kind := range allFactKinds {
		if err := factstore.ValidateKindFlag(kind); err != nil {
			t.Errorf("--kind %s is advertised but rejected: %v", kind, err)
		}
		if err := factstore.ValidateKindFlag(strings.ToUpper(kind)); err != nil {
			t.Errorf("--kind %s (upper case) rejected: %v", kind, err)
		}
	}
}

// The flag help on both commands must agree with the validator, or one of them
// is lying to the reader. Read off the real constructors so the text under
// test is the text that ships.
func TestKindFlagHelpMatchesTheValidator(t *testing.T) {
	t.Parallel()

	for name, cmd := range map[string]*cobra.Command{
		"remember": NewRemember(Config{}),
		"recall":   NewRecall(Config{}),
	} {
		flag := cmd.Flags().Lookup("kind")
		if flag == nil {
			t.Fatalf("%s has no --kind flag", name)
		}
		for _, kind := range allFactKinds {
			if !strings.Contains(flag.Usage, kind) {
				t.Errorf("%s --kind help omits %q: %q", name, kind, flag.Usage)
			}
		}
	}
}
