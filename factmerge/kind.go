package factmerge

import "strings"

// The fixed KIND set (Appendix D): the *what shape of claim* axis, orthogonal to
// WHERE (locus) and the topic taxonomy. Kept small and closed so it is a usable
// retrieval filter rather than another sprawling free-text dimension.
const (
	KindDecision   = "decision"   // a resolved choice + its rationale
	KindInvariant  = "invariant"  // a must-hold rule/constraint
	KindGotcha     = "gotcha"     // a non-obvious trap/footgun
	KindPreference = "preference" // how the user likes work done
	KindConvention = "convention" // a standing process/style norm
	// KindClosedNegative is a question settled *negatively*: what was tried
	// or considered, why it failed or was rejected (the evidence), and when to
	// revisit. Agents are systematically bad at not re-exploring dead ends
	// across sessions; these entries are the densest anti-waste knowledge the
	// brain holds (Phase 2 item 1 — agent-utility plan).
	KindClosedNegative = "closed-negative"
)

// validFactKinds is the closed set; an agent-emitted kind outside it is rejected
// and the deterministic inference is used instead.
var validFactKinds = map[string]struct{}{
	KindDecision:       {},
	KindInvariant:      {},
	KindGotcha:         {},
	KindPreference:     {},
	KindConvention:     {},
	KindClosedNegative: {},
}

// ValidFactKind reports whether kind (case/space-insensitively) is one of the
// closed KIND set.
func ValidFactKind(kind string) bool {
	_, ok := validFactKinds[strings.ToLower(strings.TrimSpace(kind))]
	return ok
}
