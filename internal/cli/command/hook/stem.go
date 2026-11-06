package hook

import (
	"strings"

	"github.com/entireio/entire-brain/factmerge"
)

// hookStemDocFrequencyGate is the share of a branch's memories a filename stem
// may appear in before the hook stops treating it as evidence.
//
// pre-edit asks the relevance engine for memories about two terms: the file's
// repo-relative path, which is precise, and the file's STEM, which is not. The
// stem was added so a memory naming "distill_cmd" still matches
// internal/cli/distill_cmd.go. For a distinctive stem that works. For an
// ordinary English word it is a filename collision dressed as a retrieval hit:
// editing internal/entityindex/query.go asks for every memory containing the
// word "query", and a store of engineering memories contains a great many.
//
// The gate is placed from the measured distribution, not from taste. Every
// stem of a 91-file sample of this repository's non-test Go files, counted as
// whole tokens over text and locus across the 1,961 active memories on `main`
// exactly as the code below counts them:
//
//	459 brain · 161 semantic · 125 facts · 77 files · 74 paths · 70 review
//	 67 commit · 65 main · 63 query · 59 merge · 56 store · 41 build
//	 32 regression · 30 bench · 17 version · 17 keys · 15 canonical
//	 13 strict · 13 progress · 12 table · 12 proposals · 12 config
//	 ---- gap: no stem anywhere at 10 or 11 ----
//	  9 redaction · 9 conversation · 5 templates · 5 pause · 4 viz
//	  3 embed · 2 handoff · 2 distill_cmd · 2 dedup · 1 verify_cmd · 0 (rest)
//
// Above the gap the stems are ordinary words that happen to name a file; below
// it they are names. 0.005 of 1,961 is 9.8 memories, which lands inside that
// gap. Graded drops agree with the split: the memories "query" lost were about
// the `brain query` verb, and the ones "merge" lost were about `git merge`.
//
// This band was NOT visible in a first reading of twenty hand-picked stems,
// which showed an empty 4..9 and invited a gate at 0.003 -- a threshold the
// full sample puts in the middle of a populated region, where it would drop
// `conversation` from conversation.go. A gate is a measurement of the whole
// corpus of stems or it is a guess.
//
// The gate only ever REMOVES an over-common stem. The path term is never
// gated, so a file whose stem is dropped is still matched by name, and the
// stem-collision class is all this addresses -- a hit earned by the path
// (go.mod, .brainignore) is a separate precision question.
const hookStemDocFrequencyGate = 0.005

// hookStemIsDistinctive reports whether stem is rare enough across these
// memories to be worth querying for. A stem absent from every memory is
// trivially rare: dropping it would change no result, and keeping the rule to
// one measured quantity is worth more than a second special case.
func hookStemIsDistinctive(stem string, facts []factmerge.Record) bool {
	stem = strings.ToLower(strings.TrimSpace(stem))
	if stem == "" {
		return false
	}
	if len(facts) == 0 {
		// Nothing to measure against. The caller only reaches the engine with
		// memories in hand, so this is the degenerate path; leaving the term in
		// matches the behaviour before this gate existed.
		return true
	}
	matches := 0
	for _, fact := range facts {
		if factMentionsToken(fact, stem) {
			matches++
		}
	}
	return float64(matches)/float64(len(facts)) < hookStemDocFrequencyGate
}

// factMentionsToken counts a memory once, however many times it says the word,
// across the same two fields the relevance engine reads.
func factMentionsToken(fact factmerge.Record, token string) bool {
	if containsWholeToken(strings.ToLower(fact.Text), token) {
		return true
	}
	for _, locus := range fact.Locus {
		if containsWholeToken(strings.ToLower(locus), token) {
			return true
		}
	}
	return false
}

// containsWholeToken reports whether token occurs in text bounded by
// non-identifier bytes on both sides.
//
// Plain strings.Contains would count "queryFacts" and "subquery" as mentions
// of "query", which is the fuzzy matching that produced the collisions in the
// first place. Identifier bounds keep the two legitimate spellings -- a bare
// word and a filename, since "." is not an identifier byte, so "query.go"
// still counts -- while rejecting a longer identifier that merely contains it.
func containsWholeToken(text, token string) bool {
	if token == "" {
		return false
	}
	for offset := 0; offset+len(token) <= len(text); {
		index := strings.Index(text[offset:], token)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(token)
		beforeOK := start == 0 || !isIdentifierByte(text[start-1])
		afterOK := end == len(text) || !isIdentifierByte(text[end])
		if beforeOK && afterOK {
			return true
		}
		offset = start + 1
	}
	return false
}

// isIdentifierByte is the ASCII identifier class. Bytes outside it are left
// non-identifier on purpose: a stem is a filename, and filenames that reach
// this are ASCII, so treating a UTF-8 continuation byte as a boundary only
// makes a match more likely, never less.
func isIdentifierByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_':
		return true
	default:
		return false
	}
}
