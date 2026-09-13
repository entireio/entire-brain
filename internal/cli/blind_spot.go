package cli

import (
	"fmt"
	"time"
)

// blind_spot.go is Phase 2 item 6 (agent-utility plan): blind spots on every
// empty. An empty retrieval result reads as "nothing exists", but what the
// agent needs to know is whether nothing is *indexed* — "no facts; last
// distill 3 days ago; 12 sessions undigested" is actionable (run distill, or
// trust the empty), while bare silence is ambiguous. The line rides only on
// EMPTY results: a result set that found something needs no apology.

// distillCoverage reports when facts were last distilled and how many
// captured sessions postdate that (their insights are not yet in the fact
// store). ok=false when the brain has no facts source at all.
func distillCoverage(manifest *exportManifest) (last time.Time, undigested int, ok bool) {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Facts == nil {
		return time.Time{}, 0, false
	}
	last = manifest.Sources.Facts.GeneratedAt
	if manifest.Sources.Sessions != nil {
		for _, s := range manifest.Sources.Sessions.Sessions {
			if s.CreatedAt.After(last) {
				undigested++
			}
		}
	}
	return last, undigested, true
}

// emptyResultBlindSpot is the one-liner appended to empty retrieval results.
// Reads the manifest and, when facts are declared, cross-checks the fact store
// behind it. Both are cheap, and this only runs on an EMPTY result — the exact
// moment the difference between "nothing was stored" and "what was stored is
// gone" decides whether the line is true.
// Empty string when nothing useful can be said (no manifest at all).
func emptyResultBlindSpot(brainDir string) string {
	manifest, err := loadBrainManifest(brainDir)
	// A missing manifest loads as an empty struct, not an error, and a nil
	// Sources is exactly that case: normalizeBrainManifest gives every manifest
	// it actually parsed a non-nil Sources, so only the absent-manifest path
	// reaches here with nil. That is the NO-BRAIN case, and it is the one this
	// line most needs to cover — see noBrainBlindSpot.
	if err != nil || manifest == nil || manifest.Sources == nil {
		return noBrainBlindSpot(err)
	}
	// Every branch below EXPLAINS an absence, and each explanation is false
	// when the facts were stored and the store lost them. The last one —
	// "the answer may genuinely not be in the brain" — is the most damaging
	// thing this tool can say: it tells a user their memory never existed,
	// immediately after it was destroyed. A known-lossy store must never reach
	// it, so the truth replaces the note here.
	//
	// This single choke point is why recall, search, query and retrieve all
	// inherit the correction without each having to remember to ask.
	if integrity := inspectFactStore(brainDir, manifest.Sources.Facts); !integrity.OK() {
		return "note: this empty result is NOT evidence of absence — " + integrity.Warning()
	}
	last, undigested, ok := distillCoverage(manifest)
	if !ok {
		if manifest.Sources.Sessions != nil && len(manifest.Sources.Sessions.Sessions) > 0 {
			return fmt.Sprintf("note: %d captured session(s) have never been distilled; run `entire brain distill`", len(manifest.Sources.Sessions.Sessions))
		}
		return "note: the brain has no distilled facts; run `entire brain refresh`, then `distill`"
	}
	if undigested > 0 {
		return fmt.Sprintf("note: facts last distilled %s; %d session(s) captured since are not yet distilled", last.Format("2006-01-02"), undigested)
	}
	return fmt.Sprintf("note: facts last distilled %s and all captured sessions are distilled — the answer may genuinely not be in the brain", last.Format("2006-01-02"))
}

// noBrainBlindSpot is the note for a repository whose brain does not exist or
// cannot be read.
//
// This case used to return the empty string — "nothing useful to say" — which
// inverted the whole point of the mechanism. A brain that HAS been built got a
// note explaining its empty result; a repository with no brain at all got a
// bare
//
//	{"branch":"main","query":"Hello","results":[]}
//
// so the note appeared when it was least needed and was missing when it
// mattered most. An agent reads that empty array as "the brain knows nothing
// about this" and writes the tool off, when the true answer is "there is no
// brain here yet". brain_code already refuses this state out loud ("semantic
// index missing; run `entire brain index`"); the text retrieval surface stayed
// silent about it.
//
// Same mechanism as every other line in this file: one `note:` string, carried
// by the existing blind_spot field on JSON responses and printed under the
// "no results" line on text ones. Nothing new is introduced.
func noBrainBlindSpot(loadErr error) string {
	if loadErr != nil {
		return "note: this repository's brain could not be read, so this empty result is not evidence of absence; run `entire brain status` for the reason"
	}
	return "note: no brain has been built for this repository, so nothing is indexed here and this empty result is not evidence of absence; run `entire brain setup`"
}
