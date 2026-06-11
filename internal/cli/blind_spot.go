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
// Reads only the already-loadable manifest — cheap enough for every empty.
// Empty string when nothing useful can be said (no manifest at all).
func emptyResultBlindSpot(brainDir string) string {
	manifest, err := loadBrainManifest(brainDir)
	// A missing manifest loads as an empty struct, not an error; a manifest
	// with no sources means "not a brain (yet)" — nothing useful to say.
	if err != nil || manifest == nil || manifest.Sources == nil {
		return ""
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
