package cli

import (
	"fmt"
	"strings"
	"testing"
)

// historyTermsBefore is the implementation this change replaced, kept verbatim
// so the difference can be proven to be nil rather than argued to be.
func historyTermsBefore(text string) []string {
	lower := strings.ToLower(text)
	candidates := []string{
		"decision", "rationale", "validation", "go test", "apply_patch", "exec_command",
		"semantic", "brief", "stale", "checkpoint", "transcript", "schema", "env",
		"provenance", "bundle", "sha256", "review", "hook", "plugin", "workspace",
		"architecture", "contract", "invariant", "tool", "test", "fallback",
	}
	var terms []string
	for _, candidate := range candidates {
		if strings.Contains(lower, candidate) || strings.Contains(normalizeHistorySearchText(lower), normalizeHistorySearchText(candidate)) {
			terms = append(terms, candidate)
		}
	}
	return terms
}

// historyTermsCorpus covers the shapes the history index actually meets, plus
// the ones most likely to expose a difference: camelCase boundaries (which is
// the only thing normalization changes), punctuation, unicode, and candidates
// that are substrings of each other ("test" inside "go test").
func historyTermsCorpus() []string {
	return []string{
		"",
		" ",
		"no candidate words here at all",
		"decision",
		"DECISION",
		"DeCiSiOn",
		"go test ./...",
		"goTest",
		"go_test",
		"go-test",
		"test",
		"a test and a go test together",
		"apply_patch exec_command",
		"applyPatch execCommand",
		"ApplyPatch",
		"semanticBrief",
		"semantic brief",
		"CheckpointTranscriptSchema",
		"checkpoint, transcript; schema.",
		"sha256:abcdef",
		"sha256",
		"SHA256",
		"provenanceBundleReview",
		"architectureContractInvariant",
		"workspacePluginHook",
		"envVar ENV env",
		"stale STALE staleness",
		"fallback fall back fallBack",
		"toolCall tool_call tool-call",
		"validationRationale",
		"a decision about the transcript schema and its provenance bundle",
		"MixedCASEwithDigits123AndMore",
		"ünïcödé decision with accents",
		"日本語 checkpoint テキスト",
		"emoji 🎉 decision 🎉 here",
		strings.Repeat("decision transcript schema ", 40),
		strings.Repeat("x", 5000),
		strings.Repeat("goTestApplyPatch ", 200),
		"\n\tdecision\n\ttranscript\n",
		"trailing space decision ",
		" leading space decision",
		"...decision...",
		"review/hook/plugin",
		"HOOKpluginWORKSPACE",
	}
}

// TestHistoryTermsIsByteIdenticalAfterHoisting is the guard that matters. The
// whole claim of this change is that it computes the same answer with less
// work, so the only thing worth asserting is that the answer did not move.
func TestHistoryTermsIsByteIdenticalAfterHoisting(t *testing.T) {
	t.Parallel()
	for i, text := range historyTermsCorpus() {
		want := historyTermsBefore(text)
		got := historyTerms(text)
		if len(got) != len(want) {
			t.Fatalf("case %d (%.40q): got %v, want %v", i, text, got, want)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("case %d (%.40q) term %d: got %q, want %q\nfull got %v\nfull want %v",
					i, text, j, got[j], want[j], got, want)
			}
		}
	}
}

// TestNormalizedHistoryTermCandidatesStayAligned: the new fast path indexes the
// precomputed slice positionally, so a candidate added to one list and not the
// other would silently compare against the wrong word.
func TestNormalizedHistoryTermCandidatesStayAligned(t *testing.T) {
	t.Parallel()
	if len(normalizedHistoryTermCandidates) != len(historyTermCandidates) {
		t.Fatalf("precomputed candidates are %d, candidates are %d; the positional index is wrong",
			len(normalizedHistoryTermCandidates), len(historyTermCandidates))
	}
	for i, candidate := range historyTermCandidates {
		if want := normalizeHistorySearchText(candidate); normalizedHistoryTermCandidates[i] != want {
			t.Fatalf("candidate %d (%q): precomputed %q, want %q",
				i, candidate, normalizedHistoryTermCandidates[i], want)
		}
	}
}

// TestHistoryTermsNormalizesTheTextOnce counts the passes. Without this the
// hoisting is invisible to the suite: the output guard above stays green
// whether the normalization runs once or twenty-five times, which is exactly
// the shape of a performance change that silently regresses later.
func TestHistoryTermsNormalizesTheTextOnce(t *testing.T) {
	t.Parallel()
	// A text long enough that re-normalizing it is measurable, and containing
	// no candidate as a plain substring, so every candidate must fall through
	// to the normalized comparison -- the worst case for the old loop.
	text := strings.Repeat("zzqqx ", 2000)
	// The baseline is deliberately NOT historyTermsBefore. That version also
	// re-normalized the CANDIDATE each iteration, so measuring against it
	// conflates two separate savings and the guard passes even when the text
	// hoist is reverted -- which is exactly what a weakening run showed. This
	// baseline precomputes the candidates and re-normalizes only the TEXT, so
	// the comparison isolates the one thing this change does.
	before := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			historyTermsTextNormalizedPerCandidate(text)
		}
	})
	after := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			historyTerms(text)
		}
	})
	// Allocation count is the deterministic proxy for "how many times did it
	// walk the text": wall time on a shared machine is not a guard.
	if after.AllocsPerOp() >= before.AllocsPerOp() {
		t.Fatalf("hoisting did not reduce the work: before %d allocs/op, after %d allocs/op",
			before.AllocsPerOp(), after.AllocsPerOp())
	}
	t.Logf("allocs/op %d -> %d (%.1fx fewer); ns/op %d -> %d",
		before.AllocsPerOp(), after.AllocsPerOp(),
		float64(before.AllocsPerOp())/float64(max(after.AllocsPerOp(), 1)),
		before.NsPerOp(), after.NsPerOp())
}

func BenchmarkHistoryTerms(b *testing.B) {
	for _, size := range []int{200, 2000} {
		text := strings.Repeat("a decision about the transcript schema ", size/10)
		b.Run(fmt.Sprintf("after-%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				historyTerms(text)
			}
		})
		b.Run(fmt.Sprintf("before-%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				historyTermsBefore(text)
			}
		})
	}
}

// TestHistoryTermsRawArmIsRedundant records a property discovered while trying
// to weaken this change: dropping the raw-substring arm left the differential
// guard GREEN, and no hand-written case could make it fail.
//
// The reason is structural, not luck. `lower` is already lowercased and the
// candidates are all lowercase literals, so normalizeHistorySearchText treats
// a literal occurrence in the text exactly as it treats the candidate itself.
// A camel boundary can only be inserted inside the candidate's own characters,
// which happens identically on both sides. So raw-true IMPLIES normalized-true,
// and the first arm of the `||` can never change the result.
//
// This is asserted as a property over random input rather than claimed, because
// if it is ever false the differential guard above would be silently weaker
// than it looks. It is NOT an invitation to delete the arm: the raw check is
// the cheap path that short-circuits before the normalized compare.
func TestHistoryTermsRawArmIsRedundant(t *testing.T) {
	t.Parallel()
	alphabet := []rune("abcdefgh 0123456789_-.:/decisontrapchmv日é")
	seed := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return seed
	}
	for iteration := 0; iteration < 4000; iteration++ {
		n := int(next()%80) + 1
		runes := make([]rune, n)
		for i := range runes {
			runes[i] = alphabet[next()%uint64(len(alphabet))]
		}
		lower := strings.ToLower(string(runes))
		normalized := normalizeHistorySearchText(lower)
		for i, candidate := range historyTermCandidates {
			if strings.Contains(lower, candidate) && !strings.Contains(normalized, normalizedHistoryTermCandidates[i]) {
				t.Fatalf("raw-true but normalized-false, so the arms are NOT equivalent:\n"+
					"  text       %q\n  normalized %q\n  candidate  %q -> %q",
					lower, normalized, candidate, normalizedHistoryTermCandidates[i])
			}
		}
	}
}

// historyTermsTextNormalizedPerCandidate is the new implementation with ONLY
// the text hoist reverted: candidates stay precomputed. It exists so the
// pass-count guard measures the hoist and nothing else.
func historyTermsTextNormalizedPerCandidate(text string) []string {
	lower := strings.ToLower(text)
	var terms []string
	for i, candidate := range historyTermCandidates {
		if strings.Contains(lower, candidate) || strings.Contains(normalizeHistorySearchText(lower), normalizedHistoryTermCandidates[i]) {
			terms = append(terms, candidate)
		}
	}
	return terms
}
