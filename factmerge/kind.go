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

// FactKinds is the closed set in a stable order. Every user-facing list of
// kinds — flag help, validation errors, the tree's narrowing hint — renders
// from this one slice, because they did not: four hand-written copies all read
// "decision|invariant|gotcha|preference|convention", and every one of them
// omitted closed-negative, the sixth member.
//
// The omission was not cosmetic. 448 of this repository's own 3,950 memories
// (11%) carry that kind, and a flag whose help does not name a value is a value
// nobody passes; ValidateKindFlag's error named the same five, so a user who
// read "closed-negative" out of `--json` and guessed the flag right was told
// the kind they were looking at was invalid. On `remember` it cost more than
// discovery: an agent reading that help never authors one at all, so the kind
// the set exists to carry — a question settled negatively, which is what stops
// the next session re-exploring a dead end — arrived only by inference.
var FactKinds = []string{
	KindDecision,
	KindInvariant,
	KindGotcha,
	KindPreference,
	KindConvention,
	KindClosedNegative,
}

// FactKindList renders the kinds for flag help and errors. One renderer, so a
// kind cannot be added to the vocabulary and left out of every sentence that
// tells a user it exists — which is exactly how closed-negative stayed hidden
// through six releases.
func FactKindList() string { return strings.Join(FactKinds, "|") }
