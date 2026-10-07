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
	return emptyResultBlindSpotOnBranch(brainDir, "")
}

// emptyResultBlindSpotOnBranch is emptyResultBlindSpot for callers that know
// which branch they queried. Facts are stored per branch, so "nothing here" and
// "nothing anywhere" are different answers and only this variant can tell them
// apart.
func emptyResultBlindSpotOnBranch(brainDir, branch string) string {
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
	// A connected repo whose branch never synced may be missing the whole
	// team's facts, and the remedy is one command — more actionable than "try
	// another branch", so it wins over the branch note below. The integrity
	// check above still wins: a store that lost facts is not merely unsynced.
	if note := unsyncedHostedFactsBlindSpot(brainDir, branch); note != "" {
		return note
	}
	// Facts are recorded against the branch the session ran on. Sessions run on
	// feature branches and queries run from main, so an empty result on one
	// branch is the ordinary case rather than a signal about the corpus.
	//
	// THIS IS CHECKED BEFORE THE COVERAGE NOTES, and the order is the whole
	// point. Both can be true at once, and when they are, naming the branch
	// that holds the facts is the only one the caller can act on: it ends with
	// a flag they can retype, where "coverage of older sessions is unknown"
	// ends with nothing to do.
	//
	// Checked last, this feature was unreachable in the case it was built for.
	// `undigested > 0` is true in every repository where a session has been
	// captured since the last distillation -- which is every actively worked
	// repository, and you are on a feature branch BECAUSE you have been
	// working. The fixtures never caught it because they set LastDistilledAt
	// to now with sessions two hours old, so undigested was always 0 and the
	// branch path was never exercised.
	//
	// The integrity check above still wins: a store that lost facts must never
	// be reported as merely the wrong branch.
	if note := otherBranchBlindSpot(brainDir, branch); note != "" {
		return note
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

// otherBranchBlindSpot reports active facts held on branches other than the one
// queried. It returns empty when the caller did not say which branch it
// queried, when the store cannot be read, or when no other branch holds
// anything active — in each case the caller falls through to its normal note.
func otherBranchBlindSpot(brainDir, branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return ""
	}
	total := 0
	var names []string
	for other, records := range byBranch {
		if other == branch {
			continue
		}
		active := 0
		for _, record := range records {
			if record.Status == factStatusActive {
				active++
			}
		}
		if active == 0 {
			continue
		}
		total += active
		names = append(names, other)
	}
	if total == 0 {
		return ""
	}
	// Deterministic: loadAllFactBranches returns a map, and Go randomises map
	// iteration, so an unsorted list would reorder between identical runs.
	sort.Strings(names)
	shown, suffix := names, ""
	if len(shown) > 3 {
		shown, suffix = shown[:3], ", ..."
	}
	return fmt.Sprintf("note: no matching facts on %s, but %d active fact(s) on %d other branch(es): %s%s — retry with --branch",
		branch, total, len(names), strings.Join(shown, ", "), suffix)
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

// otherBranchSessionBlindSpot reports captured sessions held on branches other
// than the one scanned. It is the SESSION-unit twin of otherBranchBlindSpot.
//
// Evidence recall scans canonical sessions on one branch and returns spans, not
// facts. Reusing the fact note here would attach "N active fact(s) on M other
// branch(es)" to a result that is not about facts -- a note that does not match
// the shape of the answer is the hazard the review named on the unified
// surfaces, and it applies just as much to this one.
//
// So the discipline is the same and the unit is the result's own: an empty
// evidence result on a branch holding no sessions, while other branches hold
// them, is the same wrong-branch trap and the same actionable remedy.
//
// Empty when the caller did not say which branch it scanned, when the manifest
// cannot be read, or when no other branch holds a session.
func otherBranchSessionBlindSpot(brainDir, branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return ""
	}
	byBranch := map[string]int{}
	here := 0
	for _, session := range manifest.Sources.Sessions.Sessions {
		name := strings.TrimSpace(session.Branch)
		if name == "" {
			continue
		}
		if name == branch {
			here++
			continue
		}
		byBranch[name]++
	}
	// Sessions on THIS branch mean the scan had material to work with, so an
	// empty result is about the query, not the branch.
	if here > 0 || len(byBranch) == 0 {
		return ""
	}
	total := 0
	names := make([]string, 0, len(byBranch))
	for name, n := range byBranch {
		total += n
		names = append(names, name)
	}
	// Map iteration is randomised, so an unsorted list would differ between
	// identical runs.
	sort.Strings(names)
	return fmt.Sprintf(
		"note: no sessions were captured on %s; %d session(s) on %d other branch(es): %s — retry with --branch <name>",
		branch, total, len(names), strings.Join(names, ", "))
}

// unsyncedHostedFactsBlindSpot notes a branch that has not synced against the
// CONNECTED hosted brain: repo-level hosted.json present, per-branch
// hosted-target.json absent or recording a different target (a marker left by
// a previous connection must not suppress the note). Purely local reads; no
// network.
func unsyncedHostedFactsBlindSpot(brainDir, branch string) string {
	binding, connected, err := readHostedRepoBinding(brainDir)
	if err != nil || !connected {
		return ""
	}
	if checkHostedFactsBinding(brainDir, branch, binding.RepoID, binding.BaseURL) == nil {
		return ""
	}
	// The remedy must be a command that PULLS: `facts sync` only publishes
	// (and would write the marker that silences this note without fetching
	// anything); connect runs the full sync-and-pull.
	return fmt.Sprintf("note: this repository is connected to a hosted brain but branch %q has not synced with it; run 'entire brain connect' or wait for the next daemon tick", branch)
}
