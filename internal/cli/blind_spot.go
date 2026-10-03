package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// distillCoverage reports the last recorded distillation and sessions created
// since it. Older sessions may also be unprocessed; timestamps do not prove coverage.
func distillCoverage(manifest *exportManifest) (last time.Time, undigested int, ok bool) {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Facts == nil {
		return time.Time{}, 0, false
	}
	last = manifest.Sources.Facts.LastDistilledAt
	if last.IsZero() {
		return time.Time{}, 0, false
	}
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
			return fmt.Sprintf("note: distillation coverage is unknown for %d captured session(s); run `entire brain distill`", len(manifest.Sources.Sessions.Sessions))
		}
		return "note: no distillation timestamp is recorded; run `entire brain refresh`, then `distill`"
	}
	if undigested > 0 {
		return fmt.Sprintf("note: last distillation %s; %d session(s) captured since; coverage of older sessions is unknown", last.Format("2006-01-02"), undigested)
	}
	return fmt.Sprintf("note: last distillation %s; complete session coverage is unknown, so this empty result is not evidence of absence", last.Format("2006-01-02"))
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

// briefBranchBlindSpot explains an empty fact set on the CURRENT branch when
// other branches hold facts, and names them.
//
// This is the trap in its most damaging form. Facts are stored per branch, so
// `brain brief` on a feature branch reports "0 facts" while the brain is full
// — and the brief is step 1 of the shipped agent guide, so this is the FIRST
// thing an agent sees. Nothing in the packet said why, which reads as "this
// brain knows nothing about your project" rather than "you are on a branch
// that has none".
//
// Measured on this repository: `recall "distill"` on a feature branch returns
// "no facts", and the same query with `--branch main` returns a fact. An agent
// given the empty answer has no way to discover the second half.
//
// recall and query reach emptyResultBlindSpot for this; brief did not, despite
// that function's own comment claiming to cover every retrieval surface.
func briefBranchBlindSpot(brainDir, branch string) string {
	if brainDir == "" {
		return ""
	}
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil || len(byBranch) == 0 {
		return ""
	}
	others := make([]string, 0, len(byBranch))
	for name, facts := range byBranch {
		if name == branch || len(facts) == 0 {
			continue
		}
		others = append(others, name)
	}
	if len(others) == 0 {
		return ""
	}
	sort.Strings(others)
	// Name at most three: the point is that the facts are reachable, and a
	// long list of branches buries that in a surface that is already dense.
	shown := others
	suffix := ""
	if len(shown) > 3 {
		suffix = fmt.Sprintf(" (and %d more)", len(shown)-3)
		shown = shown[:3]
	}
	return fmt.Sprintf(
		"no facts on branch %q, but %d branch(es) hold facts: %s%s — facts are stored per branch; retry with `--branch %s`",
		branch, len(others), strings.Join(shown, ", "), suffix, shown[0])
}
